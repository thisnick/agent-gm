package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
)

// Section 16 Slice 2 test 10, the retry half: FAILURE_2 twice then SUCCESS
// succeeds after backoffs of 3s and 8s **on the injected clock**, reusing one
// tmp_id across the retries. Nothing here waits eleven seconds.
//
// Plant: change gm.SendRetryBackoff, or drop the clk.Sleep call, and this
// test fails at "backoffs = ...". Planted 2026-09-06.
func TestTransientSendIsRetriedOnTheBackoffSchedule(t *testing.T) {
	clk := clock.NewFake()
	statuses := []gm.SendStatus{gm.SendStatusFailure2, gm.SendStatusFailure2, gm.SendStatusSuccess}
	var seenTmpIDs []string
	const tmpID = "0f9b4c1e-0000-4000-8000-000000000001"

	calls := 0
	res, err := core.SendWithRetry(context.Background(), clk, func(context.Context) (gm.SendResult, error) {
		seenTmpIDs = append(seenTmpIDs, tmpID)
		st := statuses[calls]
		calls++
		return gm.SendResult{Status: st}, nil
	})
	if err != nil {
		t.Fatalf("SendWithRetry: %v", err)
	}
	if res.Status != gm.SendStatusSuccess {
		t.Errorf("status = %s, want SUCCESS", res.Status)
	}
	if calls != 3 {
		t.Errorf("the library was called %d times, want 3", calls)
	}
	want := []time.Duration{3 * time.Second, 8 * time.Second}
	got := clk.Sleeps()
	if len(got) != len(want) {
		t.Fatalf("backoffs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff %d = %v, want %v", i, got[i], want[i])
		}
	}
	// One tmp_id across the retries: a fresh one per attempt would let two
	// attempts of one request correlate to two operations (spec 6.3).
	for i, id := range seenTmpIDs {
		if id != tmpID {
			t.Errorf("attempt %d sent tmp_id %q, want the one minted for the request", i, id)
		}
	}
}

// The other half of test 10: FAILURE_4 is not_default_sms_app and is NOT
// retried. Retrying it sends nothing and delays an answer the owner has to
// act on, on the phone.
func TestFailure4IsNotRetried(t *testing.T) {
	clk := clock.NewFake()
	calls := 0
	_, err := core.SendWithRetry(context.Background(), clk, func(context.Context) (gm.SendResult, error) {
		calls++
		return gm.SendResult{Status: gm.SendStatusFailure4}, nil
	})
	if calls != 1 {
		t.Errorf("the library was called %d times; FAILURE_4 must not be retried", calls)
	}
	if len(clk.Sleeps()) != 0 {
		t.Errorf("it backed off %v before giving up on FAILURE_4", clk.Sleeps())
	}
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodeNotDefaultSMSApp {
		t.Fatalf("error = %v, want not_default_sms_app", err)
	}
	// The raw Google status is admin-only: it must not reach a public
	// details block (spec sections 4.1, 6.5, 7.2).
	if _, ok := ge.Details["google_status_raw"]; ok {
		t.Error("the public error details carry google_status_raw")
	}
	if ge.GoogleStatusRaw != int32(gm.SendStatusFailure4) {
		t.Errorf("GoogleStatusRaw = %d, want 4", ge.GoogleStatusRaw)
	}
}

// A transient status that never clears exhausts the schedule and then fails,
// rather than retrying forever inside one HTTP request.
func TestTransientSendGivesUpAfterTheWholeSchedule(t *testing.T) {
	clk := clock.NewFake()
	calls := 0
	_, err := core.SendWithRetry(context.Background(), clk, func(context.Context) (gm.SendResult, error) {
		calls++
		return gm.SendResult{Status: gm.SendStatusFailure3}, nil
	})
	if err == nil {
		t.Fatal("a send that never succeeds must fail")
	}
	if calls != len(gm.SendRetryBackoff)+1 {
		t.Errorf("the library was called %d times, want %d", calls, len(gm.SendRetryBackoff)+1)
	}
	total := time.Duration(0)
	for _, d := range clk.Sleeps() {
		total += d
	}
	if total != 31*time.Second {
		t.Errorf("total backoff = %v, want 31s (3+8+20)", total)
	}
}

