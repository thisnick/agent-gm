package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Conversation is one stored thread.
type Conversation struct {
	ID                string
	AccountID         string
	SourceID          string
	Name              string
	IsGroup           bool
	ConversationType  string
	SendModeRaw       string
	Folder            string
	Unread            bool
	Pinned            bool
	ReadOnly          bool
	ForceRCSEligible  bool
	DefaultOutgoingID string
	LatestMessageID   string
	LastActivityMS    int64
	GroupAvatarURL    string
	SIMPayload        []byte
	DeletedAtMS       int64
}

// Message is one stored message.
type Message struct {
	ID                string
	AccountID         string
	ConversationID    string
	SourceID          string
	Kind              string
	Direction         string
	SenderParticipant string
	Text              string
	Subject           string
	DeliveryState     string
	DeliveryStateRaw  int32
	ReplyToMessageID  string
	OperationID       string
	TmpID             string
	IsDeleted         bool
	SentAtMS          int64
	IngestedAtMS      int64
	UpdatedAtMS       int64
	ContentHash       string
}

// ContentHash is computed over the canonical content: the text parts joined
// with a newline, then each media part's media ID and size, then the reaction
// set (spec section 5.3 step 4). An upsert whose content and raw status are
// both unchanged writes nothing at all, so a replay storm does not churn the
// WAL or bump updated_at_ms.
func ContentHash(m gm.Message) string {
	h := sha256.New()
	add := func(format string, args ...any) { _, _ = fmt.Fprintf(h, format, args...) }
	add("text:%s\n", m.Text)
	add("subject:%s\n", m.Subject)
	for _, a := range m.Attachments {
		add("media:%d:%s:%d\n", a.PartIndex, a.MediaID, a.SizeBytes)
	}
	for _, r := range m.Reactions {
		emoji := ""
		if r.Emoji != nil {
			emoji = *r.Emoji
		}
		add("react:%s:%s:%s\n", r.Type, emoji, strings.Join(r.ParticipantIDs, ","))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ErrBackwardTransition means a delivery state moved backwards. The stored
// state is left alone and the event is audited as
// message.status_out_of_order (spec section 4.4).
var ErrBackwardTransition = errors.New("delivery_state may not move backwards")

// UpsertConversation writes a conversation and replaces its participant set,
// stamping account_id on every row. Backfill and live ingestion write through
// this same function; there is no separate backfill writer whose behaviour
// could drift (spec section 5.1).
func (s *Store) UpsertConversation(ctx context.Context, accountID string, c gm.Conversation) (string, error) {
	id := ConversationID(accountID, c.SourceID)
	now := s.clock.Now().UnixMilli()
	activity := c.LastActivity.UnixMilli()
	if c.LastActivity.IsZero() {
		activity = 0
	}
	var simJSON any
	if len(c.SIMPayload) > 0 {
		raw, err := json.Marshal(c.SIMPayload)
		if err != nil {
			return "", err
		}
		simJSON = string(raw)
	}
	var deletedAt any
	if c.Deleted {
		deletedAt = now
	}

	err := s.Write(ctx, func(tx *sql.Tx) error {
		// last_activity_ms is monotonic per conversation: it is only ever
		// moved forward with MAX(existing, new), so a replayed old message
		// cannot make a conversation jump to the top of the list.
		_, err := tx.ExecContext(ctx, `
			INSERT INTO conversations (
			    id, account_id, source_id, name, is_group, conversation_type, send_mode_raw,
			    folder, unread, pinned, read_only, force_rcs_eligible, default_outgoing_id,
			    latest_message_id, last_activity_ms, group_avatar_url, sim_payload_json,
			    deleted_at_ms, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
			    name                = excluded.name,
			    is_group            = excluded.is_group,
			    conversation_type   = excluded.conversation_type,
			    send_mode_raw       = excluded.send_mode_raw,
			    folder              = excluded.folder,
			    unread              = excluded.unread,
			    pinned              = excluded.pinned,
			    read_only           = excluded.read_only,
			    force_rcs_eligible  = excluded.force_rcs_eligible,
			    default_outgoing_id = COALESCE(excluded.default_outgoing_id, conversations.default_outgoing_id),
			    latest_message_id   = COALESCE(excluded.latest_message_id, conversations.latest_message_id),
			    last_activity_ms    = MAX(conversations.last_activity_ms, excluded.last_activity_ms),
			    group_avatar_url    = excluded.group_avatar_url,
			    sim_payload_json    = COALESCE(excluded.sim_payload_json, conversations.sim_payload_json),
			    deleted_at_ms       = excluded.deleted_at_ms,
			    updated_at_ms       = excluded.updated_at_ms`,
			id, accountID, c.SourceID, nullString(c.Name), c.IsGroup, string(c.Type), string(c.SendModeRaw),
			c.Folder.String(), c.Unread, c.Pinned, c.ReadOnly, c.ForceRCSEligible(),
			nullString(c.DefaultOutgoingID), nullString(c.LatestMessageID), activity,
			nullString(c.GroupAvatarURL), simJSON, deletedAt, now, now)
		if err != nil {
			return err
		}
		if len(c.Participants) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM participants WHERE conversation_id = ?`, id); err != nil {
			return err
		}
		for _, p := range c.Participants {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO participants (id, account_id, conversation_id, source_id, contact_id,
				    display_name, first_name, phone_e164, formatted_number, identifier_type,
				    is_me, is_visible)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				ParticipantID(id, p.SourceID), accountID, id, p.SourceID, nullString(p.ContactID),
				nullString(p.DisplayName), nullString(p.FirstName), nullString(p.PhoneE164),
				nullString(p.FormattedNumber), nullString(p.IdentifierType), p.IsMe, p.IsVisible)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// UpsertMessageResult reports what an upsert did, so the caller can tell a
// no-op from a write and audit a refused transition.
type UpsertMessageResult struct {
	ID                string
	Inserted          bool
	Updated           bool
	TransitionRefused bool
	PreviousState     string
}

// UpsertMessage is spec section 5.3 steps 3-5 and the transition rules of
// spec section 4.4. It is the only writer of message rows, shared by backfill
// and the live stream.
func (s *Store) UpsertMessage(ctx context.Context, accountID, convSourceID string, m gm.Message) (UpsertMessageResult, error) {
	var res UpsertMessageResult
	convID := ConversationID(accountID, convSourceID)
	id := MessageID(accountID, convSourceID, m.SourceID)
	res.ID = id
	hash := ContentHash(m)
	now := s.clock.Now().UnixMilli()
	sentAt := m.Timestamp.UnixMilli()
	if m.Timestamp.IsZero() {
		sentAt = 0
	}

	err := s.Write(ctx, func(tx *sql.Tx) error {
		var existingHash, existingState, existingDirection string
		var existingRaw int32
		row := tx.QueryRowContext(ctx,
			`SELECT content_hash, delivery_state, delivery_state_raw, direction FROM messages WHERE id = ?`, id)
		switch err := row.Scan(&existingHash, &existingState, &existingRaw, &existingDirection); {
		case errors.Is(err, sql.ErrNoRows):
			_, err := tx.ExecContext(ctx, `
				INSERT INTO messages (id, account_id, conversation_id, source_id, kind, direction,
				    sender_participant, text, subject, delivery_state, delivery_state_raw,
				    reply_to_message_id, tmp_id, is_deleted, sent_at_ms, ingested_at_ms,
				    updated_at_ms, content_hash)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, accountID, convID, m.SourceID, string(m.Kind), string(m.Direction()),
				nullString(m.ParticipantID), nullString(m.Text), nullString(m.Subject),
				string(m.DeliveryState), m.StatusRaw, nullString(m.ReplyToMessageID),
				nullString(m.TmpID), m.DeliveryState == gm.DeliveryStateDeleted,
				sentAt, now, now, hash)
			if err != nil {
				return err
			}
			res.Inserted = true
			return nil
		case err != nil:
			return err
		}

		res.PreviousState = existingState
		// An upsert whose content and raw status are both unchanged writes
		// nothing at all.
		if existingHash == hash && existingRaw == m.StatusRaw {
			return nil
		}

		newState := m.DeliveryState
		if existingDirection == string(gm.DirectionOutgoing) &&
			!gm.TransitionAllowed(gm.DeliveryState(existingState), m.DeliveryState) {
			// A backward move is refused and the stored state is left alone.
			// delivery_state_raw is always overwritten with whatever Google
			// last said, so a reviewer can see the raw truth.
			res.TransitionRefused = true
			newState = gm.DeliveryState(existingState)
		}

		_, err := tx.ExecContext(ctx, `
			UPDATE messages SET
			    kind                = ?,
			    sender_participant  = COALESCE(?, sender_participant),
			    text                = ?,
			    subject             = ?,
			    delivery_state      = ?,
			    delivery_state_raw  = ?,
			    reply_to_message_id = COALESCE(?, reply_to_message_id),
			    tmp_id              = COALESCE(?, tmp_id),
			    is_deleted          = ?,
			    updated_at_ms       = ?,
			    content_hash        = ?
			WHERE id = ?`,
			string(m.Kind), nullString(m.ParticipantID), nullString(m.Text), nullString(m.Subject),
			string(newState), m.StatusRaw, nullString(m.ReplyToMessageID), nullString(m.TmpID),
			newState == gm.DeliveryStateDeleted, now, hash, id)
		if err != nil {
			return err
		}
		res.Updated = true
		return nil
	})
	return res, err
}

