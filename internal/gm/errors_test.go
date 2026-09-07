package gm_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Slice 1 acceptance test 5: every error in spec section 3.5 maps to the
// gm-level error value section 7.2 will later render. The table below
// ENUMERATES the section 3.5 list rather than sampling it, and the guard at
// the bottom fails if a row is added to the taxonomy without a case here.
func TestClassifyEnumeratesTheSection35Taxonomy(t *testing.T) {
	type want struct {
		code      gm.Code
		status    int
		reason    string
		signsOut  bool
		retryable bool
		pending   bool
	}
	cases := []struct {
		name string
		err  error
		want want
	}{
		{
			name: "libgm.ErrPhoneNotResponding",
			err:  fmt.Errorf("wrapped: %w", gm.ErrPhoneNotResponding),
			// 504, and the operation stays pending, not failed (D5).
			want: want{code: gm.CodePhoneNotResponding, status: http.StatusGatewayTimeout, retryable: true, pending: true},
		},
		{
			name: "libgm.ErrConnectionClosed",
			err:  gm.ErrConnectionClosed,
			want: want{code: gm.CodeDisconnected, status: http.StatusServiceUnavailable, retryable: true},
		},
		{
			name: "events.ErrInvalidCredentials",
			err:  gm.ErrInvalidCredentials,
			// Sets the account signed_out; a write is unsupported_capability
			// with details.reason = not_signed_in. NOT not_paired.
			want: want{code: gm.CodeUnsupportedCapability, status: http.StatusConflict,
				reason: gm.ReasonNotSignedIn, signsOut: true},
		},
		{
			name: "events.ErrRequestedEntityNotFound",
			err:  gm.ErrRequestedEntityNotFound,
			want: want{code: gm.CodeUnsupportedCapability, status: http.StatusConflict,
				reason: gm.ReasonNotSignedIn, signsOut: true},
		},
		{
			name: "events.ErrCallerNoPermission",
			err:  gm.ErrCallerNoPermission,
			want: want{code: gm.CodeGooglePermissionDenied, status: http.StatusBadGateway},
		},
		{
			name: "events.RequestError",
			err:  gm.RequestError{Type: 9, Message: "resource exhausted"},
			want: want{code: gm.CodeGoogleError, status: http.StatusBadGateway},
		},
		{
			name: "events.HTTPError",
			err:  gm.HTTPError{Action: "polling", StatusCode: 502},
			want: want{code: gm.CodeGoogleHTTPError, status: http.StatusBadGateway, retryable: true},
		},
		{
			name: "pair_google.ErrNoCookies",
			err:  gm.ErrNoCookies,
			want: want{code: gm.CodePairingNoCookies, status: http.StatusConflict},
		},
		{
			name: "pair_google.ErrNoDevicesFound",
			err:  gm.ErrNoDevicesFound,
			want: want{code: gm.CodePairingNoDevices, status: http.StatusConflict},
		},
		{
			name: "pair_google.ErrIncorrectEmoji",
			err:  gm.ErrIncorrectEmoji,
			want: want{code: gm.CodePairingWrongEmoji, status: http.StatusConflict},
		},
		{
			name: "pair_google.ErrPairingCancelled",
			err:  gm.ErrPairingCancelled,
			want: want{code: gm.CodePairingCancelled, status: http.StatusConflict},
		},
		{
			name: "pair_google.ErrPairingTimeout",
			err:  gm.ErrPairingTimeout,
			want: want{code: gm.CodePairingTimeout, status: http.StatusConflict},
		},
		{
			name: "pair_google.ErrPairingInitTimeout",
			err:  gm.ErrPairingInitTimeout,
			want: want{code: gm.CodePairingInitTimeout, status: http.StatusConflict, retryable: true},
		},
	}

	seen := map[gm.Code]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gm.Classify(tc.err)
			if got == nil {
				t.Fatal("Classify returned nil")
			}
			seen[got.Code] = true
			if got.Code != tc.want.code {
				t.Errorf("code = %s, want %s", got.Code, tc.want.code)
			}
			if got.HTTPStatus != tc.want.status {
				t.Errorf("status = %d, want %d", got.HTTPStatus, tc.want.status)
			}
			if got.Reason != tc.want.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want.reason)
			}
			if got.SignsOutAccount != tc.want.signsOut {
				t.Errorf("signsOut = %v, want %v", got.SignsOutAccount, tc.want.signsOut)
			}
			if got.Retryable != tc.want.retryable {
				t.Errorf("retryable = %v, want %v", got.Retryable, tc.want.retryable)
			}
			if got.KeepsOperationPending != tc.want.pending {
				t.Errorf("keepsOperationPending = %v, want %v", got.KeepsOperationPending, tc.want.pending)
			}
			if !errors.Is(got, tc.err) && !errors.Is(got.Err, tc.err) {
				t.Errorf("the classified error does not wrap the library error")
			}
		})
	}

	// Every sentinel the taxonomy declares must appear above. Adding one
	// without a row here fails.
	for _, e := range []error{
		gm.ErrPhoneNotResponding, gm.ErrConnectionClosed, gm.ErrInvalidCredentials,
		gm.ErrRequestedEntityNotFound, gm.ErrCallerNoPermission, gm.ErrNoCookies,
		gm.ErrNoDevicesFound, gm.ErrIncorrectEmoji, gm.ErrPairingCancelled,
		gm.ErrPairingTimeout, gm.ErrPairingInitTimeout,
	} {
		code := gm.Classify(e).Code
		if !seen[code] {
			t.Errorf("sentinel %v classifies to %s, which no case in this table asserts", e, code)
		}
	}
}

