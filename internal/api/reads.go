package api

import (
	"database/sql"
	"errors"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// The read routes of spec section 7.6. `messages:read` on all of them but
// the three operation and upload routes, which are `messages:write` because
// that is the scope that creates them.

// resolveRead applies spec section 7.3 to a read: `account_id` may be
// omitted, and then the query covers every account. That is a useful default
// for "what came in today" and a dangerous one for a send, which is why the
// two intents differ and why the difference lives in core rather than being
// re-decided per route.
func (d *HandlerDeps) resolveRead(r *Request) (core.Resolution, *apierr.Error) {
	refs, err := d.accountRefs(r.Ctx)
	if err != nil {
		return core.Resolution{}, apierr.From(err)
	}
	res, resErr := core.ResolveAccount(r.Query["account_id"], refs, core.Read)
	if resErr != nil {
		return core.Resolution{}, apierr.From(resErr)
	}
	return res, nil
}

// --- conversations ----------------------------------------------------------

func (d *HandlerDeps) conversationsList(r *Request) (*Response, error) {
	res, e := d.resolveRead(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointConversations)
	if e != nil {
		return nil, e
	}
	folder, e := enumParam(r.Query, "folder", "active", "archived", "spam_blocked", "inbox", "spam")
	if e != nil {
		return nil, e
	}
	// Retain the folder names advertised by older MCP catalogues as aliases.
	switch folder {
	case "inbox":
		folder = "active"
	case "spam":
		folder = "spam_blocked"
	}
	convType, e := enumParam(r.Query, "type", "sms_mms", "rcs", "unknown")
	if e != nil {
		return nil, e
	}
	unreadOnly, e := boolFlag(r.Query, "unread_only")
	if e != nil {
		return nil, e
	}
	groupOnly, e := boolParam(r.Query, "group_only")
	if e != nil {
		return nil, e
	}
	includeDeleted, e := boolFlag(r.Query, "include_deleted")
	if e != nil {
		return nil, e
	}

	pinnedOnly, e := boolFlag(r.Query, "pinned_only")
	if e != nil {
		return nil, e
	}
	after, e := optionalTimeParam(r.Query, "after")
	if e != nil {
		return nil, e
	}
	before, e := optionalTimeParam(r.Query, "before")
	if e != nil {
		return nil, e
	}
	if after != nil && before != nil && *after > *before {
		return nil, apierr.WrongTypeForField("before", "a timestamp at or after after")
	}

	q := store.ConversationQuery{
		AccountID:      res.AccountID,
		AllAccounts:    res.AllAccounts,
		Query:          r.Query["query"],
		Folder:         folder,
		Type:           convType,
		UnreadOnly:     unreadOnly,
		GroupOnly:      groupOnly,
		PinnedOnly:     pinnedOnly,
		AfterMS:        after,
		BeforeMS:       before,
		IncludeDeleted: includeDeleted,
		Cursor:         p.Cursor,
		Limit:          p.Limit,
	}
	if raw := r.Query["participant"]; raw != "" {
		id, phone, err := d.Store.ResolveParticipant(r.Ctx, raw)
		if err != nil {
			return nil, apierr.WrongTypeForField("participant",
				"an E.164 number, its bare digits, or a part_ or contact_ ID")
		}
		q.ParticipantID, q.ParticipantPhone = id, phone
	}

	rows, err := d.Store.ListConversations(r.Ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]conversationDTO, 0, len(rows))
	for _, c := range rows {
		// nil peerTypingUntil: typing is per-conversation live state and is
		// ALWAYS null in a list (spec section 7.6).
		dto, err := d.conversationFrom(r.Ctx, c, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	cursor, e := nextCursor(d, p, rows)
	if e != nil {
		return nil, e
	}
	return &Response{Data: items(out), NextCursor: cursor}, nil
}

// conversation resolves a `{conversation_id}` path parameter, applying the
// prefix rule before the lookup so a wrong-prefix ID is never `not_found`.
func (d *HandlerDeps) conversation(r *Request) (store.Conversation, *apierr.Error) {
	id, e := pathID(r, "conversation_id", store.PrefixConversation)
	if e != nil {
		return store.Conversation{}, e
	}
	c, err := d.Store.Conversation(r.Ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Conversation{}, apierr.NotFound("conversation")
	}
	if err != nil {
		return store.Conversation{}, apierr.From(err)
	}
	return c, nil
}

// conversationsGet is the only route that populates `peer_typing_until`,
// which is in-memory live state and is always null in a list.
func (d *HandlerDeps) conversationsGet(r *Request) (*Response, error) {
	c, e := d.conversation(r)
	if e != nil {
		return nil, e
	}
	dto, err := d.conversationFrom(r.Ctx, c, d.peerTypingUntil(c.ID))
	if err != nil {
		return nil, err
	}
	return &Response{Data: dto}, nil
}

// --- messages ---------------------------------------------------------------

// messageQueryFrom reads the seven filters both message listings share.
func (d *HandlerDeps) messageQueryFrom(r *Request, res core.Resolution, p page) (store.MessageQuery, *apierr.Error) {
	direction, e := enumParam(r.Query, "direction", "incoming", "outgoing")
	if e != nil {
		return store.MessageQuery{}, e
	}
	after, e := timeParam(r.Query, "after")
	if e != nil {
		return store.MessageQuery{}, e
	}
	before, e := timeParam(r.Query, "before")
	if e != nil {
		return store.MessageQuery{}, e
	}
	hasAttachment, e := boolParam(r.Query, "has_attachment")
	if e != nil {
		return store.MessageQuery{}, e
	}
	includeSystem, e := boolFlag(r.Query, "include_system")
	if e != nil {
		return store.MessageQuery{}, e
	}
	deliveryState, e := enumParam(r.Query, "delivery_state", deliveryStates()...)
	if e != nil {
		return store.MessageQuery{}, e
	}
	q := store.MessageQuery{
		AccountID:     res.AccountID,
		AllAccounts:   res.AllAccounts,
		Direction:     direction,
		AfterMS:       after,
		BeforeMS:      before,
		HasAttachment: hasAttachment,
		DeliveryState: deliveryState,
		IncludeSystem: includeSystem,
		Cursor:        p.Cursor,
		Limit:         p.Limit,
	}
	if e := d.resolveSender(r, &q); e != nil {
		return store.MessageQuery{}, e
	}
	return q, nil
}

// resolveSender turns the route's single `sender` parameter into one of its
// three resolved forms.
//
// **`sender=me` means "whichever account's own participant"** (spec section
// 7.6), resolved per account rather than to one identity: on an
// account-scoped query it is that account's `is_me` participant, and on a
// cross-account query it is *each* account's, so `sender=me` returns
// everything the owner sent from anywhere. It is never ambiguous and never an
// error.
func (d *HandlerDeps) resolveSender(r *Request, q *store.MessageQuery) *apierr.Error {
	raw := r.Query["sender"]
	if raw == "" {
		return nil
	}
	if raw == "me" {
		q.SenderMe = true
		return nil
	}
	id, phone, err := d.Store.ResolveParticipant(r.Ctx, raw)
	if err != nil {
		return apierr.WrongTypeForField("sender",
			"the literal \"me\", an E.164 number, its bare digits, or a part_ or contact_ ID")
	}
	q.SenderParticipantID, q.SenderPhone = id, phone
	return nil
}

// deliveryStates is Agent GM's own closed vocabulary (spec section 4.4).
// Google's enum is not served and is not accepted.
func deliveryStates() []string {
	return []string{
		string(gm.DeliveryStateSending), string(gm.DeliveryStateSent),
		string(gm.DeliveryStateDelivered), string(gm.DeliveryStateRead),
		string(gm.DeliveryStateFailed), string(gm.DeliveryStateCanceled),
		string(gm.DeliveryStateDeleted), string(gm.DeliveryStateReceived),
		string(gm.DeliveryStateDownloading), string(gm.DeliveryStateDownloadFailed),
		string(gm.DeliveryStateUnknown),
	}
}

func (d *HandlerDeps) messagesList(r *Request) (*Response, error) {
	res, e := d.resolveRead(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointMessages)
	if e != nil {
		return nil, e
	}
	q, e := d.messageQueryFrom(r, res, p)
	if e != nil {
		return nil, e
	}
	convID, e := queryID(r.Query, "conversation_id", store.PrefixConversation)
	if e != nil {
		return nil, e
	}
	if convID != "" {
		c, err := d.Store.Conversation(r.Ctx, convID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apierr.NotFound("conversation")
		}
		if err != nil {
			return nil, err
		}
		// A conv_ ID belonging to a different account than the account_id
		// given is invalid_request naming both, NEVER not_found (7.3).
		if err := core.CheckObjectAccount("conversation_id", c.ID, c.AccountID, res.AccountID); err != nil {
			return nil, err
		}
		q.ConversationID = c.ID
	}
	return d.serveMessages(r, p, q)
}

func (d *HandlerDeps) conversationMessagesList(r *Request) (*Response, error) {
	c, e := d.conversation(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointMessages)
	if e != nil {
		return nil, e
	}
	// The conversation already implies its account (spec section 4.1), so
	// this listing is account-scoped without the caller naming one.
	q, e := d.messageQueryFrom(r, core.Resolution{AccountID: c.AccountID}, p)
	if e != nil {
		return nil, e
	}
	q.ConversationID = c.ID
	return d.serveMessages(r, p, q)
}

func (d *HandlerDeps) serveMessages(r *Request, p page, q store.MessageQuery) (*Response, error) {
	rows, err := d.Store.ListMessages(r.Ctx, q)
	if err != nil {
		return nil, err
	}
	out, err := d.messagesFrom(r.Ctx, rows)
	if err != nil {
		return nil, err
	}
	cursor, e := nextCursor(d, p, rows)
	if e != nil {
		return nil, e
	}
	return &Response{Data: items(out), NextCursor: cursor}, nil
}

// message resolves a `{message_id}` path parameter.
func (d *HandlerDeps) message(r *Request) (store.Message, *apierr.Error) {
	id, e := pathID(r, "message_id", store.PrefixMessage)
	if e != nil {
		return store.Message{}, e
	}
	m, err := d.Store.Message(r.Ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Message{}, apierr.NotFound("message")
	}
	if err != nil {
		return store.Message{}, apierr.From(err)
	}
	return m, nil
}

func (d *HandlerDeps) messagesGet(r *Request) (*Response, error) {
	m, e := d.message(r)
	if e != nil {
		return nil, e
	}
	dto, err := d.messageFrom(r.Ctx, m)
	if err != nil {
		return nil, err
	}
	return &Response{Data: dto}, nil
}

type messageContextDTO struct {
	Before  []messageDTO `json:"before"`
	Message messageDTO   `json:"message"`
	After   []messageDTO `json:"after"`
}

// messagesContext is `GET /v1/messages/{message_id}/context`. `before` and
// `after` each default to 5 and cap at 100 (spec section 7.4).
func (d *HandlerDeps) messagesContext(r *Request) (*Response, error) {
	m, e := d.message(r)
	if e != nil {
		return nil, e
	}
	before, e := intParam(r.Query, "before", DefaultContext, MaxContext)
	if e != nil {
		return nil, e
	}
	after, e := intParam(r.Query, "after", DefaultContext, MaxContext)
	if e != nil {
		return nil, e
	}
	ctxRows, err := d.Store.MessageContextAround(r.Ctx, m.ID, before, after, false)
	if err != nil {
		return nil, err
	}
	beforeDTOs, err := d.messagesFrom(r.Ctx, ctxRows.Before)
	if err != nil {
		return nil, err
	}
	afterDTOs, err := d.messagesFrom(r.Ctx, ctxRows.After)
	if err != nil {
		return nil, err
	}
	self, err := d.messageFrom(r.Ctx, ctxRows.Message)
	if err != nil {
		return nil, err
	}
	return &Response{Data: messageContextDTO{
		Before: beforeDTOs, Message: self, After: afterDTOs,
	}}, nil
}

// messageAttachmentsList is metadata only. It is folded into `get_message` on
// MCP (spec section 8.2), which is why it has no tool of its own.
func (d *HandlerDeps) messageAttachmentsList(r *Request) (*Response, error) {
	m, e := d.message(r)
	if e != nil {
		return nil, e
	}
	rows, err := d.Store.AttachmentsForMessage(r.Ctx, m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]attachmentSummaryDTO, 0, len(rows))
	for _, a := range rows {
		out = append(out, attachmentSummaryFrom(a))
	}
	return &Response{Data: items(out)}, nil
}

// --- search -----------------------------------------------------------------

type searchCoverage struct {
	Complete             bool    `json:"complete"`
	OldestIndexedAt      *string `json:"oldest_indexed_at"`
	ConversationsPending int     `json:"conversations_pending"`
}

type searchHitDTO struct {
	Message      messageDTO       `json:"message"`
	Rank         float64          `json:"rank"`
	Snippet      string           `json:"snippet"`
	Conversation *conversationDTO `json:"conversation"`
}

// searchMessages is `GET /v1/search/messages`.
//
// `q` is required, and `mode` is `words` or `exact` -- **a search vocabulary,
// not a switch between destructive behaviours**. There is no way to inject
// FTS5 operator syntax and no mode named after a SQLite extension: the store
// builds the FTS query from the caller's words rather than passing them
// through.
func (d *HandlerDeps) searchMessages(r *Request) (*Response, error) {
	q := r.Query["q"]
	if q == "" {
		return nil, apierr.MissingParameter("q")
	}
	res, e := d.resolveRead(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointSearch)
	if e != nil {
		return nil, e
	}
	mode, e := enumParam(r.Query, "mode", store.SearchModeWords, store.SearchModeExact)
	if e != nil {
		return nil, e
	}
	if mode == "" {
		mode = store.SearchModeWords
	}
	after, e := timeParam(r.Query, "after")
	if e != nil {
		return nil, e
	}
	before, e := timeParam(r.Query, "before")
	if e != nil {
		return nil, e
	}
	hasAttachment, e := boolParam(r.Query, "has_attachment")
	if e != nil {
		return nil, e
	}
	convID, e := queryID(r.Query, "conversation_id", store.PrefixConversation)
	if e != nil {
		return nil, e
	}

	sq := store.SearchQuery{
		Q:              q,
		Mode:           mode,
		AccountID:      res.AccountID,
		AllAccounts:    res.AllAccounts,
		ConversationID: convID,
		AfterMS:        after,
		BeforeMS:       before,
		HasAttachment:  hasAttachment,
		Cursor:         p.Cursor,
		Limit:          p.Limit,
	}
	if raw := r.Query["sender"]; raw != "" {
		if raw == "me" {
			sq.SenderMe = true
		} else {
			id, phone, err := d.Store.ResolveParticipant(r.Ctx, raw)
			if err != nil {
				return nil, apierr.WrongTypeForField("sender",
					"the literal \"me\", an E.164 number, its bare digits, or a part_ or contact_ ID")
			}
			sq.SenderParticipantID, sq.SenderPhone = id, phone
		}
	}

	hits, err := d.Store.SearchMessages(r.Ctx, sq)
	if err != nil {
		return nil, err
	}
	out := make([]searchHitDTO, 0, len(hits))
	for _, h := range hits {
		msg, err := d.messageFrom(r.Ctx, h.Message)
		if err != nil {
			return nil, err
		}
		var conv *conversationDTO
		if c, err := d.Store.Conversation(r.Ctx, h.Message.ConversationID); err == nil {
			dto, err := d.conversationFrom(r.Ctx, c, nil)
			if err != nil {
				return nil, err
			}
			conv = &dto
		}
		out = append(out, searchHitDTO{Message: msg, Rank: h.Rank, Snippet: h.Snippet, Conversation: conv})
	}

	coverage, err := d.coverage(r, res)
	if err != nil {
		return nil, err
	}
	// `coverage.complete` is false while backfill is outstanding, and the
	// response carries a `history_incomplete` WARNING rather than an error
	// code, so an empty result during backfill is not read as an absent
	// message.
	if !coverage.Complete {
		r.Warn("history_incomplete")
	}
	msgs := make([]store.Message, 0, len(hits))
	for _, h := range hits {
		msgs = append(msgs, h.Message)
	}
	cursor, cErr := nextCursor(d, p, msgs)
	if cErr != nil {
		return nil, cErr
	}
	return &Response{
		Data:       itemsData{Items: out, Coverage: &coverage},
		NextCursor: cursor,
	}, nil
}

func (d *HandlerDeps) coverage(r *Request, res core.Resolution) (searchCoverage, error) {
	rows, err := d.Store.Accounts(r.Ctx)
	if err != nil {
		return searchCoverage{}, err
	}
	out := searchCoverage{Complete: true}
	for _, row := range rows {
		if res.AccountID != "" && row.ID != res.AccountID {
			continue
		}
		done, total, err := d.Store.BackfillProgress(r.Ctx, row.ID)
		if err != nil {
			return searchCoverage{}, err
		}
		if pending := total - done; pending > 0 {
			out.Complete = false
			out.ConversationsPending += int(pending)
		}
		if row.BackfillCompleteAtMS == 0 {
			out.Complete = false
		}
	}
	return out, nil
}

// --- contacts ---------------------------------------------------------------

func (d *HandlerDeps) contactsList(r *Request) (*Response, error) {
	res, e := d.resolveRead(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointContacts)
	if e != nil {
		return nil, e
	}
	top, e := boolFlag(r.Query, "top")
	if e != nil {
		return nil, e
	}
	rows, err := d.Store.ListContacts(r.Ctx, store.ContactQuery{
		AccountID:   res.AccountID,
		AllAccounts: res.AllAccounts,
		Query:       r.Query["query"],
		Top:         top,
		Cursor:      p.Cursor,
		Limit:       p.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]contactDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, contactFrom(c))
	}
	cursor, e := nextCursor(d, p, rows)
	if e != nil {
		return nil, e
	}
	return &Response{Data: items(out), NextCursor: cursor}, nil
}

