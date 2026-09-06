package api

import (
	"errors"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/store"
)

// The admin bootstrap of spec sections 7.5 and 9.7. Slice 2's only token
// source; OAuth arrives in Slice 3 and adds a second source, not a second
// security model.

type adminSessionBody struct {
	Secret string   `json:"secret"`
	Scopes []string `json:"scopes"`
}

type sessionDTO struct {
	AuthorizationID       string   `json:"authorization_id"`
	AccessToken           string   `json:"access_token"`
	RefreshToken          string   `json:"refresh_token"`
	TokenType             string   `json:"token_type"`
	Scopes                []string `json:"scopes"`
	AccessTokenExpiresAt  *string  `json:"access_token_expires_at"`
	RefreshTokenExpiresAt *string  `json:"refresh_token_expires_at"`
	Narrowed              bool     `json:"narrowed"`
}

func sessionFrom(s *authz.Session) sessionDTO {
	access := s.AccessTokenExpiresAt
	refresh := s.RefreshTokenExpiresAt
	return sessionDTO{
		AuthorizationID:       s.AuthorizationID,
		AccessToken:           s.AccessToken,
		RefreshToken:          s.RefreshToken,
		TokenType:             "Bearer",
		Scopes:                s.Scopes.Strings(),
		AccessTokenExpiresAt:  rfc3339Time(&access),
		RefreshTokenExpiresAt: rfc3339Time(&refresh),
		Narrowed:              s.Narrowed,
	}
}

// authAdminSession is `POST /v1/auth/admin-session`.
//
// The secret travels in the body and never in a URL or a header, so it cannot
// be captured from a proxy log. The failure limiter, the constant-time
// comparison and the audit rows all live in internal/authz: this handler
// decodes, delegates and renders, which is what keeps "a wrong secret and a
// rate-limited source are indistinguishable" a property of one place rather
// than of every caller.
func (d *HandlerDeps) authAdminSession(r *Request) (*Response, error) {
	var body adminSessionBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	sess, err := d.Authz.MintAdminSession(r.Ctx, body.Secret, body.Scopes, r.Source)
	if err != nil {
		return nil, translateAuthzError(err)
	}
	return &Response{Data: sessionFrom(sess)}, nil
}

type refreshBody struct {
	RefreshToken string   `json:"refresh_token"`
	Scopes       []string `json:"scopes"`
}

// authRefresh is `POST /v1/auth/refresh`. It rotates. `scopes` may only
// narrow relative to what the session was MINTED with; widening is
// `invalid_scope` and **does not spend the presented token** (spec section
// 9.6), which is what keeps a narrowed session one refresh away from nothing
// rather than one refresh away from full privilege.
func (d *HandlerDeps) authRefresh(r *Request) (*Response, error) {
	var body refreshBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	sess, err := d.Authz.Refresh(r.Ctx, body.RefreshToken, body.Scopes, r.Source)
	if err != nil {
		return nil, translateAuthzError(err)
	}
	return &Response{Data: sessionFrom(sess)}, nil
}

type whoamiDTO struct {
	AuthorizationID string   `json:"authorization_id"`
	Kind            string   `json:"kind"`
	Scopes          []string `json:"scopes"`
	ClientID        *string  `json:"client_id"`
	ExpiresAt       *string  `json:"expires_at"`
}

// authWhoami is `GET /v1/auth/whoami`. It carries the same scope as a read:
// a token that cannot read cannot ask who it is.
func (d *HandlerDeps) authWhoami(r *Request) (*Response, error) {
	row, err := d.Store.Authorization(r.Ctx, r.Auth.ID)
	clientID := ""
	if err == nil {
		clientID = row.Client
	}
	expires := r.Auth.ExpiresAt
	return &Response{Data: whoamiDTO{
		AuthorizationID: r.Auth.ID,
		Kind:            r.Auth.Kind,
		Scopes:          r.Auth.Scopes.Strings(),
		ClientID:        nullable(clientID),
		ExpiresAt:       rfc3339Time(&expires),
	}}, nil
}

type logoutDTO struct {
	AuthorizationID string `json:"authorization_id"`
	TokensRevoked   int64  `json:"tokens_revoked"`
}

// authLogout is `POST /v1/auth/logout`. It ends a **token's** session and has
// nothing to do with signing a Google **account** out (spec section 4.7),
// which is why it is not called sign-out and why it deletes no rows but the
// credential's own.
//
// Revoking the authorization as well as its tokens is what makes every ticket
// it minted die with it: an upload or download ticket re-checks its issuing
// authorization on every redemption (section 10.3), so a token outlives
// neither its scope nor its revocation.
func (d *HandlerDeps) authLogout(r *Request) (*Response, error) {
	var revoked int64
	err := d.Store.AuthzTx(r.Ctx, func(tx *store.AuthzTx) error {
		n, err := tx.RevokeTokensForAuthorization(r.Auth.ID)
		if err != nil {
			return err
		}
		revoked = n
		return tx.RevokeAuthorization(r.Auth.ID)
	})
	if err != nil {
		return nil, err
	}
	return &Response{Data: logoutDTO{AuthorizationID: r.Auth.ID, TokensRevoked: revoked}}, nil
}

// translateAuthzError turns internal/authz's typed refusals into spec section
// 7.2 codes.
//
// Every credential refusal becomes the same 401 with the same body, whatever
// the reason: an answer that distinguished "unknown" from "expired" from
// "revoked" from "wrong secret" would tell an attacker which of their guesses
// was once a real credential.
func translateAuthzError(err error) *apierr.Error {
	var limited *authz.RateLimitedError
	if errors.As(err, &limited) {
		return apierr.RateLimited(limited.RetryAfter)
	}
	switch {
	case errors.Is(err, authz.ErrInvalidScope):
		return apierr.WrongTypeForField("scopes",
			"a subset of the scopes this session was minted with; widening requires the admin secret")
	case errors.Is(err, authz.ErrInsufficientScope):
		return apierr.InsufficientScope("")
	case errors.Is(err, authz.ErrTokenAbsent),
		errors.Is(err, authz.ErrTokenUnknown),
		errors.Is(err, authz.ErrTokenExpired),
		errors.Is(err, authz.ErrTokenRevoked),
		errors.Is(err, authz.ErrTokenReused),
		errors.Is(err, authz.ErrInvalidSecret):
		return apierr.InvalidToken("the credential was refused")
	}
	return apierr.From(err)
}
