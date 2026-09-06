package core_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func sendInput(h *harness, key, text string) core.SendTextInput {
	return core.SendTextInput{
		Request: core.Request{
			AuthorizationID: "authz-1",
			Key:             key,
			Body:            []byte(`{"text":"` + text + `"}`),
			ConversationID:  store.ConversationID(h.Account.ID, convA),
			Source:          "test",
		},
		Text: text,
	}
}

// TestSlice2Test7IdempotencyIsPerKeyAndPerBody is section 16 Slice 2
// acceptance test 7: the same client_request_id with the same body returns
// the same operation and calls the backend ONCE, asserted on the fake's call
// counter; with a different body it is idempotency_conflict and calls it
// ZERO times.
//
// The third clause of test 7 -- a key supplied as a query parameter is
// invalid_request -- is a route-parsing rule and lives in internal/api, which
// is not this package's to write. What core owns and asserts here is that the
// key is required at all: a mutation without one writes nothing.
func TestSlice2Test7IdempotencyIsPerKeyAndPerBody(t *testing.T) {
	h := newHarness(t)
	h.seedConversation(convA, false)

	first, err := h.Account.SendText(h.ctx(), sendInput(h, "key-1", "hello"))
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if first.Operation.Status != store.OpSucceeded {
		t.Fatalf("status = %q, want succeeded", first.Operation.Status)
	}
	if h.Backend.CallCount("SendText") != 1 {
		t.Fatalf("SendText called %d times, want 1", h.Backend.CallCount("SendText"))
	}

	// Same key, same body, keys in a different order: still a replay.
	replayInput := sendInput(h, "key-1", "hello")
	replayInput.Body = []byte(`{  "text" : "hello" }`)
	replay, err := h.Account.SendText(h.ctx(), replayInput)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("the replay was not recognised as one")
	}
	if replay.Operation.ID != first.Operation.ID {
		t.Fatalf("replay returned operation %s, want %s",
			replay.Operation.ID, first.Operation.ID)
	}
	if h.Backend.CallCount("SendText") != 1 {
		t.Fatalf("the replay sent a second message: SendText called %d times",
			h.Backend.CallCount("SendText"))
	}

	// A replay returns the existing operation AND its message_id, once the
	// echo has landed.
	h.drain()
	settled, err := h.Store.Operation(h.ctx(), first.Operation.ID)
	if err != nil {
		t.Fatalf("reading operation: %v", err)
	}
	if settled.MessageID == "" {
		t.Fatal("the echo did not write message_id onto the operation")
	}
	again, err := h.Account.SendText(h.ctx(), replayInput)
	if err != nil {
		t.Fatalf("second replay: %v", err)
	}
	if again.Operation.MessageID != settled.MessageID {
		t.Fatalf("a replay served message_id %q, want %q",
			again.Operation.MessageID, settled.MessageID)
	}

	// Same key, a DIFFERENT body: idempotency_conflict, and zero calls.
	before := h.Backend.CallCount("SendText")
	_, err = h.Account.SendText(h.ctx(), sendInput(h, "key-1", "something else"))
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.CodeIdempotencyConflict {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
	if h.Backend.CallCount("SendText") != before {
		t.Fatalf("a conflicting body called the backend %d extra times",
			h.Backend.CallCount("SendText")-before)
	}

	// No key at all is invalid_request naming client_request_id, and writes
	// nothing.
	_, err = h.Account.SendText(h.ctx(), sendInput(h, "", "hello"))
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.CodeInvalidRequest {
		t.Fatalf("err = %v, want invalid_request", err)
	}
	if apiErr.Details["field"] != "client_request_id" {
		t.Fatalf("details.field = %v, want client_request_id", apiErr.Details["field"])
	}
	if h.Backend.CallCount("SendText") != before {
		t.Fatal("a keyless mutation reached the backend")
	}
}