// --- operations -------------------------------------------------------------

// operationsList is the caller's OWN operations. `messages:write`, because
// that is the scope that creates them.
func (d *HandlerDeps) operationsList(r *Request) (*Response, error) {
	res, e := d.resolveRead(r)
	if e != nil {
		return nil, e
	}
	p, e := d.paging(r, store.EndpointOperations)
	if e != nil {
		return nil, e
	}
	kind, e := enumParam(r.Query, "kind", operationKinds()...)
	if e != nil {
		return nil, e
	}
	status, e := enumParam(r.Query, "status", "running", "succeeded", "pending", "failed", "unknown")
	if e != nil {
		return nil, e
	}
	terminal, e := boolParam(r.Query, "terminal")
	if e != nil {
		return nil, e
	}
	after, e := timeParam(r.Query, "after")
	if e != nil {
		return nil, e
	}
	before, e := timeParam(r.Query, "before")
	if e != nil {
		return nil, e
	}
	rows, err := d.Store.ListOperations(r.Ctx, store.OperationQuery{
		// Not a filter the caller chooses: it is the caller's own scope.
		AuthorizationID: r.Auth.ID,
		AccountID:       res.AccountID,
		AllAccounts:     res.AllAccounts,
		Kind:            kind,
		Status:          store.OperationStatus(status),
		Terminal:        terminal,
		AfterMS:         after,
		BeforeMS:        before,
		Cursor:          p.Cursor,
		Limit:           p.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]operationDTO, 0, len(rows))
	for _, o := range rows {
		out = append(out, operationFrom(o))
	}
	cursor, e := nextCursor(d, p, rows)
	if e != nil {
		return nil, e
	}
	return &Response{Data: items(out), NextCursor: cursor}, nil
}

func operationKinds() []string {
	return []string{
		core.KindSendText, core.KindSendMedia, core.KindStartConversation,
		core.KindMarkRead, core.KindAddReaction, core.KindRemoveReaction,
		core.KindDeleteMessage, core.KindDeleteConv,
		core.KindArchive, core.KindUnarchive,
	}
}

// operationsGet answers `not_found` for another authorization's ID **with a
// body byte-identical to a genuinely absent one** (spec section 6.5), so an
// ID cannot be probed for existence.
func (d *HandlerDeps) operationsGet(r *Request) (*Response, error) {
	id, e := pathID(r, "operation_id", store.PrefixOperation)
	if e != nil {
		return nil, e
	}
	o, err := d.Store.Operation(r.Ctx, id)
	if errors.Is(err, store.ErrOperationNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil, apierr.NotFound("operation")
	}
	if err != nil {
		return nil, err
	}
	if o.AuthorizationID != r.Auth.ID && !r.Auth.Scopes.Has(authz.ScopeAdmin) {
		return nil, apierr.NotFound("operation")
	}
	return &Response{Data: operationFrom(o)}, nil
}
