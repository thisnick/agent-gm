package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
)

// POST /oauth/authorize, spec section 9.4.
//
// The verification order below IS the contract, and it is written as one
// straight line so a reader can check it against the spec sentence by
// sentence:
//
//	Origin when present, the cookie, the signed context, that the cookie's
//	handle hashes to the context, the form token, every hidden echo, that
//	the client and redirect are still valid, and that the selected scopes are
//	a nonempty subset of the requested set. ONLY THEN is the enrollment code
//	examined.
//
// The order matters because everything before the last step is a check on the
// BROWSER, and the enrollment code is a check on the OWNER's secret. A server
// that examined the code first would let an attacker with no session at all
// spend an owner's code attempts -- and, worse, learn from the difference in
// responses which codes exist.
func (s *Server) authorizePost(w http.ResponseWriter, r *http.Request) {
	source := s.source(r)

	// The per-source enrollment budget is checked BEFORE the submission is
	// examined, so loading a fresh authorization page does not reset it
	// (section 9.4, test 6).
	if err := s.cfg.Authz.Durable.Check(r.Context(), authz.LimitEnrollmentSource, source); err != nil {
		s.writeOAuthError(w, translate(err))
		return
	}

	// The client id is read as a function, and so only on the refusal path:
	// touching the body before this check would cache `ParseForm`'s result
	// and swallow the malformed-body 400 the next step owes. The value is the
	// form's own hidden echo and is not yet verified -- it is logged, not
	// trusted, and only so an operator can tell which connector a refusal
	// belongs to.
	if oerr := s.checkOrigin(w, r, func() string { return r.PostFormValue("client_id") }); oerr != nil {
		s.writeOAuthError(w, oerr)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "the form could not be read"))
		return
	}

	handle, ok := s.contextHandle(r)
	if !ok {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"this page has expired; start the authorization again"))
		return
	}
	raw, ok := s.verifyPayload(r.PostFormValue("context"))
	if !ok {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"this page has expired; start the authorization again"))
		return
	}
	var ctx contextPayload
	if err := json.Unmarshal(raw, &ctx); err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "the form context is not readable"))
		return
	}
	// The cookie's handle must hash to the context's. This is what binds the
	// two halves together: a context lifted from one browser is useless in
	// another, because the other browser does not hold the handle.
	if !equalHashHex(sum(domainCookieHandle, handle), ctx.HandleHash) {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"this page does not belong to this browser session"))
		return
	}
	if s.now().UnixMilli() >= ctx.ExpiresAtMS {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest,
			"this page has expired; start the authorization again"))
		return
	}
	if !equalConstantTime(r.PostFormValue("form_token"), s.formTokenFor(handle)) {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "the form token is not valid"))
		return
	}

	// Every hidden echo. A form that disagrees with its own signed context in
	// any field is refused rather than resolved in favour of one of them.
	echoes := []struct{ presented, expected string }{
		{r.PostFormValue("client_id"), ctx.ClientID},
		{r.PostFormValue("redirect_uri"), ctx.RedirectURI},
		{r.PostFormValue("state"), ctx.State},
		{r.PostFormValue("code_challenge"), ctx.Challenge},
		{r.PostFormValue("code_challenge_method"), ctx.Method},
		{r.PostFormValue("resource"), ctx.Resource},
		{r.PostFormValue("scope"), strings.Join(ctx.Scopes, " ")},
	}
	for _, e := range echoes {
		if !equalConstantTime(e.presented, e.expected) {
			s.writeOAuthError(w, badRequest(ErrInvalidRequest,
				"the form does not match the authorization it belongs to"))
			return
		}
	}

	// The client and the redirect are re-checked: a registration can be
	// revoked or expire while the owner is reading the screen.
	client, err := s.st.OAuthClientByID(r.Context(), ctx.ClientID)
	if err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidClient, "unknown client_id"))
		return
	}
	if _, ok := MatchRegisteredRedirect(client.RedirectURIs, ctx.RedirectURI); !ok {
		s.writeOAuthError(w, &oauthError{Status: http.StatusBadRequest, Code: ErrInvalidRedirectURI,
			Description: "redirect_uri is not registered for this client"})
		return
	}

	requested, err := authz.ParseScopes(ctx.Scopes)
	if err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidScope, "the requested scope is not valid"))
		return
	}
	selected, err := authz.ParseScopes(r.PostForm["scope_selected"])
	if err != nil || selected.Empty() || selected.Widens(requested) {
		s.writeOAuthError(w, badRequest(ErrInvalidScope,
			"choose at least one of the scopes this client asked for"))
		return
	}

	// The per-context budget, still before the code is examined. Per signed
	// context AND per source: an attacker who reloads the page to get a new
	// context still meets the source bucket, and an attacker on many sources
	// still meets the context bucket.
	contextKey := ctx.HandleHash
	if err := s.cfg.Authz.Durable.Check(r.Context(), authz.LimitEnrollmentContext, contextKey); err != nil {
		s.writeOAuthError(w, translate(err))
		return
	}

	page := approvalPageData{
		Context:     r.PostFormValue("context"),
		FormToken:   s.formTokenFor(handle),
		ClientID:    client.ID,
		ClientName:  client.Name,
		RedirectURI: ctx.RedirectURI,
		State:       ctx.State,
		Challenge:   ctx.Challenge,
		Method:      ctx.Method,
		Resource:    ctx.Resource,
		ScopeParam:  strings.Join(ctx.Scopes, " "),
		Scopes:      ctx.Scopes,
		Selected:    selected.Strings(),
		Accounts:    s.accounts(),
	}

	// ONLY NOW is the enrollment code examined.
	presented := r.PostFormValue("enrollment_code")
	code, cerr := s.st.EnrollmentCodeByHash(r.Context(), EnrollmentCodeHash(presented))
	if cerr != nil || !s.enrollmentUsable(code) {
		if cerr != nil && !errors.Is(cerr, store.ErrEnrollmentCodeNotFound) {
			s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
				"the code could not be checked"))
			return
		}
		// The code the caller presented is not named to them, but an
		// expiry that has just been observed is recorded for the owner
		// (section 12.4). noteEnrollmentExpired is a no-op for a code that
		// was revoked, consumed or simply unknown, so this stays one
		// indistinguishable refusal.
		if cerr == nil {
			s.noteEnrollmentExpired(r.Context(), code)
		}
		// ONE generic message, byte-identical for unknown, expired, revoked
		// and consumed. No pending request is created and nothing is
		// consumed (section 9.4, test 5).
		s.recordEnrollmentFailure(r, source, contextKey)
		page.Message = GenericEnrollmentFailure
		s.renderApprovalPage(w, http.StatusOK, page)
		return
	}

	ceiling, err := authz.ParseScopeString(code.Scopes)
	if err != nil {
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the code could not be checked"))
		return
	}
	if selected.Widens(ceiling) {
		// Above the code's ceiling: re-render showing the access the code
		// DOES allow, and leave the code redeemable. This is deliberately
		// not the generic failure -- the owner's own code is valid and the
		// only thing wrong is the selection, which they can correct.
		page.Message = "That code allows " + ceiling.String() + ". Choose within it."
		page.Allowed = ceiling.Strings()
		s.renderApprovalPage(w, http.StatusOK, page)
		return
	}

	requestID, oerr := s.createPendingRequest(r, pendingRequest{
		Client:     client,
		Context:    ctx,
		Selected:   selected,
		Enrollment: code,
		Handle:     handle,
		Source:     source,
	})
	if oerr != nil {
		s.writeOAuthError(w, oerr)
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Location", "/oauth/requests/"+requestID)
	w.WriteHeader(http.StatusSeeOther)
}

