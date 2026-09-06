package gm

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is an Agent GM error code from spec section 7.2. The gm package
// produces the code and the HTTP status the REST layer will later render, so
// the mapping of spec section 3.5 is testable with no REST surface at all
// (spec section 16, Slice 1 acceptance test 5).
type Code string

const (
	CodePhoneNotResponding      Code = "phone_not_responding"
	CodeDisconnected            Code = "disconnected"
	CodeUnsupportedCapability   Code = "unsupported_capability"
	CodeGooglePermissionDenied  Code = "google_permission_denied"
	CodeGoogleError             Code = "google_error"
	CodeGoogleHTTPError         Code = "google_http_error"
	CodeGoogleUndocumentedState Code = "google_undocumented_status"
	CodeNotDefaultSMSApp        Code = "not_default_sms_app"
	CodeConfigVersionStale      Code = "config_version_stale"
	CodePairingNoCookies        Code = "pairing_no_cookies"
	CodePairingNoDevices        Code = "pairing_no_devices"
	CodePairingWrongEmoji       Code = "pairing_wrong_emoji"
	CodePairingCancelled        Code = "pairing_cancelled"
	CodePairingTimeout          Code = "pairing_timeout"
	CodePairingInitTimeout      Code = "pairing_init_timeout"
	CodePairingNoAccount        Code = "pairing_no_account"
	CodePairingWrongAccount     Code = "pairing_wrong_account"
	CodeInternalError           Code = "internal_error"
)

// ReasonNotSignedIn is the details.reason that accompanies
// unsupported_capability when an account's credentials are dead. It is an
// account-level condition and is deliberately not `not_paired`, which means
// the server holds no accounts at all (spec sections 3.5, 7.8).
const ReasonNotSignedIn = "not_signed_in"

// Error is the gm layer's classified error.
type Error struct {
	Code Code
	// HTTPStatus is the status spec section 7.2 assigns to Code.
	HTTPStatus int
	Message    string
	// Reason populates details.reason. It is set only for
	// unsupported_capability.
	Reason string
	// SignsOutAccount is true when this error means the account's credentials
	// are dead and its state must become signed_out (spec section 4.7).
	SignsOutAccount bool
	// Retryable marks an error the caller may reasonably retry.
	Retryable bool
	// KeepsOperationPending is true only for phone_not_responding: the server
	// accepted the request and the phone may still act on it, so the
	// operation stays pending and must never be reported as failed (D5).
	KeepsOperationPending bool
	// Details carries the extra fields spec section 3.5 names, such as
	// multiple_devices on pairing_init_timeout, or the numeric type and
	// message on google_error. details.google_type and details.status are
	// the ONLY raw Google integers permitted here: every other raw Google
	// value is admin-only (spec sections 4.1, 7.2).
	Details map[string]any
	// GoogleStatusRaw is the numeric Google status behind this error, or 0.
	// It is written to operations.google_status_raw and served only on
	// GET /v1/admin/diagnostics -- never in a public error envelope, which
	// is why it is a field of its own and not an entry in Details.
	GoogleStatusRaw int32
	// Err is the underlying library error.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets errors.Is compare two classified errors by code.
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return other.Code == e.Code
}

func newError(code Code, status int, msg string) *Error {
	return &Error{Code: code, HTTPStatus: status, Message: msg}
}

// notSignedIn builds the unsupported_capability / not_signed_in error that
// both credential-death library errors produce. It is one constructor on
// purpose: an implementer must not be able to collapse it into not_paired.
func notSignedIn(msg string) *Error {
	e := newError(CodeUnsupportedCapability, http.StatusConflict, msg)
	e.Reason = ReasonNotSignedIn
	e.SignsOutAccount = true
	return e
}

