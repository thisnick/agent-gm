package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// Request is what a handler receives. It is deliberately not `*http.Request`:
// by the time a handler runs, the transport concerns are already settled --
// the route is resolved, the caller is authenticated, the scope is checked,
// the rate limit is spent, the query is known to hold nothing unknown, and
// the body is read and known to hold nothing unknown. A handler that reached
// past this struct to the raw request could undo any of that.
type Request struct {
	Ctx context.Context
	// Route is the inventory entry this request matched.
	Route Route
	// Path holds the bound path parameters, keyed by the names in the
	// pattern: `conversation_id`, `emoji`, and so on.
	Path map[string]string
	// Query is the query string, already checked against Route.Query.
	Query map[string]string
	// Body is the raw JSON body, already checked against Route.Body. It is
	// nil when the request carried none, and always nil on a RawBody route.
	Body []byte
	// RawBody is the unread request body, and is non-nil only on a route
	// declared RawBody -- the upload PUT. The handler is responsible for
	// bounding it: how many bytes are allowed is the reservation's business
	// and the transport has no way to know it.
	RawBody io.ReadCloser
	// Bearer is the presented bearer value, whatever it was. It is set on
	// every route that carried an Authorization header, including one whose
	// value is a media ticket rather than an access token -- which is the
	// whole point on an AltCredential route, where Auth is nil and the
	// handler has to check the ticket itself (spec section 10.3).
	//
	// It is a credential: it is never logged, never audited and never put in
	// an error message.
	Bearer string
	// Auth is the authenticated caller, or nil on a route whose Scope is
	// ScopeNone.
	Auth *authz.Authorization
	// Source is the resolved client source (spec section 12.3). It is never
	// the empty string.
	Source string
	// IdempotencyKeyHeader is the Idempotency-Key header as presented. A
	// handler passes it, with its body's client_request_id, to
	// IdempotencyKeyFrom rather than reading either alone.
	IdempotencyKeyHeader string
	// RequestID is the `req_` ID in this request's envelope.
	RequestID string
	// Warnings accumulate through the handler and are rendered in the
	// envelope (spec section 7.1). Text normalisation is never silent, so a
	// cleaned value adds one here rather than being quietly accepted.
	Warnings []string
}

// Warn records a warning for the envelope.
func (r *Request) Warn(w ...string) { r.Warnings = append(r.Warnings, w...) }

// DecodeBody unmarshals the body into dst. The unknown-field check has
// already run against the inventory, so this cannot be the first thing that
// notices a misspelled field.
func (r *Request) DecodeBody(dst any) *apierr.Error {
	if len(r.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Body, dst); err != nil {
		return apierr.MalformedBody(err.Error())
	}
	return nil
}

// Response is a handler's answer.
type Response struct {
	// Status is the HTTP status. Zero means 200.
	Status int
	// Data is the envelope's `data`. A listing puts its rows in
	// `data.items` (spec section 7.1).
	Data any
	// NextCursor is the envelope's `next_cursor`.
	NextCursor string
	// Header carries any extra response headers, such as Location.
	Header http.Header
	// Raw, when set, is written instead of an envelope: attachment bytes and
	// the SSE stream are not JSON envelopes. A handler setting it is
	// responsible for its own content type.
	Raw func(w http.ResponseWriter)
}

// Handler serves one route.
type Handler func(*Request) (*Response, error)

// Deps are the collaborators a server needs. They are an explicit struct
// rather than a package-level singleton so a test can build a whole server
// over two fakes and its own clock.
type Deps struct {
	Authz *authz.Service
	// PublicURL is AGENT_GM_PUBLIC_URL. Every URL this server hands out is
	// built from it and NEVER from the request's Host or X-Forwarded-Host,
	// so an agent in a sandbox on another machine reaches the same origin
	// the tunnel exposes (spec sections 10.3, 12.3).
	PublicURL string
	// Log receives one line per refused request. It never writes to stdout.
	Log func(msg string, kv ...any)
	// LogError receives an internal_error, at error level, ALWAYS. It is
	// separate from Log because the two have different audiences and
	// different levels: a refusal is ordinary traffic an operator reads when
	// they go looking, and a 500 is a bug they must be told about without
	// having to.
	LogError func(msg string, kv ...any)
}

