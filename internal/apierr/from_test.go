package apierr

import (
	"errors"
	"net/http"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// R-2, and the reason this test exists at the end of Slice 2 rather than the
// start: gm_agreement_test.go asserted that the two error VOCABULARIES agree
// and that their statuses agree, and never that the translation between them
// happens. It did not, so every code section 7.2 reserves for the Google
// layer -- phone_not_responding, not_default_sms_app, config_version_stale,
// google_undocumented_status, disconnected, every pairing_* -- reached callers
// as internal_error 500.
//
// Two vocabularies that agree perfectly are worth nothing if nothing carries a
// value from one to the other, so this drives the real classifier and asserts
// on what comes out.
//
// Plant: delete the fromGM call in From and every subtest fails naming its
// code. Planted 2026-09-07.
func TestEveryClassifiedLibraryErrorKeepsItsCode(t *testing.T) {
	// Driven through gm.Classify rather than through hand-built gm.Errors,
	// so this is the path a handler really takes.
	cases := []struct {
		name string
		err  error
		want Code
	}{
		{"phone not responding", gm.ErrPhoneNotResponding, CodePhoneNotResponding},
		{"connection closed", gm.ErrConnectionClosed, CodeDisconnected},
		{"invalid credentials", gm.ErrInvalidCredentials, CodeUnsupportedCapability},
		{"entity not found", gm.ErrRequestedEntityNotFound, CodeUnsupportedCapability},
		{"caller no permission", gm.ErrCallerNoPermission, CodeGooglePermissionDenied},
		{"no cookies", gm.ErrNoCookies, CodePairingNoCookies},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			classified := gm.Classify(tc.err)
			if classified == nil {
				t.Fatal("gm.Classify returned nil")
			}
			got := From(classified)
			if got.Code != tc.want {
				t.Fatalf("From(%s) = %q, want %q", tc.name, got.Code, tc.want)
			}
			if got.Code == CodeInternalError {
				t.Fatal("a classified library error was flattened to internal_error")
			}
			if got.HTTPStatus() != HTTPStatus(tc.want) {
				t.Errorf("status = %d, want %d", got.HTTPStatus(), HTTPStatus(tc.want))
			}
		})
	}
}

// The three codes the spec calls out by name, each built the way the gm layer
// builds it, because each carries structure a bare code would lose.
func TestTheStructuredLibraryErrorsKeepTheirDetails(t *testing.T) {
	t.Run("phone_not_responding is 504 and retryable", func(t *testing.T) {
		got := From(gm.Classify(gm.ErrPhoneNotResponding))
		if got.Code != CodePhoneNotResponding {
			t.Fatalf("code = %q", got.Code)
		}
		if got.HTTPStatus() != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", got.HTTPStatus())
		}
		if !got.Retryable() {
			t.Error("phone_not_responding is not retryable")
		}
	})

	t.Run("google_undocumented_status carries the bare integer", func(t *testing.T) {
		got := From(gm.UndocumentedResolveStatus(gm.ResolveStatus(2)))
		if got.Code != CodeGoogleUndocumentedStatus {
			t.Fatalf("code = %q", got.Code)
		}
		if got.Details["status"] != int32(2) {
			t.Errorf("details.status = %v, want the bare integer 2", got.Details["status"])
		}
	})

	t.Run("not_default_sms_app keeps the raw status off the public surface", func(t *testing.T) {
		got := From(gm.NotDefaultSMSApp())
		if got.Code != CodeNotDefaultSMSApp {
			t.Fatalf("code = %q", got.Code)
		}
		if _, leaked := got.Details["google_status_raw"]; leaked {
			t.Error("the public details carry google_status_raw")
		}
	})
}

// unsupported_capability's reason comes from section 7.8's CLOSED vocabulary,
// so it is looked up rather than copied across: a reason the vocabulary does
// not name must not reach a caller just because the gm layer said it.
func TestAnUnsupportedCapabilityReasonIsLookedUpNotCopied(t *testing.T) {
	classified := gm.Classify(gm.ErrInvalidCredentials)
	got := From(classified)
	if got.Code != CodeUnsupportedCapability {
		t.Fatalf("code = %q", got.Code)
	}
	reason, ok := got.Details["reason"].(Reason)
	if !ok {
		t.Fatalf("details.reason is %T, want a typed Reason", got.Details["reason"])
	}
	if reason != ReasonNotSignedIn {
		t.Errorf("reason = %v, want not_signed_in", reason)
	}

	// A reason outside the vocabulary is refused rather than served.
	invented := &gm.Error{
		Code:       gm.CodeUnsupportedCapability,
		HTTPStatus: http.StatusConflict,
		Message:    "invented",
		Reason:     "definitely_not_in_section_7_8",
	}
	if out := From(invented); out.Code != CodeInternalError {
		t.Errorf("an invented reason was served as %q; it must not widen the vocabulary", out.Code)
	}
}

// An error nobody classified is still internal_error with a fixed message: the
// translation must not become a hole that lets an arbitrary Go error string
// out.
func TestAnUnclassifiedErrorIsStillInternalError(t *testing.T) {
	got := From(errors.New("a file path and a SQL statement walk into a bar"))
	if got.Code != CodeInternalError {
		t.Fatalf("code = %q", got.Code)
	}
	if got.Message == "a file path and a SQL statement walk into a bar" {
		t.Error("the underlying error string reached the message")
	}
}
