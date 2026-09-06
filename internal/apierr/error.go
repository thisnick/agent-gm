package apierr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Error is one REST error. Its HTTP status and its retryable flag are read
// from spec section 7.2's table rather than stored on the value, so an
// instance cannot carry a status the table disagrees with.
type Error struct {
	// Code is the spec section 7.2 code.
	Code Code
	// Message is the human sentence. It never contains a secret, a cookie or
	// a raw Google identifier (spec section 12.2).
	Message string
	// Details is details{} in the envelope.
	Details map[string]any
	// RetryAfter is set only for rate_limited and drives the Retry-After
	// header. Zero means the header is not sent.
	RetryAfter time.Duration
	// Err is an underlying error, for errors.Is/As and for logs. It is never
	// rendered to a caller.
	Err error
	// HTTPStatusOverride is the status to send instead of the one section
	// 7.2 assigns to Code. It is set in exactly one place, by
	// MethodNotAllowed -- 405 has no row of its own in the table and the
	// code there is still invalid_request -- so the table stays the single
	// authority for every other answer.
	HTTPStatusOverride int

	// retryable is consulted only when the table says RetryMaybe, which is
	// google_error alone.
	retryable bool
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Is compares two API errors by code, so errors.Is(err, apierr.NotFound(""))
// asks "is this a not_found?" without matching on a message.
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return other.Code == e.Code
}

// HTTPStatus is the status spec section 7.2 assigns to this error's code.
func (e *Error) HTTPStatus() int {
	// 405 has no row of its own in section 7.2's table -- the code is still
	// invalid_request -- so the one place a status differs from the table is
	// declared on the error rather than hidden in a switch somewhere.
	if e.HTTPStatusOverride != 0 {
		return e.HTTPStatusOverride
	}
	return HTTPStatus(e.Code)
}

// Retryable is the retryable flag of the envelope. For every code but
// google_error the table decides and the value on the error is ignored, so a
// constructor cannot contradict spec section 7.2.
func (e *Error) Retryable() bool {
	switch RetryabilityOf(e.Code) {
	case RetryYes:
		return true
	case RetryNo:
		return false
	default: // RetryMaybe: google_error
		return e.retryable
	}
}

// ExitCode is the CLI exit code spec section 11.2 assigns to this error.
func (e *Error) ExitCode() int { return ExitCodeFor(e.Code) }

// RetryAfterHeader is the Retry-After header value in seconds, or "" when
// there is none. It rounds up and never returns "0", which a client would
// read as "retry immediately" and hammer the bucket that just refused it.
func (e *Error) RetryAfterHeader() string {
	if e.RetryAfter <= 0 {
		return ""
	}
	secs := int64(math.Ceil(e.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// New builds an error with no details.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf is New with formatting.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// withDetails is the one place a details map is attached, so every
// constructor's details are built the same way.
func withDetails(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// ---------------------------------------------------------------------------
// Strict parameter and body rejection (spec section 7.1)
// ---------------------------------------------------------------------------

// UnknownQueryParameter refuses an unknown query parameter, naming it in
// details.parameter. Nothing is allowlisted, including a cache-busting `_`:
// a misspelled filter must not return a full unfiltered list, which looks
// exactly like a correct answer.
func UnknownQueryParameter(name string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("unknown query parameter %q; this route rejects every parameter it does not define, so the request had no effect", name),
		map[string]any{"parameter": name})
}

// UnknownBodyField refuses an unknown JSON body field, naming it in
// details.field.
func UnknownBodyField(name string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("unknown body field %q; this route rejects every field it does not define, so the request had no effect", name),
		map[string]any{"field": name})
}

// MalformedBody is a body that is not the JSON this route expects, for a
// reason other than an unknown field.
func MalformedBody(reason string) *Error {
	return New(CodeInvalidRequest, "the request body could not be read as JSON: "+reason)
}

// WrongTypeForField is a body field with the right name and the wrong JSON
// type.
func WrongTypeForField(field, want string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("body field %q must be %s", field, want),
		map[string]any{"field": field})
}

// MissingParameter is a required parameter that was not supplied.
func MissingParameter(name string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("%s is required", name),
		map[string]any{"parameter": name})
}