// Server routes and serves the `/v1` surface.
type Server struct {
	deps     Deps
	handlers map[string]Handler
}

// NewServer builds a server with no handlers registered. Handlers are added
// with Handle, which refuses a name the inventory does not have -- so a
// handler for a route nobody declared is a startup failure rather than an
// endpoint nobody documented.
func NewServer(deps Deps) *Server {
	return &Server{deps: deps, handlers: map[string]Handler{}}
}

// ErrUnknownRoute is returned by Handle for a name absent from the inventory.
var ErrUnknownRoute = errors.New("no such route in the inventory")

// Handle registers the handler for one route name.
func (s *Server) Handle(name string, h Handler) error {
	if _, ok := RouteByName(name); !ok {
		return ErrUnknownRoute
	}
	if h == nil {
		return errors.New("api: a nil handler")
	}
	s.handlers[name] = h
	return nil
}

// Registered reports which route names have handlers, so a startup check can
// assert the inventory is fully served rather than discovering a 501 in
// production.
func (s *Server) Registered() []string {
	out := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		out = append(out, name)
	}
	return out
}

// ServeHTTP is the middleware chain, in the order spec section 6.2 fixes for
// a mutation and section 7.1 fixes for every request.
//
// The order is the contract, not a preference:
//
//  1. a request ID, so every answer -- including a panic -- can be correlated;
//  2. security headers, which must be on a refusal as well as on an answer;
//  3. resolve the route: no route is not_found, wrong method is 405 with
//     Allow, because a caller who typed GET for POST should be told what to
//     fix rather than sent looking for a missing object;
//  4. read the body under a 1 MiB bound -- over it is 413, and a read that
//     fails for any other reason is 400, because "too large" would be a
//     guess;
//  5. authenticate, then check the scope. 401 before 403: a caller with no
//     credential is told to get one, not that theirs is too narrow;
//  6. spend the rate limit AFTER authenticating, because the buckets are per
//     authorization;
//  7. strict parameter rejection, on the query and then the body;
//  8. the handler.
//
// Authentication precedes strict parameter rejection deliberately: an
// unauthenticated caller must not be able to use the shape of a parameter
// refusal to learn which parameters a route takes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := apierr.NewRequestID()
	setSecurityHeaders(w)
	w.Header().Set("X-Request-Id", requestID)

	source := s.deps.Authz.Sources.Resolve(r.RemoteAddr, r.Header)

	route, params, allowed, ok := Match(r.Method, r.URL.Path)
	if !ok {
		if len(allowed) > 0 {
			w.Header().Set("Allow", AllowHeader(allowed))
			s.fail(w, requestID, nil, apierr.MethodNotAllowed(r.Method, AllowHeader(allowed)))
			return
		}
		s.fail(w, requestID, nil, apierr.NotFound("route"))
		return
	}

	// A RawBody route's body is bytes, and how many of them are allowed is
	// the reservation's business, not the transport's: reading it here would
	// bound a 100 MiB upload at the 1 MiB JSON limit and buffer it in memory
	// besides. The handler gets the stream instead.
	var body []byte
	if !route.RawBody {
		var readErr *apierr.Error
		body, readErr = readBody(r)
		if readErr != nil {
			s.fail(w, requestID, nil, readErr)
			return
		}
	}

	auth, authErr := s.authenticate(r, &route, source.Value)
	if authErr != nil && !route.AltCredential {
		s.fail(w, requestID, &route, authErr)
		return
	}
	if authErr != nil {
		// AltCredential: the bearer was not an access token, which on this
		// route is not yet a refusal -- it may be a media ticket, which only
		// the handler can check (section 10.3). It runs with no
		// authorization and refuses for itself if the ticket is no good.
		auth = nil
	}

	if auth != nil {
		if err := s.deps.Authz.AllowRequest(auth, bucketFor(route)); err != nil {
			s.fail(w, requestID, &route, apierr.RateLimited(retryAfterFrom(err)))
			return
		}
	}

	query := map[string]string{}
	for name, values := range r.URL.Query() {
		if len(values) > 0 {
			query[name] = values[0]
		}
	}
	if err := route.CheckQuery(r.URL.Query()); err != nil {
		s.fail(w, requestID, &route, err)
		return
	}
	if err := route.CheckBodyFields(body); err != nil {
		s.fail(w, requestID, &route, err)
		return
	}

	handler, registered := s.handlers[route.Name]
	if !registered {
		s.fail(w, requestID, &route, apierr.New(apierr.CodeInternalError,
			"this route is declared but not served by this build"))
		return
	}

	req := &Request{
		Ctx:                  r.Context(),
		Route:                route,
		Path:                 params,
		Query:                query,
		Body:                 body,
		RawBody:              rawBodyReader(route, r),
		Bearer:               presentedBearer(r.Header),
		Auth:                 auth,
		Source:               source.Value,
		IdempotencyKeyHeader: r.Header.Get("Idempotency-Key"),
		RequestID:            requestID,
	}

	resp, err := handler(req)
	if err != nil {
		s.fail(w, requestID, &route, apierr.From(err))
		return
	}
	s.succeed(w, req, resp)
}

