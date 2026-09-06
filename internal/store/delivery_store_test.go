package store_test

// Spec section 4.4's transition rules, extending what Slice 1 already
// asserts: the corrections out of `unknown`, the `any -> deleted` row, the
// exemption of incoming messages, and -- new in Slice 2 -- that the FTS index
// follows every one of these writes.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func seedTransitionMessage(t *testing.T, st *store.Store, acct, source string, raw int32) string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c-t", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)}); err != nil {
		t.Fatalf("seeding conversation: %v", err)
	}
	state, ok := gm.DeliveryStateFor(raw)
	if !ok {
		t.Fatalf("status %d is unmapped", raw)
	}
	res, err := st.UpsertMessage(ctx, acct, "c-t", gm.Message{
		SourceID: source, Text: "aubergine", Timestamp: time.Unix(1757000000, 0),
		StatusRaw: raw, Kind: gm.KindForStatus(raw), DeliveryState: state,
	})
	if err != nil {
		t.Fatalf("seeding message: %v", err)
	}
	return res.ID
}

// `unknown -> any`: a late authoritative status corrects it (section 4.4).
func TestUnknownIsCorrectedByALateAuthoritativeStatus(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")
	id := seedTransitionMessage(t, st, acct, "m-unknown", 0) // STATUS_UNKNOWN

	m, err := st.Message(ctx, id)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if m.DeliveryState != string(gm.DeliveryStateUnknown) {
		t.Fatalf("state = %s, want unknown", m.DeliveryState)
	}

	res, err := st.UpsertMessage(ctx, acct, "c-t", gm.Message{
		SourceID: "m-unknown", Text: "aubergine", Timestamp: time.Unix(1757000000, 0),
		StatusRaw: 2, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateDelivered,
	})
	if err != nil {
		t.Fatalf("correcting: %v", err)
	}
	if res.TransitionRefused {
		t.Error("a correction out of unknown was refused")
	}
	m, err = st.Message(ctx, id)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if m.DeliveryState != string(gm.DeliveryStateDelivered) {
		t.Errorf("state = %s, want delivered", m.DeliveryState)
	}
}

// `any -> deleted` (section 4.4), including out of a terminal failure.
func TestAnyStateMovesToDeleted(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")
	id := seedTransitionMessage(t, st, acct, "m-del", 8) // OUTGOING_FAILED_GENERIC

	res, err := st.UpsertMessage(ctx, acct, "c-t", gm.Message{
		SourceID: "m-del", Text: "aubergine", Timestamp: time.Unix(1757000000, 0),
		StatusRaw: 300, Kind: gm.KindForStatus(300), DeliveryState: gm.DeliveryStateDeleted,
	})
	if err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if res.TransitionRefused {
		t.Error("failed -> deleted was refused; any state moves to deleted")
	}
	m, err := st.Message(ctx, id)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if m.DeliveryState != string(gm.DeliveryStateDeleted) || !m.IsDeleted {
		t.Errorf("message is %+v, want deleted", m)
	}
}

// The transition table governs OUTGOING messages. An incoming message's
// states are a different vocabulary and are not ordered by it.
func TestIncomingMessagesAreNotSubjectToTheOutgoingTable(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")
	id := seedTransitionMessage(t, st, acct, "m-in", 100) // INCOMING_COMPLETE

	// Back to downloading, which would be a backward move if the table
	// applied.
	res, err := st.UpsertMessage(ctx, acct, "c-t", gm.Message{
		SourceID: "m-in", Text: "aubergine", Timestamp: time.Unix(1757000000, 0),
		StatusRaw: 105, Kind: gm.KindForStatus(105), DeliveryState: gm.DeliveryStateDownloading,
	})
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if res.TransitionRefused {
		t.Error("an incoming message was judged by the outgoing transition table")
	}
	m, err := st.Message(ctx, id)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if m.DeliveryState != string(gm.DeliveryStateDownloading) {
		t.Errorf("state = %s", m.DeliveryState)
	}
}

// The SQL trigger is the backstop for a writer that does not go through
// UpsertMessage; it must let the same corrections through.
func TestTheTriggerAllowsTheCorrectionsOutOfUnknown(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")
	convID, err := st.UpsertConversation(ctx, acct, gm.Conversation{
		SourceID: "c-t", Folder: gm.FolderInbox, LastActivity: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, to := range []gm.DeliveryState{
		gm.DeliveryStateSent, gm.DeliveryStateDelivered, gm.DeliveryStateRead,
		gm.DeliveryStateFailed, gm.DeliveryStateCanceled, gm.DeliveryStateDeleted,
	} {
		id := "msg_unknown-to-" + string(to)
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO messages (id, account_id, conversation_id, source_id, kind,
				    direction, delivery_state, delivery_state_raw, is_deleted,
				    sent_at_ms, ingested_at_ms, updated_at_ms, content_hash)
				VALUES (?,?,?,?,'message','outgoing','unknown',0,0,1,1,1,'hash')`,
				id, acct, convID, id)
			return err
		}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE messages SET delivery_state = ? WHERE id = ?`, string(to), id)
			return err
		}); err != nil {
			t.Errorf("the trigger refused unknown -> %s: %v", to, err)
		}
	}
}

// New in Slice 2: messages_fts follows every write to messages, so a
// corrected or edited row is not searchable under its old text.
func TestTheSearchIndexFollowsMessageWrites(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")
	seedTransitionMessage(t, st, acct, "m-fts", 100)

	found, err := st.SearchMessages(ctx, store.SearchQuery{Q: "aubergine", AccountID: acct})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("the inserted row is not in the index: %d hits", len(found))
	}

	// An update rewrites the index entry.
	if _, err := st.UpsertMessage(ctx, acct, "c-t", gm.Message{
		SourceID: "m-fts", Text: "courgette", Timestamp: time.Unix(1757000000, 0),
		StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived,
	}); err != nil {
		t.Fatalf("updating: %v", err)
	}
	stale, err := st.SearchMessages(ctx, store.SearchQuery{Q: "aubergine", AccountID: acct})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("the index still answers with the old text: %d hits", len(stale))
	}
	fresh, err := st.SearchMessages(ctx, store.SearchQuery{Q: "courgette", AccountID: acct})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("the new text is not indexed: %d hits", len(fresh))
	}

	// And a delete removes it, which is what account erasure relies on.
	if _, err := st.EraseAccount(ctx, acct); err != nil {
		t.Fatalf("erasing: %v", err)
	}
	gone, err := st.SearchMessages(ctx, store.SearchQuery{Q: "courgette", AllAccounts: true})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(gone) != 0 {
		t.Errorf("erased messages are still in the search index: %d hits", len(gone))
	}
}