// Sentinel errors the fake raises and the adapter classifies. They mirror the
// library's own sentinels one for one so that a table test can enumerate
// spec section 3.5 without a phone.
var (
	// ErrPhoneNotResponding mirrors libgm.ErrPhoneNotResponding
	// (session_handler.go:20-32). The server already accepted the request.
	ErrPhoneNotResponding = errors.New("phone did not respond to request")
	// ErrConnectionClosed mirrors libgm.ErrConnectionClosed.
	ErrConnectionClosed = errors.New("client disconnected before response was received")
	// ErrInvalidCredentials mirrors events.ErrInvalidCredentials (tachyon 16).
	ErrInvalidCredentials = errors.New("invalid authentication credentials")
	// ErrRequestedEntityNotFound mirrors events.ErrRequestedEntityNotFound
	// (tachyon 5).
	ErrRequestedEntityNotFound = errors.New("requested entity was not found")
	// ErrCallerNoPermission mirrors events.ErrCallerNoPermission (tachyon 7).
	ErrCallerNoPermission = errors.New("the caller does not have permission")
	// ErrNoCookies mirrors libgm.ErrNoCookies.
	ErrNoCookies = errors.New("gaia pairing requires cookies")
	// ErrNoDevicesFound mirrors libgm.ErrNoDevicesFound.
	ErrNoDevicesFound = errors.New("no devices found for gaia pairing")
	// ErrIncorrectEmoji mirrors libgm.ErrIncorrectEmoji.
	ErrIncorrectEmoji = errors.New("user chose incorrect emoji on phone")
	// ErrPairingCancelled mirrors libgm.ErrPairingCancelled.
	ErrPairingCancelled = errors.New("user cancelled pairing on phone")
	// ErrPairingTimeout mirrors libgm.ErrPairingTimeout.
	ErrPairingTimeout = errors.New("pairing timed out")
	// ErrPairingInitTimeout mirrors libgm.ErrPairingInitTimeout.
	ErrPairingInitTimeout = errors.New("client init timed out")
	// ErrHadMultipleDevices mirrors libgm.ErrHadMultipleDevices. It is only
	// ever wrapped inside ErrPairingInitTimeout and never has a code of its
	// own: Google never reports "multiple devices" as an error, the library
	// picks one (spec sections 3.2, 3.5).
	ErrHadMultipleDevices = errors.New("had multiple primary-looking devices")
	// ErrWrongAccount is Agent GM's own: a cookie refresh that signs in as a
	// different Google account is refused, changing nothing. This is stricter
	// than upstream on purpose (spec section 3.2).
	ErrWrongAccount = errors.New("cookies belong to a different Google account")
	// ErrNoAccountAddress is Agent GM's own: StartGaiaPairing produced an
	// empty or implausible AuthData.Mobile.SourceID, so no account can be
	// created (spec section 3.2).
	ErrNoAccountAddress = errors.New("pairing produced no Google account address")
)

// RequestError is the gm-level rendering of events.RequestError: any non-OK
// tachyon response that is not one of the three sentinels.
type RequestError struct {
	Type    int64
	Message string
}

func (r RequestError) Error() string {
	return fmt.Sprintf("google request error %d: %s", r.Type, r.Message)
}

// HTTPError is the gm-level rendering of events.HTTPError: a transport-level
// failure carrying the action and the status code.
type HTTPError struct {
	Action     string
	StatusCode int
}

func (h HTTPError) Error() string {
	if h.Action == "" {
		return fmt.Sprintf("unexpected http %d", h.StatusCode)
	}
	return fmt.Sprintf("http %d while %s", h.StatusCode, h.Action)
}

// IsFatalListenError implements the matching rule of spec section 3.4: the
// library makes an HTTP 401 or 403 on the listen request fatal, and renders
// both as "http %d while polling". Matching on the string sends a 403 into
// the retry branch, where it loops forever on dead credentials, so the match
// is on the error value.
func IsFatalListenError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInvalidCredentials) {
		return true
	}
	var he HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden
	}
	return false
}