// ---------------------------------------------------------------------------
// Identifiers (spec sections 4.1, 7.3)
// ---------------------------------------------------------------------------

// WrongIDPrefix is an ID with the wrong typed prefix for the parameter it was
// given to. It is invalid_request naming the parameter and the expected
// prefix, and it is NEVER not_found: an ID of the wrong kind is a caller
// mistake, whereas not_found would say the object is gone and send the caller
// looking for a deleted thread (spec section 4.1). A raw Google ID lands here
// too, since it carries no Agent GM prefix.
//
// The offending value is deliberately not echoed into details: it may be a
// raw Google identifier, which is admin-only on a public surface.
func WrongIDPrefix(parameter, expectedPrefix string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("%s must be an Agent GM ID beginning with %q", parameter, expectedPrefix),
		map[string]any{"parameter": parameter, "expected_prefix": expectedPrefix})
}

// CheckIDPrefix is the one implementation of the section 4.1 prefix rule.
// Every route calls this rather than writing its own strings.HasPrefix, so no
// route can answer not_found for a wrong-prefix ID. It returns nil when value
// is well formed.
func CheckIDPrefix(value, parameter, expectedPrefix string) *Error {
	if value == "" {
		return MissingParameter(parameter)
	}
	if len(value) <= len(expectedPrefix) || value[:len(expectedPrefix)] != expectedPrefix {
		return WrongIDPrefix(parameter, expectedPrefix)
	}
	return nil
}

// IDFromAnotherAccount is a conv_ or msg_ ID that exists but belongs to a
// different account than the account_id the caller named. It is
// invalid_request naming BOTH -- never not_found, which would suggest the
// thread is gone (spec section 7.3).
func IDFromAnotherAccount(parameter, id, accountID string) *Error {
	return withDetails(CodeInvalidRequest,
		fmt.Sprintf("%s %s belongs to a different account than account_id %s; the thread still exists, so drop account_id or name the owning account", parameter, id, accountID),
		map[string]any{"parameter": parameter, "id": id, "account_id": accountID})
}

// AccountCandidate is one row of details.accounts on an ambiguous-account
// error: enough for the caller to retry without a second round trip.
type AccountCandidate struct {
	ID            string `json:"id"`
	GoogleAccount string `json:"google_account"`
	State         string `json:"state"`
}

// AmbiguousAccount is a write that omitted account_id while more than one
// account exists. It lists the candidates so the answer can be acted on
// rather than merely understood (spec section 7.3).
func AmbiguousAccount(candidates []AccountCandidate) *Error {
	list := make([]AccountCandidate, len(candidates))
	copy(list, candidates)
	return withDetails(CodeInvalidRequest,
		"more than one account exists, so a write must name one in account_id; the candidates are in details.accounts",
		map[string]any{"field": "account_id", "accounts": list})
}

// ---------------------------------------------------------------------------
// unsupported_capability (spec sections 7.7, 7.8)
// ---------------------------------------------------------------------------

// Reason is a details.reason value from spec section 7.8's CLOSED vocabulary.
// It is a struct with an unexported field rather than a string type, so no
// package outside this one can invent a reason: the vocabulary is closed by
// the compiler, not by a review.
type Reason struct{ name string }

// String is the wire value of the reason.
func (r Reason) String() string { return r.name }

// MarshalJSON writes the bare reason string.
func (r Reason) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(r.name)), nil
}

// Spec section 7.8's closed vocabulary, complete.
var (
	// ReasonNotSignedIn: the account this touches is signed_out, error,
	// parked or account_changed. Its history stays readable; only writes are
	// refused. Distinct from the service-level not_paired, which means there
	// are no accounts at all.
	ReasonNotSignedIn = Reason{"not_signed_in"}
	// ReasonConversationReadOnly: Conversation.ReadOnly is set.
	ReasonConversationReadOnly = Reason{"conversation_read_only"}
	// ReasonConversationDeleted: delete-for-me has been applied locally.
	ReasonConversationDeleted = Reason{"conversation_deleted"}
	// ReasonNotMyMessage: deleting a message the owner did not send.
	ReasonNotMyMessage = Reason{"not_my_message"}
	// ReasonNotMyReaction: removing somebody else's reaction.
	ReasonNotMyReaction = Reason{"not_my_reaction"}
	// ReasonReplyNotSupported: reply_to_message_id on an SMS/MMS
	// conversation; replies are RCS-only.
	ReasonReplyNotSupported = Reason{"reply_not_supported"}
	// ReasonRCSNotAvailable: force_rcs where capabilities.force_rcs is false.
	ReasonRCSNotAvailable = Reason{"rcs_not_available"}
	// ReasonMediaPending: the attachment's bytes are not downloaded yet.
	ReasonMediaPending = Reason{"media_pending"}
)

