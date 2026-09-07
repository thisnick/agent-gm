package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// Package mcp is the MCP surface of spec section 8: a stateless streamable
// HTTP endpoint at `/mcp`, twenty-one tools, one resource template, and the
// `instructions` block a cold agent reads before it does anything.
//
// Nothing here reimplements the API. Every tool call runs the REST handler
// through api.Server.Invoke, so the filters, the validation, the error codes
// and the DTOs are the same on both surfaces by construction.

// Path is where the endpoint lives.
const Path = "/mcp"

// Concurrency limits (spec section 8.1).
const (
	// MaxInFlightPerAuthorization is 8 in flight per authorization.
	MaxInFlightPerAuthorization = 8
	// MaxInFlightPerProcess is 32 across the process.
	MaxInFlightPerProcess = 32
	// ConcurrencyRetryAfterSeconds is what a 429 from the concurrency gate
	// tells a client to wait. A 429 with no Retry-After tells a client to
	// guess, and a guessing client retries too soon.
	ConcurrencyRetryAfterSeconds = 1
)

// MaxBodyBytes is section 8.1's 1 MiB bound.
const MaxBodyBytes = apierr.MaxBodyBytes

// messagingScopes is the set a token must intersect to reach `/mcp` at all.
var messagingScopes = []authz.Scope{
	authz.ScopeMessagesRead, authz.ScopeMessagesWrite, authz.ScopeMessagesDelete,
}

// Config is what the handler collaborates with. Every field is explicit so a
// test can build a whole MCP server over two fakes and its own clock.
type Config struct {
	// API is the REST server whose handlers every tool call runs.
	API *api.Server
	// Authz authenticates the bearer and resolves the client source.
	Authz *authz.Service
	// PublicURL is AGENT_GM_PUBLIC_URL. The `Origin` check compares against
	// it, and every URL handed out is built from it.
	PublicURL string
	// The build facts `serverInfo` reports. `Commit` and `SourceURL` are the
	// AGPL section 13 obligation of spec section 1.4, and section 16 Slice 3
	// test 28 asserts they equal the built commit.
	Version   string
	Commit    string
	SourceURL string
	// Log receives one line per refusal. It never writes to stdout.
	Log func(msg string, kv ...any)
}

// Handler serves `/mcp`.
type Handler struct {
	cfg Config

	mu       sync.Mutex
	inFlight int
	perAuth  map[string]int
}

// New builds the handler.
func New(cfg Config) *Handler {
	return &Handler{cfg: cfg, perAuth: map[string]int{}}
}

// Mount returns an http.Handler that serves `/mcp` from h and everything else
// from next.
//
// It is a wrapper rather than a route in the api inventory on purpose: `/mcp`
// is not a `/v1` route, it does not answer the section 7.1 envelope, and its
// pre-parse checks run in an order the REST chain does not have. Putting it in
// the inventory would make the two-way table test of section 16 Slice 3 test
// 16 -- which is about `/v1` routes -- have to special-case its own endpoint.
func Mount(next http.Handler, h *Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == Path {
			h.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP runs section 8.1's checks, in section 8.1's order, and then the
// JSON-RPC method.
//
// The order is the contract. `Origin` is first and is checked **before the
// transport parses anything**, because a browser page on another origin must
// not be able to reach this endpoint at all, and a check that ran after
// parsing would already have done work on its behalf. The authorization
// checks come before the concurrency gate so that an unauthenticated flood
// cannot exhaust the in-flight budget of the callers who are allowed in.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		// There is no server-initiated stream to open and no session to
		// delete: this server is stateless at the application layer, so GET
		// and DELETE have nothing to do.
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, r, apierr.MethodNotAllowed(r.Method, http.MethodPost))
		return
	}

	// 1. Origin, when present, must equal the configured public URL.
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, h.cfg.PublicURL) {
		// 403 rather than 401: this is not a credential problem and offering
		// a challenge would invite a browser page on another origin to go and
		// get one. There is no `forbidden` code in section 7.2's table
		// because no `/v1` route has this failure, so the status carries the
		// answer and the code names what was wrong with the request.
		h.failStatus(w, http.StatusForbidden, apierr.New(apierr.CodeInvalidRequest,
			"this endpoint may only be reached from "+h.cfg.PublicURL))
		return
	}

	// 2. Body over 1 MiB.
	body, tooLarge := readBounded(r.Body)
	if tooLarge {
		h.fail(w, r, apierr.PayloadTooLarge("the request body", MaxBodyBytes))
		return
	}

	// 3. Content-Type must be application/json.
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		h.fail(w, r, apierr.New(apierr.CodeInvalidRequest,
			"Content-Type must be application/json"))
		return
	}

	// 4. Accept must admit application/json or text/event-stream.
	if !acceptsJSONOrStream(r.Header.Values("Accept")) {
		h.fail(w, r, apierr.New(apierr.CodeInvalidRequest,
			"Accept must admit application/json or text/event-stream"))
		return
	}

	// 5. Exactly one Authorization header, Bearer only.
	token, headerErr := singleBearer(r.Header)
	if headerErr != nil {
		h.challenge(w, r, headerErr)
		return
	}

	// 6. The token must carry at least one messaging scope.
	source := h.cfg.Authz.Sources.Resolve(r.RemoteAddr, r.Header)
	auth, authErr := h.cfg.Authz.Authenticate(r.Context(), token)
	if authErr != nil {
		h.challenge(w, r, apierr.InvalidToken("the token was refused"))
		return
	}
	if !hasAnyMessagingScope(auth) {
		// Deliberately distinct from the 401: this credential is real, it
		// just may not be used here, and a client that retried the
		// authorization flow on a 401 would loop for ever on a 403.
		h.challenge(w, r, apierr.InsufficientScope(string(authz.ScopeMessagesRead)))
		return
	}

	// 7. Concurrency: 8 per authorization, 32 across the process.
	release, ok := h.acquire(auth.ID)
	if !ok {
		e := apierr.RateLimited(0)
		w.Header().Set("Retry-After", strconv.Itoa(ConcurrencyRetryAfterSeconds))
		h.fail(w, r, e)
		return
	}
	defer release()

	// Only now is anything parsed.
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeRPC(w, http.StatusOK, rpcFail(nil, codeParseError, "the request is not a JSON-RPC 2.0 call"))
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, http.StatusOK, rpcFail(req.ID, codeInvalidRequest, `"jsonrpc" must be "2.0"`))
		return
	}

	s := &session{
		handler: h,
		auth:    auth,
		bearer:  token,
		source:  source.Value,
		ctx:     r.Context(),
	}
	resp, notification := s.dispatch(req)
	if notification {
		// A notification has no answer. 202 with an empty body is what the
		// streamable HTTP transport expects.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

// acquire takes one slot from each budget, or reports that it could not.
func (h *Handler) acquire(authorizationID string) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight >= MaxInFlightPerProcess || h.perAuth[authorizationID] >= MaxInFlightPerAuthorization {
		return nil, false
	}
	h.inFlight++
	h.perAuth[authorizationID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.inFlight--
			h.perAuth[authorizationID]--
			if h.perAuth[authorizationID] <= 0 {
				delete(h.perAuth, authorizationID)
			}
		})
	}, true
}