func (s *Server) authenticate(r *http.Request, route *Route, source string) (*authz.Authorization, *apierr.Error) {
	if route.Scope == ScopeNone {
		// The route authenticates by something else -- the admin secret, a
		// refresh token, or a media ticket -- and does it in its own handler,
		// where the failure limiter that guards it also lives.
		return nil, nil
	}
	token, err := bearerToken(r.Header)
	if err != nil {
		return nil, err
	}
	auth, authErr := s.deps.Authz.Authenticate(r.Context(), token)
	if authErr != nil {
		return nil, apierr.InvalidToken(refusalDetail(authErr))
	}
	if err := auth.Require(authz.Scope(route.Scope)); err != nil {
		return nil, apierr.InsufficientScope(string(route.Scope))
	}
	_ = source
	return auth, nil
}

// bearerToken parses exactly one Authorization header and only the Bearer
// scheme. Two headers, another scheme, or a value carrying two tokens are all
// refused rather than resolved to whichever happens to be first (spec section
// 8.1's rule, applied here too because the hazard is the same).
// refusalDetail turns a credential refusal into a short phrase for the
// caller. Every reason maps to the same code -- 401 invalid_token -- because
// an answer that distinguished "unknown" from "expired" from "revoked" would
// tell an attacker which of their guesses was once a real token.
func refusalDetail(err error) string {
	switch {
	case errors.Is(err, authz.ErrTokenAbsent):
		return "no bearer was presented"
	default:
		return "the token was refused"
	}
}

func bearerToken(h http.Header) (string, *apierr.Error) {
	values := h.Values("Authorization")
	switch {
	case len(values) == 0:
		return "", apierr.InvalidToken("no Authorization header")
	case len(values) > 1:
		return "", apierr.InvalidToken("more than one Authorization header")
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", apierr.InvalidToken("the Authorization header must be exactly `Bearer <token>`")
	}
	return fields[1], nil
}

// presentedBearer returns the bearer value if the header is well formed, and
// "" otherwise. It never reports WHY, because a handler that cared would be
// re-implementing a refusal the transport already owns.
func presentedBearer(h http.Header) string {
	token, err := bearerToken(h)
	if err != nil {
		return ""
	}
	return token
}

// bucketFor picks the section 12.3 bucket a route spends from. There is
// deliberately no per-account and no per-scope dimension: holding several
// scopes does not multiply the allowance and neither does holding several
// accounts (section 16 Slice 2 tests 30 and 40).
func bucketFor(r Route) authz.BucketSpec {
	switch {
	case strings.HasPrefix(r.Path, "/v1/admin/"):
		return authz.BucketAdmin
	case r.Scope == ScopeWrite || r.Scope == ScopeDelete:
		return authz.BucketMutations
	default:
		return authz.BucketReads
	}
}

