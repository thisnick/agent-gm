package oauth

import (
	"net/http"
	"net/url"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
)

// The waiting page and its completion, spec section 9.5.
//
// **Without a valid context cookie every `/oauth/requests` route answers
// 404.** The request ID alone conveys no authority: it appears in a redirect,
// it is logged by proxies, and it is the sort of value that ends up in a chat
// window. Answering 403 would confirm that the request exists, so the answer
// is the one a nonexistent route gives.

// StatusExpired is the status the API reports for a pending request whose
// deadline has passed. It is derived rather than stored: a sweep that had to
// run before an expiry took effect would make expiry depend on a timer.
const StatusExpired = "expired"

// defaultPollInterval is the server's decision, which the page clamps to
// 1-60 seconds.
const defaultPollInterval = 2

// resolveRequest is the gate every `/oauth/requests` route passes through. It
// returns the request only when the cookie proves the caller is the browser
// the authorization screen was served to.
func (s *Server) resolveRequest(r *http.Request) (store.AuthorizationRequest, bool) {
	var zero store.AuthorizationRequest
	handle, ok := s.contextHandle(r)
	if !ok {
		return zero, false
	}
	id := paramsOf(r)["id"]
	if id == "" {
		return zero, false
	}
	req, err := s.st.AuthorizationRequestByID(r.Context(), id)
	if err != nil {
		return zero, false
	}
	if !equalHashHex(sum(domainCookieHandle, handle), req.HandleHash) {
		return zero, false
	}
	return req, true
}

// effectiveStatus folds expiry into the stored status.
func (s *Server) effectiveStatus(req store.AuthorizationRequest) string {
	if req.Status == store.AuthRequestPending && s.now().UnixMilli() >= req.ExpiresAtMS {
		return StatusExpired
	}
	return req.Status
}

// requestPage is `GET /oauth/requests/{id}`. It answers 200 for EVERY state
// including expiry, because it renders a page rather than performing an
// operation (section 9.5) -- a person who left the tab open overnight should
// read what happened, not a 409.
func (s *Server) requestPage(w http.ResponseWriter, r *http.Request) {
	req, ok := s.resolveRequest(r)
	if !ok {
		s.writeNotFound(w)
		return
	}
	handle, _ := s.contextHandle(r)
	status := s.effectiveStatus(req)
	data := waitingPageData{
		RequestID: req.ID,
		Status:    status,
		FormToken: s.formTokenFor(handle),
	}
	switch status {
	case store.AuthRequestPending:
		data.Message = "The owner has to approve this before it can continue."
	case store.AuthRequestApproved:
		data.Done, data.Message = true, "Approved. Continue to finish."
	case store.AuthRequestDenied:
		data.Done, data.Terminal, data.Message = true, true, "The owner denied this request."
	case store.AuthRequestCompleted:
		data.Terminal, data.Message = true, "This request has already been completed."
	default:
		data.Terminal, data.Message = true, "This request expired before it was approved."
	}
	s.renderWaitingPage(w, http.StatusOK, data)
}

