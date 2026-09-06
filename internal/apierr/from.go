package apierr

import (
	"errors"
	"net/http"
	"strings"
)

// From turns any error into an *Error.
//
// It is the single funnel every handler's return value passes through, so
// that an error nobody classified becomes `internal_error` -- a bug, reported
// as one -- rather than leaking a Go error string to a caller. Section 7.2's
// vocabulary is closed, and this is what keeps it closed at the edge.
//
// An *Error passes through unchanged. Anything else is internal_error with a
// fixed message: the underlying text may name a file path, a SQL statement or
// a Google identifier, none of which belong on a public surface.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return already
	}
	e := New(CodeInternalError, "an internal error; the request ID identifies it in the logs")
	e.Err = err
	return e
}

// MethodNotAllowed is a path that exists for another method.
//
// It is deliberately NOT `not_found`. A caller who typed GET where the route
// takes POST has a fixable mistake, and answering "no such thing" sends them
// looking for a missing object instead. The allowed methods travel in the
// Allow header and in details, so the answer says what to do.
//
// It renders as `invalid_request`, because 405 has no row of its own in
// section 7.2's table and the caller's request was indeed malformed for this
// address; the HTTP status is what carries the distinction.
func MethodNotAllowed(method, allow string) *Error {
	e := withDetails(CodeInvalidRequest,
		"the method "+method+" is not allowed here; this path takes "+allow,
		map[string]any{"method": method, "allow": strings.Split(allow, ", ")})
	e.HTTPStatusOverride = http.StatusMethodNotAllowed
	return e
}