// A library error is never retried here. ErrPhoneNotResponding in particular
// is pending, not failed, and resending it is how a real person gets the same
// text twice (D5).
func TestALibraryErrorIsNotRetried(t *testing.T) {
	clk := clock.NewFake()
	calls := 0
	_, err := core.SendWithRetry(context.Background(), clk, func(context.Context) (gm.SendResult, error) {
		calls++
		return gm.SendResult{}, gm.Classify(gm.ErrPhoneNotResponding)
	})
	if calls != 1 {
		t.Errorf("the library was called %d times; a library error is not retried here", calls)
	}
	var ge *gm.Error
	if !errors.As(err, &ge) || ge.Code != gm.CodePhoneNotResponding {
		t.Fatalf("error = %v, want phone_not_responding", err)
	}
	if !ge.KeepsOperationPending {
		t.Error("phone_not_responding does not keep the operation pending")
	}
}

// Section 16 Slice 2 test 11, all four arms. The version diff wins over the
// status, which is the whole detection rule of section 3.7 -- asserting on a
// particular status number would re-import the unsourced claim 18.1 withdrew.
func TestClassifyResolve(t *testing.T) {
	same := gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 4, V2: 6}
	ahead := gm.ConfigVersion{Year: 2026, Month: 9, Day: 3, V1: 4, V2: 6}
	conv := &gm.Conversation{SourceID: "c1"}

	t.Run("success returns the conversation", func(t *testing.T) {
		got, err := core.ClassifyResolve(
			gm.ResolveResult{Status: gm.ResolveStatusSuccess, Conversation: conv}, same, same)
		if err != nil || got != conv {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("a second CREATE_RCS is google_error", func(t *testing.T) {
		_, err := core.ClassifyResolve(gm.ResolveResult{Status: gm.ResolveStatusCreateRCS}, same, same)
		var ge *gm.Error
		if !errors.As(err, &ge) || ge.Code != gm.CodeGoogleError {
			t.Fatalf("error = %v, want google_error", err)
		}
	})

	t.Run("an unnamed integer is google_undocumented_status carrying the bare number", func(t *testing.T) {
		_, err := core.ClassifyResolve(gm.ResolveResult{Status: gm.ResolveStatus(2)}, same, same)
		var ge *gm.Error
		if !errors.As(err, &ge) || ge.Code != gm.CodeGoogleUndocumentedState {
			t.Fatalf("error = %v, want google_undocumented_status", err)
		}
		if ge.Details["status"] != int32(2) {
			t.Errorf("details.status = %v, want the bare integer 2", ge.Details["status"])
		}
		// No meaning is claimed for it.
		for _, invented := range []string{"CREATE_RCS", "FAILURE", "SUCCESS", "UNKNOWN"} {
			if ge.Message == invented {
				t.Errorf("the message invents a name for status 2: %q", ge.Message)
			}
		}
	})

	t.Run("a version mismatch is config_version_stale naming both versions", func(t *testing.T) {
		// Deliberately NOT a special status: the detection rule is the
		// version diff alone (spec 3.7, 13.2).
		for _, st := range []gm.ResolveStatus{gm.ResolveStatusCreateRCS, gm.ResolveStatus(2), gm.ResolveStatus(9)} {
			_, err := core.ClassifyResolve(gm.ResolveResult{Status: st}, same, ahead)
			var ge *gm.Error
			if !errors.As(err, &ge) || ge.Code != gm.CodeConfigVersionStale {
				t.Fatalf("status %d: error = %v, want config_version_stale", st, err)
			}
			if ge.Details["compiled_config_version"] != same.String() ||
				ge.Details["live_config_version"] != ahead.String() {
				t.Errorf("the error does not name both versions: %v", ge.Details)
			}
		}
	})

	t.Run("success with no conversation is a contract violation", func(t *testing.T) {
		_, err := core.ClassifyResolve(gm.ResolveResult{Status: gm.ResolveStatusSuccess}, same, same)
		if !errors.Is(err, core.ErrResolveNoConversation) {
			t.Fatalf("error = %v, want ErrResolveNoConversation", err)
		}
	})
}