// retryAfterFrom reads the wait a limiter reported, defaulting to a second.
// The header is always sent on a rate_limited answer: a 429 with no
// Retry-After tells a client to guess, and a guessing client retries too soon.
func retryAfterFrom(err error) time.Duration {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		if d := ra.RetryAfter(); d > 0 {
			return d
		}
	}
	return time.Second
}

// readBody enforces the 1 MiB bound of spec section 7.2. A body over it is
// 413 payload_too_large; a body that fails to read for any other reason is
// 400, because "too large" would be a guess.
func readBody(r *http.Request) ([]byte, *apierr.Error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := io.LimitReader(r.Body, apierr.MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if int64(len(body)) > apierr.MaxBodyBytes {
		return nil, apierr.PayloadTooLarge("the request body", apierr.MaxBodyBytes)
	}
	if err != nil {
		return nil, apierr.MalformedBody("the body could not be read")
	}
	if len(body) == 0 {
		return nil, nil
	}
	return body, nil
}

// setSecurityHeaders puts the same headers on every answer, including a
// refusal: a header that is only on the success path is a header an attacker
// can arrange not to receive.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func (s *Server) succeed(w http.ResponseWriter, req *Request, resp *Response) {
	if resp == nil {
		resp = &Response{}
	}
	for k, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	if resp.Raw != nil {
		if resp.Status != 0 {
			w.WriteHeader(resp.Status)
		}
		resp.Raw(w)
		return
	}
	env := apierr.NewSuccess(req.RequestID, resp.Data)
	if resp.NextCursor != "" {
		env = env.WithCursor(resp.NextCursor)
	}
	if len(req.Warnings) > 0 {
		env = env.WithWarnings(req.Warnings...)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, env)
}

func (s *Server) fail(w http.ResponseWriter, requestID string, route *Route, e *apierr.Error) {
	if e == nil {
		e = apierr.New(apierr.CodeInternalError, "an unclassified failure")
	}
	if header := e.RetryAfterHeader(); header != "" {
		w.Header().Set("Retry-After", header)
	}
	// 401 and 403 carry WWW-Authenticate with the realm, the error, the
	// resource metadata URL and, for a scope refusal, the scope the route
	// requires (spec section 7.2).
	if e.Code == apierr.CodeInvalidToken || e.Code == apierr.CodeInsufficientScope {
		scope := ""
		if route != nil {
			scope = string(route.Scope)
		}
		w.Header().Set("WWW-Authenticate",
			apierr.WWWAuthenticate(s.deps.PublicURL, e.Code, scope))
	}
	if s.deps.Log != nil {
		// The code and the request ID, never the message body and never a
		// token (spec section 12.2).
		//
		// An `internal_error` is logged through a SEPARATE hook, at error
		// level, and always. It is the one refusal whose message tells the
		// caller "the request ID identifies it in the logs", and for a whole
		// slice that was a lie: the refusal hook was wired to Debug, so at
		// the default level a 500 produced not one line while the sentence
		// promising otherwise went out to the caller. An unattributable 500
		// is precisely the error an operator cannot diagnose from outside,
		// so it is the one that must never be silent.
		if e.Code == apierr.CodeInternalError && s.deps.LogError != nil {
			cause := ""
			if e.Err != nil {
				cause = e.Err.Error()
			}
			s.deps.LogError("internal error", "request_id", requestID, "cause", cause)
		} else {
			s.deps.Log("request refused", "request_id", requestID, "code", string(e.Code))
		}
	}
	writeJSON(w, e.HTTPStatus(), e.Envelope(requestID))
}

// rawBodyReader hands the unread body to a RawBody route, and nil to every
// other one -- so a handler cannot reach a stream the transport has already
// consumed, and cannot accidentally bypass the JSON path on a route that has
// one.
func rawBodyReader(route Route, r *http.Request) io.ReadCloser {
	if !route.RawBody {
		return nil
	}
	return r.Body
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// A write failure here means the client has gone; there is nothing to
	// report it to.
	_ = json.NewEncoder(w).Encode(body)
}
