package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// One function per mutation kind, so the order of spec section 6.2 cannot be
// got wrong per route. Each of them does steps 4 to 6 -- resolve the
// conversation, check the account is usable, check the action is actionable
// -- and only then enters runOperation, which owns steps 7 to 11.
//
// Step 5 (`CheckAccountWritable`) and step 6 (the section 7.8 checks) both
// return before runOperation is called, so no refusal can leave an operation
// row behind.

// SendTextInput is POST /v1/conversations/{id}/messages for a text.
type SendTextInput struct {
	Request
	Text             string
	ReplyToMessageID string
	ForceRCS         bool
}

// SendText is the send_text mutation.
func (a *Account) SendText(ctx context.Context, in SendTextInput) (Result, error) {
	if a.Freshness != nil {
		return a.freshSend(ctx, in.ConversationID, func(ctx context.Context) (Result, error) { return a.sendText(ctx, in) })
	}
	return a.sendText(ctx, in)
}

func (a *Account) sendText(ctx context.Context, in SendTextInput) (Result, error) {
	conv, err := a.writableConversation(ctx, in.ConversationID, "send")
	if err != nil {
		return Result{}, err
	}
	// Section 7.8, before any operation row exists.
	if err := CheckReplySupported(conv, in.ReplyToMessageID); err != nil {
		return Result{}, err
	}
	if err := CheckForceRCS(conv, in.ForceRCS); err != nil {
		return Result{}, err
	}

	return a.runOperation(ctx, KindSendText, in.Request, func(ctx context.Context, op store.Operation) (Outcome, error) {
		// The three upstream-derived retries, inside this one request, all
		// reusing op.TmpID -- which was committed at step 8, before the first
		// call. A fresh TmpID per attempt would let two attempts of one
		// request correlate to two operations.
		res, err := SendWithRetry(ctx, a.Clock, func(ctx context.Context) (gm.SendResult, error) {
			return a.Backend.SendText(ctx, gm.SendTextRequest{
				ConversationID:   conv.SourceID,
				ParticipantID:    conv.DefaultOutgoingID,
				Text:             in.Text,
				TmpID:            op.TmpID,
				ReplyToMessageID: in.ReplyToMessageID,
				ForceRCS:         in.ForceRCS,
				SIMPayload:       conv.SIMPayload,
			})
		})
		return Outcome{ConversationID: conv.ID, GoogleStatusRaw: int32(res.Status)}, err
	})
}

// SendMediaInput is POST /v1/conversations/{id}/messages carrying an upload.
type SendMediaInput struct {
	Request
	Media            gm.MediaRef
	Caption          string
	ReplyToMessageID string
	ForceRCS         bool
}

// SendMedia is the send_media mutation.
func (a *Account) SendMedia(ctx context.Context, in SendMediaInput) (Result, error) {
	if a.Freshness != nil {
		return a.freshSend(ctx, in.ConversationID, func(ctx context.Context) (Result, error) { return a.sendMedia(ctx, in) })
	}
	return a.sendMedia(ctx, in)
}

func (a *Account) sendMedia(ctx context.Context, in SendMediaInput) (Result, error) {
	conv, err := a.writableConversation(ctx, in.ConversationID, "send")
	if err != nil {
		return Result{}, err
	}
	if err := CheckReplySupported(conv, in.ReplyToMessageID); err != nil {
		return Result{}, err
	}
	if err := CheckForceRCS(conv, in.ForceRCS); err != nil {
		return Result{}, err
	}
	return a.runOperation(ctx, KindSendMedia, in.Request, func(ctx context.Context, op store.Operation) (Outcome, error) {
		// Google's echo of our own media carries no size, so the byte count
		// is recorded here, BEFORE the send, and step 8 of section 5.3 puts
		// it on the attachment row when the echo arrives -- which may be
		// after a restart, which is why it is written down rather than kept
		// in memory.
		if err := a.Store.SetOperationMediaSize(ctx, op.ID, in.Media.SizeBytes); err != nil {
			return Outcome{}, err
		}
		res, err := SendWithRetry(ctx, a.Clock, func(ctx context.Context) (gm.SendResult, error) {
			return a.Backend.SendMedia(ctx, gm.SendMediaRequest{
				ConversationID:   conv.SourceID,
				ParticipantID:    conv.DefaultOutgoingID,
				Media:            in.Media,
				Caption:          in.Caption,
				TmpID:            op.TmpID,
				ReplyToMessageID: in.ReplyToMessageID,
				ForceRCS:         in.ForceRCS,
				SIMPayload:       conv.SIMPayload,
			})
		})
		return Outcome{ConversationID: conv.ID, GoogleStatusRaw: int32(res.Status)}, err
	})
}