// enrollmentUsable folds the four indistinguishable failures into one
// boolean, at one place, so that no caller can accidentally report which of
// them it was.
func (s *Server) enrollmentUsable(code store.EnrollmentCode) bool {
	switch {
	case code.Revoked(), code.Consumed():
		return false
	case s.now().UnixMilli() >= code.ExpiresAtMS:
		return false
	default:
		return true
	}
}

func (s *Server) recordEnrollmentFailure(r *http.Request, source, contextKey string) {
	if _, err := s.cfg.Authz.Durable.RecordFailure(r.Context(), authz.LimitEnrollmentSource, source); err != nil {
		s.logf("recording an enrollment failure", "error", err.Error())
	}
	if _, err := s.cfg.Authz.Durable.RecordFailure(r.Context(), authz.LimitEnrollmentContext, contextKey); err != nil {
		s.logf("recording an enrollment failure", "error", err.Error())
	}
}

type pendingRequest struct {
	Client     store.OAuthClient
	Context    contextPayload
	Selected   authz.ScopeSet
	Enrollment store.EnrollmentCode
	Handle     string
	Source     string
}

// createPendingRequest writes the request, consumes the code, activates the
// registration and audits all three -- in ONE transaction. A code consumed
// without a request would be an owner's code lost to nothing; a request
// created without consuming the code would let one code raise many.
func (s *Server) createPendingRequest(r *http.Request, in pendingRequest) (string, *oauthError) {
	ttl, err := s.cfg.Authz.SettingDuration(r.Context(), authz.SettingOAuthAuthorizationRequestTTL)
	if err != nil {
		ttl = 15 * time.Minute
	}
	now := s.now()
	id := store.AuthorizationRequestID()
	formToken := s.formTokenFor(in.Handle)

	var consumed bool
	err = s.st.AuthzTx(r.Context(), func(t *store.AuthzTx) error {
		ok, cerr := t.ConsumeEnrollmentCode(in.Enrollment.ID, id)
		if cerr != nil {
			return cerr
		}
		if !ok {
			consumed = false
			return nil
		}
		consumed = true
		if cerr := t.CreateAuthorizationRequest(store.AuthorizationRequest{
			ID:                  id,
			ClientID:            in.Client.ID,
			RedirectURI:         in.Context.RedirectURI,
			State:               in.Context.State,
			CodeChallenge:       in.Context.Challenge,
			CodeChallengeMethod: in.Context.Method,
			Resource:            in.Context.Resource,
			RequestedScopes:     strings.Join(in.Context.Scopes, " "),
			SelectedScopes:      in.Selected.String(),
			EnrollmentCodeID:    in.Enrollment.ID,
			Status:              store.AuthRequestPending,
			HandleHash:          in.Context.HandleHash,
			FormTokenHash:       formTokenHash(formToken),
			Source:              in.Source,
			ExpiresAtMS:         now.Add(ttl).UnixMilli(),
			CreatedAtMS:         now.UnixMilli(),
		}); cerr != nil {
			return cerr
		}
		if cerr := t.ActivateOAuthClient(in.Client.ID); cerr != nil {
			return cerr
		}
		if cerr := t.AppendAudit("enrollment.consumed", "ok", "", "", in.Source,
			mustJSON(map[string]any{
				"enrollment_code_id": in.Enrollment.ID,
				"label":              in.Enrollment.Label,
				"request_id":         id,
			})); cerr != nil {
			return cerr
		}
		return t.AppendAudit("authorization.created", "ok", "", "", in.Source,
			mustJSON(map[string]any{
				"request_id":       id,
				"client_id":        in.Client.ID,
				"requested_scopes": in.Context.Scopes,
				"selected_scopes":  in.Selected.Strings(),
			}))
	})
	if err != nil {
		return "", statusError(http.StatusInternalServerError, ErrServerError,
			"the authorization could not be recorded")
	}
	if !consumed {
		// Somebody redeemed the code between the read and the transaction.
		// That is the consumed case, and it answers with the same generic
		// message as every other one.
		return "", badRequest(ErrInvalidRequest, GenericEnrollmentFailure)
	}
	return id, nil
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// checkOrigin refuses a cross-origin form post. An absent Origin is accepted:
// a non-browser client does not send one, and this endpoint is reachable by
// `agm auth login`'s own browser as well as by a person's.
//
// The check stays exact -- `null` is a refusal, not an exemption, because a
// sandboxed frame and a cross-origin redirect both post with `Origin: null`.
// What it does NOT do is refuse in silence: the body names the origin it
// received and the server writes one warn line, because the one bug this
// check has ever caught in production was our own page telling the browser
// `Referrer-Policy: no-referrer` and so getting `Origin: null` back
// (see setPageSecurityHeaders), and a refusal that named no value made that
// indistinguishable from an attack.
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request, clientID func() string) *oauthError {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	if origin != s.Issuer() {
		s.refusedOrigin(w, r, origin, clientID)
		return statusError(http.StatusForbidden, ErrInvalidRequest,
			"this form may only be submitted from "+s.Issuer()+
				"; received Origin "+strconv.Quote(clip(origin)))
	}
	return nil
}

