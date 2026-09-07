package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
)

// Package mcp is the MCP surface of spec section 8: a streamable HTTP
// endpoint at `/mcp`, twenty-one tools, one resource template, and the
// `instructions` block a cold agent reads before it does anything.
//
// The protocol is the official MCP Go SDK's (decision D36). This file is the
// part that is still ours: the section 8.1 Origin check, the 1 MiB bound, the
// bearer verification against our authz store, the section 8.1 concurrency
// budget, and the section 7.1 error envelope on every refusal.

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

// ChallengeScopes is what the `/mcp` challenge tells a client to ask for.
//
// It names `messages:read messages:write` and NOT `messages:delete`, which is
// section 9.2's literal challenge.
//
// The omission is not an oversight in the spec. `scope` in a challenge is what
// the client should ASK FOR, and section 9.4 makes exactly those two the
// default an authorization request carries when the client names no scope. A
// challenge that also asked for `messages:delete` would send every connector
// to an approval screen offering to let a model delete the owner's threads,
// for no better reason than that the scope exists. A client that wants it asks
// for it; the discovery documents list all three under `scopes_supported`.
var ChallengeScopes = []string{
	string(authz.ScopeMessagesRead), string(authz.ScopeMessagesWrite),
}

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
	// The build facts the `initialize` result reports. `Commit` and
	// `SourceURL` are the AGPL section 13 obligation of spec section 1.4, and
	// section 16 Slice 3 test 28 asserts they equal the built commit.
	Version   string
	Commit    string
	SourceURL string
	// Log receives one line per refusal. It never writes to stdout.
	Log func(msg string, kv ...any)
}

// Handler serves `/mcp`.
type Handler struct {
	cfg     Config
	servers servers
	// chain is the Origin check, the bound, the SDK's bearer middleware, the
	// concurrency gate and the SDK's streamable transport, in that order.
	chain http.Handler

	mu       sync.Mutex
	inFlight int
	perAuth  map[string]int
}

// New builds the handler.
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, perAuth: map[string]int{}}

	streamable := sdk.NewStreamableHTTPHandler(h.getServer, &sdk.StreamableHTTPOptions{
		// Stateless, and it is the SDK that makes the choice for us.
		//
		// `StreamableServerTransport.SupportsProtocolVersion` refuses every
		// revision from `2026-07-28` onwards unless the transport is
		// stateless -- SEP-2575's protocol is sessionless by design and drops
		// resumability. Section 8.1 names `2026-07-28` as the revision this
		// server speaks, so sessions and that revision cannot both be had:
		// a stateful handler negotiates down to `2025-11-25` for every
		// client, silently. Stateless is therefore the setting that keeps
		// section 8.1's protocol, and it is also what section 8.1 already
		// described -- every request carries its own bearer and its own
		// protocol metadata, and durable state lives in SQLite.
		Stateless: true,
		// The transport's own bound agrees with ours. Ours refuses first,
		// before authentication, so an unauthenticated flood is refused
		// without a store lookup; this one is the SDK's belt on the same
		// number, and it is what bounds a body that arrives chunked.
		MaxRequestBodyBytes: MaxBodyBytes,
		// A tool call that outlives its HTTP request has nowhere to send its
		// answer, and every one of ours is a REST call that can be abandoned.
		PropagateRequestCancellation: true,
	})

	bearer := sdkauth.RequireBearerToken(h.verifyToken, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: apierr.ResourceMetadataURL(cfg.PublicURL),
		// Scopes is deliberately empty. The SDK enforces `Scopes` as an AND
		// over the whole list, and section 8.1 asks for an OR over three: a
		// token must carry AT LEAST ONE messaging scope. So the check is in
		// verifyToken and the 403 is ours, while the challenge FORMAT stays
		// the SDK's -- Challenge() below is asserted byte-equal to what a
		// real RequireBearerToken emits for these scopes.
	})

	h.chain = h.checkOrigin(h.bound(h.singleBearerHeader(bearer(h.requireMessagingScope(h.gate(streamable))))))
	return h
}

