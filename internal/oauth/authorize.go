package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
)

// The authorization endpoint, spec section 9.4.

// GenericEnrollmentFailure is the ONE message an invalid enrollment code
// produces, byte-identical for unknown, expired, revoked and consumed (spec
// section 9.4, section 16 Slice 3 test 5).
//
// It is a single exported constant rather than four call sites that happen to
// agree, because four sites that happen to agree are four sites one edit away
// from telling an attacker which of their guesses was once a real code.
const GenericEnrollmentFailure = "That enrollment code was not accepted. Check it and try again, or ask the owner for a new one."

// GlobalScopeDisclosure is the fixed line rendered verbatim ABOVE the scope
// checkboxes, because scopes are global across accounts (D29, sections 9.4
// and 9.7).
//
// It is part of the screen's contract and section 16 Slice 3 test 18 asserts
// it as a string. That assertion is what makes section 9.7's claim to honesty
// testable rather than aspirational: the owner approves knowing that a token
// reaches every account this server holds, including ones added later.
const GlobalScopeDisclosure = "This will let the client read and send as any Google account on this server, including accounts added later."

// authorizeParams is one parsed authorization request.
type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Scopes              []string
}

// contextPayload is the signed `agm_oauth_context` content and the hidden
// context field's content. It carries the hash of the cookie's handle, never
// the handle: a payload that leaked would not let anybody forge the cookie.
type contextPayload struct {
	HandleHash  string   `json:"h"`
	ClientID    string   `json:"c"`
	RedirectURI string   `json:"r"`
	State       string   `json:"s"`
	Challenge   string   `json:"cc"`
	Method      string   `json:"cm"`
	Resource    string   `json:"res"`
	Scopes      []string `json:"sc"`
	ExpiresAtMS int64    `json:"exp"`
}

func (s *Server) authorizeGet(w http.ResponseWriter, r *http.Request) {
	p := authorizeParams{
		ResponseType:        r.URL.Query().Get("response_type"),
		ClientID:            r.URL.Query().Get("client_id"),
		RedirectURI:         r.URL.Query().Get("redirect_uri"),
		State:               r.URL.Query().Get("state"),
		CodeChallenge:       r.URL.Query().Get("code_challenge"),
		CodeChallengeMethod: r.URL.Query().Get("code_challenge_method"),
		Resource:            r.URL.Query().Get("resource"),
		Scopes:              strings.Fields(r.URL.Query().Get("scope")),
	}

	// An unknown client and an unregistered redirect answer 4xx and NEVER
	// redirect (RFC 6749 section 4.1.2.1): until the callback is verified
	// there is nowhere safe to send an error, and redirecting to an
	// unverified URI is how an open redirector is built.
	client, err := s.st.OAuthClientByID(r.Context(), p.ClientID)
	if err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			s.writeOAuthError(w, badRequest(ErrInvalidClient, "unknown client_id"))
			return
		}
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the request could not be read"))
		return
	}
	if s.registrationExpired(client) {
		s.writeOAuthError(w, badRequest(ErrInvalidClient, "this registration has expired"))
		return
	}
	verified, ok := MatchRegisteredRedirect(client.RedirectURIs, p.RedirectURI)
	if !ok {
		s.writeOAuthError(w, &oauthError{Status: http.StatusBadRequest, Code: ErrInvalidRedirectURI,
			Description: "redirect_uri is not registered for this client"})
		return
	}
	p.RedirectURI = verified

	// Every later failure redirects to the VERIFIED callback carrying error,
	// state and the RFC 9207 iss.
	// `errCode` rather than `code`: in this package a bare `code` is an
	// authorization CODE, and the comparison audit of section 12.1 treats
	// that name as secret-derived. Naming an OAuth error string `code` here
	// would make the audit either wrong or noisy, and a noisy audit is one
	// somebody eventually silences.
	if errCode, desc := s.validateAuthorize(&p); errCode != "" {
		s.redirectError(w, p, errCode, desc)
		return
	}

	handle, err := newSecretValue()
	if err != nil {
		s.redirectError(w, p, ErrServerError, "the screen could not be prepared")
		return
	}
	ttl, err := s.cfg.Authz.SettingDuration(r.Context(), authz.SettingOAuthAuthorizationRequestTTL)
	if err != nil {
		ttl = 15 * time.Minute
	}
	payload := contextPayload{
		HandleHash:  handleHash(handle),
		ClientID:    client.ID,
		RedirectURI: p.RedirectURI,
		State:       p.State,
		Challenge:   p.CodeChallenge,
		Method:      p.CodeChallengeMethod,
		Resource:    p.Resource,
		Scopes:      p.Scopes,
		ExpiresAtMS: s.now().Add(ttl).UnixMilli(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.redirectError(w, p, ErrServerError, "the screen could not be prepared")
		return
	}
	s.setContextCookie(w, handle)
	s.renderApprovalPage(w, http.StatusOK, approvalPageData{
		Context:     s.signPayload(raw),
		FormToken:   s.formTokenFor(handle),
		ClientID:    client.ID,
		ClientName:  client.Name,
		RedirectURI: p.RedirectURI,
		State:       p.State,
		Challenge:   p.CodeChallenge,
		Method:      p.CodeChallengeMethod,
		Resource:    p.Resource,
		ScopeParam:  strings.Join(p.Scopes, " "),
		Scopes:      p.Scopes,
		Accounts:    s.accounts(),
	})
}

