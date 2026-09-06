// Package core holds the operations the surfaces share. In Slice 1 that is
// ingestion: applying one account's events to the store.
package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

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

	// Counters surfaced in GET /v1/health. They are atomic because a
	// one-shot command may ingest on its own goroutine while this account's
	// event loop is running.
	ignored  atomic.Uint64
	unknown  atomic.Uint64
	applied  atomic.Uint64
	outOfOrd atomic.Uint64
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

// IngestConversation upserts one conversation and replaces its participants.
func (in *Ingester) IngestConversation(ctx context.Context, c gm.Conversation) (string, error) {
	id, err := in.Store.UpsertConversation(ctx, in.AccountID, c)
	if err != nil {
		return "", fmt.Errorf("upserting conversation: %w", err)
	}
	in.applied.Add(1)
	return id, nil
}

// IngestMessage is spec section 5.3 for one message.
//
// isOld is used only to suppress side effects: an old message never bumps
// last_activity_ms past a newer value and is never counted as new. It is not
// used to decide whether to store -- an old event may carry information the
// database lacks.
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
		// The stored state is left alone, and the event is recorded rather
		// than applied. The audit row arrives with the audit log in Slice 2.
		in.logf("message.status_out_of_order",
			"account_id", in.AccountID, "message_id", res.ID,
			"stored_state", res.PreviousState, "reported_state", string(m.DeliveryState))
	}
	if res.Inserted || res.Updated {
		in.applied.Add(1)
	}

	// 9. Update the conversation's last_activity_ms and latest_message_id --
	// but never for a replayed old message.
	if !isOld && !m.Timestamp.IsZero() {
		convID := store.ConversationID(in.AccountID, m.ConversationID)
		if err := in.Store.BumpConversationActivity(ctx, convID, res.ID, m.Timestamp.UnixMilli()); err != nil {
			return res, fmt.Errorf("bumping conversation activity: %w", err)
		}
	}
	return res, nil
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