// StartConversationInput is POST /v1/conversations.
type StartConversationInput struct {
	Request
	// Numbers is the addressee set. It is the one place a target is named by
	// phone number rather than by ID, which is why account_id is required
	// there and merely accepted elsewhere (section 7.3).
	Numbers   []string
	GroupName string
}

// StartConversation is the start_conversation mutation. The CREATE_RCS retry
// itself lives in the gm adapter, which retries exactly once with
// CreateRCSGroup=true exactly as upstream does; ClassifyResolve then turns
// whatever came back into Agent GM's own vocabulary.
func (a *Account) StartConversation(ctx context.Context, in StartConversationInput) (Result, error) {
	if len(in.Numbers) == 0 {
		return Result{}, apierr.MissingParameter("to")
	}
	return a.runOperation(ctx, KindStartConversation, in.Request, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		res, err := a.Backend.ResolveConversation(ctx, in.Numbers, in.GroupName)
		if err != nil {
			return Outcome{}, err
		}
		compiled := a.Backend.CompiledConfigVersion()
		var live gm.ConfigVersion
		if cfg, cfgErr := a.Backend.FetchConfig(ctx); cfgErr == nil {
			live = cfg.Live
		} else {
			live = compiled
		}
		conv, err := ClassifyResolve(res, compiled, live)
		if err != nil {
			return Outcome{GoogleStatusRaw: int32(res.Status)}, err
		}
		id, err := a.Ingest().IngestConversation(ctx, *conv)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{ConversationID: id, GoogleStatusRaw: int32(res.Status)}, nil
	})
}

// MarkReadInput is POST /v1/conversations/{id}/read.
type MarkReadInput struct {
	Request
	MessageID string
}

// MarkRead is the mark_read mutation.
func (a *Account) MarkRead(ctx context.Context, in MarkReadInput) (Result, error) {
	conv, err := a.writableConversation(ctx, in.ConversationID, "mark_read")
	if err != nil {
		return Result{}, err
	}
	sourceMsg := ""
	if in.MessageID != "" {
		m, err := a.message(ctx, in.MessageID)
		if err != nil {
			return Result{}, err
		}
		sourceMsg = m.SourceID
	}
	return a.runOperation(ctx, KindMarkRead, in.Request, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		return Outcome{ConversationID: conv.ID},
			a.Backend.MarkRead(ctx, conv.SourceID, sourceMsg)
	})
}

// DeleteMessageInput is DELETE /v1/messages/{id}.
type DeleteMessageInput struct {
	Request
	MessageID string
}

// DeleteMessage is the delete_message mutation. Deleting a message the owner
// did not send is `not_my_message`, and it is refused before any operation
// row exists.
func (a *Account) DeleteMessage(ctx context.Context, in DeleteMessageInput) (Result, error) {
	msg, err := a.message(ctx, in.MessageID)
	if err != nil {
		return Result{}, err
	}
	conv, err := a.writableConversation(ctx, msg.ConversationID, "delete")
	if err != nil {
		return Result{}, err
	}
	if err := CheckMyMessage(msg, "delete"); err != nil {
		return Result{}, err
	}
	req := in.Request
	req.ConversationID = conv.ID
	return a.runOperation(ctx, KindDeleteMessage, req, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		return Outcome{ConversationID: conv.ID, MessageID: msg.ID},
			a.Backend.DeleteMessage(ctx, msg.SourceID)
	})
}

