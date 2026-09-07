package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/thisnick/agent-gm/internal/gm"
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
	if translated := fromGM(err); translated != nil {
		return translated
	}
	e := New(CodeInternalError, "an internal error; the request ID identifies it in the logs")
	e.Err = err
	return e
}

// fromGM translates the gm layer's classified error into this one, or returns
// nil when err is not one.
//
// **This is the join that makes section 7.2 reachable at all.** Almost every
// code in that table -- `phone_not_responding`, `not_default_sms_app`,
// `google_undocumented_status`, `disconnected`, every `pairing_*` -- is produced by `gm.Classify` and by nothing else. Without a
// translation here they all arrive at a caller as `internal_error` 500, which
// is not merely a wrong code: `phone_not_responding` served as a retryable
// 500 looks exactly like the thing a client should retry, and retrying it is
// how a real person gets the same text message twice (D5). Every diagnostic
// message section 11.4 writes for a failed pairing becomes an unattributable
// 500 as well.
//
// The two layers keep separate error types on purpose -- `internal/gm` may
// not know about HTTP envelopes (section 2.2) -- so the conversion has to
// live somewhere, and it lives here, in the one funnel every handler's return
// value already passes through.
func fromGM(err error) *Error {
	var g *gm.Error
	if !errors.As(err, &g) {
		return nil
	}
	code := Code(g.Code)
	if !Known(code) {
		// A gm code the section 7.2 table does not name would otherwise be
		// served as itself, widening the public vocabulary by accident.
		// gm_agreement_test.go asserts this cannot happen; this is the
		// runtime half of that assertion.
		e := New(CodeInternalError, "an internal error; the request ID identifies it in the logs")
		e.Err = err
		return e
	}
	out := New(code, g.Message)
	out.Err = g.Err
	if g.HTTPStatus != 0 && g.HTTPStatus != HTTPStatus(code) {
		// The two tables disagreeing is a bug, not a case to honour: the
		// section 7.2 table is the contract and gm_agreement_test.go proves
		// they agree, so this only ever fires while that test is red.
		out.HTTPStatusOverride = g.HTTPStatus
	}
	if len(g.Details) > 0 {
		out.Details = make(map[string]any, len(g.Details)+1)
		for k, v := range g.Details {
			out.Details[k] = v
		}
	}
	if g.Reason != "" {
		// unsupported_capability's details.reason comes from section 7.8's
		// closed vocabulary, so it is looked up rather than copied: a reason
		// the vocabulary does not name must not reach a caller.
		reason, ok := ReasonByName(g.Reason)
		if !ok {
			e := New(CodeInternalError, "an internal error; the request ID identifies it in the logs")
			e.Err = fmt.Errorf("gm produced the unsupported_capability reason %q, which section 7.8 does not name: %w", g.Reason, err)
			return e
		}
		if out.Details == nil {
			out.Details = map[string]any{}
		}
		out.Details["reason"] = reason
	}
	if RetryabilityOf(code) == RetryMaybe {
		out.retryable = g.Retryable
	}
	return out
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