// TestSlice2Test35IdempotencyIsPerAccount is section 16 Slice 2 acceptance
// test 35: the same client_request_id sent to two accounts creates two
// operations and calls each backend once.
//
// TWO FAKES, because one fake is one account.
func TestSlice2Test35IdempotencyIsPerAccount(t *testing.T) {
	a := newHarness(t)
	b := attachAccount(t, a, "owner-b@example.test")
	a.seedConversation(convA, false)
	b.seedConversation(convA, false)

	opA, err := a.Account.SendText(a.ctx(), sendInput(a, "shared-key", "hello"))
	if err != nil {
		t.Fatalf("account A send: %v", err)
	}
	// Account B's send under the SAME key would be a second real message to a
	// second real person, so the mirror hazard of section 6.3 refuses it by
	// name -- and refusing it is the whole point of the rule. A genuinely new
	// send to another account uses a new key.
	_, err = b.Account.SendText(b.ctx(), sendInput(b, "shared-key", "hello"))
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Code != apierr.CodeInvalidRequest {
		t.Fatalf("err = %v, want invalid_request for the mirror hazard", err)
	}
	if apiErr.Details["field"] != "client_request_id" {
		t.Fatalf("details.field = %v, want client_request_id", apiErr.Details["field"])
	}
	if apiErr.Details["account_id"] != a.Account.ID {
		t.Fatalf("the refusal names account %v, want the first-use account %s",
			apiErr.Details["account_id"], a.Account.ID)
	}
	if b.Backend.CallCount("SendText") != 0 {
		t.Fatal("the refused cross-account replay reached account B's backend")
	}

	// Under a DIFFERENT authorization the tuple differs, so the same key
	// value is a different operation -- two clients may use the same key.
	inB := sendInput(b, "shared-key", "hello")
	inB.AuthorizationID = "authz-2"
	opB, err := b.Account.SendText(b.ctx(), inB)
	if err != nil {
		t.Fatalf("account B send: %v", err)
	}

	if opA.Operation.ID == opB.Operation.ID {
		t.Fatal("the two accounts share one operation row")
	}
	if opA.Operation.AccountID != a.Account.ID || opB.Operation.AccountID != b.Account.ID {
		t.Fatalf("operations landed under the wrong accounts: %s, %s",
			opA.Operation.AccountID, opB.Operation.AccountID)
	}
	if a.Backend.CallCount("SendText") != 1 {
		t.Fatalf("account A's backend called %d times, want 1", a.Backend.CallCount("SendText"))
	}
	if b.Backend.CallCount("SendText") != 1 {
		t.Fatalf("account B's backend called %d times, want 1", b.Backend.CallCount("SendText"))
	}
}

