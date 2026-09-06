package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// last_sweep_at_ms moves only forward. Two sweeps for one account can
// overlap -- a timer sweep and a BROWSER_ACTIVE sweep, say -- and the slower
// one finishing second must not rewind the record, or the next sweep would
// re-walk ground the faster one already covered (spec section 5.4).
//
// Plant: drop the MAX() from SetAccountLastSweep and this test fails at
// "the later sweep was rewound". Planted 2026-09-06.
func TestAccountSweepAndBackfillTimestamps(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	st, err := store.Open(t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Two accounts, because these are per-account columns and never a
	// server_meta key: two accounts would race on one row and the first to
	// finish would mark the whole server swept (spec sections 4.2, 5.2).
	a := seedAccountForSweepTest(t, st, "a@example.test")
	b := seedAccountForSweepTest(t, st, "b@example.test")

	early := clk.Now()
	late := early.Add(10 * time.Minute)

	if err := st.SetAccountLastSweep(ctx, a, late); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountLastSweep(ctx, a, early); err != nil {
		t.Fatal(err)
	}
	rowA, err := st.Account(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if rowA.LastSweepAtMS != late.UnixMilli() {
		t.Errorf("the later sweep was rewound: last_sweep_at_ms = %d, want %d",
			rowA.LastSweepAtMS, late.UnixMilli())
	}

	// The other account is untouched: these are per-account facts.
	rowB, err := st.Account(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if rowB.LastSweepAtMS != 0 {
		t.Errorf("account b's sweep timestamp moved: %d", rowB.LastSweepAtMS)
	}

	// backfill_complete_at_ms is set once and re-stamped by a re-backfill;
	// the zero time clears it, which is what re-opening a backfill does.
	if err := st.SetAccountBackfillComplete(ctx, a, early); err != nil {
		t.Fatal(err)
	}
	if rowA, err = st.Account(ctx, a); err != nil {
		t.Fatal(err)
	}
	if rowA.BackfillCompleteAtMS != early.UnixMilli() {
		t.Fatalf("backfill_complete_at_ms = %d, want %d", rowA.BackfillCompleteAtMS, early.UnixMilli())
	}
	if err := st.SetAccountBackfillComplete(ctx, a, late); err != nil {
		t.Fatal(err)
	}
	if rowA, err = st.Account(ctx, a); err != nil {
		t.Fatal(err)
	}
	if rowA.BackfillCompleteAtMS != late.UnixMilli() {
		t.Errorf("a re-backfill did not re-stamp the completion: %d", rowA.BackfillCompleteAtMS)
	}
	if err := st.SetAccountBackfillComplete(ctx, a, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if rowA, err = st.Account(ctx, a); err != nil {
		t.Fatal(err)
	}
	if rowA.BackfillCompleteAtMS != 0 {
		t.Errorf("re-opening a backfill did not clear the completion: %d", rowA.BackfillCompleteAtMS)
	}
}

func seedAccountForSweepTest(t *testing.T, st *store.Store, address string) string {
	t.Helper()
	id := store.AccountID(address)
	if err := st.UpsertAccount(context.Background(), store.Account{
		ID:            id,
		GoogleAccount: address,
		State:         "connected",
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
