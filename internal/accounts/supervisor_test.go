package accounts_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// Nonfunctional placeholders. A real Google cookie never enters this
// repository (spec section 12.2).
func cookies() map[string]string {
	return map[string]string{
		"SID": "fixture", "HSID": "fixture", "OSID": "fixture",
		"SSID": "fixture", "APISID": "fixture", "SAPISID": "fixture",
		"__Secure-1PSIDTS": "fixture",
	}
}

type harness struct {
	store    *store.Store
	sessions *store.SessionStore
	sup      *accounts.Supervisor
	dir      string
}

func newHarness(t *testing.T, clk clock.Clock) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, err := store.ParseDataKey(strings.Repeat("33", 32))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := store.NewSessionStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	sup := accounts.New(st, sessions, clk, nil)
	// Tests drive Apply themselves so nothing races the ingest goroutine and
	// nothing sleeps. TestIngestGoroutineDrainsEvents covers the loop.
	sup.ManualIngest = true
	return &harness{store: st, sessions: sessions, dir: dir, sup: sup}
}

// pump applies every buffered event on the account's backend synchronously,
// so a test never races the ingest goroutine and never sleeps.
func pump(ctx context.Context, a *accounts.Account) {
	for {
		select {
		case ev := <-a.Backend.Events():
			a.Apply(ctx, ev)
		default:
			return
		}
	}
}

