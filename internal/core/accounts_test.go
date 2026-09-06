package core_test

import (
	"errors"
	"testing"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/core"
)

var (
	acctA = core.AccountRef{ID: "acct_00000000-0000-4000-8000-00000000000a", GoogleAccount: "a@example.test", State: "connected"}
	acctB = core.AccountRef{ID: "acct_00000000-0000-4000-8000-00000000000b", GoogleAccount: "b@example.test", State: "signed_out"}
)

// Section 16 Slice 2 test 34: ambiguity is an error that can be ACTED ON.
//
// Plant: drop details.accounts from the refusal, or return not_found instead
// of invalid_request, and this test fails. Planted 2026-09-06.
func TestAmbiguityIsAnErrorThatCanBeActedOn(t *testing.T) {
	accounts := []core.AccountRef{acctA, acctB}

	_, err := core.ResolveAccount("", accounts, core.Write)
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want an apierr", err)
	}
	if ae.Code != apierr.CodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", ae.Code)
	}
	if ae.Details["field"] != "account_id" {
		t.Errorf("details.field = %v, want account_id", ae.Details["field"])
	}
	candidates, ok := ae.Details["accounts"].([]apierr.AccountCandidate)
	if !ok || len(candidates) != 2 {
		t.Fatalf("details.accounts = %#v, want both accounts", ae.Details["accounts"])
	}
	// Every candidate carries {id, google_account, state}, so the caller can
	// retry without a second round trip -- including the signed-out one,
	// which is a real account they may have meant.
	byID := map[string]apierr.AccountCandidate{}
	for _, c := range candidates {
		byID[c.ID] = c
	}
	for _, want := range accounts {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("account %s is not among the candidates", want.ID)
			continue
		}
		if got.GoogleAccount != want.GoogleAccount || got.State != want.State {
			t.Errorf("candidate %s = %+v, want google_account %q state %q",
				want.ID, got, want.GoogleAccount, want.State)
		}
	}

	// Retrying with one of those IDs succeeds -- which is the half that makes
	// the error actionable rather than merely informative.
	for _, a := range accounts {
		res, err := core.ResolveAccount(a.ID, accounts, core.Write)
		if err != nil {
			t.Errorf("retrying with %s failed: %v", a.ID, err)
		}
		if res.AccountID != a.ID {
			t.Errorf("retry resolved to %q, want %q", res.AccountID, a.ID)
		}
	}
}

// With exactly one account the same call, omitting account_id, SUCCEEDS. A
// single-account deployment never has to think about accounts.
func TestOneAccountNeedsNoAccountID(t *testing.T) {
	one := []core.AccountRef{acctA}
	for _, intent := range []core.Intent{core.Read, core.Write} {
		res, err := core.ResolveAccount("", one, intent)
		if err != nil {
			t.Fatalf("intent %v: %v", intent, err)
		}
		if res.AccountID != acctA.ID || res.AllAccounts {
			t.Errorf("intent %v resolved to %+v, want the one account", intent, res)
		}
	}
}

// A READ omitting account_id covers every account. That is a useful default
// for "what came in today" and a dangerous one for a send, which is why they
// differ (spec section 7.3).
func TestAReadOmittingTheAccountCoversThemAll(t *testing.T) {
	accounts := []core.AccountRef{acctA, acctB}
	res, err := core.ResolveAccount("", accounts, core.Read)
	if err != nil {
		t.Fatalf("a read omitting account_id was refused: %v", err)
	}
	if !res.AllAccounts || res.AccountID != "" {
		t.Errorf("resolution = %+v, want every account", res)
	}
}

// Zero accounts: any write is not_paired -- a SERVICE-level condition, not a
// property of a conversation -- and a read is an empty page rather than an
// error (spec sections 7.2, 7.3).
func TestZeroAccounts(t *testing.T) {
	_, err := core.ResolveAccount("", nil, core.Write)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNotPaired {
		t.Fatalf("write with no accounts = %v, want not_paired", err)
	}
	res, err := core.ResolveAccount("", nil, core.Read)
	if err != nil {
		t.Fatalf("read with no accounts = %v, want an empty page", err)
	}
	if !res.AllAccounts {
		t.Errorf("resolution = %+v, want every (that is, no) account", res)
	}
}

