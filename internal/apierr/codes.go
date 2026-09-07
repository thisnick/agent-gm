// Package apierr is the one place the Agent GM REST error taxonomy lives.
//
// The REST layer, the CLI and (from Slice 3) MCP all render errors through
// this package, so a code's HTTP status, its retryable flag, the exit code it
// produces and the words of an irreversible effect cannot drift between
// surfaces. It is a vocabulary, not a router: it holds no handler logic and
// imports net/http only for the status constants.
//
// The layer below is internal/gm, which classifies library errors into the
// same code vocabulary (spec section 3.5). This package agrees with it rather
// than duplicating it; gm_agreement_test.go asserts that agreement.
//
// Spec sections 4.1, 4.7, 7.1, 7.2, 7.3, 7.7, 7.8, 11.2.
package apierr

import (
	"net/http"
	"sort"
)

// Code is an Agent GM error code from spec section 7.2.
type Code string

// The closed vocabulary of spec section 7.2's table. Every code the REST
// surface can produce is here; a code outside this list does not exist.
const (
	CodeInvalidRequest           Code = "invalid_request"
	CodeInvalidToken             Code = "invalid_token"
	CodeInsufficientScope        Code = "insufficient_scope"
	CodeNotFound                 Code = "not_found"
	CodeIdempotencyConflict      Code = "idempotency_conflict"
	CodeNotPaired                Code = "not_paired"
	CodePairingNoCookies         Code = "pairing_no_cookies"
	CodePairingNoDevices         Code = "pairing_no_devices"
	CodePairingWrongEmoji        Code = "pairing_wrong_emoji"
	CodePairingCancelled         Code = "pairing_cancelled"
	CodePairingTimeout           Code = "pairing_timeout"
	CodePairingInitTimeout       Code = "pairing_init_timeout"
	CodePairingWrongAccount      Code = "pairing_wrong_account"
	CodePairingNoAccount         Code = "pairing_no_account"
	CodeUnsupportedCapability    Code = "unsupported_capability"
	CodePayloadTooLarge          Code = "payload_too_large"
	CodeMediaUnsupportedType     Code = "media_unsupported_type"
	CodeRateLimited              Code = "rate_limited"
	CodeInternalError            Code = "internal_error"
	CodeNotDefaultSMSApp         Code = "not_default_sms_app"
	CodeGoogleUndocumentedStatus Code = "google_undocumented_status"
	CodeGoogleError              Code = "google_error"
	CodeGoogleHTTPError          Code = "google_http_error"
	CodeGooglePermissionDenied   Code = "google_permission_denied"
	CodeDisconnected             Code = "disconnected"
	CodePhoneNotResponding       Code = "phone_not_responding"
)

// Retryability is the "Retryable" column of spec section 7.2. Two of its
// three values are facts about the code; the third belongs to the single row
// the spec writes as "maybe".
type Retryability int

const (
	// RetryNo: the envelope's retryable is false for this code, always.
	RetryNo Retryability = iota
	// RetryYes: the envelope's retryable is true for this code, always.
	RetryYes
	// RetryMaybe: google_error alone. The constructor decides, because only
	// the Google status behind it knows whether a retry can help.
	RetryMaybe
)

// spec is one code's row of the section 7.2 table, held as data so that no
// switch statement anywhere can disagree with it.
type spec struct {
	status int
	retry  Retryability
}

// table is spec section 7.2 verbatim. codes_test.go re-states every row as a
// literal and fails if this map and that restatement diverge in either
// direction, so a drift is caught whichever side moved.
var table = map[Code]spec{
	CodeInvalidRequest:           {http.StatusBadRequest, RetryNo},
	CodeInvalidToken:             {http.StatusUnauthorized, RetryNo},
	CodeInsufficientScope:        {http.StatusForbidden, RetryNo},
	CodeNotFound:                 {http.StatusNotFound, RetryNo},
	CodeIdempotencyConflict:      {http.StatusConflict, RetryNo},
	CodeNotPaired:                {http.StatusConflict, RetryNo},
	CodePairingNoCookies:         {http.StatusConflict, RetryNo},
	CodePairingNoDevices:         {http.StatusConflict, RetryNo},
	CodePairingWrongEmoji:        {http.StatusConflict, RetryNo},
	CodePairingCancelled:         {http.StatusConflict, RetryNo},
	CodePairingTimeout:           {http.StatusConflict, RetryNo},
	CodePairingInitTimeout:       {http.StatusConflict, RetryYes},
	CodePairingWrongAccount:      {http.StatusConflict, RetryNo},
	CodePairingNoAccount:         {http.StatusConflict, RetryNo},
	CodeUnsupportedCapability:    {http.StatusConflict, RetryNo},
	CodePayloadTooLarge:          {http.StatusRequestEntityTooLarge, RetryNo},
	CodeMediaUnsupportedType:     {http.StatusUnsupportedMediaType, RetryNo},
	CodeRateLimited:              {http.StatusTooManyRequests, RetryYes},
	CodeInternalError:            {http.StatusInternalServerError, RetryYes},
	CodeNotDefaultSMSApp:         {http.StatusBadGateway, RetryNo},
	CodeGoogleUndocumentedStatus: {http.StatusBadGateway, RetryNo},
	CodeGoogleError:              {http.StatusBadGateway, RetryMaybe},
	CodeGoogleHTTPError:          {http.StatusBadGateway, RetryYes},
	CodeGooglePermissionDenied:   {http.StatusBadGateway, RetryNo},
	CodeDisconnected:             {http.StatusServiceUnavailable, RetryYes},
	CodePhoneNotResponding:       {http.StatusGatewayTimeout, RetryYes},
}

// Known reports whether c is one of spec section 7.2's codes.
func Known(c Code) bool {
	_, ok := table[c]
	return ok
}

// Codes returns every code of spec section 7.2, sorted, so a caller that has
// to enumerate the vocabulary -- a test, the CLI's exit-code table, an MCP
// tool description -- reads it from here rather than writing its own list.
func Codes() []Code {
	out := make([]Code, 0, len(table))
	for c := range table {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HTTPStatus is the status spec section 7.2 assigns to c. An unknown code is
// 500: a code that is not in the vocabulary is a bug in Agent GM, not a
// caller error, and answering 200 for it would be worse than answering 500.
func HTTPStatus(c Code) int {
	if s, ok := table[c]; ok {
		return s.status
	}
	return http.StatusInternalServerError
}

// RetryabilityOf is the "Retryable" column for c.
func RetryabilityOf(c Code) Retryability {
	if s, ok := table[c]; ok {
		return s.retry
	}
	return RetryNo
}