// TestSlice2Test8CrashBetweenCommitAndCall is section 16 Slice 2 acceptance
// test 8: killing the process between the operation commit and the backend
// call, then restarting, leaves the operation `unknown` with
// `crash_recovered`; feeding the echo afterwards corrects it to `succeeded`
// with a message_id and sets corrected_at. NOTHING IS RESENT.
//
// PLANT: move the `call(ctx, op)` in Account.runOperation above the
// InsertOperation/COMMIT -- the "library before the commit" mutation -- and
// this test fails at "crash recovery settled 0 operations".
func TestSlice2Test8CrashBetweenCommitAndCall(t *testing.T) {
	dir := t.TempDir()
	h := newHarnessIn(t, dir, "owner-a@example.test", nil)
	h.seedConversation(convA, false)

	// The process dies after step 8's commit and before step 9's call.
	crash := errors.New("process died between the commit and the call")
	var crashed store.Operation
	h.Account.SetCrashBefore(func(op store.Operation) error {
		crashed = op
		return crash
	})
	_, err := h.Account.SendText(h.ctx(), sendInput(h, "key-crash", "hello"))
	if !errors.Is(err, crash) {
		t.Fatalf("err = %v, want the injected crash", err)
	}
	if h.Backend.CallCount("SendText") != 0 {
		t.Fatal("the backend was called before the operation row was committed")
	}
	if crashed.ID == "" || crashed.TmpID == "" {
		t.Fatal("the operation was not committed with its tmp_id before the call")
	}

	// Restart: a new store over the same directory, and the crash sweep runs
	// before any listener binds.
	if err := h.Store.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}
	restarted := newHarnessIn(t, dir, "owner-a@example.test", nil)
	recovered, err := core.RecoverOperations(restarted.ctx(), restarted.Store,
		restarted.Account.Audit, "system")
	if err != nil {
		t.Fatalf("crash recovery: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("crash recovery settled %d operations, want 1", len(recovered))
	}
	op, err := restarted.Store.Operation(restarted.ctx(), crashed.ID)
	if err != nil {
		t.Fatalf("reading the recovered operation: %v", err)
	}
	if op.Status != store.OpUnknown {
		t.Fatalf("status = %q, want unknown", op.Status)
	}
	if op.ErrorCode != store.ErrCrashRecovered {
		t.Fatalf("error_code = %q, want crash_recovered", op.ErrorCode)
	}
	if !op.Terminal || op.TerminalAtMS == 0 {
		t.Fatal("the recovered operation is not terminal")
	}
	if !hasAudit(restarted.Audit.Rows(), "operation.crash_recovered") {
		t.Fatalf("no operation.crash_recovered audit row; kinds = %v", restarted.auditKinds())
	}
	if restarted.Backend.CallCount("SendText") != 0 {
		t.Fatal("crash recovery resent the message")
	}

	// The send DID reach Google after all, and the echo arrives on reconnect.
	restarted.seedConversation(convA, false)
	restarted.Clock.Advance(time.Minute)
	echo := message(convA, 1, restarted.Clock.Now())
	echo.TmpID = crashed.TmpID
	echo.StatusRaw = 1
	echo.DeliveryState = gm.DeliveryStateSent
	if err := restarted.Account.HandleEvent(restarted.ctx(), &gm.EventMessage{Message: echo}); err != nil {
		t.Fatalf("delivering the echo: %v", err)
	}

	corrected, err := restarted.Store.Operation(restarted.ctx(), crashed.ID)
	if err != nil {
		t.Fatalf("reading the corrected operation: %v", err)
	}
	if corrected.Status != store.OpSucceeded {
		t.Fatalf("status = %q, want succeeded after the echo", corrected.Status)
	}
	if corrected.MessageID == "" {
		t.Fatal("the correction carries no message_id")
	}
	if corrected.CorrectedAtMS == 0 {
		t.Fatal("corrected_at was not set")
	}
	if corrected.TerminalAtMS != op.TerminalAtMS {
		t.Fatal("terminal_at was rewritten; it records when the operation FIRST became terminal")
	}
	if restarted.Backend.CallCount("SendText") != 0 {
		t.Fatal("something was resent")
	}
}

