// Package oauth is Agent GM's OAuth 2.1 authorization server (spec section 9).
//
// claude.ai and ChatGPT connectors require one. The design is carried over
// from Agent MX with the Matrix-specific parts removed and the issuer fixed,
// and with one deliberate simplification recorded as decision D20: **client
// resolution is DCR-only**. There is no preregistered client table, no Client
// ID Metadata Document fetch, and therefore no outbound HTTP from this
// package at all -- which removes the SSRF surface rather than defending it.
// Every client this server will ever see registers itself.
//
// Three properties are worth stating once, because the rest of the package
// depends on them:
//
//   - **The issuer is a byte string, not a URL.** `issuer` equals
//     AGENT_GM_PUBLIC_URL exactly and `resource` is that plus `/mcp`. Both are
//     compared as string equality and never as parsed-URL equivalence (section
//     9.2), because a client that normalises differently must still match.
//   - **The request ID conveys no authority.** Every `/oauth/requests` route
//     answers 404 without the signed context cookie, so knowing an
//     `authreq_` ID buys nothing.
//   - **Nothing here stores a credential value.** Enrollment codes,
//     authorization codes and the two browser secrets all reach the database
//     as hashes. See compare.go, which is the only file in this package
//     permitted to compare a caller-supplied value against a stored one, and
//     comparison_audit_test.go, which enforces that by parsing this package's
//     own source, exactly as `internal/authz` does.
package oauth

import (
	"net/http"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// DefaultScopes is what `GET /oauth/authorize` requests when the client sends
// no `scope` (spec section 9.4).
var DefaultScopes = []string{string(authz.ScopeMessagesRead), string(authz.ScopeMessagesWrite)}

// CookieName is the signed context cookie of spec section 9.4.
const CookieName = "agm_oauth_context"

// Config is what a Server needs. Every field is required.
type Config struct {
	// PublicURL is AGENT_GM_PUBLIC_URL. It is the issuer BYTE FOR BYTE, and
	// the canonical resource is it plus "/mcp".
	PublicURL string
	// SigningKey signs the context cookie and derives the form token. It is
	// the data key, domain-separated; it never leaves this process and never
	// reaches a page.
	SigningKey []byte
	// Authz is the credential layer: it mints, rotates and revokes, and it
	// owns the store this package writes its protocol tables through.
	Authz *authz.Service
	// Clock is the injected clock. Nothing in this package calls time.Now.
	Clock clock.Clock
	// Accounts lists the accounts the authorization screen names under its
	// global-scope disclosure line (spec section 9.4). It is a function
	// rather than a slice because the set changes while the server runs, and
	// a screen that named a stale set would be exactly the dishonesty
	// section 9.7 is trying to avoid.
	Accounts func() []Account
	// Log receives one line per refusal. It never receives a credential.
	Log func(msg string, kv ...any)
	// LogWarn receives the refusals an operator has to be able to see in a
	// production log without turning debug on -- today, the cross-origin form
	// post of section 9.9, which is what a wrong `Referrer-Policy` looks like
	// from the server side. It never receives a credential.
	LogWarn func(msg string, kv ...any)
}

// Account is one Google account, as the authorization screen names it.
type Account struct {
	ID      string
	Address string
	Label   string
}

// Server serves the public OAuth routes of spec section 9.1.
type Server struct {
	cfg   Config
	st    *store.Store
	mux   map[string]map[string]http.HandlerFunc
	paths []string
}

// New builds the server. It takes no listener and binds nothing: the caller
// mounts it.
func New(cfg Config) (*Server, error) {
	if cfg.Authz == nil {
		return nil, errConfig("an authz service is required")
	}
	if cfg.Clock == nil {
		return nil, errConfig("a clock is required")
	}
	if strings.TrimSpace(cfg.PublicURL) == "" {
		return nil, errConfig("AGENT_GM_PUBLIC_URL is required")
	}
	if len(cfg.SigningKey) == 0 {
		return nil, errConfig("a signing key is required")
	}
	s := &Server{cfg: cfg, st: cfg.Authz.Store(), mux: map[string]map[string]http.HandlerFunc{}}
	s.route(http.MethodGet, "/.well-known/oauth-protected-resource", s.protectedResource)
	s.route(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", s.protectedResource)
	// OPTIONS, because RFC 9728 section 3.1 makes this document public
	// discovery data and the SDK's handler answers the CORS preflight for it.
	// A browser-based MCP client cannot read the document without one, and a
	// 405 here would make the whole authorization flow unreachable from a
	// page. Only these two paths: nothing else on this server is meant to be
	// read cross-origin.
	s.route(http.MethodOptions, "/.well-known/oauth-protected-resource", s.protectedResource)
	s.route(http.MethodOptions, "/.well-known/oauth-protected-resource/mcp", s.protectedResource)
	s.route(http.MethodGet, "/.well-known/oauth-authorization-server", s.authorizationServer)
	s.route(http.MethodPost, "/oauth/register", s.register)
	s.route(http.MethodGet, "/oauth/authorize", s.authorizeGet)
	s.route(http.MethodPost, "/oauth/authorize", s.authorizePost)
	s.route(http.MethodGet, "/oauth/poll.js", s.pollScript)
	s.route(http.MethodGet, "/oauth/requests/{id}", s.requestPage)
	s.route(http.MethodGet, "/oauth/requests/{id}/status", s.requestStatus)
	s.route(http.MethodPost, "/oauth/requests/{id}/complete", s.requestComplete)
	s.route(http.MethodPost, "/oauth/token", s.token)
	s.route(http.MethodPost, "/oauth/revoke", s.revoke)
	return s, nil
}

func (s *Server) route(method, pattern string, h http.HandlerFunc) {
	if _, ok := s.mux[pattern]; !ok {
		s.mux[pattern] = map[string]http.HandlerFunc{}
		s.paths = append(s.paths, pattern)
	}
	s.mux[pattern][method] = h
}

// Issuer is AGENT_GM_PUBLIC_URL, byte for byte (spec section 9.2).
func (s *Server) Issuer() string { return s.cfg.PublicURL }

// Resource is the canonical resource every access token is bound to: the
// issuer plus "/mcp", with no trailing-slash drift.
func (s *Server) Resource() string { return s.cfg.PublicURL + "/mcp" }

// Paths reports every path this server serves, which lets the process's own
// mux ask "is this mine?" without duplicating the table.
func (s *Server) Paths() []string { return append([]string(nil), s.paths...) }

// Handles reports whether a path belongs to this server. The `/oauth` prefix
// is included whether or not the exact path is served, because an unknown
// path under `/oauth` is this package's 404 to answer (spec section 9.1) and
// not the REST router's business.
func (s *Server) Handles(path string) bool {
	if strings.HasPrefix(path, "/oauth/") || path == "/oauth" {
		return true
	}
	if strings.HasPrefix(path, "/.well-known/oauth-") {
		return true
	}
	return false
}

func (s *Server) now() time.Time { return s.cfg.Clock.Now().UTC() }

func (s *Server) logf(msg string, kv ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(msg, kv...)
	}
}

func (s *Server) warnf(msg string, kv ...any) {
	if s.cfg.LogWarn != nil {
		s.cfg.LogWarn(msg, kv...)
		return
	}
	s.logf(msg, kv...)
}