// --- the transport's refusals ------------------------------------------------

// Challenge is the `WWW-Authenticate` a 401 and a 403 from `/mcp` both carry.
//
// The two answers are deliberately distinct -- no token is `401`, a valid
// token with no messaging scope is `403 insufficient_scope` -- but the
// challenge itself is the same string, because it says the same thing to the
// same reader: here is where to authorize, and here is what to ask for.
//
// It names `messages:read messages:write` and NOT `messages:delete`, which is
// section 9.2's literal challenge and is asserted byte for byte by section 16
// Slice 3 test 2.
//
// The omission is not an oversight in the spec. `scope` in a challenge is what
// the client should ASK FOR, and section 9.4 makes exactly those two the
// default an authorization request carries when the client names no scope. A
// challenge that also asked for `messages:delete` would send every connector
// to an approval screen offering to let a model delete the owner's threads,
// for no better reason than that the scope exists. A client that wants it asks
// for it; the discovery documents list all three under `scopes_supported`.
func Challenge(publicURL string) string {
	return `Bearer realm="` + apierr.AuthRealm + `", ` +
		`resource_metadata="` + apierr.ResourceMetadataURL(publicURL) + `", ` +
		`scope="messages:read messages:write"`
}

func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, e *apierr.Error) {
	w.Header().Set("WWW-Authenticate", Challenge(h.cfg.PublicURL))
	h.fail(w, r, e)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, e *apierr.Error) {
	h.failStatus(w, e.HTTPStatus(), e)
	_ = r
}

func (h *Handler) failStatus(w http.ResponseWriter, status int, e *apierr.Error) {
	requestID := apierr.NewRequestID()
	w.Header().Set("X-Request-Id", requestID)
	if header := e.RetryAfterHeader(); header != "" && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", header)
	}
	if h.cfg.Log != nil {
		h.cfg.Log("mcp request refused", "request_id", requestID, "code", string(e.Code))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e.Envelope(requestID))
}

func writeRPC(w http.ResponseWriter, status int, resp jsonrpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// --- the pre-parse predicates -------------------------------------------------

// sameOrigin compares an `Origin` header with the configured public URL. The
// comparison is on scheme and host only, because that is all an `Origin` is.
func sameOrigin(origin, publicURL string) bool {
	return strings.EqualFold(strings.TrimRight(origin, "/"), strings.TrimRight(publicURL, "/"))
}

// readBounded reads at most 1 MiB and reports whether there was more.
func readBounded(body io.ReadCloser) ([]byte, bool) {
	if body == nil {
		return nil, false
	}
	data, _ := io.ReadAll(io.LimitReader(body, MaxBodyBytes+1))
	if int64(len(data)) > MaxBodyBytes {
		return nil, true
	}
	return data, false
}

func isJSONContentType(v string) bool {
	media, _, _ := strings.Cut(v, ";")
	return strings.EqualFold(strings.TrimSpace(media), "application/json")
}

// acceptsJSONOrStream reports whether the Accept header admits one of the two
// media types the streamable HTTP transport can answer with. An absent Accept
// admits everything, which is what RFC 9110 says it means.
func acceptsJSONOrStream(values []string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			media, _, _ := strings.Cut(part, ";")
			switch strings.ToLower(strings.TrimSpace(media)) {
			case "application/json", "text/event-stream", "*/*", "application/*", "text/*":
				return true
			}
		}
	}
	return false
}

// singleBearer parses **exactly one** Authorization header and only the
// Bearer scheme (spec section 8.1).
//
// Two headers, another scheme, or a value carrying two tokens are all refused
// rather than resolved to whichever happens to be first. Resolving would mean
// choosing which of two credentials a caller meant, and a proxy that appended
// its own header would silently decide it.
func singleBearer(h http.Header) (string, *apierr.Error) {
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

func hasAnyMessagingScope(auth *authz.Authorization) bool {
	if auth == nil {
		return false
	}
	for _, s := range messagingScopes {
		if auth.Scopes.Has(s) {
			return true
		}
	}
	return false
}