// Classify maps a library error onto the Agent GM code and status that spec
// section 7.2 will render. Every row of spec section 3.5 is covered; the
// table test in errors_test.go enumerates the list rather than sampling it.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}

	var already *Error
	if errors.As(err, &already) {
		return already
	}

	switch {
	case errors.Is(err, ErrPhoneNotResponding):
		e := newError(CodePhoneNotResponding, http.StatusGatewayTimeout,
			"the phone did not answer within 60s; the server accepted the request and the phone may still deliver it, so the operation stays pending -- do not resend")
		e.Retryable = true
		e.KeepsOperationPending = true
		e.Err = err
		return e

	case errors.Is(err, ErrConnectionClosed):
		e := newError(CodeDisconnected, http.StatusServiceUnavailable,
			"the connection to Google closed while the request was in flight")
		e.Retryable = true
		e.Err = err
		return e

	case errors.Is(err, ErrInvalidCredentials):
		e := notSignedIn("this account's Google credentials are no longer valid; refresh its cookies with `agm pair --refresh-cookies --account <id>`")
		e.Err = err
		return e

	case errors.Is(err, ErrRequestedEntityNotFound):
		e := notSignedIn("the phone no longer knows this pairing; pair this account again")
		e.Err = err
		return e

	case errors.Is(err, ErrCallerNoPermission):
		e := newError(CodeGooglePermissionDenied, http.StatusBadGateway,
			"Google refused the request: the caller does not have permission")
		e.Err = err
		return e

	case errors.Is(err, ErrNoCookies):
		e := newError(CodePairingNoCookies, http.StatusConflict,
			"Google-account pairing requires the seven session cookies; none were supplied")
		e.Err = err
		return e

	case errors.Is(err, ErrNoDevicesFound):
		e := newError(CodePairingNoDevices, http.StatusConflict,
			"this Google account has no phone that can be paired")
		e.Err = err
		return e

	case errors.Is(err, ErrIncorrectEmoji):
		e := newError(CodePairingWrongEmoji, http.StatusConflict,
			"the emoji tapped on the phone did not match the one shown")
		e.Err = err
		return e

	case errors.Is(err, ErrPairingCancelled):
		e := newError(CodePairingCancelled, http.StatusConflict,
			"pairing was dismissed on the phone")
		e.Err = err
		return e

	case errors.Is(err, ErrPairingTimeout):
		e := newError(CodePairingTimeout, http.StatusConflict,
			"the pairing expired before it was confirmed on the phone")
		e.Err = err
		return e

	case errors.Is(err, ErrPairingInitTimeout):
		// ErrHadMultipleDevices is only ever wrapped inside this one, so it
		// is reported as details, never as a code of its own.
		e := newError(CodePairingInitTimeout, http.StatusConflict,
			"the pairing handshake with Google timed out; try again")
		e.Retryable = true
		e.Err = err
		if errors.Is(err, ErrHadMultipleDevices) {
			e.Details = map[string]any{"multiple_devices": true}
		}
		return e

	case errors.Is(err, ErrWrongAccount):
		e := newError(CodePairingWrongAccount, http.StatusConflict,
			"those cookies belong to a different Google account; nothing was changed")
		e.Err = err
		return e

	case errors.Is(err, ErrNoAccountAddress):
		e := newError(CodePairingNoAccount, http.StatusConflict,
			"Google did not return an account address for this pairing; no account was created")
		e.Err = err
		return e
	}

	var re RequestError
	if errors.As(err, &re) {
		e := newError(CodeGoogleError, http.StatusBadGateway,
			"Google returned an error for this request")
		e.Details = map[string]any{"google_type": re.Type, "google_message": re.Message}
		e.Err = err
		return e
	}

	var he HTTPError
	if errors.As(err, &he) {
		e := newError(CodeGoogleHTTPError, http.StatusBadGateway,
			"the request to Google failed at the transport level")
		e.Details = map[string]any{"status": he.StatusCode, "action": he.Action}
		e.Retryable = true
		e.Err = err
		return e
	}

	e := newError(CodeInternalError, http.StatusInternalServerError, "unclassified failure")
	e.Err = err
	return e
}

