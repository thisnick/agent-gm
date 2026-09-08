package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
)

// The listing and search routes of spec section 7.6.
//
// Every route exists in two forms: with an account_id predicate and without
// one. The all-accounts form is not an afterthought -- section 7.3 makes
// omitting account_id the *default* for reads -- which is why section 4.2
// indexes every filtered listing twice and why section 16 test 39 runs
// EXPLAIN QUERY PLAN over both forms of every route.
//
// Ordering is always `sent_at_ms DESC, id DESC`, or the table's documented
// equivalent (`last_activity_ms` for conversations, `created_at_ms` for
// operations and audit rows). It is never ingestion order: a backfilled
// message and a live message of the same age must sort identically
// (spec section 5.4).

// Endpoint names, which a cursor is bound to along with the filter set.
const (
	EndpointConversations = "conversations.list"
	EndpointMessages      = "messages.list"
	EndpointSearch        = "search.messages"
	EndpointContacts      = "contacts.list"
	EndpointOperations    = "operations.list"
	EndpointAudit         = "audit.list"
)

// DefaultLimit and MaxLimit are section 7.4's page bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

func clampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// builder accumulates a WHERE clause and its arguments.
type builder struct {
	sb   strings.Builder
	args []any
}

func (b *builder) and(clause string, args ...any) {
	b.sb.WriteString(" AND ")
	b.sb.WriteString(clause)
	b.args = append(b.args, args...)
}

// after adds the keyset predicate for a descending (ts, id) cursor. It is a
// keyset, never an offset, so ten rows sharing one millisecond still page
// exactly once each (spec sections 5.4, 7.4).
func (b *builder) after(tsColumn, idColumn string, c *Cursor) {
	if c == nil {
		return
	}
	b.and("("+tsColumn+" < ? OR ("+tsColumn+" = ? AND "+idColumn+" < ?))",
		c.SentAtMS, c.SentAtMS, c.ID)
}

func (b *builder) String() string { return b.sb.String() }