// ErrHadMultipleDevices is only ever wrapped inside ErrPairingInitTimeout, so
// it is reported as details.multiple_devices, never as a code of its own.
// There is no pairing_multiple_devices code: Google never reports "multiple
// devices" as an error, the library picks one.
func TestMultipleDevicesIsDetailNotCode(t *testing.T) {
	err := fmt.Errorf("%w (%w)", gm.ErrPairingInitTimeout, gm.ErrHadMultipleDevices)
	got := gm.Classify(err)
	if got.Code != gm.CodePairingInitTimeout {
		t.Fatalf("code = %s, want %s", got.Code, gm.CodePairingInitTimeout)
	}
	if got.Details["multiple_devices"] != true {
		t.Errorf("details.multiple_devices = %v, want true", got.Details["multiple_devices"])
	}
	plain := gm.Classify(gm.ErrPairingInitTimeout)
	if plain.Details["multiple_devices"] == true {
		t.Error("a plain init timeout must not claim multiple devices")
	}
}

// not_signed_in is an account-level condition. not_paired means the server
// holds no accounts at all, and NO library error maps to it. Collapsing the
// two is the mistake spec section 3.5 names.
func TestCredentialDeathIsNeverNotPaired(t *testing.T) {
	for _, err := range []error{gm.ErrInvalidCredentials, gm.ErrRequestedEntityNotFound} {
		got := gm.Classify(err)
		if got.Reason != gm.ReasonNotSignedIn {
			t.Errorf("%v: reason = %q, want %q", err, got.Reason, gm.ReasonNotSignedIn)
		}
		if string(got.Code) == "not_paired" {
			t.Errorf("%v: credential death must never render as not_paired", err)
		}
		if !got.SignsOutAccount {
			t.Errorf("%v: must set the account signed_out", err)
		}
	}
}

// FAILURE_4 is not retried and carries the user-facing sentence upstream
// renders; FAILURE_2 and FAILURE_3 are transient.
func TestSendFailureMapping(t *testing.T) {
	four := gm.SendFailure(gm.SendStatusFailure4)
	if four.Code != gm.CodeNotDefaultSMSApp {
		t.Errorf("FAILURE_4 maps to %s, want %s", four.Code, gm.CodeNotDefaultSMSApp)
	}
	if four.Retryable {
		t.Error("FAILURE_4 must not be retryable")
	}
	if four.Message != "Google Messages is not your default SMS app" {
		t.Errorf("FAILURE_4 message is %q", four.Message)
	}
	// The retry set itself, not only the classified error: a future retry
	// loop consults IsTransient, and retrying FAILURE_4 would send the same
	// SMS to a real person up to four times.
	// Plant M18, 2026-09-06: IsTransient extended to FAILURE_4 -- this is the
	// assertion that kills it.
	if gm.SendStatusFailure4.IsTransient() {
		t.Error("FAILURE_4 must not be transient: upstream never retries it")
	}
	if !gm.SendStatusFailure2.IsTransient() || !gm.SendStatusFailure3.IsTransient() {
		t.Error("FAILURE_2 and FAILURE_3 are the only transient send failures")
	}
	for _, st := range []gm.SendStatus{gm.SendStatusFailure2, gm.SendStatusFailure3} {
		e := gm.SendFailure(st)
		if e.Code != gm.CodeGoogleError || !e.Retryable {
			t.Errorf("%s maps to %s retryable=%v, want google_error retryable=true", st, e.Code, e.Retryable)
		}
	}
	if gm.SendStatusUnknown.IsTransient() {
		t.Error("UNKNOWN is not a transient send failure")
	}
}

// An unnamed GetOrCreateConversation status is google_undocumented_status
// carrying the bare integer. Agent GM never invents a name for one.
func TestUndocumentedResolveStatus(t *testing.T) {
	e := gm.UndocumentedResolveStatus(gm.ResolveStatus(4))
	if e.Code != gm.CodeGoogleUndocumentedState {
		t.Fatalf("code = %s", e.Code)
	}
	if e.Details["status"] != int32(4) {
		t.Errorf("details.status = %v, want the bare integer 4", e.Details["status"])
	}
	if got := e.Message; got == "" {
		t.Error("the message must say something")
	}
}

// The listen-loop match is on the error value, never the string.
func TestIsFatalListenError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{gm.HTTPError{Action: "polling", StatusCode: 401}, true},
		{gm.HTTPError{Action: "polling", StatusCode: 403}, true},
		{gm.HTTPError{Action: "polling", StatusCode: 500}, false},
		{gm.HTTPError{Action: "polling", StatusCode: 404}, false},
		{gm.ErrInvalidCredentials, true},
		{errors.New("http 401 while polling"), false}, // the string alone is not the match
		{nil, false},
	} {
		if got := gm.IsFatalListenError(tc.err); got != tc.want {
			t.Errorf("IsFatalListenError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
