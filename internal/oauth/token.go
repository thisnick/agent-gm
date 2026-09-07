package oauth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
)

// `/oauth/token` and `/oauth/revoke`, spec section 9.6.
//
// Both are `application/x-www-form-urlencoded` with `Cache-Control:
// no-store`. Neither carries a client secret, because there is none.

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if oerr := s.requireForm(r); oerr != nil {
		s.writeOAuthError(w, oerr)
		return
	}
	source := s.source(r)
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.grantAuthorizationCode(w, r, source)
	case "refresh_token":
		s.grantRefreshToken(w, r, source)
	case "":
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "grant_type is required"))
	default:
		s.writeOAuthError(w, badRequest(ErrUnsupportedGrantType,
			"this server supports authorization_code and refresh_token"))
	}
}

// grantAuthorizationCode exchanges a code for a session.
//
// `resource` is MANDATORY at /oauth/authorize and OPTIONAL here: this server
// has exactly one audience, so an omitted value is read as the canonical
// resource and a present one must match it. RFC 8707 section 2.2 permits that
// for a single-audience server, and the binding that matters was fixed at
// authorization time.
func (s *Server) grantAuthorizationCode(w http.ResponseWriter, r *http.Request, source string) {
	presentedCode := r.PostFormValue("code")
	clientID := r.PostFormValue("client_id")
	redirectURI := r.PostFormValue("redirect_uri")
	verifier := r.PostFormValue("code_verifier")
	resource := r.PostFormValue("resource")

	if presentedCode == "" || clientID == "" || redirectURI == "" || verifier == "" {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"code, client_id, redirect_uri and code_verifier are all required"))
		return
	}
	if resource != "" && resource != s.Resource() {
		s.writeOAuthError(w, badRequest(ErrInvalidTarget, "resource must be "+s.Resource()))
		return
	}

	session, err := s.cfg.Authz.ExchangeAuthorizationCode(r.Context(), authz.CodeExchange{
		CodeHash: authorizationCodeHash(presentedCode),
		ClientID: clientID,
		Source:   source,
		// The binding check. It runs after the code is known to exist and to
		// be unconsumed, and its failure CONSUMES the code -- which is what
		// stops a verifier from being guessed by retrying (section 9.6).
		Verify: func(code store.AuthorizationCode) error {
			if !equalConstantTime(clientID, code.ClientID) {
				return fmt.Errorf("%w: the code was issued to another client", authz.ErrInvalidGrant)
			}
			// Exact equality, not the port-agnostic loopback match: the
			// relaxation of RFC 8252 section 7.3 belongs at authorization
			// time, and the token endpoint requires the URI the code was
			// bound to (section 9.3).
			if !equalConstantTime(redirectURI, code.RedirectURI) {
				return fmt.Errorf("%w: redirect_uri does not match the code", authz.ErrInvalidGrant)
			}
			if !verifyPKCE(verifier, code.CodeChallenge) {
				return fmt.Errorf("%w: the PKCE verifier does not match", authz.ErrInvalidGrant)
			}
			if code.Resource != s.Resource() {
				return fmt.Errorf("%w: the code is bound to another resource", authz.ErrInvalidGrant)
			}
			return nil
		},
	})
	if err != nil {
		s.logf("oauth token exchange refused", "source", source)
		s.writeOAuthError(w, translate(err))
		return
	}
	s.writeSession(w, session)
}

func (s *Server) grantRefreshToken(w http.ResponseWriter, r *http.Request, source string) {
	presented := r.PostFormValue("refresh_token")
	clientID := r.PostFormValue("client_id")
	if presented == "" || clientID == "" {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"refresh_token and client_id are required"))
		return
	}
	var requested []string
	if scope := strings.TrimSpace(r.PostFormValue("scope")); scope != "" {
		requested = strings.Fields(scope)
	}
	session, err := s.cfg.Authz.RefreshOAuth(r.Context(), presented, clientID, requested, source)
	if err != nil {
		// A widening `scope` is invalid_scope and does NOT spend the
		// presented token; the credential layer enforces that, and this
		// translation keeps the two answers distinguishable to an honest
		// client without telling an attacker anything about the token.
		if errors.Is(err, authz.ErrInvalidScope) {
			s.writeOAuthError(w, badRequest(ErrInvalidScope,
				"scope may only narrow what this authorization already holds"))
			return
		}
		s.writeOAuthError(w, translate(err))
		return
	}
	s.writeSession(w, session)
}

func (s *Server) writeSession(w http.ResponseWriter, session *authz.Session) {
	expiresIn := int64(session.AccessTokenExpiresAt.Sub(s.now()).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	s.writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  session.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		RefreshToken: session.RefreshToken,
		Scope:        session.Scopes.String(),
	})
}

// revoke is `POST /oauth/revoke` (RFC 7009).
//
// It ALWAYS answers 200. A token belonging to another client, or to nobody,
// is ignored without confirming that it exists; revoking any token of a grant
// revokes the whole grant. The only thing this endpoint may refuse is a
// budget (section 9.8), because a 429 that never distinguishes a real token
// from a guess tells an attacker nothing.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if oerr := s.requireForm(r); oerr != nil {
		s.writeOAuthError(w, oerr)
		return
	}
	err := s.cfg.Authz.RevokeToken(r.Context(),
		r.PostFormValue("token"), r.PostFormValue("client_id"), s.source(r))
	if err != nil {
		var limited *authz.RateLimitedError
		if errors.As(err, &limited) {
			s.writeOAuthError(w, translate(err))
			return
		}
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the revocation could not be recorded"))
		return
	}
	setSecurityHeaders(w)
	w.WriteHeader(http.StatusOK)
}

// requireForm reads and bounds a form body. The content type is checked
// because a JSON body posted to a form endpoint would parse as an empty form
// and be answered as "grant_type is required", which sends an implementer
// looking in the wrong place.
func (s *Server) requireForm(r *http.Request) *oauthError {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if ct != "application/x-www-form-urlencoded" {
		return badRequest(ErrInvalidRequest,
			"this endpoint takes application/x-www-form-urlencoded")
	}
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return statusError(http.StatusRequestEntityTooLarge, ErrInvalidRequest,
				"the request body is larger than 1 MiB")
		}
		return badRequest(ErrInvalidRequest, "the form could not be read")
	}
	return nil
}