func likeContains(s string) string {
	// The caller's text is a literal, so its own wildcards are escaped and
	// the ESCAPE clause is spelled out at every use site.
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// --- conversations -----------------------------------------------------------

// ConversationQuery is the parameter set of GET /v1/conversations.
//
// ParticipantPhone and ParticipantID are the two resolved forms of the route's
// single `participant` parameter; ResolveParticipant turns the caller's
// spelling into one of them. They are kept apart so that the SQL has exactly
// one indexed predicate rather than an OR across three columns, which would
// scan `participants` (spec section 16 test 39).
type ConversationQuery struct {
	AccountID        string
	AllAccounts      bool
	Query            string
	ParticipantPhone string
	ParticipantID    string
	Folder           string
	Type             string
	UnreadOnly       bool
	GroupOnly        *bool
	PinnedOnly       bool
	AfterMS          *int64
	BeforeMS         *int64
	IncludeDeleted   bool
	Cursor           *Cursor
	Limit            int
}

// ErrNoAccountPredicate means a query was built with neither an account_id nor
// the explicit all-accounts intent. The intent has to be stated, so that a
// query can never lose its account predicate by accident (spec section 13.2).
var ErrNoAccountPredicate = errors.New("pass an AccountID or set AllAccounts")

// SQL for the conversations listing. Split out so the EXPLAIN QUERY PLAN test
// can plan the exact statement the route runs.
func (q ConversationQuery) sql() (string, []any) {
	var b builder
	if q.AccountID != "" {
		b.and("c.account_id = ?", q.AccountID)
	}
	if !q.IncludeDeleted {
		b.and("c.deleted_at_ms IS NULL")
	}
	if q.Folder != "" {
		b.and("c.folder = ?", q.Folder)
	}
	if q.Type != "" {
		b.and("c.conversation_type = ?", q.Type)
	}
	if q.UnreadOnly {
		b.and("c.unread = 1")
	}
	if q.GroupOnly != nil {
		b.and("c.is_group = ?", *q.GroupOnly)
	}
	if q.PinnedOnly {
		b.and("c.pinned = 1")
	}
	if q.AfterMS != nil {
		b.and("c.last_activity_ms >= ?", *q.AfterMS)
	}
	if q.BeforeMS != nil {
		b.and("c.last_activity_ms <= ?", *q.BeforeMS)
	}
	if q.Query != "" {
		pattern := likeContains(q.Query)
		b.and(`(c.name LIKE ? ESCAPE '\' OR EXISTS (
		    -- all-accounts: the outer conversation ID scopes its participants.
		    SELECT 1 FROM participants p WHERE p.conversation_id = c.id
		    AND (p.display_name LIKE ? ESCAPE '\' OR p.phone_e164 LIKE ? ESCAPE '\'
		         OR p.formatted_number LIKE ? ESCAPE '\')))`, pattern, pattern, pattern, pattern)
	}
	switch {
	case q.ParticipantID != "":
		b.and(`EXISTS (SELECT 1 FROM participants p
		                -- all-accounts: a part_ ID already carries its account (section 4.1).
		                WHERE p.conversation_id = c.id AND p.id = ?)`, q.ParticipantID)
	case q.ParticipantPhone != "":
		// With account_id this is participants(account_id, phone_e164);
		// without it, participants(phone_e164). Both directions are indexed
		// because a raw number is not account-specific (spec section 7.6).
		//
		// The match is exact OR on the trailing digits, because section 7.6
		// promises `participant` accepts "an E.164 number, the bare digits,
		// a national form". Stored numbers are E.164, so an exact comparison
		// answered `+12025550123` and returned an empty page for
		// `2025550123` and `(202) 555-0123` -- the two forms a human
		// actually types. An empty page is a valid answer, so nothing looked
		// wrong; the live gate found it.
		exact, suffix := PhoneMatch(q.ParticipantPhone)
		if q.AccountID != "" {
			b.and(`EXISTS (SELECT 1 FROM participants p
			                WHERE p.account_id = ? AND p.conversation_id = c.id
			                  AND (p.phone_e164 = ? OR (? <> '' AND p.phone_e164 LIKE ?)))`,
				q.AccountID, exact, suffix, "%"+suffix)
		} else {
			b.and(`EXISTS (SELECT 1 FROM participants p
			                -- all-accounts: a raw number is not account-specific, so it
			                -- matches in every account and the rows carry account_id (7.6).
			                WHERE p.conversation_id = c.id
			                  AND (p.phone_e164 = ? OR (? <> '' AND p.phone_e164 LIKE ?)))`,
				exact, suffix, "%"+suffix)
		}
	}
	b.after("c.last_activity_ms", "c.id", q.Cursor)

	sqlText := `SELECT ` + prefixed(conversationColumns, "c") + `
	  -- all-accounts: section 7.3 makes omitting account_id the default for
	  -- reads, so this listing is indexed and served in both forms.
	  FROM conversations c
	 WHERE 1=1` + b.String() + `
	 ORDER BY c.last_activity_ms DESC, c.id DESC
	 LIMIT ?`
	return sqlText, append(b.args, clampLimit(q.Limit))
}

// ListConversations serves GET /v1/conversations in both forms.
func (s *Store) ListConversations(ctx context.Context, q ConversationQuery) ([]Conversation, error) {
	if q.AccountID == "" && !q.AllAccounts {
		return nil, ErrNoAccountPredicate
	}
	sqlText, args := q.sql()
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CursorPosition is the conversation's position in the newest-activity-first
// order; it is what the next page's cursor encodes.
func (c Conversation) CursorPosition() Cursor { return Cursor{SentAtMS: c.LastActivityMS, ID: c.ID} }

// CursorPosition is the message's position in the newest-first order.
func (m Message) CursorPosition() Cursor { return Cursor{SentAtMS: m.SentAtMS, ID: m.ID} }

// CursorPosition is the contact's position in the newest-first order.
func (c Contact) CursorPosition() Cursor { return Cursor{SentAtMS: c.UpdatedAtMS, ID: c.ID} }

// --- messages ----------------------------------------------------------------

// MessageQuery is the parameter set of GET /v1/messages and
// GET /v1/conversations/{id}/messages.
//
// SenderMe, SenderParticipantID and SenderPhone are the three resolved forms
// of the route's `sender`. `sender=me` is resolved per account rather than to
// one identity: on an account-scoped query it is that account's is_me
// participant, and on a cross-account query it is *each* account's, so
// sender=me returns everything the owner sent from anywhere (section 7.6).
type MessageQuery struct {
	AccountID           string
	AllAccounts         bool
	ConversationID      string
	Direction           string
	SenderMe            bool
	SenderParticipantID string
	SenderPhone         string
	AfterMS             int64
	BeforeMS            int64
	HasAttachment       *bool
	DeliveryState       string
	IncludeSystem       bool
	Cursor              *Cursor
	Limit               int
}

func (q MessageQuery) where() *builder {
	b := &builder{}
	if q.AccountID != "" {
		b.and("m.account_id = ?", q.AccountID)
	}
	if q.ConversationID != "" {
		b.and("m.conversation_id = ?", q.ConversationID)
	}
	if !q.IncludeSystem {
		b.and("m.kind <> 'system'")
	}
	if q.Direction != "" {
		b.and("m.direction = ?", q.Direction)
	}
	if q.DeliveryState != "" {
		b.and("m.delivery_state = ?", q.DeliveryState)
	}
	switch {
	case q.SenderParticipantID != "":
		b.and("m.sender_participant = ?", q.SenderParticipantID)
	case q.SenderMe:
		if q.AccountID != "" {
			b.and(`m.sender_participant IN (SELECT p.id FROM participants p
			                                 WHERE p.account_id = ? AND p.is_me = 1)`, q.AccountID)
		} else {
			b.and(`m.sender_participant IN (SELECT p.id FROM participants p
			                                 -- all-accounts: sender=me is resolved per account,
			                                 -- so a cross-account query means EVERY account's
			                                 -- own participant (section 7.6).
			                                 WHERE p.is_me = 1)`)
		}
	case q.SenderPhone != "":
		// The same three accepted forms as `participant` -- see PhoneMatch.
		exact, suffix := PhoneMatch(q.SenderPhone)
		if q.AccountID != "" {
			b.and(`m.sender_participant IN (SELECT p.id FROM participants p
			                                 WHERE p.account_id = ?
			                                   AND (p.phone_e164 = ?
			                                        OR (? <> '' AND p.phone_e164 LIKE ?)))`,
				q.AccountID, exact, suffix, "%"+suffix)
		} else {
			b.and(`m.sender_participant IN (SELECT p.id FROM participants p
			                                 -- all-accounts: a raw number is not account-specific
			                                 -- and matches in every account (section 7.6).
			                                 WHERE p.phone_e164 = ?
			                                    OR (? <> '' AND p.phone_e164 LIKE ?))`,
				exact, suffix, "%"+suffix)
		}
	}
	if q.AfterMS > 0 {
		b.and("m.sent_at_ms >= ?", q.AfterMS)
	}
	if q.BeforeMS > 0 {
		b.and("m.sent_at_ms <= ?", q.BeforeMS)
	}
	if q.HasAttachment != nil {
		if *q.HasAttachment {
			b.and(`EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`)
		} else {
			b.and(`NOT EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`)
		}
	}
	return b
}

func (q MessageQuery) sql() (string, []any) {
	b := q.where()
	b.after("m.sent_at_ms", "m.id", q.Cursor)
	sqlText := `SELECT ` + prefixed(messageColumns, "m") + `
	  -- all-accounts: section 7.3 makes omitting account_id the default for
	  -- reads, so this listing is indexed and served in both forms.
	  FROM messages m
	 WHERE 1=1` + b.String() + `
	 ORDER BY m.sent_at_ms DESC, m.id DESC
	 LIMIT ?`
	return sqlText, append(b.args, clampLimit(q.Limit))
}

// ListMessages serves the message listings in both forms. A conversation_id
// already implies its account, so it satisfies the account predicate on its
// own (spec section 4.1).
func (s *Store) ListMessages(ctx context.Context, q MessageQuery) ([]Message, error) {
	if q.AccountID == "" && q.ConversationID == "" && !q.AllAccounts {
		return nil, ErrNoAccountPredicate
	}
	sqlText, args := q.sql()
	return s.scanMessages(ctx, sqlText, args...)
}

func (s *Store) scanMessages(ctx context.Context, sqlText string, args ...any) ([]Message, error) {
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessageContext is the neighbourhood of one message, newest-first in Before
// and oldest-first in After, as GET /v1/messages/{id}/context serves it.
type MessageContext struct {
	Before  []Message
	Message Message
	After   []Message
}

const contextBeforeSQL = `SELECT ` + `%COLS%` + `
	  -- all-accounts: scoped by conversation_id, which carries the account (section 4.1).
	  FROM messages m
	 WHERE m.conversation_id = ?
	   AND (m.sent_at_ms < ? OR (m.sent_at_ms = ? AND m.id < ?))
	   AND (? OR m.kind <> 'system')
	 ORDER BY m.sent_at_ms DESC, m.id DESC
	 LIMIT ?`

const contextAfterSQL = `SELECT ` + `%COLS%` + `
	  -- all-accounts: scoped by conversation_id, which carries the account (section 4.1).
	  FROM messages m
	 WHERE m.conversation_id = ?
	   AND (m.sent_at_ms > ? OR (m.sent_at_ms = ? AND m.id > ?))
	   AND (? OR m.kind <> 'system')
	 ORDER BY m.sent_at_ms ASC, m.id ASC
	 LIMIT ?`

func contextSQL(tpl string) string {
	return strings.Replace(tpl, "%COLS%", prefixed(messageColumns, "m"), 1)
}

// MessageContextAround serves GET /v1/messages/{id}/context. before and after
// default to 5 and cap at 100 (spec section 7.4).
func (s *Store) MessageContextAround(ctx context.Context, messageID string, before, after int, includeSystem bool) (MessageContext, error) {
	var out MessageContext
	m, err := s.Message(ctx, messageID)
	if err != nil {
		return out, err
	}
	out.Message = m
	before = clampContext(before)
	after = clampContext(after)

	out.Before, err = s.scanMessages(ctx, contextSQL(contextBeforeSQL),
		m.ConversationID, m.SentAtMS, m.SentAtMS, m.ID, includeSystem, before)
	if err != nil {
		return out, err
	}
	out.After, err = s.scanMessages(ctx, contextSQL(contextAfterSQL),
		m.ConversationID, m.SentAtMS, m.SentAtMS, m.ID, includeSystem, after)
	return out, err
}

func clampContext(n int) int {
	if n <= 0 {
		return 5
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// --- search ------------------------------------------------------------------

// Search modes. There is no mode named after a SQLite extension and no way to
// reach FTS5 operator syntax from the caller's `q` (spec section 7.6).
const (
	SearchModeWords = "words"
	SearchModeExact = "exact"
)

// SearchQuery is the parameter set of GET /v1/search/messages.
type SearchQuery struct {
	Q              string
	Mode           string
	AccountID      string
	AllAccounts    bool
	ConversationID string
	SenderMe       bool
	// SenderParticipantID and SenderPhone are the resolved forms of `sender`.
	SenderParticipantID string
	SenderPhone         string
	AfterMS             int64
	BeforeMS            int64
	HasAttachment       *bool
	Cursor              *Cursor
	Limit               int
}

// SearchResult is one hit.
type SearchResult struct {
	Message Message
	Rank    float64
	Snippet string
}

// FTSQuery turns the caller's text into an FTS5 query that cannot carry
// operator syntax.
//
// Every term is tokenised on non-alphanumeric runes and re-emitted as a
// double-quoted FTS5 string with internal quotes doubled, so `NEAR`, `*`,
// `OR`, a column filter and a parenthesis are all just words. `words` ANDs the
// terms, which is the ordinary case; `exact` builds the same query and the
// caller then filters the page by substring in SQL, so neither mode is an
// unindexed table scan (spec section 7.6).
func FTSQuery(q string) string {
	terms := strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " AND ")
}

func (q SearchQuery) sql() (string, []any, bool) {
	match := FTSQuery(q.Q)
	if match == "" {
		return "", nil, false
	}
	mq := MessageQuery{
		AccountID:           q.AccountID,
		ConversationID:      q.ConversationID,
		SenderMe:            q.SenderMe,
		SenderParticipantID: q.SenderParticipantID,
		SenderPhone:         q.SenderPhone,
		AfterMS:             q.AfterMS,
		BeforeMS:            q.BeforeMS,
		HasAttachment:       q.HasAttachment,
	}
	b := mq.where()
	if q.Mode == SearchModeExact {
		// The page is filtered by substring after the index has narrowed it.
		b.and(`(instr(lower(COALESCE(m.text, '')), lower(?)) > 0
		        OR instr(lower(COALESCE(m.subject, '')), lower(?)) > 0)`, q.Q, q.Q)
	}
	b.after("m.sent_at_ms", "m.id", q.Cursor)

	sqlText := `SELECT ` + prefixed(messageColumns, "m") + `,
	        bm25(messages_fts) AS rank,
	        snippet(messages_fts, 0, '[', ']', '…', 12) AS snip
	  -- all-accounts: section 7.3 makes omitting account_id the default for
	  -- reads, so search is indexed and served in both forms.
	  FROM messages_fts
	  JOIN messages m ON m.rowid = messages_fts.rowid
	 WHERE messages_fts MATCH ?` + b.String() + `
	 ORDER BY m.sent_at_ms DESC, m.id DESC
	 LIMIT ?`
	args := append([]any{match}, b.args...)
	return sqlText, append(args, clampLimit(q.Limit)), true
}

// SearchMessages serves GET /v1/search/messages in both forms.
func (s *Store) SearchMessages(ctx context.Context, q SearchQuery) ([]SearchResult, error) {
	if q.AccountID == "" && q.ConversationID == "" && !q.AllAccounts {
		return nil, ErrNoAccountPredicate
	}
	sqlText, args, ok := q.sql()
	if !ok {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var sender, text, subject, replyTo, opID, tmpID sql.NullString
		m := &r.Message
		if err := rows.Scan(&m.ID, &m.AccountID, &m.ConversationID, &m.SourceID, &m.Kind,
			&m.Direction, &sender, &text, &subject, &m.DeliveryState, &m.DeliveryStateRaw,
			&replyTo, &opID, &tmpID, &m.IsDeleted, &m.SentAtMS, &m.IngestedAtMS,
			&m.UpdatedAtMS, &m.ContentHash, &r.Rank, &r.Snippet); err != nil {
			return nil, err
		}
		m.SenderParticipant = sender.String
		m.Text = text.String
		m.Subject = subject.String
		m.ReplyToMessageID = replyTo.String
		m.OperationID = opID.String
		m.TmpID = tmpID.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- contacts ----------------------------------------------------------------

// ContactQuery is the parameter set of GET /v1/contacts.
type ContactQuery struct {
	AccountID   string
	AllAccounts bool
	Query       string
	Top         bool
	Cursor      *Cursor
	Limit       int
}

func (q ContactQuery) sql() (string, []any) {
	var b builder
	if q.AccountID != "" {
		b.and("c.account_id = ?", q.AccountID)
	}
	if q.Top {
		b.and("c.is_top = 1")
	}
	if q.Query != "" {
		b.and(`(c.display_name LIKE ? ESCAPE '\' OR c.phone_e164 LIKE ? ESCAPE '\')`,
			likeContains(q.Query), likeContains(q.Query))
	}
	b.after("c.updated_at_ms", "c.id", q.Cursor)
	sqlText := `SELECT ` + prefixed(contactColumns, "c") + `
	  -- all-accounts: the same person in two accounts is two contact rows, and
	  -- section 7.3 makes the cross-account listing the default for reads.
	  FROM contacts c
	 WHERE 1=1` + b.String() + `
	 ORDER BY c.updated_at_ms DESC, c.id DESC
	 LIMIT ?`
	return sqlText, append(b.args, clampLimit(q.Limit))
}

// ListContacts serves GET /v1/contacts in both forms.
func (s *Store) ListContacts(ctx context.Context, q ContactQuery) ([]Contact, error) {
	if q.AccountID == "" && !q.AllAccounts {
		return nil, ErrNoAccountPredicate
	}
	sqlText, args := q.sql()
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveParticipant turns the caller's `participant` or `sender` spelling
// into the one indexed form the SQL uses. It accepts a part_ ID, a contact_
// ID (resolved to that contact's number) or a phone number.
//
// A contact_ ID is resolved rather than matched directly because
// participants carries no index on contact_id: matching it there would scan
// the table the section 16 test 39 forbids scanning.
func (s *Store) ResolveParticipant(ctx context.Context, raw string) (participantID, phone string, err error) {
	switch {
	case HasPrefix(raw, PrefixParticipant):
		return raw, "", nil
	case HasPrefix(raw, PrefixContact):
		c, err := s.Contact(ctx, raw)
		if err != nil {
			return "", "", err
		}
		return "", c.PhoneE164, nil
	default:
		return "", raw, nil
	}
}

// prefixed qualifies a bare column list with a table alias.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// PhoneMatch turns what a caller typed into the two comparisons a phone
// lookup needs: the value as given, and its trailing digits.
//
// Section 7.6 says `participant` and `sender` accept "an E.164 number
// (+12025550123), the bare digits, a national form". Stored numbers are
// E.164, so an exact comparison answers only the first, and the other two --
// the forms a human actually types -- returned an empty page. That is the
// worst kind of wrong answer, because an empty page is a valid one.
//
// The suffix is empty for anything under seven digits. A four-digit
// fragment would match half the address book, and a filter that matches
// everything is not a filter; a caller who genuinely means a short code can
// still give the exact stored value.
func PhoneMatch(raw string) (exact, suffix string) {
	digits := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 7 {
		return raw, ""
	}
	// A leading country code is dropped from the SUFFIX only: matching on
	// the last ten digits is what makes +12025550123, 12025550123,
	// 2025550123 and (202) 555-0123 the same person, without asserting
	// anything about which country they are in.
	const national = 10
	if len(digits) > national {
		digits = digits[len(digits)-national:]
	}
	return raw, string(digits)
}