// Mount returns an http.Handler that serves `/mcp` from h and everything else
// from next.
//
// It is a wrapper rather than a route in the api inventory on purpose: `/mcp`
// is not a `/v1` route, it does not answer the section 7.1 envelope on
// success, and its pre-transport checks run in an order the REST chain does
// not have. Putting it in the inventory would make the two-way table test of
// section 16 Slice 3 test 16 -- which is about `/v1` routes -- have to
// special-case its own endpoint.
func Mount(next http.Handler, h *Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == Path {
			h.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP runs the chain.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	// The SDK and its bearer middleware refuse with `http.Error`, which is
	// plain text. Section 7.1's envelope is this server's answer shape on
	// every surface, and a connector that has only ever seen JSON from us
	// should not have to parse prose to learn it was rate limited. The
	// wrapper substitutes the envelope on a refusal and gets out of the way
	// on a 2xx, so an SSE stream still streams.
	ew := &envelopeWriter{ResponseWriter: w, log: h.cfg.Log, challenge: Challenge(h.cfg.PublicURL)}
	h.chain.ServeHTTP(ew, r)
	ew.finish()
}

// checkOrigin is section 8.1's first check: `Origin`, when present, must equal
// the configured public URL, and it is checked **before the transport parses
// anything**, because a browser page on another origin must not be able to
// reach this endpoint at all and a check that ran after parsing would already
// have done work on its behalf.
//
// It is ours and not the SDK's. The SDK offers
// `StreamableHTTPOptions.CrossOriginProtection`, but it is opt-in behind a
// MCPGODEBUG parameter, it is deprecated in favour of external middleware,
// and it compares the `Origin` against the request's own `Host` -- which is
// the tunnel's hostname here, not `AGENT_GM_PUBLIC_URL`, so behind the
// deployment of section 14 it would compare the wrong two strings.
func (h *Handler) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, h.cfg.PublicURL) {
			// 403 rather than 401: this is not a credential problem, and
			// offering a challenge would invite a browser page on another
			// origin to go and get one. There is no `forbidden` code in
			// section 7.2's table because no `/v1` route has this failure, so
			// the status carries the answer and the code names what was wrong
			// with the request.
			h.refuse(w, http.StatusForbidden, apierr.New(apierr.CodeInvalidRequest,
				"this endpoint may only be reached from "+h.cfg.PublicURL))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bound is section 8.1's 1 MiB body bound, applied before authentication so
// that an unauthenticated body cannot buy a store lookup, let alone memory.
func (h *Handler) bound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			h.refuse(w, http.StatusRequestEntityTooLarge,
				apierr.PayloadTooLarge("the request body", MaxBodyBytes))
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// gate is section 8.1's concurrency budget: 8 in flight per authorization, 32
// across the process, and the excess is `429` with `Retry-After`.
//
// It runs after authentication so that an unauthenticated flood cannot
// exhaust the in-flight budget of the callers who are allowed in, and it is
// keyed on the authorization rather than the session or the source, because
// the authorization is what the budget belongs to.
func (h *Handler) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := callerFromContext(r.Context())
		id := ""
		if call != nil && call.auth != nil {
			id = call.auth.ID
		}
		release, ok := h.acquire(id)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(ConcurrencyRetryAfterSeconds))
			h.refuse(w, http.StatusTooManyRequests, apierr.RateLimited(0))
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
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

// getServer picks the SDK server whose tool set matches what this caller may
// use. It is the SDK's own hook for a server that is not the same for every
// client, and it is how section 8.2's scope gating survives the move: a model
// is never invited to attempt something that will be refused.
func (h *Handler) getServer(r *http.Request) *sdk.Server {
	call := callerFromContext(r.Context())
	if call == nil {
		// Unreachable: the bearer middleware runs first and refuses without
		// one. Returning nil makes the SDK answer 400 rather than serve a
		// server nobody authenticated for.
		return nil
	}
	return h.serverFor(call.auth)
}

// --- authentication ------------------------------------------------------------

