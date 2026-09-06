// Package core holds the operations the surfaces share: ingestion (backfill,
// the live stream and the reconciliation sweep), and the mutation pipeline.
//
// The structural rule of spec section 5.1 is enforced here by construction:
// backfill, the live event stream and the sweep all write through
// Ingester.IngestMessage and Ingester.IngestConversation. There is no
// separate backfill writer whose behaviour could drift, so a bug in ordering
// or dedup is reproducible from either source -- which is exactly what
// section 16's acceptance test 2 asserts by running both orders.
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Logger is the small logging surface core needs. Logs carry the acct_ ID,
// never the Google account address, and never a message body (spec 12.2).
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

// Ingester applies one account's events to the store. There is one per
// account, driven by one goroutine (spec section 5.3).
type Ingester struct {
	Store     *store.Store
	AccountID string
	Log       Logger
	// Account is the engine this ingester belongs to. It carries the audit
	// writer, the clock and the operation correlation this path needs. It is
	// nil in the narrowest unit tests, and every use of it is guarded.
	Account *Account

	// Counters surfaced in GET /v1/health. They are atomic because a
	// one-shot command may ingest on its own goroutine while this account's
	// event loop is running.
	ignored  atomic.Uint64
	unknown  atomic.Uint64
	applied  atomic.Uint64
	outOfOrd atomic.Uint64
	newMsgs  atomic.Uint64
}

// Ignored counts messages dropped by shouldIgnoreStatus.
func (in *Ingester) Ignored() uint64 { return in.ignored.Load() }

// Unknown counts events the section 3.4 catalogue does not name.
func (in *Ingester) Unknown() uint64 { return in.unknown.Load() }

// CountUnknown records one unnamed event.
func (in *Ingester) CountUnknown() { in.unknown.Add(1) }

// Applied counts rows written or updated.
func (in *Ingester) Applied() uint64 { return in.applied.Load() }

// OutOfOrder counts refused backward delivery-state moves.
func (in *Ingester) OutOfOrder() uint64 { return in.outOfOrd.Load() }

// New counts messages counted as new. An IsOld replay is never counted here:
// section 5.4 uses IsOld only to suppress side effects, and "how many new
// messages arrived" is one of them.
func (in *Ingester) New() uint64 { return in.newMsgs.Load() }

// IngestConversation upserts one conversation and replaces its participants.
func (in *Ingester) IngestConversation(ctx context.Context, c gm.Conversation) (string, error) {
	id, err := in.Store.UpsertConversation(ctx, in.AccountID, c)
	if err != nil {
		return "", fmt.Errorf("upserting conversation: %w", err)
	}
	in.applied.Add(1)
	return id, nil
}

// IngestMessage is spec section 5.3 for one message, and it is the ONLY
// writer of message rows: backfill, the live stream and the sweep all arrive
// here.
//
// isOld is used only to suppress side effects (spec section 5.4). An old
// message:
//
//   - never resolves an operation,
//   - never bumps last_activity_ms past a newer value,
//   - and is never counted as new.
//
// It is not used to decide whether to store -- an old event may carry
// information the database lacks.
func (in *Ingester) IngestMessage(ctx context.Context, m gm.Message, isDM, isOld bool) (store.UpsertMessageResult, error) {
	var zero store.UpsertMessageResult

	// 1. If shouldIgnoreStatus says ignore, drop it and count it.
	if gm.ShouldIgnoreStatus(m.StatusRaw, isDM) {
		in.ignored.Add(1)
		return zero, nil
	}
	if m.ConversationID == "" {
		return zero, errors.New("message carries no conversation ID")
	}

	// 2 and 3 happen inside the store: kind comes from the status band and
	// the msg_ ID is derived from (account_id, conversation source ID,
	// message source ID).
	res, err := in.Store.UpsertMessage(ctx, in.AccountID, m.ConversationID, m)
	if err != nil {
		return zero, fmt.Errorf("upserting message: %w", err)
	}
	if res.TransitionRefused {
		in.outOfOrd.Add(1)
		in.auditOutOfOrder(ctx, res, m)
	}
	if res.Inserted || res.Updated {
		in.applied.Add(1)
	}
	if res.Inserted && !isOld {
		in.newMsgs.Add(1)
	}

	// 6 and 7. Attachments are upserted from the MessageInfo entries; the
	// reaction set is REPLACED, because Message.Reactions is authoritative
	// and complete. Neither runs on a no-op: a replay whose content_hash and
	// delivery_state_raw are both unchanged writes nothing at all.
	if res.Inserted || res.Updated {
		if err := in.applyParts(ctx, res.ID, m); err != nil {
			return res, err
		}
	}

	// 8. Correlate the remote echo back to its send attempt, within this
	// account. An old message never resolves an operation.
	if !isOld && m.TmpID != "" && in.Account != nil {
		if err := in.Account.correlateEcho(ctx, res.ID, m); err != nil {
			return res, err
		}
	}

	// 9. Update the conversation's last_activity_ms and latest_message_id.
	if err := in.applyActivity(ctx, m, res.ID, isOld); err != nil {
		return res, err
	}
	return res, nil
}

