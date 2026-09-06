package accounts_test

import (
	"context"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/gm/fake"
	"github.com/thisnick/agent-gm/internal/store"
)

// R-1. `agent-gm serve` built its supervisor with neither a Backfiller nor a
// Sweeper, and no type in the tree implemented either outside a test double.
// So section 5.2's backfill and section 5.4's sweep -- the sweep D26 calls
// mandatory BECAUSE the library's dedup loses messages -- never ran in the
// shipped binary, while both passed their own tests against injected fakes.
//
// The first fix is that a real implementation exists and the compiler knows
// it. accounts.Workers is asserted to satisfy both interfaces in workers.go;
// this asserts the behaviour.
//
// Plant: make Workers.Start return nil without launching the walk, and
// TestWorkersActuallyBackfillAndSweep fails at "backfill_state has no rows".
// Planted 2026-09-07.
func TestWorkersActuallyBackfillAndSweep(t *testing.T) {
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

	const address = "workers@example.test"
	be := fake.New(address, fake.WithClock(clk))
	seedForBackfill(t, be, clk)
	// The backend has to be paired before it will connect, which is what a
	// resumed account's session gives it in production.
	if _, err := be.StartGooglePairing(ctx, fixtureCookies(), 0, func(string) {}); err != nil {
		t.Fatalf("pairing the fake: %v", err)
	}

	id := store.AccountID(address)
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: address, State: store.StateConnected, SessionPresent: true,
	}); err != nil {
		t.Fatal(err)
	}
	a := sup.Adopt(ctx, id, address, be)

	// Wired exactly as cmd/agent-gm/serve.go wires it.
	workers := accounts.NewWorkers(st, func(accountID string) (*core.Account, error) {
		acct, err := sup.Get(accountID)
		if err != nil {
			return nil, err
		}
		k := testKey(t)
		return &core.Account{
			ID: accountID, Store: st, Backend: acct.Backend, Clock: clk,
			Config: core.DefaultConfig(), Source: "test", DataKey: &k,
		}, nil
	}, nil)
	sup.Backfill = workers
	sup.Sweep = workers

	if err := sup.Start(ctx, a); err != nil {
		t.Fatalf("starting the account: %v", err)
	}

	// The backfill is a goroutine, so this waits on the OUTCOME -- rows in
	// backfill_state -- rather than on a duration.
	waitFor(t, 5*time.Second, func() bool {
		var n int64
		row := st.Reader().QueryRow(`SELECT COUNT(*) FROM backfill_state`)
		if err := row.Scan(&n); err != nil {
			return false
		}
		return n > 0
	}, "backfill_state has no rows: nothing ran the backfill")

	// And the messages the fake holds are in the database, which is what
	// backfill is for.
	var messages int64
	if err := st.Reader().QueryRow(`SELECT COUNT(*) FROM messages WHERE account_id = ?`, id).
		Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages == 0 {
		t.Error("backfill wrote no messages")
	}

	// The sweep, driven the way the supervisor drives it.
	before := be.CallCount("ListConversations")
	if err := workers.Sweep(ctx, id, clk.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if be.CallCount("ListConversations") <= before {
		t.Error("the sweep made no ListConversations call; it did not run")
	}

	// last_sweep_at_ms is recorded, so GET /v1/health reports a sweep rather
	// than a permanent null.
	row, err := st.Account(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastSweepAtMS == 0 {
		t.Error("accounts.last_sweep_at_ms is still null after a sweep")
	}
}

// A backfill paused by THIS account's phone-side sync does not start, and
// resumes when that phone reports the sync complete. One sleepy phone never
// stalls another account (spec section 5.2).
func TestWorkersPauseIsPerAccount(t *testing.T) {
	w := accounts.NewWorkers(nil, func(string) (*core.Account, error) {
		t.Error("a paused backfill built an engine, so it did not wait")
		return nil, nil
	}, nil)
	ctx := context.Background()
	// Paused BEFORE the start, which is the ordering that actually happens:
	// a MOBILE_DATABASE_SYNC_STARTED alert can arrive before ClientReady.
	w.Pause("acct_a")
	if err := w.Start(ctx, "acct_a"); err != nil {
		t.Fatal(err)
	}
	// Pausing acct_a says nothing about acct_b: its state is its own.
	if got := w.Progress("acct_b"); got.State != "pending" {
		t.Errorf("acct_b's backfill state is %q; pausing acct_a must not touch it", got.State)
	}
	w.Stop("acct_a")
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func testKey(t *testing.T) store.DataKey {
	t.Helper()
	k, err := store.ParseDataKey("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func seedForBackfill(t *testing.T, be *fake.Backend, clk *clock.Fake) {
	t.Helper()
	be.SeedConversation(gm.Conversation{
		SourceID: "conv-workers", Name: "Fixture", Type: gm.ConversationTypeRCS,
		SendModeRaw: gm.SendModeAuto, Folder: gm.FolderInbox,
		DefaultOutgoingID: "me", LastActivity: clk.Now(),
		Participants: []gm.Participant{
			{SourceID: "me", IsMe: true, IsVisible: true},
			{SourceID: "them", PhoneE164: "+12025550123", IsVisible: true},
		},
	})
	for i := 0; i < 3; i++ {
		be.SeedMessage(gm.Message{
			SourceID: "msg-workers-" + string(rune('a'+i)), ConversationID: "conv-workers",
			ParticipantID: "them", Text: "fixture", Timestamp: clk.Now(),
			StatusRaw: 100, Kind: gm.MessageKindMessage, DeliveryState: gm.DeliveryStateReceived,
		})
	}
}

func fixtureCookies() map[string]string {
	out := map[string]string{}
	for _, n := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
		out[n] = "FIXTURE-" + n
	}
	return out
}