// TestSlice2Test9PhoneNotRespondingIsPendingNotFailed is section 16 Slice 2
// acceptance test 9: ErrPhoneNotResponding yields an operation in `pending`
// with terminal:false, not `failed`; the echo settles it to `succeeded`, or
// to `failed` if the echo reports a failed status; with no echo it becomes
// `unknown` after operations.pending_timeout, advanced on the injected clock.
//
// PLANT: map KeepsOperationPending to store.OpFailed in Account.settle --
// the "phone_not_responding produces failed" mutation -- and this test fails
// at "status = failed, want pending".
func TestSlice2Test9PhoneNotRespondingIsPendingNotFailed(t *testing.T) {
	pendingSend := func(t *testing.T, h *harness, key string) store.Operation {
		t.Helper()
		h.seedConversation(convA, false)
		h.Backend.ScriptErrors(gm.ErrPhoneNotResponding)
		res, err := h.Account.SendText(h.ctx(), sendInput(h, key, "hello"))
		var apiErr *gm.Error
		if !errors.As(err, &apiErr) || apiErr.Code != gm.CodePhoneNotResponding {
			t.Fatalf("err = %v, want phone_not_responding", err)
		}
		op := res.Operation
		if op.Status != store.OpPending {
			t.Fatalf("status = %q, want pending: the server accepted the request "+
				"and the phone may still deliver it", op.Status)
		}
		if op.Terminal {
			t.Fatal("a pending operation is not terminal")
		}
		if op.ErrorCode != string(gm.CodePhoneNotResponding) || !op.ErrorRetryable {
			t.Fatalf("error = %q retryable=%v, want phone_not_responding retryable",
				op.ErrorCode, op.ErrorRetryable)
		}
		return op
	}

	t.Run("the_echo_settles_it_to_succeeded", func(t *testing.T) {
		h := newHarness(t)
		op := pendingSend(t, h, "key-pnr")
		h.Clock.Advance(time.Minute)
		echo := message(convA, 1, h.Clock.Now())
		echo.TmpID = op.TmpID
		echo.StatusRaw = 1
		echo.DeliveryState = gm.DeliveryStateSent
		if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: echo}); err != nil {
			t.Fatalf("echo: %v", err)
		}
		settled, _ := h.Store.Operation(h.ctx(), op.ID)
		if settled.Status != store.OpSucceeded {
			t.Fatalf("status = %q, want succeeded", settled.Status)
		}
		if settled.MessageID == "" {
			t.Fatal("the echo did not write message_id")
		}
	})

	t.Run("a_failed_echo_settles_it_to_failed", func(t *testing.T) {
		h := newHarness(t)
		op := pendingSend(t, h, "key-pnr-fail")
		h.Clock.Advance(time.Minute)
		echo := message(convA, 2, h.Clock.Now())
		echo.TmpID = op.TmpID
		echo.StatusRaw = 8 // OUTGOING_FAILED_GENERIC
		echo.DeliveryState = gm.DeliveryStateFailed
		if err := h.Account.HandleEvent(h.ctx(), &gm.EventMessage{Message: echo}); err != nil {
			t.Fatalf("echo: %v", err)
		}
		settled, _ := h.Store.Operation(h.ctx(), op.ID)
		if settled.Status != store.OpFailed {
			t.Fatalf("status = %q, want failed", settled.Status)
		}
	})

	t.Run("with_no_echo_it_becomes_unknown_at_pending_timeout", func(t *testing.T) {
		h := newHarness(t)
		op := pendingSend(t, h, "key-pnr-timeout")
		timeout := h.Account.Config.PendingTimeout

		// Not yet.
		h.Clock.Advance(timeout - time.Minute)
		reaped, err := core.ReapPendingOperations(h.ctx(), h.Store, h.Account.Audit, timeout, "system")
		if err != nil {
			t.Fatalf("reaper: %v", err)
		}
		if len(reaped) != 0 {
			t.Fatalf("the reaper settled %d operations before the timeout", len(reaped))
		}

		// Now -- advanced on the injected clock, not waited out.
		h.Clock.Advance(2 * time.Minute)
		reaped, err = core.ReapPendingOperations(h.ctx(), h.Store, h.Account.Audit, timeout, "system")
		if err != nil {
			t.Fatalf("reaper: %v", err)
		}
		if len(reaped) != 1 {
			t.Fatalf("the reaper settled %d operations, want 1", len(reaped))
		}
		settled, _ := h.Store.Operation(h.ctx(), op.ID)
		if settled.Status != store.OpUnknown {
			t.Fatalf("status = %q, want unknown", settled.Status)
		}
		if !settled.Terminal {
			t.Fatal("unknown is terminal")
		}
	})
}