// applyActivity is step 9 and the monotonicity rule of spec section 5.4.
//
// last_activity_ms is monotonic per conversation: it only ever moves forward.
// The guard is here as well as in the store's MAX() because the write itself
// must not happen at all when there is nothing to move -- a replay storm that
// bumped updated_at_ms on every conversation would churn the WAL for no
// change, which section 5.4 rules out in the same breath as the ordering
// rule. An IsOld replay never gets here.
func (in *Ingester) applyActivity(ctx context.Context, m gm.Message, messageID string, isOld bool) error {
	if isOld || m.Timestamp.IsZero() {
		return nil
	}
	convID := store.ConversationID(in.AccountID, m.ConversationID)
	current, err := in.Store.Conversation(ctx, convID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading conversation activity: %w", err)
	}
	activity := m.Timestamp.UnixMilli()
	if err == nil && activity <= current.LastActivityMS {
		return nil
	}
	if err := in.Store.BumpConversationActivity(ctx, convID, messageID, activity); err != nil {
		return fmt.Errorf("bumping conversation activity: %w", err)
	}
	return nil
}

// applyParts is steps 6 and 7: attachments from the MessageInfo entries, and
// a set-replace of the reactions.
func (in *Ingester) applyParts(ctx context.Context, messageID string, m gm.Message) error {
	for _, att := range m.Attachments {
		var sealed []byte
		if in.Account != nil && in.Account.DataKey != nil && len(att.DecryptionKey) > 0 {
			id := store.AttachmentID(messageID, fmt.Sprint(att.PartIndex), att.MediaID)
			var err error
			sealed, err = store.SealAttachmentKey(*in.Account.DataKey, id, att.DecryptionKey)
			if err != nil {
				return fmt.Errorf("sealing attachment key: %w", err)
			}
		}
		// The state is derived from the attachment, not asserted: see
		// store.DownloadStateFor. Writing `pending` here unconditionally is
		// what made every download unreachable.
		if _, err := in.Store.UpsertAttachment(ctx, in.AccountID, messageID, att,
			sealed, store.DownloadStateForMessage(
				att.MediaID, att.ThumbnailMediaID, string(m.DeliveryState))); err != nil {
			return fmt.Errorf("upserting attachment: %w", err)
		}
	}
	if len(m.Reactions) == 0 {
		return nil
	}
	if err := in.Store.ReplaceReactions(ctx, store.ConversationID(in.AccountID, m.ConversationID),
		messageID, in.myParticipant(ctx, m), m.Reactions); err != nil {
		return fmt.Errorf("replacing reactions: %w", err)
	}
	return nil
}

// myParticipant names this account's own participant in the thread, so
// is_mine is decided at write time rather than guessed by a later reader.
func (in *Ingester) myParticipant(ctx context.Context, m gm.Message) string {
	conv, err := in.Store.Conversation(ctx, store.ConversationID(in.AccountID, m.ConversationID))
	if err != nil {
		return ""
	}
	return conv.DefaultOutgoingID
}

func (in *Ingester) auditOutOfOrder(ctx context.Context, res store.UpsertMessageResult, m gm.Message) {
	// The stored state is left alone and the event is recorded rather than
	// applied. delivery_state_raw still carries whatever Google last said.
	in.logf("message.status_out_of_order",
		"account_id", in.AccountID, "message_id", res.ID,
		"stored_state", res.PreviousState, "reported_state", string(m.DeliveryState))
	if in.Account == nil || in.Account.Audit == nil {
		return
	}
	err := in.Account.Audit.Append(ctx, audit.Event{
		Kind:       audit.KindMessageStatusOutOfOrder,
		AccountID:  in.AccountID,
		TargetType: "message",
		TargetID:   res.ID,
		Result:     audit.ResultRefused,
		Source:     in.Account.auditSource(),
		Payload: map[string]any{
			"stored_state":       res.PreviousState,
			"reported_state":     string(m.DeliveryState),
			"delivery_state_raw": int64(m.StatusRaw),
		},
	})
	if err != nil {
		in.logf("audit append failed", "account_id", in.AccountID, "error", err.Error())
	}
}

func (in *Ingester) logf(msg string, kv ...any) {
	if in.Log != nil {
		in.Log.Warn(msg, kv...)
	}
}

// IsDM reports whether a conversation is a direct one, which is what
// shouldIgnoreStatus's isDM argument means.
func IsDM(c *gm.Conversation) bool {
	return c != nil && !c.IsGroup
}