// UndocumentedResolveStatus builds the google_undocumented_status error for a
// GetOrCreateConversation status the proto has no name for. Agent GM reports
// the bare integer and claims nothing about what it means (spec section 3.7).
func UndocumentedResolveStatus(status ResolveStatus) *Error {
	e := newError(CodeGoogleUndocumentedState, http.StatusBadGateway,
		"Google answered with a conversation status this version of the protocol has no name for")
	e.Details = map[string]any{"status": int32(status)}
	return e
}

// ConfigVersionStale is Agent GM's own diagnosis, not Google's answer: a
// conversation-creating call returned a non-SUCCESS status AND the live
// ConfigVersion differs from the compiled one in year, month or day. The
// version diff is the whole detection rule; no particular status code is
// required or claimed (spec section 3.7, D3).
func ConfigVersionStale(compiled, live ConfigVersion, status ResolveStatus) *Error {
	e := newError(CodeConfigVersionStale, http.StatusBadGateway,
		fmt.Sprintf("Google Messages for web version %s is compiled in but Google is serving %s; "+
			"bumping the pinned mautrix-gmessages commit is the fix", compiled, live))
	e.Details = map[string]any{
		"compiled_config_version": compiled.String(),
		"live_config_version":     live.String(),
		"status":                  int32(status),
	}
	return e
}

// ResolveCreateRCSTwice is a second CREATE_RCS from
// GetOrCreateConversation. The adapter has already done the one documented
// thing about the first (retry once with CreateRCSGroup=true, exactly as
// upstream does), so a second is a refusal Agent GM reports rather than a
// hint it acts on (spec section 3.7).
func ResolveCreateRCSTwice() *Error {
	e := newError(CodeGoogleError, http.StatusBadGateway,
		"Google asked twice for an RCS group to be created; the retry it asks for has already been made")
	e.Details = map[string]any{
		"google_type":    int32(ResolveStatusCreateRCS),
		"google_message": "CREATE_RCS",
	}
	return e
}

// NotDefaultSMSApp is FAILURE_4, which is not retried: upstream renders it as
// the user-facing string "Google Messages is not your default SMS app"
// (connector/errors.go:41-42).
//
// It carries no details: the raw Google status is admin-only (spec sections
// 4.1, 6.5), and `details.google_type` and `details.status` are the only raw
// Google integers permitted on a public surface (spec section 7.2). The value
// is recorded on the operation row as google_status_raw and served on
// GET /v1/admin/diagnostics, which is where a reviewer reads it.
func NotDefaultSMSApp() *Error {
	e := newError(CodeNotDefaultSMSApp, http.StatusBadGateway,
		"Google Messages is not your default SMS app")
	e.GoogleStatusRaw = int32(SendStatusFailure4)
	return e
}

// SendFailure classifies a non-SUCCESS SendMessageResponse status.
//
// The raw status travels on the Error's GoogleStatusRaw field, not in
// Details: it is written to operations.google_status_raw and served only on
// GET /v1/admin/diagnostics. What reaches the public envelope is
// details.google_type, which section 7.2 names as one of exactly two raw
// Google integers a caller may see.
func SendFailure(status SendStatus) *Error {
	if status == SendStatusFailure4 {
		return NotDefaultSMSApp()
	}
	e := newError(CodeGoogleError, http.StatusBadGateway,
		fmt.Sprintf("Google Messages on the phone rejected the message (%s)", status))
	e.Details = map[string]any{
		"google_type":    int32(status),
		"google_message": status.String(),
	}
	e.GoogleStatusRaw = int32(status)
	e.Retryable = status.IsTransient()
	return e
}
