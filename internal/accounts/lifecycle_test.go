package accounts_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
	"github.com/thisnick/agent-gm/internal/wire"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// recordingAuditor is the Auditor the supervisor writes through. internal/audit
// belongs to another package; this one only needs the interface.
type recordingAuditor struct {
	mu   sync.Mutex
	rows []auditRow
}

type auditRow struct {
	kind   string
	fields map[string]any
}

func (r *recordingAuditor) Append(_ context.Context, kind string, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	r.rows = append(r.rows, auditRow{kind: kind, fields: copied})
	return nil
}

func (r *recordingAuditor) of(kind string) []auditRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []auditRow
	for _, row := range r.rows {
		if row.kind == kind {
			out = append(out, row)
		}
	}
	return out
}

type sweepCall struct {
	accountID string
	since     time.Time
}

type recordingSweeper struct {
	mu    sync.Mutex
	calls []sweepCall
}

func (s *recordingSweeper) Sweep(_ context.Context, accountID string, since time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sweepCall{accountID: accountID, since: since})
	return nil
}

func (s *recordingSweeper) forAccount(id string) []sweepCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sweepCall
	for _, c := range s.calls {
		if c.accountID == id {
			out = append(out, c)
		}
	}
	return out
}

// recordingBackfiller is one backfill worker per account. Pause and Resume are
// per account on purpose: one sleepy phone must not stall another account.
type recordingBackfiller struct {
	mu      sync.Mutex
	running map[string]bool
	paused  map[string]bool
}

func newBackfiller() *recordingBackfiller {
	return &recordingBackfiller{running: map[string]bool{}, paused: map[string]bool{}}
}

func (b *recordingBackfiller) Start(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running[id] = true
	return nil
}

func (b *recordingBackfiller) Stop(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, id)
	delete(b.paused, id)
}

func (b *recordingBackfiller) Pause(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused[id] = true
}

func (b *recordingBackfiller) Resume(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused[id] = false
}

func (b *recordingBackfiller) Progress(id string) accounts.BackfillHealth {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := "pending"
	if b.running[id] {
		state = "running"
	}
	if b.paused[id] {
		state = "paused"
	}
	return accounts.BackfillHealth{State: state}
}

func (b *recordingBackfiller) isPaused(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused[id]
}

func (b *recordingBackfiller) isRunning(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running[id]
}

// ---------------------------------------------------------------------------
// Row counting: the negative half of D30 is asserted, never inferred
// ---------------------------------------------------------------------------

type rowCounts struct {
	conversations int
	messages      int
	attachments   int
	reactions     int
	contacts      int
	operations    int
}

// countRows counts every row type spec section 4.7 says a sign-out, a
// revocation, a cookie expiry, an account_changed, an abandoned re-pair and
// max_concurrent parking must leave alone. It counts across ALL accounts, so
// a deletion that hit the wrong account is caught too.
func countRows(t *testing.T, ctx context.Context, st *store.Store) rowCounts {
	t.Helper()
	var c rowCounts
	convs, err := st.Conversations(ctx, store.ConversationFilter{})
	if err != nil {
		t.Fatalf("counting conversations: %v", err)
	}
	c.conversations = len(convs)

	msgs, err := st.Messages(ctx, store.MessageFilter{AllAccounts: true, IncludeSystem: true})
	if err != nil {
		t.Fatalf("counting messages: %v", err)
	}
	c.messages = len(msgs)
	for _, m := range msgs {
		atts, err := st.AttachmentsForMessage(ctx, m.ID)
		if err != nil {
			t.Fatalf("counting attachments: %v", err)
		}
		c.attachments += len(atts)
		reacts, err := st.ReactionsForMessage(ctx, m.ID)
		if err != nil {
			t.Fatalf("counting reactions: %v", err)
		}
		c.reactions += len(reacts)
	}

	contacts, err := st.ListContacts(ctx, store.ContactQuery{AllAccounts: true, Limit: 1000})
	if err != nil {
		t.Fatalf("counting contacts: %v", err)
	}
	c.contacts = len(contacts)

	ops, err := st.ListOperations(ctx, store.OperationQuery{AllAccounts: true, Limit: 1000})
	if err != nil {
		t.Fatalf("counting operations: %v", err)
	}
	c.operations = len(ops)
	return c
}

func requireNoRowsDeleted(t *testing.T, what string, before, after rowCounts) {
	t.Helper()
	if before != after {
		t.Errorf("%s deleted rows: before %+v, after %+v", what, before, after)
	}
}