// Reasons returns spec section 7.8's vocabulary in the spec's order.
func Reasons() []Reason {
	return []Reason{
		ReasonNotSignedIn,
		ReasonConversationReadOnly,
		ReasonConversationDeleted,
		ReasonNotMyMessage,
		ReasonNotMyReaction,
		ReasonReplyNotSupported,
		ReasonRCSNotAvailable,
		ReasonMediaPending,
	}
}

// ReasonByName parses a wire value back into a Reason. It is how the CLI and
// MCP read a reason off a response without being able to mint a new one.
func ReasonByName(name string) (Reason, bool) {
	for _, r := range Reasons() {
		if r.name == name {
			return r, true
		}
	}
	return Reason{}, false
}

// UnsupportedCapability refuses an action that cannot apply here. It carries
// the object ID, the action, the capability and the capability's current
// value alongside details.reason, so the caller learns why rather than only
// that (spec sections 7.7, 7.8).
//
// It is emitted BEFORE any operation row exists, so a refused action never
// leaves a record that looks like an attempt.
//
// The zero Reason is a programming error: it means a caller assembled a
// Reason{} instead of using one of this package's values, which the closed
// vocabulary exists to prevent.
//
// current is the capability's value as a bool or a short string. It is never
// a raw Google integer: details.google_type and details.status are the only
// two of those on a public surface (spec section 7.2).
func UnsupportedCapability(reason Reason, objectID, action, capability string, current any) *Error {
	if reason.name == "" {
		panic("apierr: UnsupportedCapability with the zero Reason; use one of the section 7.8 values")
	}
	return withDetails(CodeUnsupportedCapability,
		fmt.Sprintf("cannot %s %s: %s (%s is %v)", action, objectID, reason.name, capability, current),
		map[string]any{
			"reason":           reason,
			"object_id":        objectID,
			"action":           action,
			"capability":       capability,
			"capability_value": current,
		})
}

// ---------------------------------------------------------------------------
// The rest of the taxonomy
// ---------------------------------------------------------------------------

// IdempotencyConflict is the same idempotency key presented with a different
// body (spec section 6.3). It is the caller's mistake, which is why spec
// section 11.2 gives it exit 2 rather than a server-failure code.
func IdempotencyConflict(key string) *Error {
	return withDetails(CodeIdempotencyConflict,
		fmt.Sprintf("idempotency key %q was already used with a different request body; nothing was done", key),
		map[string]any{"idempotency_key": key})
}

// RateLimited carries the Retry-After the caller must honour (spec sections
// 7.2, 12.3).
func RateLimited(retryAfter time.Duration) *Error {
	e := New(CodeRateLimited, "too many requests; retry after the Retry-After header says")
	e.RetryAfter = retryAfter
	return e
}

// NotFound is a missing object. Its message is byte-identical whether the
// object never existed or the caller may not see it, so the answer leaks
// nothing about another account's data (spec section 7.2).
func NotFound(what string) *Error {
	return New(CodeNotFound, "No such "+what+".")
}

// InvalidToken is an absent, expired, unknown or wrong-audience bearer.
func InvalidToken(detail string) *Error {
	return New(CodeInvalidToken, "the bearer token was rejected: "+detail)
}

// InsufficientScope is a valid token with the wrong scope. The scope the
// route requires is named so the caller can ask for it.
func InsufficientScope(required string) *Error {
	return withDetails(CodeInsufficientScope,
		fmt.Sprintf("this route requires the %q scope", required),
		map[string]any{"scope": required})
}