// BumpConversationActivity moves last_activity_ms forward and records the
// latest message. It never moves backwards.
func (s *Store) BumpConversationActivity(ctx context.Context, conversationID, latestMessageID string, activityMS int64) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE conversations
			   SET latest_message_id = CASE WHEN ? > last_activity_ms THEN ? ELSE latest_message_id END,
			       last_activity_ms  = MAX(last_activity_ms, ?),
			       updated_at_ms     = ?
			 WHERE id = ?`, activityMS, nullString(latestMessageID), activityMS, now, conversationID)
		return err
	})
}

const conversationColumns = `id, account_id, source_id, name, is_group, conversation_type,
	send_mode_raw, folder, unread, pinned, read_only, force_rcs_eligible, default_outgoing_id,
	latest_message_id, last_activity_ms, group_avatar_url, sim_payload_json, deleted_at_ms`

func scanConversation(sc interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	var name, outgoing, latest, avatar, simJSON sql.NullString
	var deleted sql.NullInt64
	err := sc.Scan(&c.ID, &c.AccountID, &c.SourceID, &name, &c.IsGroup, &c.ConversationType,
		&c.SendModeRaw, &c.Folder, &c.Unread, &c.Pinned, &c.ReadOnly, &c.ForceRCSEligible,
		&outgoing, &latest, &c.LastActivityMS, &avatar, &simJSON, &deleted)
	if err != nil {
		return c, err
	}
	c.Name = name.String
	c.DefaultOutgoingID = outgoing.String
	c.LatestMessageID = latest.String
	c.GroupAvatarURL = avatar.String
	if simJSON.Valid && simJSON.String != "" {
		// Opaque: stored as it arrived and re-sent verbatim.
		_ = json.Unmarshal([]byte(simJSON.String), &c.SIMPayload)
	}
	c.DeletedAtMS = deleted.Int64
	return c, nil
}

// ConversationFilter selects conversations. AccountID is optional; omitting it
// is the all-accounts form, which spec section 7.3 makes the default for
// reads, so both forms are indexed.
type ConversationFilter struct {
	AccountID string
	Folder    string
	Limit     int
}

// Conversations lists conversations, newest activity first with the ID as the
// tiebreaker. Ordering is never by ingestion order.
func (s *Store) Conversations(ctx context.Context, f ConversationFilter) ([]Conversation, error) {
	q := `SELECT ` + conversationColumns + ` FROM conversations WHERE deleted_at_ms IS NULL`
	var args []any
	if f.AccountID != "" {
		q += ` AND account_id = ?`
		args = append(args, f.AccountID)
	}
	if f.Folder != "" {
		q += ` AND folder = ?`
		args = append(args, f.Folder)
	}
	q += ` ORDER BY last_activity_ms DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(f.Limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
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

// Conversation reads one conversation by its conv_ ID.
func (s *Store) Conversation(ctx context.Context, id string) (Conversation, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE id = ?`, id)
	c, err := scanConversation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("conversation %s: %w", id, sql.ErrNoRows)
	}
	return c, err
}

const messageColumns = `id, account_id, conversation_id, source_id, kind, direction,
	sender_participant, text, subject, delivery_state, delivery_state_raw,
	reply_to_message_id, operation_id, tmp_id, is_deleted, sent_at_ms, ingested_at_ms,
	updated_at_ms, content_hash`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var sender, text, subject, replyTo, opID, tmpID sql.NullString
	err := sc.Scan(&m.ID, &m.AccountID, &m.ConversationID, &m.SourceID, &m.Kind, &m.Direction,
		&sender, &text, &subject, &m.DeliveryState, &m.DeliveryStateRaw,
		&replyTo, &opID, &tmpID, &m.IsDeleted, &m.SentAtMS, &m.IngestedAtMS,
		&m.UpdatedAtMS, &m.ContentHash)
	if err != nil {
		return m, err
	}
	m.SenderParticipant = sender.String
	m.Text = text.String
	m.Subject = subject.String
	m.ReplyToMessageID = replyTo.String
	m.OperationID = opID.String
	m.TmpID = tmpID.String
	return m, nil
}

// MessageFilter selects messages. AccountID is optional; AllAccounts must be
// set explicitly when it is omitted, so a query can never lose its account
// predicate by accident (spec section 13.2).
type MessageFilter struct {
	AccountID      string
	AllAccounts    bool
	ConversationID string
	IncludeSystem  bool
	Limit          int
}

// Messages lists messages, newest first with the ID as the tiebreaker.
// System events are excluded unless IncludeSystem is set.
func (s *Store) Messages(ctx context.Context, f MessageFilter) ([]Message, error) {
	if f.AccountID == "" && f.ConversationID == "" && !f.AllAccounts {
		return nil, errors.New("store.Messages: pass an AccountID, a ConversationID, or set AllAccounts")
	}
	q := `SELECT ` + messageColumns + ` FROM messages WHERE 1=1`
	var args []any
	if f.AccountID != "" {
		q += ` AND account_id = ?`
		args = append(args, f.AccountID)
	}
	if f.ConversationID != "" {
		q += ` AND conversation_id = ?`
		args = append(args, f.ConversationID)
	}
	if !f.IncludeSystem {
		q += ` AND kind <> 'system'`
	}
	q += ` ORDER BY sent_at_ms DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(f.Limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
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

// Message reads one message by its msg_ ID.
func (s *Store) Message(ctx context.Context, id string) (Message, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	return scanMessage(row)
}

// MessageByTmpID finds the message the phone echoed back for one send
// attempt, correlating by tmp_id within the account (spec section 6.3).
func (s *Store) MessageByTmpID(ctx context.Context, accountID, tmpID string) (Message, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE account_id = ? AND tmp_id = ?`, accountID, tmpID)
	return scanMessage(row)
}

// Participants lists one conversation's participant set.
func (s *Store) Participants(ctx context.Context, conversationID string) ([]gm.Participant, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT source_id, contact_id, display_name, first_name, phone_e164,
		        formatted_number, identifier_type, is_me, is_visible
		   FROM participants WHERE conversation_id = ? ORDER BY source_id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []gm.Participant
	for rows.Next() {
		var p gm.Participant
		var contact, name, first, phone, formatted, idType sql.NullString
		if err := rows.Scan(&p.SourceID, &contact, &name, &first, &phone,
			&formatted, &idType, &p.IsMe, &p.IsVisible); err != nil {
			return nil, err
		}
		p.ContactID = contact.String
		p.DisplayName = name.String
		p.FirstName = first.String
		p.PhoneE164 = phone.String
		p.FormattedNumber = formatted.String
		p.IdentifierType = idType.String
		out = append(out, p)
	}
	return out, rows.Err()
}
