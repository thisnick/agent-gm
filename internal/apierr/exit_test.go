package apierr

import (
	"testing"
)

// specExit restates spec section 11.2's mapping table as literals, written
// out from the spec independently of exit.go.
var specExit = map[string]int{
	"invalid_request":        2,
	"idempotency_conflict":   2,
	"payload_too_large":      2,
	"media_unsupported_type": 2,

	"invalid_token": 3,

	"insufficient_scope": 4,

	"not_found": 5,

	"unsupported_capability": 6,

	"rate_limited":         7,
	"disconnected":         7,
	"phone_not_responding": 7,
	"google_http_error":    7,
	"pairing_init_timeout": 7,

	"not_paired":                 10,
	"pairing_no_cookies":         10,
	"pairing_no_devices":         10,
	"pairing_wrong_emoji":        10,
	"pairing_cancelled":          10,
	"pairing_timeout":            10,
	"pairing_wrong_account":      10,
	"pairing_no_account":         10,
	"not_default_sms_app":        10,
	"google_error":               10,
	"google_undocumented_status": 10,
	"google_permission_denied":   10,
	"internal_error":             10,
}

// TestExitCodeMappingMatchesSpec11_2 proves every code maps to the exit code
// the spec states.
func TestExitCodeMappingMatchesSpec11_2(t *testing.T) {
	for name, want := range specExit {
		if got := ExitCodeFor(Code(name)); got != want {
			t.Errorf("ExitCodeFor(%q) = %d, spec section 11.2 says %d", name, got, want)
		}
	}
}

// TestExitCodeMappingIsExhaustiveBothWays is Slice 2 test 21's table half:
// every code of spec section 7.2 maps onto exactly one exit code, and the
// mapping names no code section 7.2 does not.
func TestExitCodeMappingIsExhaustiveBothWays(t *testing.T) {
	mapping := ExitCodes()

	for _, c := range Codes() {
		if _, ok := mapping[c]; !ok {
			t.Errorf("code %q has no exit code; spec section 11.2 says the mapping is exhaustive", c)
		}
		if _, ok := specExit[string(c)]; !ok {
			t.Errorf("code %q is not in spec section 11.2's table", c)
		}
	}
	for c := range mapping {
		if !Known(c) {
			t.Errorf("the exit table maps %q, which spec section 7.2 does not name", c)
		}
	}
	if len(mapping) != len(Codes()) {
		t.Errorf("the exit table has %d rows, the vocabulary has %d", len(mapping), len(Codes()))
	}
	if len(specExit) != len(Codes()) {
		t.Errorf("the transcribed section 11.2 table has %d rows, the vocabulary has %d", len(specExit), len(Codes()))
	}
}

// TestExitCodeOneIsUnassigned proves nothing maps onto 1, which spec section
// 11.2 reserves so that "the process died" stays distinct from "Agent GM
// decided something".
func TestExitCodeOneIsUnassigned(t *testing.T) {
	for c, code := range ExitCodes() {
		if code == 1 {
			t.Errorf("%q maps to exit 1, which spec section 11.2 leaves unassigned", c)
		}
	}
}

// TestExitCodeConstantsAreTheSpecsNumbers pins the ten constants of spec
// section 11.2.
func TestExitCodeConstantsAreTheSpecsNumbers(t *testing.T) {
	pairs := []struct {
		got  int
		want int
		name string
	}{
		{ExitOK, 0, "success"},
		{ExitUsage, 2, "CLI usage or validation error"},
		{ExitAuthRequired, 3, "authentication required or expired credentials"},
		{ExitForbidden, 4, "authorization or insufficient scope"},
		{ExitNotFound, 5, "requested resource absent"},
		{ExitUnsupported, 6, "unsupported capability"},
		{ExitRetryable, 7, "retryable network, Google, phone, or rate-limit failure"},
		{ExitOperationFailed, 8, "an operation reached a terminal failure"},
		{ExitLocalConfig, 9, "local configuration or credential-store failure"},
		{ExitServerFailure, 10, "server contract or internal failure"},
	}
	for _, p := range pairs {
		if p.got != p.want {
			t.Errorf("%s is %d, spec section 11.2 says %d", p.name, p.got, p.want)
		}
	}
}

// TestTheThreeExitCodesTheSpecCallsOut asserts the three rows spec section
// 11.2 argues for in prose, because each is a place an implementer would
// naturally choose the other number.
func TestTheThreeExitCodesTheSpecCallsOut(t *testing.T) {
	// A caller mistake, not a server failure: the same key with a different
	// body.
	if got := ExitCodeFor(CodeIdempotencyConflict); got != 2 {
		t.Errorf("idempotency_conflict exits %d, spec section 11.2 says 2", got)
	}
	// Retryable, NOT an operation failure: the operation is pending and a
	// script that treats it as failed resends the message.
	if got := ExitCodeFor(CodePhoneNotResponding); got != 7 {
		t.Errorf("phone_not_responding exits %d, spec section 11.2 says 7, not 8", got)
	}
	if ExitCodeFor(CodePhoneNotResponding) == ExitOperationFailed {
		t.Error("phone_not_responding must never exit 8")
	}
	// Account-level, which is why the section 11.2 gloss names the account.
	if got := ExitCodeFor(CodeUnsupportedCapability); got != 6 {
		t.Errorf("unsupported_capability exits %d, spec section 11.2 says 6", got)
	}
	notSignedIn := UnsupportedCapability(ReasonNotSignedIn, "acct_1", "send", "state", "signed_out")
	if got := notSignedIn.ExitCode(); got != 6 {
		t.Errorf("not_signed_in exits %d, spec section 11.2 says 6", got)
	}
}

// TestExitCodesWithNoErrorCode records the three exit codes spec section 11.2
// defines that no section 7.2 code produces: 0, 8 and 9 are decided by the
// CLI, not by the server's answer.
func TestExitCodesWithNoErrorCode(t *testing.T) {
	for _, code := range []int{ExitOK, ExitOperationFailed, ExitLocalConfig} {
		for c, mapped := range ExitCodes() {
			if mapped == code {
				t.Errorf("%q maps to exit %d, which no error code may produce", c, code)
			}
		}
	}
}