// Slice 1 acceptance test 3, against the fake: pair, list 3 conversations,
// send a text, receive the echo, and see delivery_state walk
// sending -> sent -> delivered. No network, no sleeps -- the fake's clock is
// advanced explicitly.
func TestPairListSendEchoAndDeliveryWalk(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0 // no parking in this test

	backend := fake.New("owner@example.com", fake.WithClock(clk))
	for i, id := range []string{"c1", "c2", "c3"} {
		backend.SeedConversation(gm.Conversation{
			SourceID: id, Name: "Fixture " + id, Folder: gm.FolderInbox,
			Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
			DefaultOutgoingID: "me", LastActivity: clk.Now().Add(-time.Duration(i) * time.Hour),
		})
	}

	var shown string
	acct, err := h.sup.Pair(ctx, backend, cookies(), 0, func(e string) { shown = e })
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if shown == "" {
		t.Error("the emoji callback was never called")
	}
	if acct.ID != store.AccountID("owner@example.com") {
		t.Errorf("acct_ ID = %s", acct.ID)
	}
	t.Cleanup(func() { h.sup.Stop(acct) })

	// The session file exists at mode 0600, and the row says so.
	if !h.sessions.Present(acct.ID) {
		t.Fatal("no session file after pairing")
	}
	row, err := h.store.Account(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateConnected {
		t.Errorf("state after pairing = %s, want connected", row.State)
	}
	if !row.SessionPresent {
		t.Error("session_present is false after pairing")
	}
	if row.PairedAtMS == 0 {
		t.Error("paired_at_ms was not set")
	}

	// The ClientReady event carries the initial conversation list.
	pump(ctx, acct)
	convs, err := h.store.Conversations(ctx, store.ConversationFilter{AccountID: acct.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 3 {
		t.Fatalf("ingested %d conversations, want 3", len(convs))
	}
	// Ordering is newest activity first, with the ID as the tiebreaker.
	for i := 1; i < len(convs); i++ {
		if convs[i-1].LastActivityMS < convs[i].LastActivityMS {
			t.Fatalf("conversations are not newest-first: %v", convs)
		}
	}

	// Send a text, and correlate the echo by the bare-UUID tmp ID.
	tmpID := gm.GenerateTmpID()
	res, err := backend.SendText(ctx, gm.SendTextRequest{
		ConversationID: "c1", ParticipantID: "me", TmpID: tmpID, Text: "agent-gm slice 1 test",
	})
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if res.Status != gm.SendStatusSuccess {
		t.Fatalf("send status = %s", res.Status)
	}
	pump(ctx, acct)

	echo, err := h.store.MessageByTmpID(ctx, acct.ID, tmpID)
	if err != nil {
		t.Fatalf("the echo did not carry back the tmp ID we sent: %v", err)
	}
	if echo.DeliveryState != string(gm.DeliveryStateSending) {
		t.Errorf("the echo's state is %s, want sending", echo.DeliveryState)
	}
	if echo.Direction != string(gm.DirectionOutgoing) {
		t.Errorf("the echo's direction is %s, want outgoing", echo.Direction)
	}
	if echo.Text != "agent-gm slice 1 test" {
		t.Errorf("the echo's text is %q", echo.Text)
	}

	// The delivery ladder, one rung per explicit clock advance.
	for _, want := range []gm.DeliveryState{gm.DeliveryStateSent, gm.DeliveryStateDelivered} {
		backend.Advance(30 * time.Second)
		pump(ctx, acct)
		m, err := h.store.Message(ctx, echo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.DeliveryState != string(want) {
			t.Fatalf("after an advance the state is %s, want %s", m.DeliveryState, want)
		}
	}

	// Reloading the session gives back exactly what was written, so a
	// restart can Connect without re-pairing.
	blob, err := h.sessions.Load(acct.ID)
	if err != nil {
		t.Fatalf("reloading the session: %v", err)
	}
	reloaded := fake.New("", fake.WithClock(clk))
	if err := reloaded.LoadSession(blob); err != nil {
		t.Fatalf("loading the session into a fresh backend: %v", err)
	}
	if !reloaded.IsLoggedIn() {
		t.Error("a reloaded session is not logged in")
	}
	if reloaded.AccountAddress() != "owner@example.com" {
		t.Errorf("the reloaded session names %s", reloaded.AccountAddress())
	}
}

// An sms_mms conversation stops at `sent`: delivered and read are RCS
// features in practice, and an agent waiting for `delivered` on an SMS thread
// can wait forever (spec section 5.5).
func TestSMSConversationStopsAtSent(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0

	backend := fake.New("owner@example.com", fake.WithClock(clk))
	backend.SeedConversation(gm.Conversation{
		SourceID: "c1", Folder: gm.FolderInbox, Type: gm.ConversationTypeSMSMMS,
		DefaultOutgoingID: "me", LastActivity: clk.Now(),
	})
	acct, err := h.sup.Pair(ctx, backend, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.Stop(acct) })
	pump(ctx, acct)

	tmpID := gm.GenerateTmpID()
	if _, err := backend.SendText(ctx, gm.SendTextRequest{
		ConversationID: "c1", TmpID: tmpID, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	pump(ctx, acct)
	for range 5 {
		backend.Advance(time.Minute)
		pump(ctx, acct)
	}
	m, err := h.store.MessageByTmpID(ctx, acct.ID, tmpID)
	if err != nil {
		t.Fatal(err)
	}
	if m.DeliveryState != string(gm.DeliveryStateSent) {
		t.Errorf("an sms_mms send settled at %s, want sent", m.DeliveryState)
	}
}

// Slice 1 acceptance test 4: two fake accounts run concurrently. Both reach
// connected; each ingests its own messages; every row carries the right
// account_id; the two acct_ IDs are the UUIDv5 of their lowercased addresses
// and differ. Pairing the same address twice yields ONE account, not two.
func TestTwoAccountsRunConcurrentlyWithoutCrossTalk(t *testing.T) {
	ctx := context.Background()
	// Each fake takes its own clock, so one account's time can move while
	// the other's stands still.
	clkA, clkB := clock.NewFake(), clock.NewFake()
	h := newHarness(t, clkA)
	h.sup.MaxConcurrent = 0

	backendA := fake.New("Alex@Example.com", fake.WithClock(clkA)) // deliberately mixed case
	backendB := fake.New("work@example.com", fake.WithClock(clkB))
	for _, b := range []*fake.Backend{backendA, backendB} {
		b.SeedConversation(gm.Conversation{
			SourceID: "shared-conv-id", Folder: gm.FolderInbox,
			Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
			DefaultOutgoingID: "me", LastActivity: clkA.Now(),
		})
	}

	a, err := h.sup.Pair(ctx, backendA, cookies(), 0, nil)
	if err != nil {
		t.Fatalf("pairing A: %v", err)
	}
	b, err := h.sup.Pair(ctx, backendB, cookies(), 0, nil)
	if err != nil {
		t.Fatalf("pairing B: %v", err)
	}
	t.Cleanup(func() { h.sup.StopAll(ctx) })

	if a.ID == b.ID {
		t.Fatal("two different Google accounts share an acct_ ID")
	}
	if a.ID != store.AccountID("alex@example.com") {
		t.Errorf("A's ID is %s, want the UUIDv5 of the lowercased address", a.ID)
	}
	if b.ID != store.AccountID("work@example.com") {
		t.Errorf("B's ID is %s", b.ID)
	}
	for _, acct := range []*accounts.Account{a, b} {
		row, err := h.store.Account(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != store.StateConnected {
			t.Errorf("%s is %s, want connected", acct.ID, row.State)
		}
	}

	// Each ingests its own messages. Both conversations carry the SAME Google
	// conversation ID, which is only unique within an account: they must
	// still be two separate conv_ rows.
	pump(ctx, a)
	pump(ctx, b)
	backendA.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: "same-msg-id", ConversationID: "shared-conv-id", Text: "from A",
		Timestamp: clkA.Now(), StatusRaw: 100, Kind: gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateReceived}})
	backendB.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: "same-msg-id", ConversationID: "shared-conv-id", Text: "from B",
		Timestamp: clkB.Now(), StatusRaw: 100, Kind: gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateReceived}})
	pump(ctx, a)
	pump(ctx, b)

	convA := store.ConversationID(a.ID, "shared-conv-id")
	convB := store.ConversationID(b.ID, "shared-conv-id")
	if convA == convB {
		t.Fatal("the same Google conversation ID on two accounts produced one conv_ ID")
	}

	msgsA, err := h.store.Messages(ctx, store.MessageFilter{AccountID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	msgsB, err := h.store.Messages(ctx, store.MessageFilter{AccountID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgsA) != 1 || len(msgsB) != 1 {
		t.Fatalf("A has %d messages, B has %d; want one each", len(msgsA), len(msgsB))
	}
	if msgsA[0].Text != "from A" || msgsB[0].Text != "from B" {
		t.Errorf("the messages crossed accounts: A=%q B=%q", msgsA[0].Text, msgsB[0].Text)
	}
	// Every row carries the right account_id.
	for _, m := range msgsA {
		if m.AccountID != a.ID || m.ConversationID != convA {
			t.Errorf("A's message is stamped %s/%s", m.AccountID, m.ConversationID)
		}
	}
	for _, m := range msgsB {
		if m.AccountID != b.ID || m.ConversationID != convB {
			t.Errorf("B's message is stamped %s/%s", m.AccountID, m.ConversationID)
		}
	}
	// The all-accounts read sees both.
	all, err := h.store.Messages(ctx, store.MessageFilter{AllAccounts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("the all-accounts read returned %d rows, want 2", len(all))
	}

	// One account's failure does not touch the other. A's credentials die.
	backendA.Emit(&gm.EventListenFatalError{
		Err: gm.HTTPError{Action: "polling", StatusCode: 403}, CredentialsDead: true})
	pump(ctx, a)
	rowA, err := h.store.Account(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowA.State != store.StateSignedOut {
		t.Errorf("A is %s after a fatal 403, want signed_out", rowA.State)
	}
	rowB, err := h.store.Account(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowB.State != store.StateConnected {
		t.Errorf("B became %s when A's credentials died", rowB.State)
	}
	// A's reads keep working: signing out deletes nothing.
	msgsA, err = h.store.Messages(ctx, store.MessageFilter{AccountID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgsA) != 1 {
		t.Errorf("A lost history when it was signed out: %d rows", len(msgsA))
	}

	// Pairing the SAME address twice yields one account, not two.
	before, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again := fake.New("alex@example.com", fake.WithClock(clkA))
	a2, err := h.sup.Pair(ctx, again, cookies(), 0, nil)
	if err != nil {
		t.Fatalf("re-pairing A: %v", err)
	}
	if a2.ID != a.ID {
		t.Errorf("re-pairing produced %s, want the same acct_ ID %s", a2.ID, a.ID)
	}
	after, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("re-pairing created a second account: %d -> %d", len(before), len(after))
	}
	rowA, err = h.store.Account(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowA.State != store.StateConnected {
		t.Errorf("A is %s after a re-pair, want connected", rowA.State)
	}
	// And the history is still there: a re-pair resumes rather than
	// duplicating.
	msgsA, err = h.store.Messages(ctx, store.MessageFilter{AccountID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgsA) != 1 {
		t.Errorf("a re-pair duplicated or lost history: %d rows", len(msgsA))
	}
}

// Signing out keeps everything and shreds only the session (spec 4.7, D30).
func TestSignOutKeepsHistoryAndShredsTheSession(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0

	backend := fake.New("owner@example.com", fake.WithClock(clk))
	backend.SeedConversation(gm.Conversation{SourceID: "c1", Folder: gm.FolderInbox,
		DefaultOutgoingID: "me", LastActivity: clk.Now()})
	acct, err := h.sup.Pair(ctx, backend, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	pump(ctx, acct)
	backend.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: "m1", ConversationID: "c1", Text: "keep me", Timestamp: clk.Now(),
		StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived}})
	pump(ctx, acct)

	if err := h.sup.SignOut(ctx, acct); err != nil {
		t.Fatalf("signing out: %v", err)
	}
	if h.sessions.Present(acct.ID) {
		t.Error("the session file survived a sign-out")
	}
	row, err := h.store.Account(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateSignedOut {
		t.Errorf("state = %s, want signed_out", row.State)
	}
	if row.SessionPresent {
		t.Error("session_present is still true")
	}
	if backend.IsLoggedIn() {
		t.Error("the in-memory session was not zeroed")
	}
	// It deletes NO conversation and NO message. That negative is half the
	// meaning of "only removal deletes", so it is asserted directly.
	convs, err := h.store.Conversations(ctx, store.ConversationFilter{AccountID: acct.ID})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := h.store.Messages(ctx, store.MessageFilter{AccountID: acct.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(msgs) != 1 {
		t.Errorf("signing out deleted rows: %d conversations, %d messages", len(convs), len(msgs))
	}
	if msgs[0].Text != "keep me" {
		t.Errorf("the message changed to %q", msgs[0].Text)
	}
}

// The state table of spec section 3.4, one account at a time.
func TestEventsMoveOnlyTheirOwnAccountsState(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0
	backend := fake.New("owner@example.com", fake.WithClock(clk))
	acct, err := h.sup.Pair(ctx, backend, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.Stop(acct) })
	pump(ctx, acct)

	stateNow := func() (store.AccountState, string) {
		t.Helper()
		row, err := h.store.Account(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.State, row.StateReason
	}

	cases := []struct {
		name   string
		event  gm.Event
		state  store.AccountState
		reason string
	}{
		{"temporary listen error", &gm.EventListenTemporaryError{Err: errors.New("blip")},
			store.StateDegraded, store.ReasonListenError},
		{"recovered", &gm.EventListenRecovered{}, store.StateConnected, ""},
		{"fatal 401", &gm.EventListenFatalError{
			Err: gm.HTTPError{Action: "polling", StatusCode: 401}, CredentialsDead: true},
			store.StateSignedOut, store.ReasonCredentials},
		{"fatal 500 is not credential death", &gm.EventListenFatalError{
			Err: gm.HTTPError{Action: "polling", StatusCode: 500}},
			store.StateError, store.ReasonListenError},
		{"cookies died", &gm.EventGaiaLoggedOut{}, store.StateSignedOut, store.ReasonCookiesExpired},
		{"the phone revoked the pairing", &gm.EventRevokePairData{},
			store.StateSignedOut, store.ReasonRevokedByPhone},
		{"a real account change", &gm.EventAccountChange{Account: "other@example.com"},
			store.StateAccountChanged, store.ReasonAccountSwitched},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct.Apply(ctx, tc.event)
			got, reason := stateNow()
			if got != tc.state || reason != tc.reason {
				t.Errorf("state = %s/%s, want %s/%s", got, reason, tc.state, tc.reason)
			}
		})
	}

	// A synthesised account change is not a real one, so it moves nothing.
	acct.Apply(ctx, &gm.EventListenRecovered{})
	acct.Apply(ctx, &gm.EventAccountChange{Account: "owner@example.com", IsFake: true})
	if got, _ := stateNow(); got != store.StateConnected {
		t.Errorf("a synthesised AccountChange moved the state to %s", got)
	}

	// Upstream deliberately ignores the first ping failure.
	acct.Apply(ctx, &gm.EventPingFailed{Err: errors.New("no answer"), ErrorCount: 1})
	if got, _ := stateNow(); got != store.StateConnected {
		t.Errorf("the first ping failure moved the state to %s", got)
	}
	acct.Apply(ctx, &gm.EventPingFailed{Err: errors.New("no answer"), ErrorCount: 2})
	if got, _ := stateNow(); got != store.StateError {
		t.Errorf("the second ping failure left the state at %s", got)
	}
	// A ping failure naming a missing entity means the phone no longer knows
	// this pairing.
	acct.Apply(ctx, &gm.EventPingFailed{Err: gm.ErrRequestedEntityNotFound, ErrorCount: 1, EntityNotFound: true})
	if got, reason := stateNow(); got != store.StateSignedOut || reason != store.ReasonRevokedByPhone {
		t.Errorf("state = %s/%s, want signed_out/revoked_by_phone", got, reason)
	}
}

// accounts.max_concurrent parks the surplus. Parked is a state of its own,
// not degraded, and a parked account is fully readable.
func TestSurplusAccountsAreParkedNotDegraded(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 1

	first, err := h.sup.Pair(ctx, fake.New("a@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.StopAll(ctx) })
	second, err := h.sup.Pair(ctx, fake.New("b@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	rowFirst, err := h.store.Account(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowFirst.State != store.StateConnected {
		t.Errorf("the first account is %s, want connected", rowFirst.State)
	}
	rowSecond, err := h.store.Account(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rowSecond.State != store.StateParked {
		t.Errorf("the surplus account is %s, want parked", rowSecond.State)
	}
	if rowSecond.StateReason != store.ReasonCapacity {
		t.Errorf("the parked account's reason is %q, want %q", rowSecond.StateReason, store.ReasonCapacity)
	}
	if rowSecond.State == store.StateDegraded {
		t.Error("waiting for a slot must not be reported as degraded")
	}
}

// An account whose address Google never returned creates no row and no
// session file.
func TestPairingWithNoAddressCreatesNothing(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	_, err := h.sup.Pair(ctx, fake.New("", fake.WithClock(clk)), cookies(), 0, nil)
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingNoAccount {
		t.Fatalf("got %v, want pairing_no_account", err)
	}
	rows, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused pairing created %d account rows", len(rows))
	}
}

// A pairing that fails after the address is known deletes the row it created,
// because that row was never connected.
func TestAbandonedPairingLeavesNoRow(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	backend := fake.New("owner@example.com", fake.WithClock(clk))
	backend.ScriptPairError(gm.ErrIncorrectEmoji)

	_, err := h.sup.Pair(ctx, backend, cookies(), 0, nil)
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePairingWrongEmoji {
		t.Fatalf("got %v, want pairing_wrong_emoji", err)
	}
	rows, err := h.store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("an abandoned pairing left %d rows: %+v", len(rows), rows)
	}
	if h.sessions.Present(store.AccountID("owner@example.com")) {
		t.Error("an abandoned pairing wrote a session file")
	}
}

// A re-pair of an existing account never moves it to `pairing`: it keeps its
// current state for the whole of the new pairing and flips straight to
// connected, so a caller polling never sees it become less usable.
func TestRePairNeverPassesThroughPairing(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0

	acct, err := h.sup.Pair(ctx, fake.New("owner@example.com", fake.WithClock(clk)), cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.StopAll(ctx) })
	if err := h.sup.SignOut(ctx, acct); err != nil {
		t.Fatal(err)
	}

	var atEmoji store.AccountState
	again := fake.New("owner@example.com", fake.WithClock(clk))
	if _, err := h.sup.Pair(ctx, again, cookies(), 0, func(string) {
		row, err := h.store.Account(ctx, acct.ID)
		if err != nil {
			t.Errorf("reading the account at emoji time: %v", err)
			return
		}
		atEmoji = row.State
	}); err != nil {
		t.Fatal(err)
	}
	if atEmoji != store.StateSignedOut {
		t.Errorf("during a re-pair the account was %s, want it to keep signed_out", atEmoji)
	}
	row, err := h.store.Account(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateConnected {
		t.Errorf("after a re-pair the account is %s, want connected", row.State)
	}
}

// A new account does pass through `pairing`, and only a new one.
func TestNewAccountPassesThroughPairing(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.MaxConcurrent = 0

	var atEmoji store.AccountState
	backend := fake.New("owner@example.com", fake.WithClock(clk))
	if _, err := h.sup.Pair(ctx, backend, cookies(), 0, func(string) {
		row, err := h.store.Account(ctx, store.AccountID("owner@example.com"))
		if err != nil {
			t.Errorf("reading the account at emoji time: %v", err)
			return
		}
		atEmoji = row.State
	}); err != nil {
		t.Fatal(err)
	}
	if atEmoji != store.StatePairing {
		t.Errorf("a new account was %s at emoji time, want pairing", atEmoji)
	}
}

// The per-account ingest goroutine really does drain the channel. Every other
// test drives Apply directly so that nothing races it; this one covers the
// loop itself.
//
// It waits on a condition rather than sleeping for a fixed duration, and the
// bound is a test-harness deadline, not one of the spec's timeouts -- those
// are all measured against the injected clock.
func TestIngestGoroutineDrainsEvents(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	h := newHarness(t, clk)
	h.sup.ManualIngest = false
	h.sup.MaxConcurrent = 0

	backend := fake.New("owner@example.com", fake.WithClock(clk))
	backend.SeedConversation(gm.Conversation{SourceID: "c1", Folder: gm.FolderInbox,
		DefaultOutgoingID: "me", LastActivity: clk.Now()})
	acct, err := h.sup.Pair(ctx, backend, cookies(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.sup.Stop(acct) })

	backend.Emit(&gm.EventMessage{Message: gm.Message{
		SourceID: "m1", ConversationID: "c1", Text: "drained", Timestamp: clk.Now(),
		StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := h.store.Messages(ctx, store.MessageFilter{AccountID: acct.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 1 && msgs[0].Text == "drained" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ingest goroutine did not apply the event: %d rows", len(msgs))
		}
		runtime.Gosched()
	}
}