// TestSlice2Test10SendRetriesThroughTheOperationPipeline is the
// pipeline-level half of section 16 Slice 2 acceptance test 10; send_test.go
// already covers SendWithRetry at the unit level. What is added here is that
// the retries happen INSIDE one operation row, reusing ONE tmp_id, and that
// FAILURE_4 is not_default_sms_app and is not retried.
func TestSlice2Test10SendRetriesThroughTheOperationPipeline(t *testing.T) {
	t.Run("failure_2_twice_then_success_reuses_one_tmp_id", func(t *testing.T) {
		h := newHarness(t)
		h.seedConversation(convA, false)
		h.Backend.ScriptSendStatuses(gm.SendStatusFailure2, gm.SendStatusFailure2, gm.SendStatusSuccess)

		res, err := h.Account.SendText(h.ctx(), sendInput(h, "key-retry", "hello"))
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if res.Operation.Status != store.OpSucceeded {
			t.Fatalf("status = %q, want succeeded", res.Operation.Status)
		}
		if h.Backend.CallCount("SendText") != 3 {
			t.Fatalf("SendText called %d times, want 3", h.Backend.CallCount("SendText"))
		}
		want := []time.Duration{3 * time.Second, 8 * time.Second}
		got := h.Clock.Sleeps()
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("backoffs = %v, want %v", got, want)
		}
		// One tmp_id across the retries: a fresh one per attempt would let one
		// request correlate to two operations.
		final, _ := h.Store.Operation(h.ctx(), res.Operation.ID)
		if final.TmpID != res.Operation.TmpID || final.TmpID == "" {
			t.Fatalf("tmp_id changed across the retries: %q then %q",
				res.Operation.TmpID, final.TmpID)
		}
		// And the echo, which carries that one tmp_id back, correlates.
		h.drain()
		echoed, _ := h.Store.Operation(h.ctx(), res.Operation.ID)
		if echoed.MessageID == "" {
			t.Fatal("the echo did not correlate to the operation")
		}
	})

	t.Run("failure_4_is_not_default_sms_app_and_is_not_retried", func(t *testing.T) {
		h := newHarness(t)
		h.seedConversation(convA, false)
		h.Backend.ScriptSendStatuses(gm.SendStatusFailure4)

		res, err := h.Account.SendText(h.ctx(), sendInput(h, "key-f4", "hello"))
		var gmErr *gm.Error
		if !errors.As(err, &gmErr) || gmErr.Code != gm.CodeNotDefaultSMSApp {
			t.Fatalf("err = %v, want not_default_sms_app", err)
		}
		if h.Backend.CallCount("SendText") != 1 {
			t.Fatalf("SendText called %d times, want 1: FAILURE_4 is not retried",
				h.Backend.CallCount("SendText"))
		}
		if len(h.Clock.Sleeps()) != 0 {
			t.Fatalf("FAILURE_4 backed off %v", h.Clock.Sleeps())
		}
		if res.Operation.Status != store.OpFailed {
			t.Fatalf("status = %q, want failed", res.Operation.Status)
		}
	})
}