type statusDTO struct {
	RequestID           string `json:"request_id"`
	Status              string `json:"status"`
	ExpiresInSeconds    int64  `json:"expires_in_seconds"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// requestStatus is `GET /oauth/requests/{id}/status`.
func (s *Server) requestStatus(w http.ResponseWriter, r *http.Request) {
	req, ok := s.resolveRequest(r)
	if !ok {
		s.writeNotFound(w)
		return
	}
	remaining := (req.ExpiresAtMS - s.now().UnixMilli()) / 1000
	if remaining < 0 {
		remaining = 0
	}
	s.writeJSON(w, http.StatusOK, statusDTO{
		RequestID:           req.ID,
		Status:              s.effectiveStatus(req),
		ExpiresInSeconds:    remaining,
		PollIntervalSeconds: defaultPollInterval,
	})
}

// requestComplete is `POST /oauth/requests/{id}/complete`.
//
// It requires the cookie, the waiting page's form token, and a same-origin
// `Origin`. An approved, unexpired request answers 303 to the EXACT
// registered callback carrying `code`, `state`, `iss` and the granted
// `scope`, and clears the cookie. A DENIED request redirects with
// `error=access_denied`, because a denial is a decision the client is
// entitled to hear. Expired, completed or still-pending answers 409, and a
// completed request can never mint a second code.
func (s *Server) requestComplete(w http.ResponseWriter, r *http.Request) {
	req, ok := s.resolveRequest(r)
	if !ok {
		s.writeNotFound(w)
		return
	}
	if oerr := s.checkOrigin(r); oerr != nil {
		s.writeOAuthError(w, oerr)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "the form could not be read"))
		return
	}
	handle, _ := s.contextHandle(r)
	if !equalConstantTime(r.PostFormValue("form_token"), s.formTokenFor(handle)) {
		s.writeOAuthError(w, badRequest(ErrInvalidRequest, "the form token is not valid"))
		return
	}

	switch s.effectiveStatus(req) {
	case store.AuthRequestDenied:
		s.clearContextCookie(w)
		s.redirectTo(w, req.RedirectURI, url.Values{
			"error":             {ErrAccessDenied},
			"error_description": {"the owner denied this request"},
			"state":             {req.State},
			"iss":               {s.Issuer()},
		})
		return
	case store.AuthRequestApproved:
		// fall through to the mint below
	default:
		s.writeOAuthError(w, statusError(http.StatusConflict, ErrInvalidRequest,
			"this request is not waiting to be completed"))
		return
	}

	granted := req.GrantedScopes
	if granted == "" {
		granted = req.SelectedScopes
	}
	scopes, err := authz.ParseScopeString(granted)
	if err != nil || scopes.Empty() || scopes.Has(authz.ScopeAdmin) {
		s.writeOAuthError(w, statusError(http.StatusConflict, ErrInvalidScope,
			"this request does not carry a usable grant"))
		return
	}

	ttl, err := s.cfg.Authz.SettingDuration(r.Context(), authz.SettingOAuthAuthorizationCodeTTL)
	if err != nil {
		ttl = 2 * time.Minute
	}
	value, err := newSecretValue()
	if err != nil {
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the code could not be issued"))
		return
	}
	now := s.now()

	var completed bool
	err = s.st.AuthzTx(r.Context(), func(t *store.AuthzTx) error {
		ok, cerr := t.CompleteAuthorizationRequest(req.ID)
		if cerr != nil {
			return cerr
		}
		if !ok {
			completed = false
			return nil
		}
		completed = true
		if cerr := t.CreateAuthorizationCode(store.AuthorizationCode{
			CodeHash:      authorizationCodeHash(value),
			RequestID:     req.ID,
			ClientID:      req.ClientID,
			RedirectURI:   req.RedirectURI,
			CodeChallenge: req.CodeChallenge,
			Scopes:        scopes.String(),
			Resource:      req.Resource,
			ExpiresAtMS:   now.Add(ttl).UnixMilli(),
			CreatedAtMS:   now.UnixMilli(),
		}); cerr != nil {
			return cerr
		}
		return t.AppendAudit("authorization.approved", "ok", "", "", req.Source,
			mustJSON(map[string]any{
				"request_id": req.ID,
				"client_id":  req.ClientID,
				"scopes":     scopes.Strings(),
				"stage":      "code_issued",
			}))
	})
	if err != nil {
		s.writeOAuthError(w, statusError(http.StatusInternalServerError, ErrServerError,
			"the code could not be issued"))
		return
	}
	if !completed {
		// The conditional UPDATE lost, which means somebody completed this
		// request first. A completed request can never mint a second code.
		s.writeOAuthError(w, statusError(http.StatusConflict, ErrInvalidRequest,
			"this request has already been completed"))
		return
	}

	s.clearContextCookie(w)
	s.redirectTo(w, req.RedirectURI, url.Values{
		"code":  {value},
		"state": {req.State},
		"iss":   {s.Issuer()},
		"scope": {scopes.String()},
	})
}

// redirectTo sends a 303 to the exact registered callback with the given
// parameters merged into whatever query it already carried.
func (s *Server) redirectTo(w http.ResponseWriter, redirectURI string, params url.Values) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidRedirectURI, "the redirect URI could not be used"))
		return
	}
	q := target.Query()
	for k, values := range params {
		for _, v := range values {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	target.RawQuery = q.Encode()
	setSecurityHeaders(w)
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusSeeOther)
}