// DeleteConversationInput is DELETE /v1/conversations/{id}.
type DeleteConversationInput struct {
	Request
	ConversationID string
}

// DeleteConversation is the delete_conversation mutation.
//
// It is Google's delete-for-me and nothing else (D14): it removes the owner's
// own copy of the thread, and the other people in it keep theirs. There is no
// delete option and no second kind of delete, which is why this takes no
// switch and why a request carrying one is refused at the route.
//
// `libgm.DeleteConversation` wants a participant's number as well as the
// conversation ID, so the thread's first visible other participant is
// resolved here rather than at the route: which number that is, is a fact
// about the conversation, and the route's job is to name the conversation.
func (a *Account) DeleteConversation(ctx context.Context, in DeleteConversationInput) (Result, error) {
	conv, err := a.writableConversation(ctx, in.ConversationID, "delete")
	if err != nil {
		return Result{}, err
	}
	phone, err := a.firstOtherParticipantPhone(ctx, conv.ID)
	if err != nil {
		return Result{}, err
	}
	req := in.Request
	req.ConversationID = conv.ID
	return a.runOperation(ctx, KindDeleteConv, req, func(ctx context.Context, _ store.Operation) (Outcome, error) {
		return Outcome{ConversationID: conv.ID},
			a.Backend.DeleteConversation(ctx, conv.SourceID, phone)
	})
}

// firstOtherParticipantPhone finds a number to address the delete with.
//
// An empty string is not an error: a thread whose participants Agent GM has
// never seen a number for is still the owner's to delete, and upstream
// accepts an empty number. Failing here would make a thread undeletable
// because of an ingest gap, which is the wrong trade.
func (a *Account) firstOtherParticipantPhone(ctx context.Context, conversationID string) (string, error) {
	ps, err := a.Store.Participants(ctx, conversationID)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if !p.IsMe && p.PhoneE164 != "" {
			return p.PhoneE164, nil
		}
	}
	return "", nil
}

// --- shared resolution ------------------------------------------------------

// writableConversation is steps 4, 5 and the object half of step 6: resolve
// the conversation, check THIS account is usable, check the thread is
// actionable. It never creates an operation row and cannot: it returns before
// runOperation is entered.
func (a *Account) writableConversation(ctx context.Context, conversationID, action string) (store.Conversation, error) {
	conv, err := a.conversation(ctx, conversationID)
	if err != nil {
		return conv, err
	}
	// 5. Is the account usable? A signed_out, error, parked or
	// account_changed account reads but does not write.
	row, err := a.Store.Account(ctx, a.ID)
	if err != nil {
		return conv, err
	}
	if err := CheckAccountWritable(AccountRef{
		ID:            row.ID,
		GoogleAccount: row.GoogleAccount,
		State:         string(row.State),
	}); err != nil {
		return conv, err
	}
	// 6. Is the thread actionable?
	if err := CheckConversationActionable(conv, action); err != nil {
		return conv, err
	}
	return conv, nil
}

func (a *Account) conversation(ctx context.Context, id string) (store.Conversation, error) {
	if e := apierr.CheckIDPrefix(id, "conversation_id", store.PrefixConversation); e != nil {
		return store.Conversation{}, e
	}
	conv, err := a.Store.Conversation(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		// Indistinguishable from a thread that was never seen, deliberately.
		return conv, apierr.NotFound("No such conversation.")
	}
	if err != nil {
		return conv, err
	}
	if err := CheckObjectAccount("conversation_id", conv.ID, conv.AccountID, a.ID); err != nil {
		return conv, err
	}
	return conv, nil
}

func (a *Account) message(ctx context.Context, id string) (store.Message, error) {
	if e := apierr.CheckIDPrefix(id, "message_id", store.PrefixMessage); e != nil {
		return store.Message{}, e
	}
	msg, err := a.Store.Message(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return msg, apierr.NotFound("No such message.")
	}
	if err != nil {
		return msg, fmt.Errorf("reading message: %w", err)
	}
	if err := CheckObjectAccount("message_id", msg.ID, msg.AccountID, a.ID); err != nil {
		return msg, err
	}
	return msg, nil
}