// A malformed ID is invalid_request naming the parameter and the expected
// prefix -- NEVER not_found, which would suggest the account is gone
// (spec section 4.1). An acct_ ID that simply does not exist IS not_found.
func TestAMalformedAccountIDIsNeverNotFound(t *testing.T) {
	accounts := []core.AccountRef{acctA}
	for _, bad := range []string{
		"conv_00000000-0000-4000-8000-000000000001", // right shape, wrong prefix
		"acct_not-a-uuid",
		"12345",
		"someone@example.test", // a Google address is not an ID
	} {
		_, err := core.ResolveAccount(bad, accounts, core.Read)
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Errorf("%q gave %v, want an apierr", bad, err)
			continue
		}
		if ae.Code == apierr.CodeNotFound {
			t.Errorf("%q answered not_found; a malformed ID is invalid_request", bad)
		}
		if ae.Code != apierr.CodeInvalidRequest {
			t.Errorf("%q gave code %q, want invalid_request", bad, ae.Code)
		}
		if ae.Details["expected_prefix"] != "acct_" {
			t.Errorf("%q did not name the expected prefix: %v", bad, ae.Details)
		}
	}

	absent := "acct_00000000-0000-4000-8000-0000000000ff"
	_, err := core.ResolveAccount(absent, accounts, core.Read)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeNotFound {
		t.Errorf("a well-formed absent ID gave %v, want not_found", err)
	}
}

// A conv_ or msg_ ID paired with the WRONG account_id is invalid_request
// naming both, never not_found (spec section 7.3).
func TestAnObjectFromAnotherAccountIsNamedNotHidden(t *testing.T) {
	const conv = "conv_00000000-0000-4000-8000-000000000001"
	if err := core.CheckObjectAccount("conversation_id", conv, acctA.ID, acctA.ID); err != nil {
		t.Errorf("the matching account was refused: %v", err)
	}
	// No account named at all is not a mismatch: a conversation ID already
	// implies its account (spec section 4.1).
	if err := core.CheckObjectAccount("conversation_id", conv, acctA.ID, ""); err != nil {
		t.Errorf("omitting account_id beside a conv_ ID was refused: %v", err)
	}

	err := core.CheckObjectAccount("conversation_id", conv, acctA.ID, acctB.ID)
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want an apierr", err)
	}
	if ae.Code == apierr.CodeNotFound {
		t.Fatal("a mismatched account answered not_found, which suggests the thread is gone")
	}
	if ae.Code != apierr.CodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", ae.Code)
	}
	if ae.Details["parameter"] != "conversation_id" || ae.Details["id"] != conv ||
		ae.Details["account_id"] != acctB.ID {
		t.Errorf("the error does not name both: %v", ae.Details)
	}
}

// Step 5 of section 6.2: every state that reads but does not write answers
// unsupported_capability with not_signed_in, which is a per-account condition
// and NOT the service-level not_paired. `parked` is in the set because a
// parked account holds no client to write with (section 7.8, as amended).
func TestOnlyConnectedAndDegradedAccountsAreWritable(t *testing.T) {
	writable := map[string]bool{"connected": true, "degraded": true}
	for _, state := range []string{
		"pairing", "connected", "degraded", "error", "signed_out", "parked", "account_changed",
	} {
		a := core.AccountRef{ID: acctA.ID, GoogleAccount: acctA.GoogleAccount, State: state}
		err := core.CheckAccountWritable(a)
		if writable[state] {
			if err != nil {
				t.Errorf("state %q was refused: %v", state, err)
			}
			continue
		}
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Errorf("state %q gave %v, want unsupported_capability", state, err)
			continue
		}
		if ae.Code != apierr.CodeUnsupportedCapability {
			t.Errorf("state %q gave code %q, want unsupported_capability", state, ae.Code)
		}
		if ae.Code == apierr.CodeNotPaired {
			t.Errorf("state %q answered not_paired, which means there are no accounts at all", state)
		}
		if got := ae.Details["reason"]; got != apierr.ReasonNotSignedIn {
			t.Errorf("state %q gave reason %v, want not_signed_in", state, got)
		}
		// The refusal names the state, so an agent can tell "waiting for a
		// slot" from "the cookies died" without reading logs.
		if ae.Details["capability_value"] != state {
			t.Errorf("state %q is not named in the refusal: %v", state, ae.Details)
		}
	}
}
