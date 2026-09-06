package apierr

import (
	"testing"
)

// specRow restates one row of spec section 7.2's table as literals. It is
// written out by hand, from the spec, so that this test and codes.go are two
// independent statements of the same table rather than one statement read
// twice. A drift in either direction fails.
type specRow struct {
	code   string
	status int
	retry  Retryability
}

// specTable is spec section 7.2, transcribed. The HTTP column is a literal
// integer rather than an http.Status constant, so a wrong constant cannot
// hide behind a familiar name.
var specTable = []specRow{
	{"invalid_request", 400, RetryNo},
	{"invalid_token", 401, RetryNo},
	{"insufficient_scope", 403, RetryNo},
	{"not_found", 404, RetryNo},
	{"idempotency_conflict", 409, RetryNo},
	{"not_paired", 409, RetryNo},
	{"pairing_no_cookies", 409, RetryNo},
	{"pairing_no_devices", 409, RetryNo},
	{"pairing_wrong_emoji", 409, RetryNo},
	{"pairing_cancelled", 409, RetryNo},
	{"pairing_timeout", 409, RetryNo},
	{"pairing_init_timeout", 409, RetryYes},
	{"pairing_wrong_account", 409, RetryNo},
	{"unsupported_capability", 409, RetryNo},
	{"payload_too_large", 413, RetryNo},
	{"media_unsupported_type", 415, RetryNo},
	{"rate_limited", 429, RetryYes},
	{"internal_error", 500, RetryYes},
	{"not_default_sms_app", 502, RetryNo},
	{"config_version_stale", 502, RetryNo},
	{"google_undocumented_status", 502, RetryNo},
	{"google_error", 502, RetryMaybe},
	{"google_http_error", 502, RetryYes},
	{"google_permission_denied", 502, RetryNo},
	{"disconnected", 503, RetryYes},
	{"phone_not_responding", 504, RetryYes},
}

// TestCodeTableMatchesSpec7_2 proves each code's HTTP status and retryability
// equal the spec's literals.
func TestCodeTableMatchesSpec7_2(t *testing.T) {
	for _, row := range specTable {
		t.Run(row.code, func(t *testing.T) {
			c := Code(row.code)
			if !Known(c) {
				t.Fatalf("spec section 7.2 names %q but the package does not define it", row.code)
			}
			if got := HTTPStatus(c); got != row.status {
				t.Errorf("HTTPStatus(%q) = %d, spec section 7.2 says %d", row.code, got, row.status)
			}
			if got := RetryabilityOf(c); got != row.retry {
				t.Errorf("RetryabilityOf(%q) = %v, spec section 7.2 says %v", row.code, got, row.retry)
			}
		})
	}
}

// TestCodeTableIsExhaustiveBothWays proves the vocabulary and spec section
// 7.2's table are the same set: a code the spec names and the package lacks
// fails, and a code the package invents and the spec does not name fails too.
func TestCodeTableIsExhaustiveBothWays(t *testing.T) {
	inSpec := make(map[Code]bool, len(specTable))
	for _, row := range specTable {
		if inSpec[Code(row.code)] {
			t.Fatalf("the transcribed spec table lists %q twice", row.code)
		}
		inSpec[Code(row.code)] = true
	}

	for _, c := range Codes() {
		if !inSpec[c] {
			t.Errorf("the package defines %q, which spec section 7.2 does not name", c)
		}
	}
	for c := range inSpec {
		if !Known(c) {
			t.Errorf("spec section 7.2 names %q, which the package does not define", c)
		}
	}
	if len(Codes()) != len(specTable) {
		t.Errorf("Codes() has %d entries, spec section 7.2 has %d", len(Codes()), len(specTable))
	}
}

// TestRetryableFlagComesFromTheTable proves the envelope's retryable flag is
// read from the table and not from the value, so no constructor can answer a
// flag that contradicts spec section 7.2. google_error is the single row the
// spec writes as "maybe" and therefore the single code the constructor
// decides.
func TestRetryableFlagComesFromTheTable(t *testing.T) {
	for _, row := range specTable {
		c := Code(row.code)
		// Build an error that lies: retryable set to the opposite of the
		// table wherever the table is definite.
		e := &Error{Code: c, Message: "x", retryable: row.retry != RetryYes}
		switch row.retry {
		case RetryYes:
			if !e.Retryable() {
				t.Errorf("%q: table says retryable, envelope said false", row.code)
			}
		case RetryNo:
			if e.Retryable() {
				t.Errorf("%q: table says not retryable, envelope said true", row.code)
			}
		case RetryMaybe:
			if !e.Retryable() {
				t.Errorf("%q: the maybe row must honour the constructor", row.code)
			}
			if GoogleError(5, "m", false).Retryable() {
				t.Errorf("google_error built as not-retryable answered true")
			}
			if !GoogleError(5, "m", true).Retryable() {
				t.Errorf("google_error built as retryable answered false")
			}
		}
	}
}

// TestUnknownCodeIsAServerFailure proves a code outside the vocabulary is
// answered as a server failure rather than as a success or a caller error.
func TestUnknownCodeIsAServerFailure(t *testing.T) {
	const bogus Code = "teapot_overflow"
	if Known(bogus) {
		t.Fatal("the bogus code is somehow known")
	}
	if got := HTTPStatus(bogus); got != 500 {
		t.Errorf("HTTPStatus(unknown) = %d, want 500", got)
	}
	if got := ExitCodeFor(bogus); got != ExitServerFailure {
		t.Errorf("ExitCodeFor(unknown) = %d, want %d", got, ExitServerFailure)
	}
}
