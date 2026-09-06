package store_test

// Spec sections 6.3, 6.4 and 6.6: idempotency, the transition table, crash
// recovery and the pending_timeout reaper.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/store"
)

func newOperation(acct, auth, kind, key string) store.Operation {
	return store.Operation{
		AccountID: acct, AuthorizationID: auth, Kind: kind,
		IdempotencyKey: key, RequestFingerprint: "fp", RequestPayloadJSON: `{}`,
	}
}

// The fingerprint is over the CANONICAL body: reordered keys are a replay and
// any changed value is not (spec section 6.3).
func TestRequestFingerprintIsCanonical(t *testing.T) {
	a, err := store.RequestFingerprint([]byte(`{"b":2,"a":[1,{"y":1,"x":2}],"c":"z"}`))
	if err != nil {
		t.Fatalf("fingerprinting: %v", err)
	}
	b, err := store.RequestFingerprint([]byte("  {\n \"c\":\"z\",\n \"a\" : [1, {\"x\":2,\"y\":1}],\n \"b\":2}\n"))
	if err != nil {
		t.Fatalf("fingerprinting: %v", err)
	}
	if a != b {
		t.Errorf("reordered keys and whitespace changed the fingerprint:\n %s\n %s", a, b)
	}

	for _, different := range []string{
		`{"b":2,"a":[1,{"y":1,"x":3}],"c":"z"}`, // a changed value
		`{"b":2,"a":[{"y":1,"x":2},1],"c":"z"}`, // array order is significant
		`{"b":2,"a":[1,{"y":1,"x":2}]}`,         // a dropped key
		`{"b":"2","a":[1,{"y":1,"x":2}],"c":"z"}`,
	} {
		c, err := store.RequestFingerprint([]byte(different))
		if err != nil {
			t.Fatalf("fingerprinting %s: %v", different, err)
		}
		if c == a {
			t.Errorf("%s fingerprinted the same as the original", different)
		}
	}
}

