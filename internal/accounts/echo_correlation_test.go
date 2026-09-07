package accounts_test

import (
	"context"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// Live-gate finding 3: `operations.message_id` was null on every succeeded
// send, text and media alike, though the message row existed carrying the
// tmp_id.
//
// Echo correlation hung off a `*core.Account`, and the SUPERVISOR -- the
// thing that actually drains events in the running server -- builds its
// Ingester without one. So the code ran in every test that went through an
// engine and never once in production. Correlation needs the store and the
// account ID and nothing else, so it now asks for nothing else.
//
// This drives the supervisor's own ingest path, which is the one that was
// broken.
//
// Plant: put the correlation back behind `in.Account != nil` and this fails
// at "operations.message_id is still null". Planted 2026-09-07.
func TestEchoCorrelationRunsOnTheSupervisorsIngestPath(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	dir := t.TempDir()
	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions, err := store.NewSessionStore(dir, testKey(t))
	if err != nil {
		t.Fatal(err)
	}

	sup := accounts.New(st, sessions, clk, nil)
	sup.ManualIngest = true

	const address = "echo@example.test"
	be := fake.New(address, fake.WithClock(clk))
	id := store.AccountID(address)
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: address, State: store.StateConnected, SessionPresent: true,
	}); err != nil {
		t.Fatal(err)
	}
	a := sup.Adopt(ctx, id, address, be)

	conv := gm.Conversation{
		SourceID: "conv-echo", Name: "Echo", Type: gm.ConversationTypeRCS,
		SendModeRaw: gm.SendModeAuto, Folder: gm.FolderInbox,
		DefaultOutgoingID: "me", LastActivity: clk.Now(),
		Participants: []gm.Participant{{SourceID: "me", IsMe: true, IsVisible: true}},
	}
	be.SeedConversation(conv)
	convID, err := st.UpsertConversation(ctx, id, conv)
	if err != nil {
		t.Fatal(err)
	}

	// An operation in the shape a succeeded send leaves: terminal, with a
	// tmp_id and no message_id yet.
	const tmpID = "0f9b4c1e-0000-4000-8000-0000000000ec"
	op := store.Operation{
		ID: store.OperationID(), AccountID: id, Kind: "send_text",
		AuthorizationID: "auth_test", IdempotencyKey: "echo-1",
		RequestFingerprint: "fp", ConversationID: convID, TmpID: tmpID,
		Status: store.OpRunning, RequestPayloadJSON: "{}",
	}
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SettleOperation(ctx, op.ID, store.Settlement{Status: store.OpSucceeded}); err != nil {
		t.Fatal(err)
	}
	before, err := st.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.MessageID != "" {
		t.Fatalf("the fixture already has a message_id: %q", before.MessageID)
	}

	// The echo, applied through the ACCOUNT's ingester -- the one the
	// supervisor's event loop uses.
	if _, err := a.Ingester().IngestMessage(ctx, gm.Message{
		SourceID: "msg-echo", ConversationID: "conv-echo", ParticipantID: "me",
		Text: "hello", Timestamp: clk.Now(), TmpID: tmpID, StatusRaw: 1,
		Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateSent,
	}, true, false); err != nil {
		t.Fatalf("ingesting the echo: %v", err)
	}

	after, err := st.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.MessageID == "" {
		t.Fatal("operations.message_id is still null after the echo; the message row " +
			"carries the tmp_id and nothing wrote it back")
	}
	want := store.MessageID(id, "conv-echo", "msg-echo")
	if after.MessageID != want {
		t.Errorf("message_id = %q, want %q", after.MessageID, want)
	}
	if after.Status != store.OpSucceeded {
		t.Errorf("the echo changed a succeeded operation to %q", after.Status)
	}
	_ = time.Second
}