// TestSlice2Test11ResolveThroughTheOperationPipeline is the pipeline-level
// half of section 16 Slice 2 acceptance test 11; send_test.go covers
// ClassifyResolve at the unit level. What is added here is that each outcome
// lands on an operation row with the right status.
func TestSlice2Test11ResolveThroughTheOperationPipeline(t *testing.T) {
	start := func(h *harness, key string) (core.Result, error) {
		return h.Account.StartConversation(h.ctx(), core.StartConversationInput{
			Request: core.Request{
				AuthorizationID: "authz-1",
				Key:             key,
				Body:            []byte(`{"to":["` + fictionalA + `"]}`),
				Source:          "test",
			},
			Numbers: []string{fictionalA},
		})
	}

	t.Run("create_rcs_retries_once_and_succeeds", func(t *testing.T) {
		h := newHarness(t)
		h.Backend.ScriptResolveStatuses(gm.ResolveStatusCreateRCS, gm.ResolveStatusSuccess)
		res, err := start(h, "key-rcs")
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if res.Operation.Status != store.OpSucceeded {
			t.Fatalf("status = %q, want succeeded", res.Operation.Status)
		}
		if res.Operation.ConversationID == "" {
			t.Fatal("the operation carries no conversation_id")
		}
	})

	t.Run("a_second_create_rcs_is_google_error", func(t *testing.T) {
		h := newHarness(t)
		h.Backend.ScriptResolveStatuses(gm.ResolveStatusCreateRCS, gm.ResolveStatusCreateRCS)
		res, err := start(h, "key-rcs-2")
		var gmErr *gm.Error
		if !errors.As(err, &gmErr) || gmErr.Code != gm.CodeGoogleError {
			t.Fatalf("err = %v, want google_error", err)
		}
		if res.Operation.Status != store.OpFailed {
			t.Fatalf("status = %q, want failed", res.Operation.Status)
		}
	})

	t.Run("an_unnamed_integer_is_google_undocumented_status", func(t *testing.T) {
		h := newHarness(t)
		h.Backend.ScriptResolveStatuses(gm.ResolveStatus(7))
		_, err := start(h, "key-rcs-7")
		var gmErr *gm.Error
		if !errors.As(err, &gmErr) || gmErr.Code != gm.CodeGoogleUndocumentedState {
			t.Fatalf("err = %v, want google_undocumented_status", err)
		}
		// The bare number, with no invented name for it.
		if got := fmt.Sprint(gmErr.Details["status"]); got != "7" {
			t.Fatalf("details.status = %#v, want the bare integer 7 with no invented name",
				gmErr.Details["status"])
		}
	})

	t.Run("a_config_version_mismatch_is_config_version_stale", func(t *testing.T) {
		h := newHarness(t)
		// The detection rule is the version diff ALONE. No particular status
		// number is scripted, because asserting on one would re-import the
		// unsourced claim section 18.1 withdrew.
		h.Backend.SetLiveConfigVersion(gm.ConfigVersion{Year: 2027, Month: 1, Day: 4, V1: 1, V2: 1})
		h.Backend.ScriptResolveStatuses(gm.ResolveStatus(4))
		_, err := start(h, "key-stale")
		var gmErr *gm.Error
		if !errors.As(err, &gmErr) || gmErr.Code != gm.CodeConfigVersionStale {
			t.Fatalf("err = %v, want config_version_stale", err)
		}
	})
}