// NotPaired means there is no Google Messages session at ALL. It is never
// used for a paired-but-unusable account, which is unsupported_capability
// with not_signed_in (spec sections 7.2, 7.8).
func NotPaired() *Error {
	return New(CodeNotPaired, "no Google account is paired; run `agm pair` first")
}

// PayloadTooLarge is a body over MaxBodyBytes or media over
// media.upload_max_bytes. The limit is in the message rather than in details
// because details on a public surface is reserved for values a caller acts
// on, and a raw integer there is spoken for by spec section 7.2.
func PayloadTooLarge(what string, limitBytes int64) *Error {
	return Newf(CodePayloadTooLarge, "%s is larger than the %d byte limit", what, limitBytes)
}

// MediaUnsupportedType is a mime type absent from libgm.MimeToMediaType.
func MediaUnsupportedType(mime string) *Error {
	return withDetails(CodeMediaUnsupportedType,
		fmt.Sprintf("Google Messages has no media type for %q", mime),
		map[string]any{"mime_type": mime})
}

// Internal is a bug in Agent GM. Its message never carries the underlying
// error; that goes to the log with the request ID.
func Internal(err error) *Error {
	e := New(CodeInternalError, "an internal error occurred; the request ID identifies it in the server log")
	e.Err = err
	return e
}

// NotDefaultSMSApp is SendMessageResponse_FAILURE_4 and is never retried.
func NotDefaultSMSApp() *Error {
	return New(CodeNotDefaultSMSApp, "Google Messages is not the default SMS app on the paired phone")
}

// ConfigVersionStale is Agent GM's own diagnosis, not Google's answer: a
// conversation-creating call failed AND the compiled and live ConfigVersion
// differ. It names both versions and says the fix is a pin bump, because
// without that sentence the owner has no way to act on it (spec sections
// 3.7, 7.2).
func ConfigVersionStale(compiled, live string) *Error {
	return withDetails(CodeConfigVersionStale,
		fmt.Sprintf("Google Messages for web version %s is compiled in but Google is serving %s; "+
			"bumping the pinned mautrix-gmessages commit is the fix", compiled, live),
		map[string]any{"compiled_config_version": compiled, "live_config_version": live})
}

// GoogleUndocumentedStatus is a Google enum value the pinned proto has no
// name for. details.status is the BARE integer and no meaning is claimed: an
// invented name would be a guess an agent could act on (spec section 3.7).
func GoogleUndocumentedStatus(status int64) *Error {
	return withDetails(CodeGoogleUndocumentedStatus,
		"Google answered with a status this version of the protocol has no name for; the bare value is in details.status",
		map[string]any{"status": status})
}

// GoogleError is a tachyon error. details.google_type and
// details.google_message are diagnostic values with no Agent GM meaning; they
// exist because an owner reading this needs something to search for.
//
// retryable is a parameter because spec section 7.2 writes this row's
// retryable column as "maybe": only the Google status behind it knows.
func GoogleError(googleType int64, googleMessage string, retryable bool) *Error {
	e := withDetails(CodeGoogleError,
		"Google returned an error for this request",
		map[string]any{"google_type": googleType, "google_message": googleMessage})
	e.retryable = retryable
	return e
}

// GoogleHTTPError is a transport-level failure. details.status is the HTTP
// status Google answered with.
func GoogleHTTPError(status int64) *Error {
	return withDetails(CodeGoogleHTTPError,
		"the request to Google failed at the transport level",
		map[string]any{"status": status})
}

// GooglePermissionDenied is ErrCallerNoPermission.
func GooglePermissionDenied() *Error {
	return New(CodeGooglePermissionDenied, "Google refused the request: the caller does not have permission")
}

// Disconnected is ErrConnectionClosed: the long poll is down.
func Disconnected() *Error {
	return New(CodeDisconnected, "the connection to Google is down; retry shortly")
}

// PhoneNotResponding is ErrPhoneNotResponding. The operation is PENDING, not
// failed, and the message says so, because a caller that resends here sends
// the message twice (spec section 7.2, D5).
func PhoneNotResponding() *Error {
	return New(CodePhoneNotResponding,
		"the phone did not answer in time; the server accepted the request and the operation is still pending -- do not resend it")
}
