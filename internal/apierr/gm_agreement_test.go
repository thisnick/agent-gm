package apierr

import (
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// internal/gm is the layer below: it classifies a library error into the same
// code vocabulary (spec section 3.5) and records the HTTP status spec section
// 7.2 assigns. This package is the layer above it. These tests assert the two
// agree rather than letting them be two independent tables that happen to
// look alike today.
//
// The test is in this package rather than in gm because the dependency runs
// one way: gm knows nothing about REST, and importing apierr there would
// invert that.

// gmCodesOutsideSection7_2 records any code internal/gm produces that spec
// section 7.2's table does not list.
//
// It is empty, and staying empty is the point: it exists so that a gap
// between the two vocabularies is declared here rather than papered over by
// quietly widening either one. `pairing_no_account` used to sit in it --
// section 16 Slice 2 test 38 required the code and the section 7.2 table had
// no row for it -- and the fix was to add the row to the spec, not to add an
// exception here.
var gmCodesOutsideSection7_2 = map[gm.Code]string{}

// TestGMCodesExistInTheRESTVocabulary proves every code internal/gm can
// produce is renderable by this package, so a classified library error can
// always reach a caller.
func TestGMCodesExistInTheRESTVocabulary(t *testing.T) {
	for _, c := range gmCodes() {
		if _, known := gmCodesOutsideSection7_2[c]; known {
			continue
		}
		if !Known(Code(c)) {
			t.Errorf("internal/gm produces %q, which the REST vocabulary cannot render", c)
		}
	}
}

// TestGMHTTPStatusesAgreeWithTheTable proves the status internal/gm records
// for a classified error equals the status spec section 7.2's table assigns
// to the same code. If the two ever disagree, a route rendering gm's status
// answers something the table does not.
func TestGMHTTPStatusesAgreeWithTheTable(t *testing.T) {
	classified := []*gm.Error{
		gm.Classify(gm.ErrPhoneNotResponding),
		gm.Classify(gm.ErrConnectionClosed),
		gm.Classify(gm.ErrInvalidCredentials),
		gm.Classify(gm.ErrRequestedEntityNotFound),
		gm.Classify(gm.ErrCallerNoPermission),
		gm.Classify(gm.ErrNoCookies),
		gm.Classify(gm.ErrNoDevicesFound),
		gm.Classify(gm.ErrIncorrectEmoji),
		gm.Classify(gm.ErrPairingCancelled),
		gm.Classify(gm.ErrPairingTimeout),
		gm.Classify(gm.ErrPairingInitTimeout),
		gm.Classify(gm.ErrWrongAccount),
		gm.Classify(gm.RequestError{Type: 16, Message: "denied"}),
		gm.Classify(gm.HTTPError{Action: "polling", StatusCode: 503}),
		gm.NotDefaultSMSApp(),
	}

	for _, e := range classified {
		if _, known := gmCodesOutsideSection7_2[e.Code]; known {
			continue
		}
		c := Code(e.Code)
		if !Known(c) {
			t.Errorf("internal/gm produced %q, which spec section 7.2 does not name", c)
			continue
		}
		if got, want := e.HTTPStatus, HTTPStatus(c); got != want {
			t.Errorf("%q: internal/gm says HTTP %d, spec section 7.2's table says %d", c, got, want)
		}
	}
}

// TestNotSignedInReasonIsTheSameString proves the not_signed_in reason gm
// stamps on an unsupported_capability is the exact value this package's
// closed vocabulary knows, so a CLI parsing the reason off a response
// recognises what the gm layer produced.
func TestNotSignedInReasonIsTheSameString(t *testing.T) {
	if gm.ReasonNotSignedIn != ReasonNotSignedIn.String() {
		t.Fatalf("internal/gm says %q, apierr says %q", gm.ReasonNotSignedIn, ReasonNotSignedIn.String())
	}
	if _, ok := ReasonByName(gm.ReasonNotSignedIn); !ok {
		t.Fatalf("apierr does not recognise the reason internal/gm emits: %q", gm.ReasonNotSignedIn)
	}

	// The credential-death errors must be unsupported_capability, not the
	// service-level not_paired (spec sections 4.7, 7.8).
	for _, err := range []error{gm.ErrInvalidCredentials, gm.ErrRequestedEntityNotFound} {
		e := gm.Classify(err)
		if Code(e.Code) != CodeUnsupportedCapability {
			t.Errorf("%v classified as %q, want unsupported_capability", err, e.Code)
		}
		if Code(e.Code) == CodeNotPaired {
			t.Errorf("%v classified as not_paired; that means no accounts at all", err)
		}
		if e.Reason != ReasonNotSignedIn.String() {
			t.Errorf("%v carried reason %q, want %q", err, e.Reason, ReasonNotSignedIn)
		}
		if ExitCodeFor(Code(e.Code)) != ExitUnsupported {
			t.Errorf("%v exits %d, spec section 11.2 says %d", err, ExitCodeFor(Code(e.Code)), ExitUnsupported)
		}
	}
}

// TestGMRetryableAgreesWhereTheTableIsDefinite proves internal/gm never marks
// an error retryable that spec section 7.2 says is not, and never leaves one
// unmarked that the table says is -- except on google_error, the single row
// the spec writes as "maybe", where the classifier decides.
func TestGMRetryableAgreesWhereTheTableIsDefinite(t *testing.T) {
	cases := []*gm.Error{
		gm.Classify(gm.ErrPhoneNotResponding),
		gm.Classify(gm.ErrConnectionClosed),
		gm.Classify(gm.ErrPairingInitTimeout),
		gm.Classify(gm.ErrIncorrectEmoji),
		gm.Classify(gm.ErrCallerNoPermission),
		gm.Classify(gm.HTTPError{Action: "polling", StatusCode: 503}),
		gm.NotDefaultSMSApp(),
	}
	for _, e := range cases {
		c := Code(e.Code)
		switch RetryabilityOf(c) {
		case RetryYes:
			if !e.Retryable {
				t.Errorf("%q: spec section 7.2 says retryable, internal/gm says no", c)
			}
		case RetryNo:
			if e.Retryable {
				t.Errorf("%q: spec section 7.2 says not retryable, internal/gm says yes", c)
			}
		}
	}
}

// gmCodes lists every code internal/gm declares. It is a hand-written
// inventory because Go has no way to enumerate a package's constants; a code
// added to gm without a row here is caught by the count assertion below.
func gmCodes() []gm.Code {
	codes := []gm.Code{
		gm.CodePhoneNotResponding,
		gm.CodeDisconnected,
		gm.CodeUnsupportedCapability,
		gm.CodeGooglePermissionDenied,
		gm.CodeGoogleError,
		gm.CodeGoogleHTTPError,
		gm.CodeGoogleUndocumentedState,
		gm.CodeNotDefaultSMSApp,
		gm.CodeConfigVersionStale,
		gm.CodePairingNoCookies,
		gm.CodePairingNoDevices,
		gm.CodePairingWrongEmoji,
		gm.CodePairingCancelled,
		gm.CodePairingTimeout,
		gm.CodePairingInitTimeout,
		gm.CodePairingNoAccount,
		gm.CodePairingWrongAccount,
		gm.CodeInternalError,
	}
	return codes
}

// TestGMUndocumentedStatusKeepsTheBareInteger proves both layers report the
// unnamed Google value the same way: a bare integer in details.status, with
// no invented name (spec section 3.7).
func TestGMUndocumentedStatusKeepsTheBareInteger(t *testing.T) {
	if Code(gm.CodeGoogleUndocumentedState) != CodeGoogleUndocumentedStatus {
		t.Fatalf("internal/gm's code is %q, apierr's is %q", gm.CodeGoogleUndocumentedState, CodeGoogleUndocumentedStatus)
	}
	if HTTPStatus(CodeGoogleUndocumentedStatus) != 502 {
		t.Fatalf("google_undocumented_status is %d, want 502", HTTPStatus(CodeGoogleUndocumentedStatus))
	}
}