// validateAuthorize applies section 9.4's refusal table, in its order. Every
// row of it redirects; the two that do not (unknown client, unregistered
// redirect) are handled by the caller before this runs.
func (s *Server) validateAuthorize(p *authorizeParams) (code, description string) {
	if p.ResponseType != "code" {
		return ErrUnsupportedResponseType, "the only response_type is `code`"
	}
	if strings.TrimSpace(p.State) == "" {
		return ErrInvalidRequest, "state is required"
	}
	// RFC 7636 defaults an omitted method to `plain`, which is not
	// supported. The method must be PRESENT and S256; defaulting it would
	// silently accept an unprotected exchange (section 9.4, test 4).
	if p.CodeChallengeMethod == "" {
		return ErrInvalidRequest, "code_challenge_method is required and must be S256"
	}
	if p.CodeChallengeMethod != "S256" {
		return ErrInvalidRequest, "the only code_challenge_method is S256"
	}
	if !validChallenge(p.CodeChallenge) {
		return ErrInvalidRequest, "code_challenge must be a base64url S256 challenge"
	}
	if p.Resource != s.Resource() {
		return ErrInvalidTarget, "resource must be " + s.Resource()
	}
	if len(p.Scopes) == 0 {
		p.Scopes = append([]string(nil), DefaultScopes...)
	}
	set, err := authz.ParseScopes(p.Scopes)
	if err != nil || set.Empty() {
		return ErrInvalidScope, "the requested scope is not one this server issues"
	}
	// `admin` is never enrollable and is invalid_scope here. The only way to
	// hold it is the admin secret (section 9.7).
	if set.Has(authz.ScopeAdmin) {
		return ErrInvalidScope, "admin is never issued through OAuth"
	}
	p.Scopes = set.Strings()
	return "", ""
}

// validChallenge accepts a base64url S256 challenge: 43 characters, the
// base64url alphabet, no padding.
func validChallenge(v string) bool {
	if len(v) != 43 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// redirectError sends the caller back to the verified callback carrying
// `error`, `state` and the RFC 9207 `iss` -- byte-exact, because a client that
// compares the issuer it asked for against the one it got back will reject a
// mismatch (section 16 Slice 3 test 4).
func (s *Server) redirectError(w http.ResponseWriter, p authorizeParams, code, description string) {
	target, err := url.Parse(p.RedirectURI)
	if err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidRedirectURI, "the redirect URI could not be used"))
		return
	}
	q := target.Query()
	q.Set("error", code)
	if description != "" {
		q.Set("error_description", description)
	}
	if p.State != "" {
		q.Set("state", p.State)
	}
	q.Set("iss", s.Issuer())
	target.RawQuery = q.Encode()
	setSecurityHeaders(w)
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) registrationExpired(c store.OAuthClient) bool {
	if c.Activated() || c.ExpiresAtMS == 0 {
		return false
	}
	return s.now().UnixMilli() >= c.ExpiresAtMS
}

func (s *Server) setContextCookie(w http.ResponseWriter, handle string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    s.signPayload([]byte(handle)),
		Path:     "/oauth",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearContextCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/oauth",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// contextHandle reads the signed cookie and returns the handle it carries.
// Every caller treats a false here as "there is no context", which on the
// `/oauth/requests` routes means 404: the request ID alone conveys no
// authority (section 9.5).
func (s *Server) contextHandle(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}
	raw, ok := s.verifyPayload(c.Value)
	if !ok {
		return "", false
	}
	return string(raw), true
}

func (s *Server) accounts() []Account {
	if s.cfg.Accounts == nil {
		return nil
	}
	return s.cfg.Accounts()
}
