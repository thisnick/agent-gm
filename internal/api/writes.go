package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/store"
)

// The write routes of spec section 7.7.
//
// **A handler here contains no business logic.** The order of a mutation is
// spec section 6.2's, internal/core implements it, and every route below
// calls core rather than re-deriving it:
//
//	 3. resolve the account
//	 4. resolve the conversation
//	 5. check the account is usable
//	 6. check the action is actionable   <- unsupported_capability, and NO
//	                                        operation row exists yet
//	 7. look up the idempotency key
//	 8. INSERT the operation and COMMIT
//	 9. call the library
//	10. UPDATE the operation and COMMIT
//	11. respond
//
// Step 6 sitting entirely above the operation insert is what makes "a refused
// action never leaves a record that looks like an attempt" (section 7.8) true
// on every route at once. A handler that checked capabilities itself would
// have to get that ordering right eleven times.

// mutationDTO is the answer shape every mutation shares.
type mutationDTO struct {
	Operation *operationDTO `json:"operation"`
	Changed   bool          `json:"changed"`
	// MessageID is always PRESENT, and null until the remote echo lands --
	// including on a succeeded send (spec sections 6.5, 7.7). It was
	// `omitempty`, so a send whose echo had not arrived answered with no
	// message_id key at all, and a client cannot tell "not yet" from "this
	// route does not have one" by a key's absence.
	MessageID      *string          `json:"message_id"`
	ConversationID *string          `json:"conversation_id,omitempty"`
	Conversation   *conversationDTO `json:"conversation,omitempty"`
	// Effect is the one sentence a destructive route carries, byte for byte
	// the same string the CLI prompts with and the MCP tool description
	// closes on, so no surface can claim less than another.
	Effect string `json:"effect,omitempty"`
}

func mutationFrom(res core.Result) mutationDTO {
	out := mutationDTO{
		Operation: operationOrNull(coreResult{
			Operation: res.Operation, HasOperation: res.HasOperation, Changed: res.Changed,
		}),
		Changed: res.Changed,
	}
	if res.HasOperation {
		out.MessageID = nullable(res.Operation.MessageID)
		out.ConversationID = nullable(res.Operation.ConversationID)
	}
	return out
}

// idempotency reads the key from the two transports spec section 6.3 gives it
// and no others, and refuses a contradiction rather than picking one.
func idempotency(r *Request, clientRequestID string) (string, *apierr.Error) {
	return IdempotencyKeyFrom(r.IdempotencyKeyHeader, clientRequestID)
}

// mutationTarget resolves the conversation a mutation names and returns the
// engine for the account that owns it.
//
// A `conv_` ID already implies its account (spec section 4.1), which is why
// no write route but `POST /v1/conversations` takes `account_id`: there is
// nothing for it to disambiguate. The prefix check runs first, so a `msg_` ID
// in a conversation slot is `invalid_request` naming the parameter and the
// expected prefix, never `not_found`.
func (d *HandlerDeps) mutationTarget(r *Request) (store.Conversation, *core.Account, *apierr.Error) {
	c, e := d.conversation(r)
	if e != nil {
		return store.Conversation{}, nil, e
	}
	eng, err := d.engine(c.AccountID, r.Source)
	if err != nil {
		return store.Conversation{}, nil, apierr.From(err)
	}
	return c, eng, nil
}

// --- start a conversation ---------------------------------------------------

type startConversationBody struct {
	AccountID       string   `json:"account_id"`
	Recipients      []string `json:"recipients"`
	Name            string   `json:"name"`
	ClientRequestID string   `json:"client_request_id"`
}