// verifyToken is the SDK's TokenVerifier over our authz store.
//
// Everything the store decides stays the store's: the scopes an authorization
// holds, its expiry, and its revocation -- which is why revocation is
// effective on the **next request** rather than at some cache's convenience.
// There is no token state anywhere in this package: every request re-reads
// the authorization, session or no session.
func (h *Handler) verifyToken(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
	auth, err := h.cfg.Authz.Authenticate(ctx, token)
	if err != nil || auth == nil {
		// ErrInvalidToken is the SDK's signal for 401-with-a-challenge. The
		// reason is deliberately not passed on: a caller learns that the
		// token was refused, not which of the ways it was refused, because
		// the difference between "revoked", "expired" and "never existed" is
		// an oracle.
		return nil, fmt.Errorf("the token was refused: %w", sdkauth.ErrInvalidToken)
	}

	source := h.cfg.Authz.Sources.Resolve(r.RemoteAddr, r.Header)
	return &sdkauth.TokenInfo{
		Scopes: auth.Scopes.Strings(),
		// UserID is the SDK's session-hijacking guard: it refuses a request
		// that continues a session established by a different user.
		//
		// **It is inert here and there is no test for it**, because the
		// transport is stateless (see the Stateless option above) and a
		// stateless transport has no session to hijack: the SDK reads this
		// field only in `serveStateful` (mcp/streamable.go:569,713). It is
		// set anyway, and said to be inert rather than quietly dropped,
		// because it costs one field and it is the property section 8.1 used
		// to get for free by having no sessions at all -- so the day
		// `Stateless` is reconsidered, the guard is already keyed on the
		// authorization rather than being remembered.
		UserID:     auth.ID,
		Expiration: auth.ExpiresAt,
		Extra: map[string]any{
			callerKey: &caller{auth: auth, bearer: token, source: source.Value},
		},
	}, nil
}