// requireSameOrigin is checkOrigin plus the requirement that the header be
// there at all. It guards `POST /oauth/requests/{id}/complete`, which section
// 9.5 says takes a same-origin Origin -- and a check that accepted its
// absence would be satisfied by any client that simply omitted it.
func (s *Server) requireSameOrigin(w http.ResponseWriter, r *http.Request, clientID func() string) *oauthError {
	if r.Header.Get("Origin") == "" {
		s.refusedOrigin(w, r, "", clientID)
		return statusError(http.StatusForbidden, ErrInvalidRequest,
			"this form may only be submitted from "+s.Issuer()+", and carries no Origin")
	}
	return s.checkOrigin(w, r, clientID)
}

// refusedOrigin logs the refusal and stamps the answer with the same request
// id the log line carries, so an owner reading a 403 in a browser and an
// operator reading the log are looking at one event. It logs no credential:
// an origin, a client id and a path are all public to whoever sent them.
func (s *Server) refusedOrigin(w http.ResponseWriter, r *http.Request, origin string, clientID func() string) {
	requestID := apierr.NewRequestID()
	w.Header().Set("X-Request-Id", requestID)
	received := "(absent)"
	if origin != "" {
		received = clip(origin)
	}
	s.warnf("refusing a form post from another origin",
		"received_origin", received,
		"expected_origin", s.Issuer(),
		"request_id", requestID,
		"client_id", clip(clientID()),
		"path", r.URL.Path)
}

// clip bounds a value copied from a request into a log line or an error body.
// The origin and the client id are whatever the sender wrote.
func clip(v string) string {
	const max = 200
	if len(v) <= max {
		return v
	}
	return v[:max] + "..."
}

func (s *Server) source(r *http.Request) string {
	return s.cfg.Authz.Sources.Resolve(r.RemoteAddr, r.Header).Value
}