// conversationsStart is `POST /v1/conversations`: **the one write whose
// target is a phone number rather than an ID**, so nothing else can imply the
// account and `account_id` is required when more than one exists (spec
// section 7.3).
func (d *HandlerDeps) conversationsStart(r *Request) (*Response, error) {
	var body startConversationBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	if len(body.Recipients) == 0 {
		// Before an operation row exists.
		return nil, apierr.MissingParameter("recipients")
	}
	if body.Name != "" && len(body.Recipients) < 2 {
		return nil, apierr.WrongTypeForField("name",
			"omitted for a one-to-one conversation; a name is a group's")
	}

	refs, err := d.accountRefs(r.Ctx)
	if err != nil {
		return nil, err
	}
	res, resErr := core.ResolveAccount(body.AccountID, refs, core.Write)
	if resErr != nil {
		return nil, resErr
	}
	eng, err := d.engine(res.AccountID, r.Source)
	if err != nil {
		return nil, err
	}
	row, err := d.Store.Account(r.Ctx, res.AccountID)
	if err != nil {
		return nil, err
	}
	if err := core.CheckAccountWritable(core.AccountRef{
		ID: row.ID, GoogleAccount: row.GoogleAccount, State: string(row.State),
	}); err != nil {
		return nil, err
	}

	result, err := eng.StartConversation(r.Ctx, core.StartConversationInput{
		Request: core.Request{
			AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source,
		},
		Numbers:   body.Recipients,
		GroupName: body.Name,
	})
	out := mutationFrom(result)
	if result.HasOperation && result.Operation.ConversationID != "" {
		if c, cErr := d.Store.Conversation(r.Ctx, result.Operation.ConversationID); cErr == nil {
			if dto, dErr := d.conversationFrom(r.Ctx, c, nil); dErr == nil {
				out.Conversation = &dto
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return &Response{Data: out}, nil
}

// --- send -------------------------------------------------------------------

type sendBody struct {
	Text             string   `json:"text"`
	UploadIDs        []string `json:"upload_ids"`
	ReplyToMessageID string   `json:"reply_to_message_id"`
	ForceRCS         bool     `json:"force_rcs"`
	ClientRequestID  string   `json:"client_request_id"`
}

// MaxUploadsPerMessage is spec section 10.2's "one attachment per message".
const MaxUploadsPerMessage = 1

func (d *HandlerDeps) messagesSend(r *Request) (*Response, error) {
	var body sendBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	// **One attachment per message**, and two is invalid_request NAMING the
	// limit rather than silently dropping one -- an agent that sent two and
	// saw one arrive would have no way to tell which.
	if len(body.UploadIDs) > MaxUploadsPerMessage {
		err := apierr.Newf(apierr.CodeInvalidRequest,
			"upload_ids accepts at most %d element; Google Messages carries one attachment per message, "+
				"so send the rest as further messages", MaxUploadsPerMessage)
		err.Details = map[string]any{
			"field": "upload_ids", "limit": MaxUploadsPerMessage, "supplied": len(body.UploadIDs),
		}
		return nil, err
	}
	if body.Text == "" && len(body.UploadIDs) == 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"a message needs text, an upload, or both")
	}
	if body.ReplyToMessageID != "" {
		if e := apierr.CheckIDPrefix(body.ReplyToMessageID, "reply_to_message_id", store.PrefixMessage); e != nil {
			return nil, e
		}
	}

	conv, eng, e := d.mutationTarget(r)
	if e != nil {
		return nil, e
	}
	req := core.Request{
		AuthorizationID: r.Auth.ID, Key: key, Body: r.Body,
		ConversationID: conv.ID, Source: r.Source,
	}

	if len(body.UploadIDs) == 1 {
		media, e := d.consumeUploadForSend(r, body.UploadIDs[0], eng)
		if e != nil {
			return nil, e
		}
		result, err := eng.SendMedia(r.Ctx, core.SendMediaInput{
			Request:          req,
			Media:            media,
			Caption:          body.Text,
			ReplyToMessageID: body.ReplyToMessageID,
			ForceRCS:         body.ForceRCS,
		})
		if err != nil {
			return nil, err
		}
		return &Response{Data: mutationFrom(result)}, nil
	}

	result, err := eng.SendText(r.Ctx, core.SendTextInput{
		Request:          req,
		Text:             body.Text,
		ReplyToMessageID: body.ReplyToMessageID,
		ForceRCS:         body.ForceRCS,
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: mutationFrom(result)}, nil
}

// --- typing -----------------------------------------------------------------

// conversationsTyping is `204`. **Fire-and-forget: no operation and no
// idempotency key**, because it has no lasting effect (D15). An operation row
// for a typing notification would be a permanent record of something that
// stopped being true three seconds later.
func (d *HandlerDeps) conversationsTyping(r *Request) (*Response, error) {
	conv, eng, e := d.mutationTarget(r)
	if e != nil {
		return nil, e
	}
	row, err := d.Store.Account(r.Ctx, conv.AccountID)
	if err != nil {
		return nil, err
	}
	if err := core.CheckAccountWritable(core.AccountRef{
		ID: row.ID, GoogleAccount: row.GoogleAccount, State: string(row.State),
	}); err != nil {
		return nil, err
	}
	if err := core.CheckConversationActionable(conv, "type in"); err != nil {
		return nil, err
	}
	if err := eng.Backend.SetTyping(r.Ctx, conv.SourceID); err != nil {
		return nil, err
	}
	return &Response{Status: http.StatusNoContent, Raw: func(http.ResponseWriter) {}}, nil
}

// --- mark read --------------------------------------------------------------

type markReadBody struct {
	MessageID       string `json:"message_id"`
	ClientRequestID string `json:"client_request_id"`
}

func (d *HandlerDeps) conversationsMarkRead(r *Request) (*Response, error) {
	var body markReadBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	if body.MessageID != "" {
		if e := apierr.CheckIDPrefix(body.MessageID, "message_id", store.PrefixMessage); e != nil {
			return nil, e
		}
	}
	conv, eng, e := d.mutationTarget(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.MarkRead(r.Ctx, core.MarkReadInput{
		Request: core.Request{
			AuthorizationID: r.Auth.ID, Key: key, Body: r.Body,
			ConversationID: conv.ID, Source: r.Source,
		},
		MessageID: body.MessageID,
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: mutationFrom(result)}, nil
}

// --- patch ------------------------------------------------------------------

type patchBody struct {
	Folder          *string `json:"folder"`
	Pinned          *bool   `json:"pinned"`
	Unread          *bool   `json:"unread"`
	ClientRequestID string  `json:"client_request_id"`
}

// conversationsUpdate is archive, unarchive, pin, unpin and mark-unread.
//
// **Repeating a change that is already applied returns `changed: false` with
// `operation: null` and calls the backend zero times** (spec section 7.7,
// section 16 Slice 2 test 16). That is not an optimisation: an idempotent
// PATCH that re-sent the change anyway would be indistinguishable from one
// that had to, and would spend a mutation budget on nothing.
func (d *HandlerDeps) conversationsUpdate(r *Request) (*Response, error) {
	var body patchBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	patch := core.ConversationPatch{Pinned: body.Pinned, Unread: body.Unread}
	if body.Folder != nil {
		switch *body.Folder {
		case "archived":
			v := true
			patch.Archived = &v
		case "active":
			v := false
			patch.Archived = &v
		default:
			// spam_blocked is Google's own classification and is not
			// something a caller may assert; offering it here would be a
			// route that silently did nothing.
			return nil, apierr.WrongTypeForField("folder", "either \"active\" or \"archived\"")
		}
	}
	if patch.Archived == nil && patch.Pinned == nil && patch.Unread == nil {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"name at least one of folder, pinned or unread")
	}

	conv, eng, e := d.mutationTarget(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.PatchConversation(r.Ctx, patch, core.Request{
		AuthorizationID: r.Auth.ID, Key: key, Body: r.Body,
		ConversationID: conv.ID, Source: r.Source,
	})
	if err != nil {
		return nil, err
	}
	out := mutationFrom(result)
	if fresh, fErr := d.Store.Conversation(r.Ctx, conv.ID); fErr == nil {
		if dto, dErr := d.conversationFrom(r.Ctx, fresh, nil); dErr == nil {
			out.Conversation = &dto
		}
	}
	return &Response{Data: out}, nil
}

// --- reactions --------------------------------------------------------------

type reactionBody struct {
	Emoji           string `json:"emoji"`
	ClientRequestID string `json:"client_request_id"`
}

// reactionTargetEngine resolves the message a reaction names and the engine
// for its account.
func (d *HandlerDeps) reactionTargetEngine(r *Request) (store.Message, *core.Account, *apierr.Error) {
	m, e := d.message(r)
	if e != nil {
		return store.Message{}, nil, e
	}
	eng, err := d.engine(m.AccountID, r.Source)
	if err != nil {
		return store.Message{}, nil, apierr.From(err)
	}
	return m, eng, nil
}

// reactionsAdd is `ADD`, or `SWITCH` when the owner already has a different
// reaction (D23), or `operation: null` when they already have exactly this
// one. The emoji is canonicalised first (spec section 3.7), so `❤` and `❤️`
// are the same reaction and derive the same `react_` ID.
func (d *HandlerDeps) reactionsAdd(r *Request) (*Response, error) {
	var body reactionBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	if body.Emoji == "" {
		return nil, apierr.MissingParameter("emoji")
	}
	m, eng, e := d.reactionTargetEngine(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.AddReaction(r.Ctx, core.ReactionInput{
		Request:   core.Request{AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source},
		MessageID: m.ID,
		Emoji:     body.Emoji,
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: mutationFrom(result)}, nil
}

// reactionsRemove is `DELETE /v1/messages/{id}/reactions/{emoji}`. **The path
// segment is canonicalised before matching**, so `❤` and `❤️` address the
// same row whichever was used to add it.
func (d *HandlerDeps) reactionsRemove(r *Request) (*Response, error) {
	var body reactionBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	m, eng, e := d.reactionTargetEngine(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.RemoveReaction(r.Ctx, core.ReactionInput{
		Request:   core.Request{AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source},
		MessageID: m.ID,
		Emoji:     core.ReactionPathSegment(r.Path["emoji"]),
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: mutationFrom(result)}, nil
}

// reactionsRemoveByID is `DELETE /v1/reactions/{reaction_id}`. Somebody
// else's is `unsupported_capability` with `reason: "not_my_reaction"`, and
// core refuses it before any operation row exists.
func (d *HandlerDeps) reactionsRemoveByID(r *Request) (*Response, error) {
	var body reactionBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	id, e := pathID(r, "reaction_id", store.PrefixReaction)
	if e != nil {
		return nil, e
	}
	reaction, err := d.Store.Reaction(r.Ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apierr.NotFound("reaction")
	}
	if err != nil {
		return nil, err
	}
	m, err := d.Store.Message(r.Ctx, reaction.MessageID)
	if err != nil {
		return nil, apierr.NotFound("reaction")
	}
	eng, err := d.engine(m.AccountID, r.Source)
	if err != nil {
		return nil, err
	}
	result, err := eng.RemoveReaction(r.Ctx, core.ReactionInput{
		Request:    core.Request{AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source},
		MessageID:  m.ID,
		ReactionID: id,
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: mutationFrom(result)}, nil
}

// --- deletes: messages:delete, and only these two ---------------------------

type deleteBody struct {
	ClientRequestID string `json:"client_request_id"`
}

// messagesDelete carries the effect sentence. There is no other delete and no
// delete option: a request carrying an `action` or `scope` switch is
// `invalid_request` naming it, which the inventory guarantees by not listing
// one (D14).
func (d *HandlerDeps) messagesDelete(r *Request) (*Response, error) {
	var body deleteBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	m, eng, e := d.reactionTargetEngine(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.DeleteMessage(r.Ctx, core.DeleteMessageInput{
		Request:   core.Request{AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source},
		MessageID: m.ID,
	})
	if err != nil {
		return nil, err
	}
	out := mutationFrom(result)
	out.Effect = apierr.EffectMessageDelete
	return &Response{Data: out}, nil
}

// conversationsDelete carries the conversation effect sentence.
//
// Google's delete-for-me and nothing else (D14): the owner's copy of the
// thread goes and the other people in it keep theirs. The effect sentence in
// the response is the same constant the CLI prompts with and the MCP tool
// description ends with, so a model or a human cannot read a broader claim
// off one surface than another.
func (d *HandlerDeps) conversationsDelete(r *Request) (*Response, error) {
	var body deleteBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	conv, eng, e := d.mutationTarget(r)
	if e != nil {
		return nil, e
	}
	result, err := eng.DeleteConversation(r.Ctx, core.DeleteConversationInput{
		Request:        core.Request{AuthorizationID: r.Auth.ID, Key: key, Body: r.Body, Source: r.Source},
		ConversationID: conv.ID,
	})
	if err != nil {
		return nil, err
	}
	out := mutationFrom(result)
	out.Effect = apierr.EffectConversationDelete
	return &Response{Data: out}, nil
}