// seedAccountData gives one account a conversation, a message, an attachment,
// a contact and an operation, so "zero rows deleted" has something to count.
func seedAccountData(t *testing.T, ctx context.Context, h *harness, a *accounts.Account,
	backend *fake.Backend, clk clock.Clock, convID, msgID string) {
	t.Helper()
	backend.SeedConversation(gm.Conversation{
		SourceID: convID, Name: "Fixture " + convID, Folder: gm.FolderInbox,
		Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		DefaultOutgoingID: "me", LastActivity: clk.Now(),
	})
	backend.Emit(&gm.EventConversation{Conversation: gm.Conversation{
		SourceID: convID, Name: "Fixture " + convID, Folder: gm.FolderInbox,
		Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		DefaultOutgoingID: "me", LastActivity: clk.Now(),
	}})
	backend.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: msgID, ConversationID: convID, Text: "keep me", Timestamp: clk.Now(),
		StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived,
	}})
	pump(ctx, a)

	storedMsgID := store.MessageID(a.ID, convID, msgID)
	if _, err := h.store.UpsertAttachment(ctx, a.ID, storedMsgID, gm.Attachment{
		PartIndex: 0, MediaID: "media-" + msgID, Filename: "f.jpg", MimeType: "image/jpeg",
		SizeBytes: 12,
	}, nil, "pending"); err != nil {
		t.Fatalf("seeding an attachment: %v", err)
	}
	if _, err := h.store.UpsertContact(ctx, a.ID, gm.Contact{
		SourceID: "p-" + convID, DisplayName: "Fixture", PhoneE164: "+12025550143",
	}, ""); err != nil {
		t.Fatalf("seeding a contact: %v", err)
	}
	if err := h.store.InsertOperation(ctx, store.Operation{
		AccountID: a.ID, Kind: "send_text", AuthorizationID: "authz_fixture",
		Status: store.OpSucceeded, Terminal: true,
	}); err != nil {
		t.Fatalf("seeding an operation: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 31
// ---------------------------------------------------------------------------

// Signing out keeps history and refuses writes. Two fakes, because one fake is
// one account (spec section 13.1), and two CLOCKS, because this test advances
// account a's time and leaves account b's standing still.
func TestSignOutKeepsEveryRowAndAccountBIsUnaffected(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0

	backendA := fake.New("a@example.com", fake.WithClock(clkA))
	acctA, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctA)
	seedAccountData(t, ctx, h, acctA, backendA, clkA, "ca", "ma")

	backendB := fake.New("b@example.com", fake.WithClock(clkB))
	acctB, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.StopAll(ctx) })
	pump(ctx, acctB)
	seedAccountData(t, ctx, h, acctB, backendB, clkB, "cb", "mb")

	// Only account a's clock moves. Account b's phone is asleep and its time
	// stands still; nothing below depends on b having ticked.
	clkA.Advance(time.Hour)

	before := countRows(t, ctx, h.store)
	if before.conversations != 2 || before.messages != 2 || before.attachments != 2 ||
		before.contacts != 2 || before.operations != 2 {
		t.Fatalf("the fixture did not seed what the test counts: %+v", before)
	}

	if err := h.sup.SignOut(ctx, acctA); err != nil {
		t.Fatalf("signing out: %v", err)
	}

	// 1. the session file is shredded and the in-memory AuthData zeroed
	if h.sessions.Present(acctA.ID) {
		t.Error("sessions/<acct>.enc survived a sign-out")
	}
	if backendA.IsLoggedIn() {
		t.Error("the in-memory AuthData was not zeroed")
	}
	// 2. state and session_present
	rowA, err := h.store.Account(ctx, acctA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowA.State != accounts.StateSignedOut {
		t.Errorf("state = %s, want signed_out", rowA.State)
	}
	if rowA.SessionPresent {
		t.Error("session_present is still 1")
	}
	// 3. it holds no goroutine and no client
	if acctA.Running() || acctA.HasClient() {
		t.Error("a signed-out account still holds a client or a goroutine")
	}
	// 4. ZERO rows deleted -- the negative half of D30, counted rather than
	//    inferred from the positive case.
	requireNoRowsDeleted(t, "signing out", before, countRows(t, ctx, h.store))

	// 5. account a's history is still readable
	convs, err := h.store.Conversations(ctx, store.ConversationFilter{AccountID: acctA.ID})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := h.store.Messages(ctx, store.MessageFilter{AccountID: acctA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(msgs) != 1 || msgs[0].Text != "keep me" {
		t.Errorf("account a's history changed: %d conversations, %d messages", len(convs), len(msgs))
	}

	// 6. every write naming a is unsupported_capability / not_signed_in --
	//    NOT the service-level not_paired, which would say there are no
	//    accounts at all when in fact account b is right there.
	err = h.sup.CheckWritable(ctx, acctA.ID)
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("a write against a signed-out account returned %v", err)
	}
	if ae.Code != apierr.CodeUnsupportedCapability {
		t.Errorf("code = %s, want unsupported_capability", ae.Code)
	}
	if ae.Code == apierr.CodeNotPaired {
		t.Error("a per-account condition was reported as the service-level not_paired")
	}
	if got := ae.Details["reason"]; got != apierr.ReasonNotSignedIn {
		t.Errorf("details.reason = %v, want not_signed_in", got)
	}

	// 7. account b is unaffected and still sends.
	if err := h.sup.CheckWritable(ctx, acctB.ID); err != nil {
		t.Errorf("account b cannot write after account a signed out: %v", err)
	}
	rowB, err := h.store.Account(ctx, acctB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowB.State != accounts.StateConnected {
		t.Errorf("account b is %s, want connected", rowB.State)
	}
	if _, err := backendB.SendText(ctx, gm.SendTextRequest{
		ConversationID: "cb", Text: "still sending", TmpID: gm.GenerateTmpID(),
	}); err != nil {
		t.Errorf("account b could not send: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 32
// ---------------------------------------------------------------------------

// Re-pairing resumes the same acct_ row: no second account, no duplicated
// history, and a full sweep with since = accounts.last_event_at_ms.
func TestRePairResumesTheSameAccountWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0
	sweeper := &recordingSweeper{}
	h.sup.Sweep = sweeper
	auditor := &recordingAuditor{}
	h.sup.Audit = auditor

	backendA := fake.New("a@example.com", fake.WithClock(clkA))
	acctA, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctA)
	seedAccountData(t, ctx, h, acctA, backendA, clkA, "ca", "ma")

	backendB := fake.New("b@example.com", fake.WithClock(clkB))
	acctB, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.StopAll(ctx) })
	pump(ctx, acctB)

	if err := h.sup.SignOut(ctx, acctA); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, ctx, h.store)
	rowBefore, err := h.store.Account(ctx, acctA.ID)
	if err != nil {
		t.Fatal(err)
	}
	lastEvent := rowBefore.LastEventAtMS
	if lastEvent == 0 {
		t.Fatal("the fixture never moved last_event_at_ms, so `since` proves nothing")
	}

	// Only account a's clock advances while it is signed out. Account b's
	// phone is still asleep.
	clkA.Advance(2 * time.Hour)

	// Pairing again with the SAME address. AuthData.Mobile.SourceID hashes to
	// the same acct_ ID, so this resumes rather than adds.
	again := fake.New("a@example.com", fake.WithClock(clkA))
	resumed, err := h.sup.Pair(ctx, again, cookies(), 0, nil)
	if err != nil {
		t.Fatalf("re-pairing: %v", err)
	}
	if resumed.ID != acctA.ID {
		t.Errorf("re-pairing produced acct_ ID %s, want %s", resumed.ID, acctA.ID)
	}

	rows, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("after a re-pair there are %d account rows, want 2", len(rows))
	}
	rowAfter, err := h.store.Account(ctx, acctA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowAfter.State != accounts.StateConnected {
		t.Errorf("the resumed account is %s, want connected", rowAfter.State)
	}
	// Conversation and message counts are UNCHANGED: every re-derived ID
	// equals the stored one.
	requireNoRowsDeleted(t, "re-pairing", before, countRows(t, ctx, h.store))

	// A full reconciliation sweep runs with since = accounts.last_event_at_ms.
	calls := sweeper.forAccount(acctA.ID)
	if len(calls) == 0 {
		t.Fatal("re-pairing ran no reconciliation sweep")
	}
	last := calls[len(calls)-1]
	if last.since.UnixMilli() != lastEvent {
		t.Errorf("the sweep ran with since = %d, want last_event_at_ms = %d",
			last.since.UnixMilli(), lastEvent)
	}
	if len(auditor.of(accounts.AuditResumed)) != 1 {
		t.Errorf("a re-pair wrote %d account.resumed rows, want 1",
			len(auditor.of(accounts.AuditResumed)))
	}

	// Messages that arrived while it was signed out are ingested once.
	pump(ctx, resumed)
	again.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: "while-out", ConversationID: "ca", Text: "arrived while signed out",
		Timestamp: clkA.Now(), StatusRaw: 100, Kind: gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateReceived,
	}})
	pump(ctx, resumed)
	// The same message replayed as an IsOld backlog entry must not double up.
	again.Emit(&gm.EventMessage{IsOld: true, Message: gm.Message{
		SourceID: "while-out", ConversationID: "ca", Text: "arrived while signed out",
		Timestamp: clkA.Now(), StatusRaw: 100, Kind: gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateReceived,
	}})
	pump(ctx, resumed)

	msgs, err := h.store.Messages(ctx, store.MessageFilter{AccountID: acctA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("account a has %d messages, want 2 (one seeded, one ingested exactly once)", len(msgs))
	}
	// Account b was never touched by any of it.
	if _, err := h.store.Account(ctx, acctB.ID); err != nil {
		t.Errorf("account b's row went missing: %v", err)
	}
}

