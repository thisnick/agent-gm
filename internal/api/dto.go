package api

import (
	"context"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// The DTOs of spec sections 6.5, 7.5 and 7.6, exactly as docs/api.md
// documents them.
//
// **Raw Google values are never served on a public surface.**
// `send_mode_raw`, `delivery_state_raw`, `operations.google_status_raw` and
// Google's own conversation, message and participant IDs are in the database
// and appear on `GET /v1/admin/diagnostics` (spec section 4.1) and NOWHERE
// else. There is deliberately no field for any of them on the types below, so
// serving one would mean adding it here, in the file whose whole subject is
// that they are not served.
//
// Two fields are computed rather than stored (section 4.6): the
// conversation's `type`, and `capabilities.force_rcs`. A caller that wants to
// know whether `force_rcs` will be accepted reads the capability, never a
// mode.

// --- timestamps -------------------------------------------------------------

// rfc3339 renders an epoch-millisecond column as RFC 3339 UTC with
// millisecond precision (spec section 4.4), or null for the zero value. A
// pointer, because "no such moment" and "the Unix epoch" are different facts
// and a caller must be able to tell them apart.
func rfc3339(ms int64) *string {
	if ms == 0 {
		return nil
	}
	s := time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
	return &s
}

// rfc3339Time is rfc3339 for a *time.Time, which is what the accounts
// package's health blocks carry.
func rfc3339Time(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return &s
}

// nullable renders "" as JSON null, so `subject` and `state_reason` are
// absent rather than empty. An empty string and "there is none" are
// different, and a caller branching on truthiness gets the same answer for
// both only by accident.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// --- listings ---------------------------------------------------------------

// itemsData is the shape of every listing's `data`: **a listing puts its rows
// in `data.items`**, never in `data` as a bare array (spec section 7.1), so a
// listing that later needs a sibling of its rows adds a key rather than
// changing its shape.
type itemsData struct {
	Items any `json:"items"`
	// Coverage is search's sibling of the rows, and is omitted everywhere
	// else -- it is the reason `data` is an object at all.
	Coverage *searchCoverage `json:"coverage,omitempty"`
}

// items wraps rows, substituting an empty slice for nil so a page with
// nothing on it serves `[]` rather than `null`.
func items[T any](rows []T) itemsData {
	if rows == nil {
		rows = []T{}
	}
	return itemsData{Items: rows}
}

// --- conversation -----------------------------------------------------------

// participantDTO is one person in a thread. `participants` are the people in
// a thread; `recipients` are the phone numbers you address when creating one
// (spec section 7.6). They are two different things, not two names for one.
type participantDTO struct {
	ID          string  `json:"id"`
	ContactID   *string `json:"contact_id"`
	DisplayName string  `json:"display_name"`
	Phone       *string `json:"phone"`
	IsMe        bool    `json:"is_me"`
}

// capabilitiesDTO is what may be done in a thread. Every field is computed
// from the conversation row; none is stored as a blob a migration could
// leave stale.
type capabilitiesDTO struct {
	SendText           bool `json:"send_text"`
	SendMedia          bool `json:"send_media"`
	Reply              bool `json:"reply"`
	React              bool `json:"react"`
	MarkRead           bool `json:"mark_read"`
	Typing             bool `json:"typing"`
	ForceRCS           bool `json:"force_rcs"`
	Archive            bool `json:"archive"`
	Pin                bool `json:"pin"`
	DeleteMessage      bool `json:"delete_message"`
	DeleteConversation bool `json:"delete_conversation"`
}

// conversationDTO is spec section 7.6's conversation object.
type conversationDTO struct {
	ID              string           `json:"id"`
	AccountID       string           `json:"account_id"`
	Name            string           `json:"name"`
	IsGroup         bool             `json:"is_group"`
	Type            string           `json:"type"`
	Folder          string           `json:"folder"`
	Unread          bool             `json:"unread"`
	Pinned          bool             `json:"pinned"`
	ReadOnly        bool             `json:"read_only"`
	Participants    []participantDTO `json:"participants"`
	PeerTypingUntil *string          `json:"peer_typing_until"`
	IsDeleted       bool             `json:"is_deleted"`
	DeletedAt       *string          `json:"deleted_at"`
	LastActivityAt  *string          `json:"last_activity_at"`
	LatestMessageID *string          `json:"latest_message_id"`
	Capabilities    capabilitiesDTO  `json:"capabilities"`
	CreatedAt       *string          `json:"created_at"`
	UpdatedAt       *string          `json:"updated_at"`
}

// capabilitiesFor derives spec section 4.6's two computed values and the
// nine plain ones.
//
// A read-only or locally deleted thread can do nothing, which is the same
// fact core.CheckConversationActionable refuses on; deriving it here from the
// same two columns means the capability block and the refusal cannot
// disagree. **`force_rcs` is true exactly when the conversation is `rcs` and
// its send mode is automatic** -- which is precisely the `force_rcs_eligible`
// column, computed at ingest from a raw send mode that is never served.
func capabilitiesFor(c store.Conversation) capabilitiesDTO {
	writable := !c.ReadOnly && c.DeletedAtMS == 0
	return capabilitiesDTO{
		SendText:           writable,
		SendMedia:          writable,
		Reply:              writable && c.ConversationType == string(gm.ConversationTypeRCS),
		React:              writable,
		MarkRead:           writable,
		Typing:             writable,
		ForceRCS:           writable && c.ForceRCSEligible,
		Archive:            writable,
		Pin:                writable,
		DeleteMessage:      writable,
		DeleteConversation: writable,
	}
}

// conversationFrom renders one row. peerTypingUntil is in-memory live state
// and is passed in rather than read from the row: it is never persisted, and
// **it is always null in a list** (spec section 7.6), which the list caller
// gets by passing nil.
func (d *HandlerDeps) conversationFrom(ctx context.Context, c store.Conversation, peerTypingUntil *time.Time) (conversationDTO, error) {
	ps, err := d.Store.Participants(ctx, c.ID)
	if err != nil {
		return conversationDTO{}, err
	}
	out := make([]participantDTO, 0, len(ps))
	for _, p := range ps {
		var contactID *string
		if p.ContactID != "" {
			v := store.ContactID(c.AccountID, p.ContactID)
			contactID = &v
		}
		out = append(out, participantDTO{
			ID:          store.ParticipantID(c.ID, p.SourceID),
			ContactID:   contactID,
			DisplayName: p.DisplayName,
			Phone:       nullable(p.PhoneE164),
			IsMe:        p.IsMe,
		})
	}
	convType := c.ConversationType
	if convType == "" {
		convType = "unknown"
	}
	return conversationDTO{
		ID:              c.ID,
		AccountID:       c.AccountID,
		Name:            c.Name,
		IsGroup:         c.IsGroup,
		Type:            convType,
		Folder:          c.Folder,
		Unread:          c.Unread,
		Pinned:          c.Pinned,
		ReadOnly:        c.ReadOnly,
		Participants:    out,
		PeerTypingUntil: rfc3339Time(peerTypingUntil),
		IsDeleted:       c.DeletedAtMS != 0,
		DeletedAt:       rfc3339(c.DeletedAtMS),
		LastActivityAt:  rfc3339(c.LastActivityMS),
		LatestMessageID: nullable(c.LatestMessageID),
		Capabilities:    capabilitiesFor(c),
		CreatedAt:       rfc3339(c.LastActivityMS),
		UpdatedAt:       rfc3339(c.LastActivityMS),
	}, nil
}

// --- message ----------------------------------------------------------------

type senderDTO struct {
	ID   *string `json:"id"`
	IsMe bool    `json:"is_me"`
}

type deliveryDTO struct {
	State     string  `json:"state"`
	Error     *string `json:"error"`
	UpdatedAt *string `json:"updated_at"`
}

// attachmentSummaryDTO is the attachment as it appears inside a message. The
// full object, with its download ticket, is `GET /v1/attachments/{id}`.
type attachmentSummaryDTO struct {
	ID            string  `json:"id"`
	MimeType      string  `json:"mime_type"`
	Filename      *string `json:"filename"`
	Size          int64   `json:"size"`
	Width         *int64  `json:"width"`
	Height        *int64  `json:"height"`
	DownloadState string  `json:"download_state"`
}

// reactionDTO is one person's reaction.
//
// **A reaction whose type has no unicode serves `{"emoji": null, "type":
// "emotify"}` rather than being dropped** (spec sections 3.7, 7.6). Dropping
// it would tell a caller that nobody reacted, which is a different and false
// statement; serving a null emoji says "somebody reacted with something this
// build cannot spell", which is true.
type reactionDTO struct {
	ID            string  `json:"id"`
	Emoji         *string `json:"emoji"`
	Type          string  `json:"type"`
	ParticipantID string  `json:"participant_id"`
	IsMine        bool    `json:"is_mine"`
}

// messageDTO is spec section 7.6's message object.
type messageDTO struct {
	ID               string                 `json:"id"`
	AccountID        string                 `json:"account_id"`
	ConversationID   string                 `json:"conversation_id"`
	Kind             string                 `json:"kind"`
	Direction        string                 `json:"direction"`
	Sender           senderDTO              `json:"sender"`
	Text             string                 `json:"text"`
	Subject          *string                `json:"subject"`
	Delivery         deliveryDTO            `json:"delivery"`
	ReplyToMessageID *string                `json:"reply_to_message_id"`
	OperationID      *string                `json:"operation_id"`
	Attachments      []attachmentSummaryDTO `json:"attachments"`
	Reactions        []reactionDTO          `json:"reactions"`
	IsDeleted        bool                   `json:"is_deleted"`
	SentAt           *string                `json:"sent_at"`
}

func reactionFrom(r store.Reaction) reactionDTO {
	var emoji *string
	if r.HasEmoji && r.Emoji != "" {
		v := r.Emoji
		emoji = &v
	}
	return reactionDTO{
		ID:            r.ID,
		Emoji:         emoji,
		Type:          r.EmojiType,
		ParticipantID: r.ParticipantID,
		IsMine:        r.IsMine,
	}
}

func attachmentSummaryFrom(a store.Attachment) attachmentSummaryDTO {
	var w, h *int64
	if a.Width != 0 {
		v := a.Width
		w = &v
	}
	if a.Height != 0 {
		v := a.Height
		h = &v
	}
	return attachmentSummaryDTO{
		ID:            a.ID,
		MimeType:      a.MimeType,
		Filename:      nullable(a.Filename),
		Size:          a.SizeBytes,
		Width:         w,
		Height:        h,
		DownloadState: a.DownloadState,
	}
}

// messageFrom renders one message with its attachments and reactions.
func (d *HandlerDeps) messageFrom(ctx context.Context, m store.Message) (messageDTO, error) {
	atts, err := d.Store.AttachmentsForMessage(ctx, m.ID)
	if err != nil {
		return messageDTO{}, err
	}
	rs, err := d.Store.ReactionsForMessage(ctx, m.ID)
	if err != nil {
		return messageDTO{}, err
	}
	attDTOs := make([]attachmentSummaryDTO, 0, len(atts))
	for _, a := range atts {
		attDTOs = append(attDTOs, attachmentSummaryFrom(a))
	}
	reactDTOs := make([]reactionDTO, 0, len(rs))
	for _, r := range rs {
		reactDTOs = append(reactDTOs, reactionFrom(r))
	}
	return messageDTO{
		ID:             m.ID,
		AccountID:      m.AccountID,
		ConversationID: m.ConversationID,
		Kind:           m.Kind,
		Direction:      m.Direction,
		Sender: senderDTO{
			ID:   nullable(m.SenderParticipant),
			IsMe: m.Direction == string(gm.DirectionOutgoing),
		},
		Text:    m.Text,
		Subject: nullable(m.Subject),
		Delivery: deliveryDTO{
			// delivery_state_raw is NOT here. It is admin-only (section 4.1).
			State:     m.DeliveryState,
			Error:     nil,
			UpdatedAt: rfc3339(m.UpdatedAtMS),
		},
		ReplyToMessageID: nullable(m.ReplyToMessageID),
		OperationID:      nullable(m.OperationID),
		Attachments:      attDTOs,
		Reactions:        reactDTOs,
		IsDeleted:        m.IsDeleted,
		SentAt:           rfc3339(m.SentAtMS),
	}, nil
}

func (d *HandlerDeps) messagesFrom(ctx context.Context, ms []store.Message) ([]messageDTO, error) {
	out := make([]messageDTO, 0, len(ms))
	for _, m := range ms {
		dto, err := d.messageFrom(ctx, m)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

// --- contact ----------------------------------------------------------------

type contactDTO struct {
	ID          string  `json:"id"`
	AccountID   string  `json:"account_id"`
	DisplayName string  `json:"display_name"`
	Phone       *string `json:"phone"`
	IsTop       bool    `json:"is_top"`
	AvatarHash  *string `json:"avatar_hash"`
	UpdatedAt   *string `json:"updated_at"`
}

func contactFrom(c store.Contact) contactDTO {
	return contactDTO{
		ID:          c.ID,
		AccountID:   c.AccountID,
		DisplayName: c.DisplayName,
		Phone:       nullable(c.PhoneE164),
		IsTop:       c.IsTop,
		AvatarHash:  nullable(c.AvatarHash),
		UpdatedAt:   rfc3339(c.UpdatedAtMS),
	}
}

// --- operation (spec section 6.5) -------------------------------------------

// errorDTO is the section 7.1 error object as it appears inside an operation.
type errorDTO struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// operationDTO is spec section 6.5's object. **This is the only definition;
// every surface serves exactly these fields**, and `google_status_raw` is not
// among them.
type operationDTO struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"account_id"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	Terminal       bool      `json:"terminal"`
	TerminalAt     *string   `json:"terminal_at"`
	CorrectedAt    *string   `json:"corrected_at"`
	ConversationID *string   `json:"conversation_id"`
	MessageID      *string   `json:"message_id"`
	Error          *errorDTO `json:"error"`
	CreatedAt      *string   `json:"created_at"`
	UpdatedAt      *string   `json:"updated_at"`
}

// operationFrom renders one operation row.
//
// `terminal` is derived from `status` through the store's own predicate
// rather than read from the column beside it, so the two can never disagree
// on the wire. A `pending` operation carries `phone_not_responding` with
// `retryable: true` **and `terminal: false`**, which is how a caller tells
// "not yet" from "no".
func operationFrom(o store.Operation) operationDTO {
	var e *errorDTO
	if o.ErrorCode != "" {
		code := apierr.Code(o.ErrorCode)
		retryable := o.ErrorRetryable
		if !o.HasErrorRetryable {
			retryable = apierr.RetryabilityOf(code) == apierr.RetryYes
		}
		e = &errorDTO{
			Code:      o.ErrorCode,
			Message:   o.ErrorMessage,
			Retryable: retryable,
			Details:   map[string]any{},
		}
	}
	return operationDTO{
		ID:             o.ID,
		AccountID:      o.AccountID,
		Kind:           o.Kind,
		Status:         string(o.Status),
		Terminal:       store.OperationTerminal(o.Status),
		TerminalAt:     rfc3339(o.TerminalAtMS),
		CorrectedAt:    rfc3339(o.CorrectedAtMS),
		ConversationID: nullable(o.ConversationID),
		MessageID:      nullable(o.MessageID),
		Error:          e,
		CreatedAt:      rfc3339(o.CreatedAtMS),
		UpdatedAt:      rfc3339(o.UpdatedAtMS),
	}
}

// operationOrNull renders `operation: null` for the no-op mutations of spec
// section 7.7 -- a PATCH that changes nothing, a reaction the owner already
// has. It is a pointer rather than an empty object because "no operation" and
// "an operation with no fields" are different answers, and only one of them
// is honest.
func operationOrNull(r coreResult) *operationDTO {
	if !r.HasOperation {
		return nil
	}
	dto := operationFrom(r.Operation)
	return &dto
}

// coreResult is the shape of core.Result this package renders. It is declared
// as an interface-free local alias so the DTO layer names what it needs
// rather than importing the whole mutation pipeline into a rendering file.
type coreResult struct {
	Operation    store.Operation
	HasOperation bool
	Changed      bool
}

// --- account (spec section 7.5) ---------------------------------------------

// accountListDTO is `GET /v1/accounts`: the first block only. The four health
// blocks are on `GET /v1/accounts/{id}` and on `GET /v1/health`, and the
// asymmetry is deliberate -- a many-account listing stays small.
//
// `google_account` is served because the owner needs to tell their accounts
// apart. It is never an ID and never appears in a URL (spec section 4.1).
type accountListDTO struct {
	ID              string  `json:"id"`
	GoogleAccount   string  `json:"google_account"`
	Label           *string `json:"label"`
	State           string  `json:"state"`
	StateReason     *string `json:"state_reason"`
	PairingID       *string `json:"pairing_id"`
	PhoneID         *string `json:"phone_id"`
	PhoneResponding bool    `json:"phone_responding"`
	PairedAt        *string `json:"paired_at"`
	LastEventAt     *string `json:"last_event_at"`
}

// accountDetailDTO is the list block plus the four health blocks.
type accountDetailDTO struct {
	accountListDTO
	Google   any `json:"google"`
	Backfill any `json:"backfill"`
	Sweep    any `json:"sweep"`
	Counters any `json:"counters"`
}