// singleBearerHeader is section 8.1's "exactly one Authorization header is
// parsed, and only the Bearer scheme".
//
// It is ours because the SDK's is not equivalent: `auth.verify` reads
// `req.Header.Get("Authorization")`, which returns the FIRST of several
// values and silently ignores the rest. Section 8.1 refuses two headers,
// another scheme, or a value carrying two tokens **rather than resolving
// them** -- resolving would mean choosing which of two credentials a caller
// meant, and a proxy that appended its own header would silently decide it.
// Driven at v1.7.0: without this middleware, two Authorization headers are
// answered from the first one.
func (h *Handler) singleBearerHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		var why string
		switch {
		case len(values) == 0:
			why = "no Authorization header"
		case len(values) > 1:
			why = "more than one Authorization header"
		default:
			if fields := strings.Fields(values[0]); len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				why = "the Authorization header must be exactly `Bearer <token>`"
			}
		}
		if why != "" {
			w.Header().Set("WWW-Authenticate", Challenge(h.cfg.PublicURL))
			h.refuse(w, http.StatusUnauthorized, apierr.InvalidToken(why))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireMessagingScope is section 8.1's "the token must carry AT LEAST ONE
// messaging scope".
//
// It is a middleware of ours rather than `RequireBearerTokenOptions.Scopes`
// because the SDK enforces that list as an AND over all of it, and this is an
// OR over three: a read-only token is a legitimate `/mcp` caller. The refusal
// is `403 insufficient_scope`, deliberately distinct from the `401` -- this
// credential is real, it just may not be used here, and a client that retried
// the authorization flow on a 401 would loop for ever on a 403. The challenge
// is the same string either way, because it says the same thing to the same
// reader.
func (h *Handler) requireMessagingScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := callerFromContext(r.Context())
		if call == nil || !hasAnyMessagingScope(call.auth) {
			w.Header().Set("WWW-Authenticate", Challenge(h.cfg.PublicURL))
			h.refuse(w, http.StatusForbidden,
				apierr.InsufficientScope(string(authz.ScopeMessagesRead)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerFromContext recovers what verifyToken resolved. It comes off the
// SDK's own `TokenInfo`, which the bearer middleware puts in the request
// context, so there is exactly one place a request's authority is established.
func callerFromContext(ctx context.Context) *caller {
	ti := sdkauth.TokenInfoFromContext(ctx)
	if ti == nil {
		return nil
	}
	c, _ := ti.Extra[callerKey].(*caller)
	return c
}

// Challenge is the `WWW-Authenticate` a 401 and a 403 from `/mcp` both carry.
//
// The two answers are deliberately distinct -- no token is `401`, a valid
// token with no messaging scope is `403 insufficient_scope` -- but the
// challenge itself is the same string, because it says the same thing to the
// same reader: here is where to authorize, and here is what to ask for.
//
// The FORMAT is the SDK's, not ours (decision D36): it is what
// `auth.RequireBearerToken` emits, which is why there is no `realm` parameter
// any more. A test drives a real `RequireBearerToken` and asserts this
// function returns exactly what it wrote, so the SDK stays the authority and
// a bump that changes the format fails that test rather than a connector.
func Challenge(publicURL string) string {
	return fmt.Sprintf("Bearer resource_metadata=%q, scope=%q",
		apierr.ResourceMetadataURL(publicURL), strings.Join(ChallengeScopes, " "))
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

// --- refusals ---------------------------------------------------------------------

func (h *Handler) refuse(w http.ResponseWriter, status int, e *apierr.Error) {
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

// envelopeWriter turns the SDK's plain-text refusals into section 7.1's
// envelope, and leaves everything else exactly as the SDK wrote it.
//
// A 2xx passes straight through, headers and bytes, so an SSE stream is not
// buffered and `Flush` still reaches the socket. Only a status of 400 or above
// is captured, and only when the body is not already JSON -- a refusal this
// package wrote itself goes through untouched.
type envelopeWriter struct {
	http.ResponseWriter
	log func(msg string, kv ...any)
	// challenge is section 9.2's `WWW-Authenticate`, substituted on a 401 or
	// a 403. See finish() for why it is substituted rather than added.
	challenge string

	wroteHeader bool
	capturing   bool
	// maybeJSONRPC is set when the captured body might be the SDK's own
	// JSON-RPC error response rather than an `http.Error` refusal. finish()
	// confirms it by parsing.
	maybeJSONRPC bool
	status       int
	buf          bytes.Buffer
	done         bool
}

func (w *envelopeWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	// Capture ONLY what `http.Error` wrote, which is the SDK's own idiom for
	// a transport refusal and always `text/plain`. Everything else goes
	// through untouched -- in particular a JSON-RPC error response, which
	// from protocol 2026-07-28 the SDK delivers with an HTTP status of its
	// own (`extractErrorStatus`: -32601 as 404, -32602 as 400) and a JSON-RPC
	// body. Rewriting one of those into a section 7.1 envelope would leave
	// the client with neither shape, and the official client closes the
	// connection when it gets one.
	ct := w.Header().Get("Content-Type")
	if status >= 400 && (ct == "" || strings.HasPrefix(ct, "text/plain")) {
		w.capturing = true
		return
	}
	if status >= 400 && strings.HasPrefix(ct, "application/json") {
		// Possibly the SDK's own JSON-RPC error response with a 4xx status.
		// Capture it and let finish() decide by reading the body, rather than
		// by the Content-Type -- this package's own refusals are
		// `application/json` too, and demoting one of those to 200 would tell
		// a client its unauthenticated call had succeeded.
		w.maybeJSONRPC = true
		w.capturing = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *envelopeWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.capturing {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps the streaming path streaming. A captured refusal has nothing to
// flush yet, so flushing it would commit an empty body.
func (w *envelopeWriter) Flush() {
	if w.capturing {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets net/http reach the real writer for hijacking and for the
// ResponseController shims the SDK uses on a long-lived stream.
func (w *envelopeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish writes the substituted envelope, once.
func (w *envelopeWriter) finish() {
	if w.done || !w.capturing {
		return
	}
	w.done = true

	// A JSON-RPC error response keeps its body and loses its 4xx status.
	//
	// From protocol 2026-07-28 the SDK gives some JSON-RPC errors an HTTP
	// status of their own -- `extractErrorStatus` maps -32601 to 404 and
	// -32602 to 400 (mcp/streamable.go:1042). Its OWN CLIENT then treats any
	// non-2xx that is not transient as a CONNECTION failure and tears the
	// session down (`checkResponse`, mcp/streamable.go:2544). Driven: one
	// `tools/call` naming a tool that does not exist, or one `resources/read`
	// of a missing attachment, ends the session -- so a model that
	// hallucinates a tool name disconnects the connector.
	//
	// That is not a refusal a model can correct itself from, which is the
	// whole point of section 8.2. The error object is passed through
	// unchanged, code and all -- the SDK stays the authority for WHICH error
	// this is -- and only the transport status is demoted to 200, which is
	// what every protocol before 2026-07-28 did and what the client survives.
	if w.maybeJSONRPC && isJSONRPCError(w.buf.Bytes()) {
		w.ResponseWriter.WriteHeader(http.StatusOK)
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		return
	}
	if w.maybeJSONRPC {
		// JSON, but not a JSON-RPC error: this package wrote it, so it is
		// already section 7.1's envelope and keeps its status.
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
		return
	}

	e := errorForStatus(w.status, w.buf.String())
	if w.status == http.StatusUnauthorized || w.status == http.StatusForbidden {
		// SET, not Add: section 8.1 has exactly one challenge and section 9.2
		// fixes its content.
		//
		// The SDK's bearer middleware writes its own, and it cannot write the
		// right one: `RequireBearerTokenOptions.Scopes` is what puts a
		// `scope` parameter in the challenge AND is enforced as an AND over
		// the whole list, and section 8.1's rule is an OR over three, so the
		// options that would produce section 9.2's `scope="messages:read
		// messages:write"` would also refuse a legitimate read-only token.
		// Leaving it off is not neutral: driven with the SDK's own OAuth
		// client, a challenge with no `scope` makes the client fall back to
		// the metadata's `scopes_supported` and ask for `messages:delete`
		// too, which is the exact outcome ChallengeScopes exists to prevent.
		w.Header().Set("WWW-Authenticate", w.challenge)
	}
	requestID := apierr.NewRequestID()
	w.Header().Set("X-Request-Id", requestID)
	if w.log != nil {
		w.log("mcp request refused", "request_id", requestID, "code", string(e.Code), "status", w.status)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_ = json.NewEncoder(w.ResponseWriter).Encode(e.Envelope(requestID))
}

// isJSONRPCError reports whether a body is a JSON-RPC response carrying an
// error object -- which is what distinguishes the SDK's protocol answer from
// this package's own section 7.1 refusal.
func isJSONRPCError(body []byte) bool {
	var parsed struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	return parsed.JSONRPC == "2.0" && len(parsed.Error) > 0
}

// errorForStatus names the section 7.2 code for a transport refusal the SDK
// wrote. The SDK's own sentence is kept as the message, because it says the
// true and specific thing -- which `Accept` was missing, which protocol
// version was not supported -- and inventing a vaguer one here would lose it.
func errorForStatus(status int, body string) *apierr.Error {
	message := strings.TrimSpace(body)
	if message == "" {
		message = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return apierr.InvalidToken(message)
	case http.StatusForbidden:
		return apierr.InsufficientScope(string(authz.ScopeMessagesRead))
	case http.StatusRequestEntityTooLarge:
		return apierr.PayloadTooLarge("the request body", MaxBodyBytes)
	case http.StatusTooManyRequests:
		return apierr.RateLimited(0)
	case http.StatusMethodNotAllowed:
		return apierr.New(apierr.CodeInvalidRequest, message)
	case http.StatusInternalServerError:
		return apierr.New(apierr.CodeInternalError, message)
	default:
		return apierr.New(apierr.CodeInvalidRequest, message)
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

// sameOrigin compares an `Origin` header with the configured public URL on
// **scheme, host and port, and nothing else**, because that is all an origin
// is (RFC 6454).
//
// It used to compare the trimmed strings, which read the same and was not:
// AGENT_GM_PUBLIC_URL is allowed to carry a path -- `loadPublicURL` refuses
// only a query or a fragment -- and an `Origin` header never carries one. So a
// deployment at `https://example.test/gm` answered `403` to every browser
// client on its own correct origin, which is the one case the check exists to
// let through. `https://gm.agent-wx.app` has no path and was never affected,
// which is exactly why it survived a slice.
func sameOrigin(origin, publicURL string) bool {
	o, err := url.Parse(origin)
	if err != nil || o.Scheme == "" || o.Host == "" {
		// `null` and anything unparseable are not this origin. A browser
		// sends `Origin: null` from a sandboxed or redirected context, and
		// treating it as a match would admit the case the header exists to
		// mark as untrusted.
		return false
	}
	p, err := url.Parse(publicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(o.Scheme, p.Scheme) && strings.EqualFold(o.Host, p.Host)
}