// A re-pair yielding a DIFFERENT address creates a second account and never
// adopts the first one's rows.
func TestRePairWithADifferentAddressCreatesASecondAccount(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	first := fake.New("a@example.com", fake.WithClock(clk))
	acctA, err := h.sup.Pair(ctx, first, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctA)
	seedAccountData(t, ctx, h, acctA, first, clk, "ca", "ma")

	second := fake.New("other@example.com", fake.WithClock(clk))
	acctOther, err := h.sup.Pair(ctx, second, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if acctOther.ID == acctA.ID {
		t.Fatal("a different address reused the first account's acct_ ID")
	}
	convs, err := h.store.Conversations(ctx, store.ConversationFilter{AccountID: acctOther.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 0 {
		t.Errorf("the second account adopted %d of the first account's conversations", len(convs))
	}
}

// `agm pair --account <id>` refuses with pairing_wrong_account when the
// signed-in address does not match, and changes nothing.
func TestPairAsRefusesTheWrongAccount(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	expected := store.AccountID("a@example.com")
	wrong := fake.New("someone-else@example.com", fake.WithClock(clk))
	_, err := h.sup.PairAs(ctx, expected, wrong, cookies(), 0, nil)
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingWrongAccount {
		t.Fatalf("got %v, want pairing_wrong_account", err)
	}
	rows, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused pairing created %d account rows: %+v", len(rows), rows)
	}
	entries, err := os.ReadDir(h.sessions.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused pairing wrote %d session files", len(entries))
	}

	// The matching address is accepted by the same call.
	right := fake.New("a@example.com", fake.WithClock(clk))
	acct, err := h.sup.PairAs(ctx, expected, right, cookies(), 0, nil)
	if err != nil {
		t.Fatalf("pairing the expected account: %v", err)
	}
	if acct.ID != expected {
		t.Errorf("acct_ ID = %s, want %s", acct.ID, expected)
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 36 -- the negative half of D30
// ---------------------------------------------------------------------------

// Each of signing out, a RevokePairData, cookie expiry, account_changed, an
// abandoned re-pair and max_concurrent parking deletes ZERO rows, asserted by
// row counts before and after EACH.
func TestOnlyRemovalDeletesRows(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	backendA := fake.New("a@example.com", fake.WithClock(clkA))
	acctA, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctA)
	seedAccountData(t, ctx, h, acctA, backendA, clkA, "ca", "ma")

	backendB := fake.New("b@example.com", fake.WithClock(clkB))
	acctB, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctB)
	seedAccountData(t, ctx, h, acctB, backendB, clkB, "cb", "mb")

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"signing out", func(t *testing.T) {
			if err := h.sup.SignOut(ctx, acctA); err != nil {
				t.Fatal(err)
			}
		}},
		{"a RevokePairData from the phone", func(t *testing.T) {
			acctB.Apply(ctx, &gm.EventRevokePairData{})
		}},
		{"cookie expiry", func(t *testing.T) {
			acctB.Apply(ctx, &gm.EventGaiaLoggedOut{})
		}},
		{"account_changed", func(t *testing.T) {
			acctB.Apply(ctx, &gm.EventAccountChange{Account: "someone@example.com"})
		}},
		{"an abandoned re-pair", func(t *testing.T) {
			again := fake.New("a@example.com", fake.WithClock(clkA))
			again.ScriptPairError(gm.ErrIncorrectEmoji)
			if _, err := h.sup.Pair(ctx, again, cookies(), 0, nil); err == nil {
				t.Fatal("the abandoned re-pair unexpectedly succeeded")
			}
		}},
		{"max_concurrent parking", func(t *testing.T) {
			h.sup.SetMaxConcurrent(ctx, 1)
			third := fake.New("c@example.com", fake.WithClock(clkB))
			acctC, err := h.sup.Pair(ctx, third, cookies(), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			row, err := h.store.Account(ctx, acctC.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != accounts.StateParked {
				t.Fatalf("the surplus account is %s, want parked", row.State)
			}
		}},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			before := countRows(t, ctx, h.store)
			if before.conversations == 0 || before.messages == 0 {
				t.Fatal("nothing to count; the fixture is empty")
			}
			step.run(t)
			requireNoRowsDeleted(t, step.name, before, countRows(t, ctx, h.store))
		})
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 37
// ---------------------------------------------------------------------------

// accounts.max_concurrent bounds concurrency and is diagnosable: exactly one
// connected, the other parked with state_reason "capacity", the parked one
// readable, its writes not_signed_in, holding no goroutine and no client, and
// raising the setting connects it without a restart.
func TestMaxConcurrentParksTheSurplusAndRaisingItConnects(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	// The real ingest goroutine runs here, because "holds no goroutine" is
	// the assertion and a manual harness would make it vacuous.
	h.sup.ManualIngest = false
	h.sup.MaxConcurrent = 1
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	first, err := h.sup.Pair(ctx, fake.New("a@example.com", fake.WithClock(clkA)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.sup.Pair(ctx, fake.New("b@example.com", fake.WithClock(clkB)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	rowFirst, err := h.store.Account(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowFirst.State != accounts.StateConnected {
		t.Errorf("the first account is %s, want connected", rowFirst.State)
	}
	rowSecond, err := h.store.Account(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowSecond.State != accounts.StateParked {
		t.Fatalf("the surplus account is %s, want parked", rowSecond.State)
	}
	if rowSecond.StateReason != string(accounts.ReasonCapacity) {
		t.Errorf("state_reason = %q, want %q", rowSecond.StateReason, accounts.ReasonCapacity)
	}
	// Parked is a state of its own. "Waiting for a slot" and "retrying a
	// listen error" must be tellable apart from `state` alone.
	if rowSecond.State == accounts.StateDegraded || rowSecond.State == accounts.StateError {
		t.Error("waiting for a slot was reported as degraded or error")
	}

	// It holds NO client and NO goroutine -- asserted, not assumed from the
	// state string.
	if second.Running() {
		t.Error("a parked account is running")
	}
	if second.HasClient() {
		t.Error("a parked account holds a client")
	}
	if got := h.sup.Goroutines(); got != 1 {
		t.Errorf("with one connected and one parked account there are %d goroutines, want 1", got)
	}

	// It is fully readable.
	if _, err := h.store.UpsertConversation(ctx, second.ID, gm.Conversation{
		SourceID: "cb", Name: "Parked", Folder: gm.FolderInbox,
		Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
		DefaultOutgoingID: "me", LastActivity: clkB.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	convs, err := h.store.Conversations(ctx, store.ConversationFilter{AccountID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Errorf("a parked account read back %d conversations, want 1", len(convs))
	}

	// Its writes are refused with not_signed_in.
	err = h.sup.CheckWritable(ctx, second.ID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeUnsupportedCapability {
		t.Fatalf("a write against a parked account returned %v", err)
	}
	if got := ae.Details["reason"]; got != apierr.ReasonNotSignedIn {
		t.Errorf("details.reason = %v, want not_signed_in", got)
	}

	// Raising the setting connects it WITHOUT a restart.
	h.sup.SetMaxConcurrent(ctx, 2)
	rowSecond, err = h.store.Account(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowSecond.State != accounts.StateConnected {
		t.Errorf("after raising accounts.max_concurrent the parked account is %s, want connected",
			rowSecond.State)
	}
	if !second.Running() || !second.HasClient() {
		t.Error("the un-parked account holds no client or no goroutine")
	}
	if got := h.sup.Goroutines(); got != 2 {
		t.Errorf("with two connected accounts there are %d goroutines, want 2", got)
	}
}

// A freed slot goes to the NEWEST parked account -- last_event_at_ms order,
// newest first (spec section 4.7).
func TestAFreedSlotGoesToTheNewestParkedAccount(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 1
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	holder, err := h.sup.Pair(ctx, fake.New("holder@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	older, err := h.sup.Pair(ctx, fake.New("older@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := h.sup.Pair(ctx, fake.New("newer@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The holder must never be preempted, so its last event is the newest of
	// all: only the ordering AMONG PARKED accounts is under test.
	if err := h.store.TouchAccountEvent(ctx, holder.ID, clk.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.TouchAccountEvent(ctx, older.ID, clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.TouchAccountEvent(ctx, newer.ID, clk.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Signing the holder out frees exactly one slot.
	if err := h.sup.SignOut(ctx, holder); err != nil {
		t.Fatal(err)
	}
	rowNewer, err := h.store.Account(ctx, newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	rowOlder, err := h.store.Account(ctx, older.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowNewer.State != accounts.StateConnected {
		t.Errorf("the newest parked account is %s, want connected", rowNewer.State)
	}
	if rowOlder.State != accounts.StateParked {
		t.Errorf("the oldest parked account is %s, want parked -- the one free slot went to the wrong account",
			rowOlder.State)
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 38
// ---------------------------------------------------------------------------

// A pairing that never completes leaves nothing behind: no acct_ row, no
// session file, and -- the part that matters -- a subsequent write with one
// real account still succeeds WITHOUT account_id.
func TestAPairingThatNeverCompletesLeavesNothingBehind(t *testing.T) {
	// pairedAlready gives each case exactly ONE real account, so the section
	// 7.3 ambiguity rule is the assertion: an abandoned pairing row would
	// make the write demand account_id.
	setup := func(t *testing.T) (context.Context, *harness, *clock.Fake, string) {
		t.Helper()
		ctx := context.Background()
		clk := clock.NewFake()
		h := newHarness(t, clk)
		h.sup.MaxConcurrent = 0
		t.Cleanup(func() { h.sup.StopAll(ctx) })
		real, err := h.sup.Pair(ctx, fake.New("real@example.com", fake.WithClock(clk)), cookies(), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, h, clk, real.ID
	}

	assertNothingLeftBehind := func(t *testing.T, ctx context.Context, h *harness, realID string) {
		t.Helper()
		rows, err := h.store.Accounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != realID {
			t.Errorf("the abandoned pairing left %d account rows: %+v", len(rows), rows)
		}
		entries, err := os.ReadDir(h.sessions.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("the abandoned pairing wrote a session file: %d files on disk", len(entries))
		}
		// The write succeeds WITHOUT account_id: the abandoned row never
		// tripped the section 7.3 ambiguity rule.
		chosen, err := h.sup.ChooseWriteAccount(ctx, "")
		if err != nil {
			t.Fatalf("a write with one real account was refused: %v", err)
		}
		if chosen.ID != realID {
			t.Errorf("the write chose %s, want %s", chosen.ID, realID)
		}
	}

	t.Run("cancelled", func(t *testing.T) {
		ctx, h, clk, realID := setup(t)
		pairCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		abandoned := fake.New("ghost@example.com", fake.WithClock(clk))
		_, err := h.sup.Pair(pairCtx, abandoned, cookies(), 0, func(string) {
			// The owner walked away between the emoji and the confirmation.
			// The `pairing` row exists at exactly this moment.
			if _, err := h.store.Account(ctx, store.AccountID("ghost@example.com")); err != nil {
				t.Errorf("no pairing row existed at emoji time: %v", err)
			}
			cancel()
		})
		var ge *gm.Error
		if !errors.As(err, &ge) || ge.Code != gm.CodePairingCancelled {
			t.Fatalf("got %v, want pairing_cancelled", err)
		}
		assertNothingLeftBehind(t, ctx, h, realID)
	})

	t.Run("timed out", func(t *testing.T) {
		ctx, h, clk, realID := setup(t)
		h.sup.PairingTimeout = 5 * time.Minute

		pairCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		held := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		abandoned := fake.New("ghost@example.com", fake.WithClock(clk))
		go func() {
			_, err := h.sup.Pair(pairCtx, abandoned, cookies(), 0, func(string) {
				close(held)
				<-release
			})
			done <- err
		}()
		<-held

		// A live pairing younger than AGENT_GM_PAIRING_TIMEOUT is left alone.
		deleted, err := h.sup.SweepPairings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 0 {
			t.Errorf("a live pairing was swept before it timed out: %v", deleted)
		}
		if _, err := h.store.Account(ctx, store.AccountID("ghost@example.com")); err != nil {
			t.Fatalf("the in-flight pairing row vanished: %v", err)
		}

		// Time passes ON THE INJECTED CLOCK. No test sleeps.
		clk.Advance(6 * time.Minute)
		deleted, err = h.sup.SweepPairings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 1 {
			t.Errorf("the timed-out pairing was not swept: %v", deleted)
		}

		cancel()
		close(release)
		if err := <-done; err == nil {
			t.Error("a timed-out pairing returned success")
		}
		assertNothingLeftBehind(t, ctx, h, realID)
	})

	t.Run("the process is killed mid-pairing", func(t *testing.T) {
		ctx, h, _, realID := setup(t)
		// What a crash leaves behind: a `pairing` row with no live pairing.
		now := h.clock.Now().UnixMilli()
		ghost := store.AccountID("ghost@example.com")
		if err := h.store.UpsertAccount(ctx, store.Account{
			ID: ghost, GoogleAccount: "ghost@example.com", State: accounts.StatePairing,
			CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatal(err)
		}
		// Even before the sweep runs, a `pairing` row does not count toward
		// the ambiguity rule: the write succeeds without account_id.
		if _, err := h.sup.ChooseWriteAccount(ctx, ""); err != nil {
			t.Errorf("an abandoned pairing row made a single-account write ambiguous: %v", err)
		}

		// The restart: a fresh supervisor over the same store, with no live
		// pairings at all.
		restarted := accounts.New(h.store, h.sessions, h.clock, nil)
		deleted, err := restarted.SweepPairings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 1 || deleted[0] != ghost {
			t.Errorf("the startup sweep deleted %v, want [%s]", deleted, ghost)
		}
		assertNothingLeftBehind(t, ctx, h, realID)
	})

	t.Run("no account address", func(t *testing.T) {
		ctx, h, clk, realID := setup(t)
		// AuthData.Mobile.SourceID was empty, so there is no address to
		// derive an acct_ ID from: pairing_no_account, and nothing created.
		_, err := h.sup.Pair(ctx, fake.New("", fake.WithClock(clk)), cookies(), 0, nil)
		var ge *gm.Error
		if !errors.As(err, &ge) || ge.Code != gm.CodePairingNoAccount {
			t.Fatalf("got %v, want pairing_no_account", err)
		}
		assertNothingLeftBehind(t, ctx, h, realID)
	})
}

// ---------------------------------------------------------------------------
// Accounts diverge (spec section 13.1)
// ---------------------------------------------------------------------------

// Two fakes, two clocks, and three divergences at once: A healthy while B
// returns ErrInvalidCredentials; A backfilling while B is paused by its own
// MOBILE_DATABASE_SYNC_STARTED; A connected while B is parked.
func TestAccountsDiverge(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0
	backfill := newBackfiller()
	h.sup.Backfill = backfill
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	backendA := fake.New("a@example.com", fake.WithClock(clkA))
	acctA, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	backendB := fake.New("b@example.com", fake.WithClock(clkB))
	acctB, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acctA)
	pump(ctx, acctB)

	t.Run("A backfills while B is paused by its own phone", func(t *testing.T) {
		if !backfill.isRunning(acctA.ID) || !backfill.isRunning(acctB.ID) {
			t.Fatal("backfill did not start for both accounts")
		}
		// B's phone starts a database sync. B's clock moves; A's does not.
		clkB.Advance(30 * time.Minute)
		acctB.Apply(ctx, &gm.EventUserAlert{Alert: gm.AlertMobileDatabaseSyncStarted})
		if !backfill.isPaused(acctB.ID) {
			t.Error("B's backfill was not paused by B's phone")
		}
		if backfill.isPaused(acctA.ID) {
			t.Error("B's sleepy phone paused A's backfill")
		}
		acctB.Apply(ctx, &gm.EventUserAlert{Alert: gm.AlertMobileDatabaseSyncComplete})
		if backfill.isPaused(acctB.ID) {
			t.Error("B's backfill did not resume on SYNC_COMPLETE")
		}
	})

	t.Run("A stays healthy while B's credentials die", func(t *testing.T) {
		h.sup.Stop(acctB)
		backendB.ScriptErrors(gm.ErrInvalidCredentials)
		if err := h.sup.Start(ctx, acctB); err == nil {
			t.Fatal("B reconnected with dead credentials")
		}
		rowB, err := h.store.Account(ctx, acctB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rowB.State != accounts.StateSignedOut ||
			rowB.StateReason != string(accounts.ReasonCredentials) {
			t.Errorf("B is %s/%s, want signed_out/credentials", rowB.State, rowB.StateReason)
		}
		rowA, err := h.store.Account(ctx, acctA.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rowA.State != accounts.StateConnected || rowA.StateReason != "" {
			t.Errorf("B's dead credentials moved A to %s/%s", rowA.State, rowA.StateReason)
		}
		if !acctA.HasClient() {
			t.Error("B's failure disconnected A")
		}
	})

	t.Run("A connected while C is parked", func(t *testing.T) {
		h.sup.SetMaxConcurrent(ctx, 1)
		clkC := clock.NewFake()
		acctC, err := h.sup.Pair(ctx, fake.New("c@example.com", fake.WithClock(clkC)), cookies(), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		rowC, err := h.store.Account(ctx, acctC.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rowC.State != accounts.StateParked {
			t.Errorf("C is %s, want parked", rowC.State)
		}
		rowA, err := h.store.Account(ctx, acctA.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rowA.State != accounts.StateConnected {
			t.Errorf("A is %s, want connected", rowA.State)
		}
	})
}

// ---------------------------------------------------------------------------
// Audit, the state-change feed, and the health block
// ---------------------------------------------------------------------------

// Every transition writes account.state_changed carrying from, to and
// state_reason, and publishes one event on the feed.
func TestEveryTransitionIsAuditedAndStreamed(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	auditor := &recordingAuditor{}
	h.sup.Audit = auditor
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	acct, err := h.sup.Pair(ctx, fake.New("a@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := h.sup.Subscribe(acct.ID)
	defer sub.Close()
	all := h.sup.Subscribe("")
	defer all.Close()

	acct.Apply(ctx, &gm.EventListenTemporaryError{Err: errors.New("blip")})
	acct.Apply(ctx, &gm.EventGaiaLoggedOut{})

	want := []struct {
		to     accounts.State
		reason accounts.Reason
	}{
		{accounts.StateDegraded, accounts.ReasonListenError},
		{accounts.StateSignedOut, accounts.ReasonCookiesExpired},
	}
	for _, w := range want {
		select {
		case ev := <-sub.Events():
			if ev.To != w.to || ev.Reason != w.reason {
				t.Errorf("feed event = %s/%s, want %s/%s", ev.To, ev.Reason, w.to, w.reason)
			}
			if ev.AccountID != acct.ID {
				t.Errorf("feed event carries account_id %s", ev.AccountID)
			}
		default:
			t.Fatalf("no feed event for the move to %s", w.to)
		}
	}
	// The all-accounts subscription saw them too, each tagged.
	if len(all.Events()) < 2 {
		t.Errorf("the all-accounts subscription got %d events, want at least 2", len(all.Events()))
	}

	rows := auditor.of(accounts.AuditStateChanged)
	if len(rows) < 2 {
		t.Fatalf("only %d account.state_changed rows were written", len(rows))
	}
	last := rows[len(rows)-1]
	if last.fields["to"] != string(accounts.StateSignedOut) ||
		last.fields["state_reason"] != string(accounts.ReasonCookiesExpired) ||
		last.fields["from"] != string(accounts.StateDegraded) ||
		last.fields["account_id"] != acct.ID {
		t.Errorf("the audit row is %+v", last.fields)
	}

	// Re-asserting the same state is not a transition and must not emit.
	drain(sub)
	acct.Apply(ctx, &gm.EventGaiaLoggedOut{})
	select {
	case ev := <-sub.Events():
		t.Errorf("re-asserting signed_out emitted %+v", ev)
	default:
	}
}

func drain(sub *accounts.Subscription) {
	for {
		select {
		case <-sub.Events():
		default:
			return
		}
	}
}

// A slow subscriber must not block the supervisor: its events are dropped and
// counted, and every transition still lands in the store.
func TestASlowSubscriberDoesNotBlockTheSupervisor(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	acct, err := h.sup.Pair(ctx, fake.New("a@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := h.sup.Subscribe(acct.ID)
	defer sub.Close()

	// Never read from sub. Alternate two states so every apply is a real
	// transition, and overrun the buffer several times over.
	for i := 0; i < accounts.SubscriberBuffer*3; i++ {
		acct.Apply(ctx, &gm.EventListenTemporaryError{Err: errors.New("blip")})
		acct.Apply(ctx, &gm.EventListenRecovered{})
	}
	if sub.Dropped() == 0 {
		t.Error("a subscriber that never read dropped nothing, so it was being waited on")
	}
	row, err := h.store.Account(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != accounts.StateConnected {
		t.Errorf("the supervisor stalled: state = %s", row.State)
	}
}

// The per-account health block: the google block is cached from the last
// FetchConfig / IsDefaultSMSApp, and is null for an account that is not
// connected. config_version_stale is informational, not a fault (D32).
func TestHealthBlockIsCachedAndNullWhenNotConnected(t *testing.T) {
	ctx := context.Background()
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	backendA := fake.New("a@example.com", fake.WithClock(clkA))
	backendA.SetCompiledConfigVersion(gm.ConfigVersion{Year: 2026, Month: 9, Day: 2})
	backendA.SetLiveConfigVersion(gm.ConfigVersion{Year: 2026, Month: 9, Day: 5})
	backendA.SetDefaultSMSApp(true)
	acctA, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	backendB := fake.New("b@example.com", fake.WithClock(clkB))
	acctB, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := h.sup.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]accounts.AccountHealth{}
	for _, b := range blocks {
		byID[b.AccountID] = b
	}
	ha := byID[acctA.ID]
	if ha.Google == nil {
		t.Fatal("a connected account has a null google block")
	}
	if !ha.Google.ConfigVersionStale {
		t.Error("a live config version on a different date is not reported stale")
	}
	if !ha.Google.IsDefaultSMSApp {
		t.Error("is_default_sms_app was not cached")
	}
	// state_reason is a POINTER so that "no reason" encodes as null, the way
	// GET /v1/accounts encodes it: section 7.5 says the health block is the
	// same per-account object, and "" is not null.
	if ha.State != accounts.StateConnected || ha.StateReason != nil {
		t.Errorf("health reports %s/%v", ha.State, ha.StateReason)
	}
	if !ha.PhoneResponding {
		t.Error("phone_responding starts false")
	}

	// A phone that stops answering is a health fact, not a state change.
	acctA.Apply(ctx, &gm.EventPhoneNotResponding{})
	refreshed, err := h.sup.HealthFor(ctx, acctA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.PhoneResponding {
		t.Error("phone_responding stayed true after PhoneNotResponding")
	}
	if refreshed.State != accounts.StateConnected {
		t.Errorf("an unresponsive phone moved the state to %s", refreshed.State)
	}

	// Signing B out makes its google block null: a stale cached value on a
	// disconnected account would read as a live fact about a phone nobody is
	// talking to.
	if err := h.sup.SignOut(ctx, acctB); err != nil {
		t.Fatal(err)
	}
	hb, err := h.sup.HealthFor(ctx, acctB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hb.Google != nil {
		t.Errorf("a signed-out account's google block is %+v, want null", hb.Google)
	}
	// A's block is untouched by B's sign-out.
	stillA, err := h.sup.HealthFor(ctx, acctA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillA.Google == nil {
		t.Error("B's sign-out blanked A's google block")
	}
}

// sweep.last_sweep_at and backfill.completed_at come from the accounts row,
// not from process memory: an operator asking "when did this account last
// sweep?" is usually asking BECAUSE the process restarted.
func TestSweepAndBackfillTimestampsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	h.sup.Sweep = &recordingSweeper{}
	backfill := newBackfiller()
	h.sup.Backfill = backfill
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	acct, err := h.sup.Pair(ctx, fake.New("a@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Connecting ran one sweep, and it was stamped on the row.
	row, err := h.store.Account(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastSweepAtMS == 0 {
		t.Fatal("a completed sweep did not stamp accounts.last_sweep_at_ms")
	}
	completed := clk.Now().Add(-time.Hour)
	if err := h.store.SetAccountBackfillComplete(ctx, acct.ID, completed); err != nil {
		t.Fatal(err)
	}

	// The restart: a fresh supervisor over the same store, holding no
	// in-memory counters at all.
	restarted := accounts.New(h.store, h.sessions, clk, nil)
	restarted.Backfill = backfill
	block, err := restarted.HealthFor(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if block.Sweep.LastSweepAt == nil {
		t.Fatal("last_sweep_at is null after a restart")
	}
	if want := wire.Instant(time.UnixMilli(row.LastSweepAtMS)); *block.Sweep.LastSweepAt != want {
		t.Errorf("last_sweep_at = %s, want the stored %s",
			*block.Sweep.LastSweepAt, want)
	}
	// The total is a process-lifetime counter, so a fresh process reports 0
	// while the timestamp above still answers "has it swept?".
	if block.Sweep.SweepsTotal != 0 {
		t.Errorf("sweeps_total = %d in a fresh process, want 0", block.Sweep.SweepsTotal)
	}
	if block.Backfill.CompletedAt == nil ||
		*block.Backfill.CompletedAt != wire.Instant(completed) {
		t.Errorf("backfill.completed_at = %v, want the stored %v",
			block.Backfill.CompletedAt, wire.Instant(completed))
	}
	// A Backfiller supplies the live state; the completion instant stays the
	// stored one.
	if block.Backfill.State != "running" {
		t.Errorf("backfill.state = %q, want the worker's %q", block.Backfill.State, "running")
	}

	// Clearing it -- what re-opening a backfill after a re-pair does -- makes
	// completed_at null again.
	if err := h.store.SetAccountBackfillComplete(ctx, acct.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	block, err = restarted.HealthFor(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if block.Backfill.CompletedAt != nil {
		t.Errorf("backfill.completed_at = %v after clearing, want null", block.Backfill.CompletedAt)
	}
}

// The reason vocabulary is closed: an invented one does not exist, and every
// state the supervisor can write pairs with a reason from section 4.7.
func TestReasonVocabularyIsClosed(t *testing.T) {
	for _, r := range accounts.Reasons() {
		if !r.Valid() {
			t.Errorf("%q is in Reasons() but reports itself invalid", r)
		}
	}
	if !accounts.ReasonNone.Valid() {
		t.Error("the empty reason must be valid: it is the absence of one")
	}
	if accounts.Reason("looks_plausible").Valid() {
		t.Error("an invented reason was accepted")
	}
	if len(accounts.States()) != 7 {
		t.Errorf("the state vocabulary has %d entries, want 7", len(accounts.States()))
	}
	if accounts.Resting(accounts.StatePairing) {
		t.Error("pairing must never be a resting state")
	}
	for _, s := range accounts.States() {
		if s == accounts.StatePairing {
			continue
		}
		if !accounts.Resting(s) {
			t.Errorf("%s is not a resting state", s)
		}
	}
	// Only connected and degraded accept writes.
	for _, s := range accounts.States() {
		want := s == accounts.StateConnected || s == accounts.StateDegraded
		if accounts.Writable(s) != want {
			t.Errorf("Writable(%s) = %v, want %v", s, accounts.Writable(s), want)
		}
	}
}