// TestSlice2TwoAccountsDoNotCrossTalk is section 13.2's two-account
// paragraph, in the ordinary suite because it needs no gate: ingest,
// ordering, dedup and the reconciliation sweep with two fakes interleaved,
// asserting no cross-talk.
//
//   - every row lands under the right account_id;
//   - one account's sweep does not touch the other's rows;
//   - one account's dedup loss does not trigger the other's sweep.
func TestSlice2TwoAccountsDoNotCrossTalk(t *testing.T) {
	a := newHarness(t)
	b := attachAccount(t, a, "owner-b@example.test")

	// The same conversation SOURCE ID on both phones: two accounts in a group
	// with the same person hold two separate threads with two separate conv_
	// IDs, which is correct -- they are two threads on two phones.
	a.seedConversation(convA, false)
	b.seedConversation(convA, false)

	base := a.Clock.Now()
	for i := 1; i <= 4; i++ {
		ma := message(convA, i, base.Add(time.Duration(i)*time.Minute))
		ma.Text = "from A"
		mb := message(convA, i, base.Add(time.Duration(i)*time.Minute))
		mb.Text = "from B"
		if err := a.Account.HandleEvent(a.ctx(), &gm.EventMessage{Message: ma}); err != nil {
			t.Fatalf("A ingest: %v", err)
		}
		if err := b.Account.HandleEvent(b.ctx(), &gm.EventMessage{Message: mb}); err != nil {
			t.Fatalf("B ingest: %v", err)
		}
	}

	convAID := store.ConversationID(a.Account.ID, convA)
	convBID := store.ConversationID(b.Account.ID, convA)
	if convAID == convBID {
		t.Fatal("the two accounts derived one conv_ ID for two threads")
	}
	assertAllUnder(t, a, convAID, "from A")
	assertAllUnder(t, b, convBID, "from B")

	// Account A loses a batch to the library's dedup. Account B loses
	// nothing.
	lost := loseHalfABatchOn(t, a, 10)
	beforeBSweeps := b.Account.Sweeps()
	beforeBSweepAt := b.accountRow().LastSweepAtMS
	beforeBRows := b.countMessages(convBID)

	a.Backend.SetSessionID("a-new-session-for-A")
	if err := a.Account.HandleEvent(a.ctx(),
		&gm.EventUserAlert{Alert: gm.AlertBrowserActive}); err != nil {
		t.Fatalf("A browser active: %v", err)
	}

	for _, id := range lost {
		if _, err := a.Store.Message(a.ctx(), id); err != nil {
			t.Fatalf("A's sweep did not recover %s: %v", id, err)
		}
	}
	if b.Account.Sweeps() != beforeBSweeps {
		t.Fatalf("account A's dedup loss triggered account B's sweep (%d -> %d)",
			beforeBSweeps, b.Account.Sweeps())
	}
	if b.accountRow().LastSweepAtMS != beforeBSweepAt {
		t.Fatal("account A's sweep wrote account B's last_sweep_at_ms")
	}
	if b.countMessages(convBID) != beforeBRows {
		t.Fatalf("account A's sweep changed account B's row count (%d -> %d)",
			beforeBRows, b.countMessages(convBID))
	}
	assertAllUnder(t, b, convBID, "from B")

	// And B's own backends were never called by A's sweep.
	if b.Backend.CallCount("ListMessages") != 0 {
		t.Fatalf("account A's sweep called account B's backend %d times",
			b.Backend.CallCount("ListMessages"))
	}
}

func assertAllUnder(t *testing.T, h *harness, convID, wantText string) {
	t.Helper()
	rows := h.messages(convID)
	if len(rows) == 0 {
		t.Fatalf("%s holds no rows", convID)
	}
	for _, r := range rows {
		if r.AccountID != h.Account.ID {
			t.Fatalf("%s is under account %s, want %s", r.ID, r.AccountID, h.Account.ID)
		}
		if r.ConversationID != convID {
			t.Fatalf("%s is under conversation %s, want %s", r.ID, r.ConversationID, convID)
		}
		if wantText != "" && r.Text != wantText {
			t.Fatalf("%s carries %q, want %q -- the two accounts' rows are crossed",
				r.ID, r.Text, wantText)
		}
	}
}

// loseHalfABatchOn is loseHalfABatch for an account that already holds
// history, numbering its lost messages from `from`.
func loseHalfABatchOn(t *testing.T, h *harness, from int) []string {
	t.Helper()
	base := h.Clock.Now()
	conv := h.conversation(convA)
	var batch []gm.Event
	var lost []string
	for i := from; i < from+3; i++ {
		m := message(convA, i, base.Add(time.Duration(i)*time.Minute))
		m.Text = "from A"
		h.Backend.SeedMessage(m)
		batch = append(batch, &gm.EventMessage{Message: m})
		lost = append(lost, store.MessageID(h.Account.ID, convA, m.SourceID))
	}
	h.Backend.EmitBatch(0, batch...)
	h.drain()

	remote := h.seedConversation(convA, false)
	remote.LastActivity = base.Add(time.Duration(from+3) * time.Minute)
	remote.LatestMessageID = lastSourceID(from + 2)
	h.Backend.SeedConversation(remote)
	_ = conv
	h.Clock.Advance(10 * time.Minute)
	return lost
}

func lastSourceID(i int) string { return message("", i, time.Time{}).SourceID }
