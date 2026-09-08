package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// The RFC 6749 / RFC 7591 / RFC 8707 error codes this server produces. They
// are the `error` member of every OAuth error body and of every error
// redirect.
const (
	ErrInvalidRequest          = "invalid_request"
	ErrInvalidClientMetadata   = "invalid_client_metadata"
	ErrInvalidRedirectURI      = "invalid_redirect_uri"
	ErrInvalidClient           = "invalid_client"
	ErrInvalidGrant            = "invalid_grant"
	ErrInvalidScope            = "invalid_scope"
	ErrInvalidTarget           = "invalid_target"
	ErrUnsupportedResponseType = "unsupported_response_type"
	ErrUnsupportedGrantType    = "unsupported_grant_type"
	ErrAccessDenied            = "access_denied"
	ErrServerError             = "server_error"
	ErrTemporarilyUnavailable  = "temporarily_unavailable"
)

// errConfig is a construction failure, which is a startup failure.
func errConfig(msg string) error { return fmt.Errorf("oauth: %s", msg) }

// oauthError is one `{ "error", "error_description" }` body (spec section
// 9.1). Every OAuth error is this shape, including the ones the HTTP
// framework would otherwise answer itself.
type oauthError struct {
	Status      int
	Code        string
	Description string
	// RetryAfter is set on a 429 and is always sent with one: a 429 with no
	// Retry-After tells a client to guess, and a guessing client retries too
	// soon.
	RetryAfter time.Duration
}

func (e *oauthError) Error() string { return e.Code + ": " + e.Description }

func badRequest(code, description string) *oauthError {
	return &oauthError{Status: http.StatusBadRequest, Code: code, Description: description}
}

func statusError(status int, code, description string) *oauthError {
	return &oauthError{Status: status, Code: code, Description: description}
}

// writeOAuthError renders an error body with `Cache-Control: no-store` and
// the browser security headers of section 9.9. The headers are on the refusal
// as well as on the answer: a header that is only on the success path is a
// header an attacker can arrange not to receive.
func (s *Server) writeOAuthError(w http.ResponseWriter, e *oauthError) {
	setSecurityHeaders(w)
	if e.RetryAfter > 0 {
		secs := int64((e.RetryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             e.Code,
		"error_description": e.Description,
	})
}

// writeNotFound answers an unknown path under `/oauth` with the REST
// `not_found` envelope rather than an OAuth error body (spec section 9.1).
// The two shapes are different on purpose: an unknown path is not an OAuth
// protocol failure, and answering it as one would tell a client that a route
// it invented is part of the protocol.
func (s *Server) writeNotFound(w http.ResponseWriter) {
	setSecurityHeaders(w)
	requestID := apierr.NewRequestID()
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(apierr.NotFound("route").Envelope(requestID))
}

// setSecurityHeaders puts spec section 9.9's headers on every OAuth answer.
//
// `script-src 'self'` is there for exactly one file: `/oauth/poll.js`. The
// polling script is served rather than inlined precisely so that this
// directive can stay `'self'` and no page needs a nonce or an `unsafe-inline`
// (section 9.5).
//
// `Referrer-Policy: no-referrer` is right for everything that is NOT a page a
// browser renders -- the JSON answers, the metadata documents, and above all
// the redirect back to the client, which carries the authorization code in its
// URL. Pages use setPageSecurityHeaders instead; see the note there.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; script-src 'self'; connect-src 'self'; style-src 'self'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}

// setPageSecurityHeaders is setSecurityHeaders for the HTML a browser renders
// and for the script that HTML loads: section 9.9's headers, with
// `Referrer-Policy: same-origin` in place of `no-referrer`.
//
// A page's referrer policy decides the `Origin` header of the form post it
// makes: the Fetch standard serialises `Origin` as `null` when the request's
// referrer policy is `no-referrer` (Fetch, "append a request Origin header").
// Browsers apply that to a form navigation, so a page served `no-referrer`
// posts its own same-origin form with `Origin: null` and section 9.5's origin
// check refuses it -- which is what production did, and what no curl exercise
// of the form could show, since a client that sets `Origin` by hand never
// consults the page. Engines differ in whether they take that step, so one
// browser passing proves nothing about another. `same-origin` keeps the
// referrer off the cross-origin callback while letting a same-origin post
// carry its real origin.
func setPageSecurityHeaders(w http.ResponseWriter) {
	setSecurityHeaders(w)
	w.Header().Set("Referrer-Policy", "same-origin")
}

// Browsers apply form-action to the completion POST's redirect as well.
// Allow only the registered callback origin (or private-use scheme), never
// its query or untrusted text that could inject another CSP directive.
func allowCallbackFormAction(w http.ResponseWriter, callback string) {
	u, err := url.Parse(callback)
	if err != nil || u.Scheme == "" {
		return
	}
	source := u.Scheme + ":"
	if u.Scheme == "http" || u.Scheme == "https" {
		source += "//" + u.Host
	}
	if strings.ContainsAny(source, ";, \t\r\n") {
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", strings.Replace(h.Get("Content-Security-Policy"), "form-action 'self'", "form-action 'self' "+source, 1))
}

// translate maps a credential-layer refusal onto an OAuth error body.
//
// A rate limit keeps its Retry-After, and every other refusal collapses to
// `invalid_grant`: an answer that distinguished "unknown" from "expired" from
// "revoked" would tell an attacker which of their guesses was once real.
func translate(err error) *oauthError {
	var limited *authz.RateLimitedError
	if errors.As(err, &limited) {
		return &oauthError{
			Status:      http.StatusTooManyRequests,
			Code:        ErrTemporarilyUnavailable,
			Description: "too many attempts; retry later",
			RetryAfter:  limited.RetryAfter,
		}
	}
	switch {
	case errors.Is(err, authz.ErrInvalidScope):
		return badRequest(ErrInvalidScope, "the requested scope is not available")
	case errors.Is(err, authz.ErrGrantReplayed),
		errors.Is(err, authz.ErrInvalidGrant),
		errors.Is(err, authz.ErrTokenUnknown),
		errors.Is(err, authz.ErrTokenExpired),
		errors.Is(err, authz.ErrTokenRevoked),
		errors.Is(err, authz.ErrTokenReused),
		errors.Is(err, authz.ErrTokenAbsent):
		return badRequest(ErrInvalidGrant, "the grant is not valid")
	default:
		return statusError(http.StatusInternalServerError, ErrServerError,
			"the request could not be completed")
	}
}

// maxBodyBytes is the 1 MiB bound of spec sections 7.2 and 9.1. A body over it
// is 413 with the OAuth error shape, not with whatever the framework would
// have said.
const maxBodyBytes = apierr.MaxBodyBytes

// methodsFor renders the Allow header for one path, in a stable order.
func methodsFor(handlers map[string]http.HandlerFunc) string {
	var out []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if _, ok := handlers[m]; ok {
			out = append(out, m)
		}
	}
	return strings.Join(out, ", ")
}
