package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/store"
)

// Dynamic client registration, RFC 7591 and spec section 9.3.
//
// `POST /oauth/register` answers 201 with a `client_id` and NO
// `client_secret`. Public native clients only, which is the whole security
// model: there is no secret to leak, so PKCE and the registered redirect are
// what bind an authorization to the client that started it.

// RegistrationTTL is how long a registration lives before an authorization
// activates it (spec section 9.3).
const RegistrationTTL = 24 * time.Hour

// RegistrationsPerSourcePerHour is section 9.3's budget. It is counted over
// rows rather than a token bucket, so it survives a restart -- a registration
// is a durable object, and forgiving it on restart would forgive something
// that is still there.
const RegistrationsPerSourcePerHour = 20

// registrationRequest is the subset of RFC 7591 metadata this server reads.
// Unknown members are accepted and preserved (RFC 7591 requires a server to
// ignore metadata it does not understand), but the four that carry security
// meaning are validated.
type registrationRequest struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// allowedGrants is the closed set a registration's `grant_types` must be a
// subset of.
var allowedGrants = map[string]bool{"authorization_code": true, "refresh_token": true}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	body, readErr := readBody(r)
	if readErr != nil {
		s.writeOAuthError(w, readErr)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		s.writeOAuthError(w, badRequest(ErrInvalidClientMetadata,
			"registration metadata must be application/json"))
		return
	}
	var req registrationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeOAuthError(w, badRequest(ErrInvalidClientMetadata,
			"the registration body is not valid JSON"))
		return
	}

	source := s.source(r)
	registered, oerr := s.Register(r.Context(), req, source)
	if oerr != nil {
		s.logf("oauth registration refused", "error", oerr.Code, "source", source)
		s.writeOAuthError(w, oerr)
		return
	}
	s.writeJSON(w, http.StatusCreated, registered)
}

// Register is the registration decision, separated from the transport so a
// test can drive it directly and so the refusal order is one readable list.
func (s *Server) Register(ctx context.Context, req registrationRequest, source string) (*registrationResponse, *oauthError) {
	// A client-chosen client_id is refused. RFC 7591 lets a server ignore
	// one; ignoring it would leave the client believing it registered an ID
	// it did not get, and the first authorization would fail with an error
	// naming a client nobody has ever heard of.
	if strings.TrimSpace(req.ClientID) != "" {
		return nil, badRequest(ErrInvalidClientMetadata,
			"client_id is chosen by this server and may not be supplied")
	}
	// Public native clients only.
	if req.TokenEndpointAuthMethod == "" {
		req.TokenEndpointAuthMethod = "none"
	}
	if req.TokenEndpointAuthMethod != "none" {
		return nil, badRequest(ErrInvalidClientMetadata,
			"token_endpoint_auth_method must be `none`; this server registers public clients only")
	}
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range req.GrantTypes {
		if !allowedGrants[g] {
			return nil, badRequest(ErrInvalidClientMetadata,
				"grant_types must be a subset of authorization_code and refresh_token, not "+g)
		}
	}
	if len(req.ResponseTypes) == 0 {
		req.ResponseTypes = []string{"code"}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			return nil, badRequest(ErrInvalidClientMetadata,
				"the only response type is `code`, not "+rt)
		}
	}
	if len(req.RedirectURIs) == 0 {
		return nil, &oauthError{Status: http.StatusBadRequest, Code: ErrInvalidRedirectURI,
			Description: "at least one redirect URI is required"}
	}
	if len(req.RedirectURIs) > MaxRedirectURIs {
		return nil, &oauthError{Status: http.StatusBadRequest, Code: ErrInvalidRedirectURI,
			Description: "at most 10 redirect URIs may be registered"}
	}
	for _, uri := range req.RedirectURIs {
		if err := ValidateRedirectURI(uri); err != nil {
			return nil, &oauthError{Status: http.StatusBadRequest, Code: ErrInvalidRedirectURI,
				Description: err.Error()}
		}
	}

	now := s.now()
	since := now.Add(-time.Hour).UnixMilli()
	count, err := s.st.CountRegistrationsSince(ctx, source, since)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, ErrServerError,
			"the registration could not be recorded")
	}
	if count >= RegistrationsPerSourcePerHour {
		return nil, &oauthError{
			Status:      http.StatusTooManyRequests,
			Code:        ErrTemporarilyUnavailable,
			Description: "too many registrations from this source; retry later",
			RetryAfter:  time.Hour,
		}
	}

	metadata, err := json.Marshal(req)
	if err != nil {
		return nil, statusError(http.StatusInternalServerError, ErrServerError,
			"the registration could not be recorded")
	}
	client := store.OAuthClient{
		ID:                      store.ClientID(),
		Name:                    req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              req.GrantTypes,
		ResponseTypes:           req.ResponseTypes,
		TokenEndpointAuthMethod: "none",
		MetadataJSON:            string(metadata),
		Source:                  source,
		ExpiresAtMS:             now.Add(RegistrationTTL).UnixMilli(),
		CreatedAtMS:             now.UnixMilli(),
	}
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		return t.CreateOAuthClient(client)
	}); err != nil {
		return nil, statusError(http.StatusInternalServerError, ErrServerError,
			"the registration could not be recorded")
	}

	return &registrationResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        now.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		GrantTypes:              client.GrantTypes,
		ResponseTypes:           client.ResponseTypes,
		TokenEndpointAuthMethod: "none",
	}, nil
}

// SweepExpiredRegistrations removes expired, never-activated registrations
// that no authorization request names, auditing each removal (spec section
// 9.3). The caller runs it every 60 seconds.
//
// It returns the IDs it removed so a caller can log a count without this
// function knowing what a logger is.
func (s *Server) SweepExpiredRegistrations(ctx context.Context) ([]string, error) {
	var removed []string
	err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		ids, err := t.ExpiredUnreferencedClients()
		if err != nil {
			return err
		}
		sort.Strings(ids)
		for _, id := range ids {
			if err := t.DeleteOAuthClient(id); err != nil {
				return err
			}
			if err := t.AppendAudit("client.revoked", "ok", "", "", "server",
				`{"client_id":"`+id+`","reason":"registration_expired"}`); err != nil {
				return err
			}
		}
		removed = ids
		return nil
	})
	return removed, err
}