func TestOperationTransitionTable(t *testing.T) {
	all := []store.OperationStatus{store.OpRunning, store.OpSucceeded, store.OpPending,
		store.OpFailed, store.OpUnknown}
	allowed := map[string]bool{
		"running>succeeded": true, "running>failed": true, "running>pending": true,
		"running>unknown":   true,
		"pending>succeeded": true, "pending>failed": true, "pending>unknown": true,
		"unknown>succeeded": true, "unknown>failed": true,
	}
	for _, from := range all {
		for _, to := range all {
			want := from == to || allowed[string(from)+">"+string(to)]
			if got := store.OperationTransitionAllowed(from, to); got != want {
				t.Errorf("%s -> %s: allowed = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range []store.OperationStatus{store.OpSucceeded, store.OpFailed, store.OpUnknown} {
		if !store.OperationTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []store.OperationStatus{store.OpRunning, store.OpPending} {
		if store.OperationTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

// terminal_at_ms records when the operation FIRST became terminal and is
// never cleared; corrected_at_ms is set when a late fact moves it out of
// `unknown` (spec section 6.4).
func TestTerminalAtIsNeverRewrittenAndCorrectedAtIsSet(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")

	op := newOperation(acct, "auth_1", "send_text", "key-1")
	op.ID = store.OperationID()
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	// Crash recovery makes it terminal for the first time.
	recovered, err := st.RecoverRunningOperations(ctx)
	if err != nil {
		t.Fatalf("recovering: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Status != store.OpUnknown ||
		recovered[0].ErrorCode != store.ErrCrashRecovered {
		t.Fatalf("crash recovery gave %+v", recovered)
	}
	firstTerminal := recovered[0].TerminalAtMS
	if firstTerminal == 0 {
		t.Fatal("terminal_at_ms was not set when the operation first became terminal")
	}
	if recovered[0].CorrectedAtMS != 0 {
		t.Error("corrected_at_ms was set before any correction")
	}

	// A late echo corrects it. terminal_at stays; corrected_at appears.
	clk.Advance(2 * time.Hour)
	corrected, err := st.SettleOperation(ctx, op.ID, store.Settlement{
		Status: store.OpSucceeded, MessageID: "msg_late",
	})
	if err != nil {
		t.Fatalf("correcting: %v", err)
	}
	if corrected.Status != store.OpSucceeded {
		t.Errorf("status = %s, want succeeded", corrected.Status)
	}
	if corrected.TerminalAtMS != firstTerminal {
		t.Errorf("terminal_at_ms was rewritten: %d -> %d", firstTerminal, corrected.TerminalAtMS)
	}
	if corrected.CorrectedAtMS == 0 {
		t.Error("corrected_at_ms was not set by the correction out of unknown")
	}
	if corrected.MessageID != "msg_late" {
		t.Errorf("message_id = %q", corrected.MessageID)
	}

	// And nothing leaves succeeded.
	if _, err := st.SettleOperation(ctx, op.ID, store.Settlement{Status: store.OpRunning}); !errors.Is(err, store.ErrOperationTransition) {
		t.Errorf("succeeded -> running: err = %v, want ErrOperationTransition", err)
	}
	after, err := st.Operation(ctx, op.ID)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if after.Status != store.OpSucceeded {
		t.Errorf("a refused transition changed the stored status to %s", after.Status)
	}
}

// ErrPhoneNotResponding is `pending`, not `failed`, and it is not terminal:
// that is how a caller tells "not yet" from "no" (spec section 6.4).
func TestPendingIsNotTerminalAndTheReaperSettlesIt(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")

	op := newOperation(acct, "auth_1", "send_text", "key-pending")
	op.ID = store.OperationID()
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	pending, err := st.SettleOperation(ctx, op.ID, store.Settlement{
		Status: store.OpPending, ErrorCode: "phone_not_responding", ErrorRetryable: true,
	})
	if err != nil {
		t.Fatalf("settling to pending: %v", err)
	}
	if pending.Terminal {
		t.Error("a pending operation is terminal; it must not be")
	}
	if pending.TerminalAtMS != 0 {
		t.Error("terminal_at_ms was set on a non-terminal operation")
	}
	if !pending.ErrorRetryable {
		t.Error("phone_not_responding is retryable")
	}

	// Before the timeout the reaper leaves it alone.
	clk.Advance(23 * time.Hour)
	reaped, err := st.ReapPendingOperations(ctx, int64(24*time.Hour/time.Millisecond))
	if err != nil {
		t.Fatalf("reaping early: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("the reaper settled %d operations before the timeout", len(reaped))
	}

	// After it, the operation becomes unknown -- on the injected clock, never
	// waited out.
	clk.Advance(2 * time.Hour)
	reaped, err = st.ReapPendingOperations(ctx, int64(24*time.Hour/time.Millisecond))
	if err != nil {
		t.Fatalf("reaping: %v", err)
	}
	if len(reaped) != 1 || reaped[0].Status != store.OpUnknown ||
		reaped[0].ErrorCode != store.ErrPendingTimeout || !reaped[0].Terminal {
		t.Fatalf("the reaper gave %+v", reaped)
	}
}

// Crash recovery is process-wide, not per account: a crash is a property of
// the process (spec section 6.6). It returns the rows it settled so the
// caller can audit each one with its account_id.
func TestCrashRecoveryIsProcessWideAndReturnsWhatItSettled(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	for i, acct := range []string{one, two} {
		op := newOperation(acct, "auth_1", "send_text", "key-crash-"+string(rune('a'+i)))
		op.ID = store.OperationID()
		if err := st.InsertOperation(ctx, op); err != nil {
			t.Fatalf("inserting: %v", err)
		}
	}
	// A pending operation is left alone at startup.
	left := newOperation(one, "auth_1", "send_text", "key-left")
	left.ID = store.OperationID()
	if err := st.InsertOperation(ctx, left); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if _, err := st.SettleOperation(ctx, left.ID, store.Settlement{
		Status: store.OpPending, ErrorCode: "phone_not_responding", ErrorRetryable: true,
	}); err != nil {
		t.Fatalf("settling: %v", err)
	}

	recovered, err := st.RecoverRunningOperations(ctx)
	if err != nil {
		t.Fatalf("recovering: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered %d operations, want both accounts' one each", len(recovered))
	}
	accounts := map[string]bool{}
	for _, o := range recovered {
		accounts[o.AccountID] = true
		if o.Status != store.OpUnknown || o.ErrorCode != store.ErrCrashRecovered || !o.Terminal {
			t.Errorf("recovered row is %+v", o)
		}
	}
	if !accounts[one] || !accounts[two] {
		t.Errorf("crash recovery did not cover both accounts: %v", accounts)
	}

	still, err := st.Operation(ctx, left.ID)
	if err != nil {
		t.Fatalf("reading the pending operation: %v", err)
	}
	if still.Status != store.OpPending {
		t.Errorf("crash recovery touched a pending operation: %s", still.Status)
	}

	// A second sweep finds nothing: recovery is idempotent.
	again, err := st.RecoverRunningOperations(ctx)
	if err != nil {
		t.Fatalf("recovering again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a second crash sweep settled %d more rows", len(again))
	}
}

// Spec section 16 test 35, and the mirror hazard of section 6.3.
func TestIdempotencyIsPerAccountAndTheMirrorHazardIsDetectable(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")
	const key = "same-key"

	first := newOperation(one, "auth_1", "send_text", key)
	first.ID = store.OperationID()
	if err := st.InsertOperation(ctx, first); err != nil {
		t.Fatalf("inserting the first: %v", err)
	}

	// Before writing the second, the store can say the key was already used
	// by this authorization for this kind against a DIFFERENT account, and
	// name it. That is what both agent-facing surfaces refuse on.
	named, used, err := st.KeyUsedByAnotherAccount(ctx, "auth_1", "send_text", key, two)
	if err != nil {
		t.Fatalf("checking the mirror hazard: %v", err)
	}
	if !used {
		t.Fatal("the key's earlier use against another account was not detected")
	}
	if named != one {
		t.Errorf("the hazard named %q, want %q", named, one)
	}

	// Against the SAME account it is a replay, not a hazard.
	if _, used, err := st.KeyUsedByAnotherAccount(ctx, "auth_1", "send_text", key, one); err != nil || used {
		t.Errorf("a same-account replay was reported as a cross-account hazard (used=%v, err=%v)", used, err)
	}
	// A different kind is a different tuple.
	if _, used, err := st.KeyUsedByAnotherAccount(ctx, "auth_1", "mark_read", key, two); err != nil || used {
		t.Errorf("a different kind tripped the hazard (used=%v, err=%v)", used, err)
	}

	// The store itself does not adopt across accounts: the same key against
	// the second account is a second row.
	second := newOperation(two, "auth_1", "send_text", key)
	second.ID = store.OperationID()
	if err := st.InsertOperation(ctx, second); err != nil {
		t.Fatalf("inserting the second: %v", err)
	}
	a, err := st.OperationByKey(ctx, "auth_1", one, "send_text", key)
	if err != nil {
		t.Fatalf("looking up the first: %v", err)
	}
	b, err := st.OperationByKey(ctx, "auth_1", two, "send_text", key)
	if err != nil {
		t.Fatalf("looking up the second: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("one key against two accounts produced one operation; it must be two")
	}
	if a.AccountID != one || b.AccountID != two {
		t.Errorf("the rows landed under the wrong accounts: %s, %s", a.AccountID, b.AccountID)
	}

	// A key this authorization has never used is not found.
	if _, err := st.OperationByKey(ctx, "auth_1", one, "send_text", "unused"); !errors.Is(err, store.ErrOperationNotFound) {
		t.Errorf("an unused key gave %v, want ErrOperationNotFound", err)
	}
}

func TestOperationCorrelatesByTmpIDWithinTheAccount(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	opOne := newOperation(one, "auth_1", "send_text", "k1")
	opOne.ID = store.OperationID()
	opTwo := newOperation(two, "auth_1", "send_text", "k2")
	opTwo.ID = store.OperationID()
	for _, o := range []store.Operation{opOne, opTwo} {
		if err := st.InsertOperation(ctx, o); err != nil {
			t.Fatalf("inserting: %v", err)
		}
	}
	// Both accounts happen to mint the same bare UUID; correlation is still
	// unambiguous because it is scoped to the account.
	const tmp = "3f1c0a4e-0000-4000-8000-000000000000"
	for _, id := range []string{opOne.ID, opTwo.ID} {
		if err := st.SetOperationTmpID(ctx, id, tmp); err != nil {
			t.Fatalf("setting tmp_id: %v", err)
		}
	}
	got, err := st.OperationByTmpID(ctx, two, tmp)
	if err != nil {
		t.Fatalf("correlating: %v", err)
	}
	if got.ID != opTwo.ID {
		t.Errorf("correlated to %s, want %s", got.ID, opTwo.ID)
	}
}

func TestListOperationsIsNewestFirstAndFiltered(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")

	var ids []string
	for i := 0; i < 3; i++ {
		o := newOperation(acct, "auth_1", "send_text", "k"+string(rune('a'+i)))
		o.ID = store.OperationID()
		if err := st.InsertOperation(ctx, o); err != nil {
			t.Fatalf("inserting: %v", err)
		}
		ids = append(ids, o.ID)
		clk.Advance(time.Second)
	}
	other := newOperation(acct, "auth_2", "mark_read", "kz")
	other.ID = store.OperationID()
	if err := st.InsertOperation(ctx, other); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	got, err := st.ListOperations(ctx, store.OperationQuery{AuthorizationID: "auth_1", AllAccounts: true})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d operations, want the caller's 3 only", len(got))
	}
	for i := range got {
		if got[i].ID != ids[len(ids)-1-i] {
			t.Errorf("position %d is %s, want %s (newest first)", i, got[i].ID, ids[len(ids)-1-i])
		}
	}

	byKind, err := st.ListOperations(ctx, store.OperationQuery{
		AuthorizationID: "auth_2", AccountID: acct, Kind: "mark_read"})
	if err != nil {
		t.Fatalf("listing by kind: %v", err)
	}
	if len(byKind) != 1 || byKind[0].ID != other.ID {
		t.Errorf("kind filter gave %d rows", len(byKind))
	}

	if _, err := st.ListOperations(ctx, store.OperationQuery{AuthorizationID: "auth_1"}); !errors.Is(err, store.ErrNoAccountPredicate) {
		t.Errorf("an unstated account scope was accepted: %v", err)
	}
}

// The sweep never deletes a row that is not terminal (spec section 6.6).
func TestIdempotencySweepSparesNonTerminalRows(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	acct := seedAccount(t, st, "owner@example.com")

	running := newOperation(acct, "auth_1", "send_text", "still-running")
	running.ID = store.OperationID()
	settled := newOperation(acct, "auth_1", "send_text", "settled")
	settled.ID = store.OperationID()
	for _, o := range []store.Operation{running, settled} {
		if err := st.InsertOperation(ctx, o); err != nil {
			t.Fatalf("inserting: %v", err)
		}
	}
	if _, err := st.SettleOperation(ctx, settled.ID, store.Settlement{Status: store.OpSucceeded}); err != nil {
		t.Fatalf("settling: %v", err)
	}

	clk.Advance(31 * 24 * time.Hour)
	n, err := st.SweepIdempotencyKeys(ctx, int64(30*24*time.Hour/time.Millisecond))
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep deleted %d rows, want the one terminal row", n)
	}
	if _, err := st.Operation(ctx, running.ID); err != nil {
		t.Errorf("the sweep deleted a non-terminal row: %v", err)
	}
	if _, err := st.Operation(ctx, settled.ID); !errors.Is(err, store.ErrOperationNotFound) {
		t.Errorf("the terminal row survived the sweep: %v", err)
	}
}
