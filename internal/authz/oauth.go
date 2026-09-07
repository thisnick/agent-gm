package authz

// The OAuth half of the credential layer (spec section 9.6).
//
// Slice 2's package comment promised that Slice 3 adds a second token SOURCE
// and not a second security model, and this file is where that promise is
// kept: an OAuth grant is an `authorizations` row of a different `kind` with a
// `client_id`, its tokens are the same `tokens` rows under the same
// audience-bound hash, and its refresh runs the SAME rotation, reuse and
// never-widen code as the admin bootstrap does (see refreshParams).
//
// What is genuinely new here is the authorization-code exchange, which has
// two rules with teeth:
//
//   - a **replayed** code is `invalid_grant` and revokes the tokens the first
//     exchange produced;
//   - a **wrong PKCE verifier** is `invalid_grant` and CONSUMES the code, so
//     a verifier cannot be guessed by retrying.
//
// Both are enforced here rather than at the HTTP layer, because both are
// atomic decisions over rows and the HTTP layer cannot make them atomic.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/store"
)

// The audit kinds the OAuth half writes (spec section 12.4). They are the
// same strings `internal/audit` declares as typed constants; this package
// keeps them as plain strings so that the credential layer does not depend on
// the audit subsystem.
const (
	AuditAuthorizationCreated  = "authorization.created"
	AuditAuthorizationApproved = "authorization.approved"
	AuditAuthorizationDenied   = "authorization.denied"
	AuditAuthorizationRevoked  = "authorization.revoked"
	AuditEnrollmentCreated     = "enrollment.created"
	AuditEnrollmentConsumed    = "enrollment.consumed"
	AuditEnrollmentRevoked     = "enrollment.revoked"
	AuditEnrollmentExpired     = "enrollment.expired"
	AuditClientRevoked         = "client.revoked"
	// AuditOAuthTokenIssued records one authorization-code exchange.
	AuditOAuthTokenIssued = "authorization.token_issued"
	// AuditOAuthTokenRefreshed records one rotation of an OAuth grant.
	AuditOAuthTokenRefreshed = "authorization.token_refreshed"
	// AuditOAuthTokenNarrowed records a refresh that narrowed the grant.
	AuditOAuthTokenNarrowed = "authorization.token_narrowed"
	// AuditOAuthTokenFailed records an unknown or invalid presented value at
	// /oauth/token or /oauth/revoke, with the resulting cooldown state and
	// nothing derived from what was presented.
	AuditOAuthTokenFailed = "authorization.token_failed"
)

// The typed OAuth refusals. Each maps to exactly one RFC 6749 `error` value
// at /oauth/token, named in its comment, so `internal/oauth` translates
// rather than decides.
var (
	// ErrInvalidGrant is RFC 6749 `invalid_grant`: an unknown, expired,
	// consumed or replayed authorization code, a refresh token that resolves
	// to nothing, or a credential from the other path (section 9.6).
	ErrInvalidGrant = errors.New("invalid_grant")
	// ErrGrantReplayed is `invalid_grant` with a side effect: presenting a
	// code that has already been exchanged revokes the tokens the first
	// exchange produced.
	ErrGrantReplayed = errors.New("invalid_grant: authorization code replayed")
)

// LimitOAuthToken is spec section 9.8's durable half for the /oauth/token
// refresh grant and for /oauth/revoke: 30 unknown or invalid presented tokens
// per 15 minutes per source, a cooldown that doubles per further failure in
// the window and caps at 24 hours. It is a SEPARATE bucket from
// LimitRefreshToken so that exhausting one endpoint does not lock an owner
// out of the other, which is the honest reading of section 9.8's "all three
// carry" -- three budgets of the same size, not one shared one.
var LimitOAuthToken = DurableLimitSpec{
	Kind:         store.AttemptKindOAuthToken,
	Surface:      "oauth_token",
	PerSource:    30,
	Global:       0,
	Window:       15 * time.Minute,
	BaseCooldown: time.Minute,
	MaxCooldown:  24 * time.Hour,
}

// LimitEnrollmentSource and LimitEnrollmentContext are section 9.4's
// enrollment-code budget: **the eleventh failed attempt within 15 minutes is
// 429**, per source and per signed context alike.
//
// PerSource is therefore 10, not 11. The limiter records a failure and then
// refuses the NEXT request that arrives while the cooldown is running, so a
// limit of 10 makes the tenth failure set the cooldown and the eleventh
// attempt the one that is refused -- which is what section 9.4 asks for. A
// limit of 11 would let the eleventh through and refuse the twelfth, and the
// difference is exactly one free guess.
var (
	LimitEnrollmentSource = DurableLimitSpec{
		Kind:         store.AttemptKindEnrollmentSource,
		Surface:      "enrollment_code",
		PerSource:    10,
		Global:       0,
		Window:       15 * time.Minute,
		BaseCooldown: time.Minute,
		MaxCooldown:  24 * time.Hour,
	}
	LimitEnrollmentContext = DurableLimitSpec{
		Kind:         store.AttemptKindEnrollmentContext,
		Surface:      "enrollment_code",
		PerSource:    10,
		Global:       0,
		Window:       15 * time.Minute,
		BaseCooldown: time.Minute,
		MaxCooldown:  24 * time.Hour,
	}
)

// oauthTTLs reads the three OAuth TTL settings, in the shape refreshParams
// wants.
func (s *Service) oauthTTLs(ctx context.Context) (access, idle, absolute time.Duration, err error) {
	if access, err = s.settings.Duration(ctx, SettingOAuthAccessTokenTTL); err != nil {
		return
	}
	if idle, err = s.settings.Duration(ctx, SettingOAuthRefreshTokenIdleTTL); err != nil {
		return
	}
	absolute, err = s.settings.Duration(ctx, SettingOAuthRefreshTokenAbsoluteTTL)
	return
}

// CodeExchange is one `grant_type=authorization_code` request, after
// `internal/oauth` has parsed it and before anything has been decided.
type CodeExchange struct {
	// CodeHash is the stored form of the presented code. The value never
	// reaches this package.
	CodeHash string
	ClientID string
	Source   string
	// Verify is the caller's binding check: PKCE, the exact redirect URI, the
	// client, and the resource. It runs AFTER the code is known to exist and
	// to be unconsumed, and BEFORE the tokens are minted -- and a failure
	// consumes the code, which is section 9.6's rule that a verifier cannot
	// be guessed by retrying.
	//
	// It lives in `internal/oauth` because PKCE and redirect matching are
	// protocol policy; it runs here because "and consumes the code" is only
	// true if the consumption commits with the refusal.
	Verify func(code store.AuthorizationCode) error
}

// ExchangeAuthorizationCode mints an OAuth grant from an authorization code
// (spec section 9.6).
//
// The order below is the contract:
//
//  1. the durable budget, before the presented value is examined at all;
//  2. a READ-ONLY lookup, before any write transaction opens (section 9.8),
//     so a caller presenting a value that belongs to nobody cannot take the
//     single writer's lock;
//  3. expiry and replay, replay being the one that revokes;
//  4. the caller's Verify, whose failure consumes the code;
//  5. the mint, the consumption and the audit row, in ONE transaction.
func (s *Service) ExchangeAuthorizationCode(ctx context.Context, in CodeExchange) (*Session, error) {
	source := normalizeSource(in.Source)

	if err := s.Buckets.Allow(BucketUnauthenticated, source); err != nil {
		return nil, err
	}
	if err := s.Durable.Check(ctx, LimitOAuthToken, source); err != nil {
		return nil, err
	}

	code, err := s.st.AuthorizationCodeByHash(ctx, in.CodeHash)
	if err != nil {
		if errors.Is(err, store.ErrAuthorizationCodeNotFound) {
			return nil, s.recordOAuthFailure(ctx, source, "unknown_code", ErrInvalidGrant)
		}
		return nil, err
	}

	now := s.clk.Now()
	if code.Consumed() {
		// A replay. The tokens the FIRST exchange produced are revoked, and
		// so is the authorization behind them: a code seen twice means the
		// value leaked, and the honest response is to invalidate what it
		// bought rather than to refuse only the second attempt.
		return nil, s.revokeAfterReplay(ctx, source, code)
	}
	if now.UnixMilli() >= code.ExpiresAtMS {
		return nil, s.recordOAuthFailure(ctx, source, "expired_code", ErrInvalidGrant)
	}

	if in.Verify != nil {
		if verr := in.Verify(code); verr != nil {
			// Consume it. Without this a wrong verifier costs the attacker
			// nothing and the challenge can be brute-forced by retrying
			// against the same code (section 9.6).
			if cerr := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
				if _, err := t.ConsumeAuthorizationCode(code.CodeHash, ""); err != nil {
					return err
				}
				return t.AppendAudit(AuditOAuthTokenFailed, "refused", "", "", source,
					renderPayload(map[string]any{
						"request_id": code.RequestID,
						"client_id":  code.ClientID,
						"reason":     "binding_check_failed",
						"source":     source,
					}))
			}); cerr != nil {
				return nil, cerr
			}
			if _, rerr := s.Durable.RecordFailure(ctx, LimitOAuthToken, source); rerr != nil {
				return nil, rerr
			}
			return nil, verr
		}
	}

	scopes, err := ParseScopeString(code.Scopes)
	if err != nil {
		return nil, err
	}
	if scopes.Empty() {
		return nil, fmt.Errorf("%w: the code grants no scopes", ErrInvalidGrant)
	}
	// Belt and braces on the one rule that must never be broken by any path:
	// an OAuth authorization can never hold `admin` (sections 9.5, 9.7,
	// section 16 Slice 3 test 13). The authorization screen refuses it, the
	// enrollment ceiling cannot contain it, and it is refused here too --
	// three independent refusals, because one of them is one edit away from
	// being wrong.
	if scopes.Has(ScopeAdmin) {
		return nil, fmt.Errorf("%w: an OAuth authorization can never hold admin", ErrInvalidScope)
	}

	accessTTL, idleTTL, absoluteTTL, err := s.oauthTTLs(ctx)
	if err != nil {
		return nil, err
	}

	authID := store.AuthorizationID()
	familyID := "fam_" + uuid.NewString()
	absoluteDeadline := now.Add(absoluteTTL)
	refreshExpiry := earlier(now.Add(idleTTL), absoluteDeadline)
	accessExpiry := now.Add(accessTTL)

	accessValue, err := newTokenValue()
	if err != nil {
		return nil, err
	}
	refreshValue, err := newTokenValue()
	if err != nil {
		return nil, err
	}

	fields := map[string]any{
		"authorization_id": authID,
		"client_id":        code.ClientID,
		"request_id":       code.RequestID,
		"scopes":           scopes.Strings(),
		"source":           source,
	}

	var raced bool
	err = s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		ok, cerr := t.ConsumeAuthorizationCode(code.CodeHash, authID)
		if cerr != nil {
			return cerr
		}
		if !ok {
			// Somebody exchanged it between the read and this transaction.
			// That is a replay too, and it is handled outside the
			// transaction so the revocation is one decision rather than two
			// interleaved ones.
			raced = true
			return nil
		}
		if cerr := t.CreateAuthorization(store.Authorization{
			ID:           authID,
			Kind:         store.AuthKindOAuth,
			Client:       code.ClientID,
			Scopes:       scopes.String(),
			MintedScopes: scopes.String(),
			Source:       source,
			ExpiresAtMS:  absoluteDeadline.UnixMilli(),
			CreatedAtMS:  now.UnixMilli(),
		}); cerr != nil {
			return cerr
		}
		if cerr := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindAccess, accessValue),
			AuthorizationID: authID,
			Kind:            store.TokenKindAccess,
			FamilyID:        familyID,
			ExpiresAtMS:     accessExpiry.UnixMilli(),
		}); cerr != nil {
			return cerr
		}
		if cerr := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindRefresh, refreshValue),
			AuthorizationID: authID,
			Kind:            store.TokenKindRefresh,
			FamilyID:        familyID,
			ExpiresAtMS:     refreshExpiry.UnixMilli(),
		}); cerr != nil {
			return cerr
		}
		return t.AppendAudit(AuditOAuthTokenIssued, "ok", "", authID, source, renderPayload(fields))
	})
	if err != nil {
		return nil, err
	}
	if raced {
		fresh, ferr := s.st.AuthorizationCodeByHash(ctx, in.CodeHash)
		if ferr != nil {
			return nil, s.recordOAuthFailure(ctx, source, "unknown_code", ErrInvalidGrant)
		}
		return nil, s.revokeAfterReplay(ctx, source, fresh)
	}
	s.observe(ctx, AuditOAuthTokenIssued, fields)

	return &Session{
		AuthorizationID:       authID,
		AccessToken:           accessValue,
		RefreshToken:          refreshValue,
		Scopes:                scopes,
		MintedScopes:          scopes,
		AccessTokenExpiresAt:  accessExpiry,
		RefreshTokenExpiresAt: refreshExpiry,
	}, nil
}

// revokeAfterReplay is the second half of section 9.6's replay rule: the
// refusal AND the revocation of what the first exchange produced, in one
// transaction with its audit row.
func (s *Service) revokeAfterReplay(ctx context.Context, source string, code store.AuthorizationCode) error {
	if code.AuthorizationID != "" {
		if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
			revoked, err := t.RevokeTokensForAuthorization(code.AuthorizationID)
			if err != nil {
				return err
			}
			if err := t.RevokeAuthorization(code.AuthorizationID); err != nil &&
				!errors.Is(err, store.ErrAuthorizationNotFound) {
				return err
			}
			return t.AppendAudit(AuditAuthorizationRevoked, "refused", "", code.AuthorizationID, source,
				renderPayload(map[string]any{
					"authorization_id": code.AuthorizationID,
					"client_id":        code.ClientID,
					"request_id":       code.RequestID,
					"reason":           "authorization_code_replayed",
					"tokens_revoked":   revoked,
					"source":           source,
				}))
		}); err != nil {
			return err
		}
		s.observe(ctx, AuditAuthorizationRevoked, map[string]any{
			"authorization_id": code.AuthorizationID,
			"reason":           "authorization_code_replayed",
			"source":           source,
		})
	}
	if _, err := s.Durable.RecordFailure(ctx, LimitOAuthToken, source); err != nil {
		return err
	}
	return ErrGrantReplayed
}

// recordOAuthFailure counts one unknown or invalid presented value at
// /oauth/token and returns the caller's refusal unchanged. The payload
// carries the source and the cooldown state and nothing derived from what was
// presented.
func (s *Service) recordOAuthFailure(ctx context.Context, source, reason string, refusal error) error {
	attempt, err := s.Durable.RecordFailure(ctx, LimitOAuthToken, source)
	if err != nil {
		return err
	}
	fields := map[string]any{
		"source":             source,
		"reason":             reason,
		"failures_in_window": attempt.Failures,
		"cooldown_steps":     attempt.CooldownSteps,
		"cooldown_until_ms":  attempt.CooldownUntilMS,
	}
	if aerr := s.appendStandalone(ctx, AuditOAuthTokenFailed, "refused", "", source, fields); aerr != nil {
		return aerr
	}
	return refusal
}

// RefreshOAuth is the `refresh_token` grant at POST /oauth/token.
//
// It is the SAME implementation as the admin refresh, with the OAuth TTLs,
// the OAuth budget, the OAuth audit kinds, the client binding and -- the
// security-relevant difference -- a guard that the presented token belongs to
// an OAuth authorization. An admin refresh token here is `invalid_grant` and
// revokes nothing (section 9.6, section 16 Slice 3 test 13).
func (s *Service) RefreshOAuth(ctx context.Context, presentedRefreshToken, clientID string, requestedScopes []string, source string) (*Session, error) {
	return s.refreshSession(ctx, refreshParams{
		PresentedToken:  presentedRefreshToken,
		RequestedScopes: requestedScopes,
		Source:          source,
		Kind:            store.AuthKindOAuth,
		Limit:           LimitOAuthToken,
		TTLs:            s.oauthTTLs,
		ClientID:        clientID,
		AuditRefreshed:  AuditOAuthTokenRefreshed,
		AuditNarrowed:   AuditOAuthTokenNarrowed,
		AuditFailed:     AuditOAuthTokenFailed,
		WrongKind:       ErrInvalidGrant,
	})
}

// RevokeToken is POST /oauth/revoke (RFC 7009, spec section 9.6).
//
// It ALWAYS returns nil. A token belonging to another client, or to nobody,
// is ignored without confirming that it exists -- so the caller cannot use
// this endpoint as an oracle. Revoking any token of a grant revokes the whole
// grant.
//
// The two budgets still apply, and a rate-limited caller still gets a 429:
// section 9.8 makes the budget the one thing this endpoint may say no to,
// precisely because it never says anything else.
func (s *Service) RevokeToken(ctx context.Context, presentedToken, clientID, source string) error {
	source = normalizeSource(source)
	if err := s.Buckets.Allow(BucketUnauthenticated, source); err != nil {
		return err
	}
	if err := s.Durable.Check(ctx, LimitOAuthToken, source); err != nil {
		return err
	}
	// `client_id` is REQUIRED (spec section 9.6 names it), and its absence
	// revokes nothing while still answering 200.
	//
	// A reviewer driving this endpoint found that an omitted client_id
	// revoked whatever the token belonged to -- including an ADMIN BOOTSTRAP
	// session, which no OAuth client owns and which this endpoint has no
	// business touching. An unauthenticated endpoint that can end the
	// owner's own session on a guessed value is a bigger hole than the
	// oracle the silence was protecting against.
	if strings.TrimSpace(clientID) == "" {
		if _, err := s.Durable.RecordFailure(ctx, LimitOAuthToken, source); err != nil {
			return err
		}
		return nil
	}

	// A presented value may be either kind, and the caller does not say
	// which. Both hashes are computed and both are looked up, read-only,
	// before any write transaction opens (section 9.8).
	for _, kind := range []string{store.TokenKindAccess, store.TokenKindRefresh} {
		presentedDigest := tokenDigest(s.audience, kind, presentedToken)
		row, err := s.st.TokenByHash(ctx, hexDigest(presentedDigest))
		if err != nil {
			if errors.Is(err, store.ErrTokenNotFound) {
				continue
			}
			return err
		}
		if !equalHashHex(presentedDigest, row.TokenHash) {
			continue
		}
		auth, err := s.st.Authorization(ctx, row.AuthorizationID)
		if err != nil {
			if errors.Is(err, store.ErrAuthorizationNotFound) {
				continue
			}
			return err
		}
		// Only an OAuth grant is revocable here. An admin bootstrap session
		// belongs to no client and is ended by `/v1/auth/logout` or by the
		// owner, never by an unauthenticated caller presenting a value.
		if auth.Kind != store.AuthKindOAuth {
			continue
		}
		// Another client's token is ignored in silence. Returning an error
		// here -- or even taking a different amount of work -- would confirm
		// that the value exists.
		if auth.Client != clientID {
			continue
		}
		if auth.Revoked() {
			return nil
		}
		return s.revokeAuthorization(ctx, auth.ID, "token_revoked", source)
	}
	// Nothing matched. Count it against the durable budget: guessing at
	// /oauth/revoke is guessing at a credential, and section 9.8 budgets all
	// three unauthenticated endpoints in the same shape.
	if _, err := s.Durable.RecordFailure(ctx, LimitOAuthToken, source); err != nil {
		return err
	}
	return nil
}

// RevokeAuthorization is the owner's revocation, from
// DELETE /v1/admin/authorizations/{id} and from `agm admin authorizations
// revoke`. Unlike RevokeToken it reports whether the authorization existed,
// because the caller here is the owner and there is nothing to hide from
// them.
func (s *Service) RevokeAuthorization(ctx context.Context, id, reason, source string) error {
	if _, err := s.st.Authorization(ctx, id); err != nil {
		return err
	}
	return s.revokeAuthorization(ctx, id, reason, normalizeSource(source))
}

func (s *Service) revokeAuthorization(ctx context.Context, id, reason, source string) error {
	var revoked int64
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		n, err := t.RevokeTokensForAuthorization(id)
		if err != nil {
			return err
		}
		revoked = n
		if err := t.RevokeAuthorization(id); err != nil {
			return err
		}
		return t.AppendAudit(AuditAuthorizationRevoked, "ok", "", id, source,
			renderPayload(map[string]any{
				"authorization_id": id,
				"reason":           reason,
				"tokens_revoked":   n,
				"source":           source,
			}))
	}); err != nil {
		return err
	}
	s.observe(ctx, AuditAuthorizationRevoked, map[string]any{
		"authorization_id": id, "reason": reason, "tokens_revoked": revoked, "source": source,
	})
	return nil
}

// Authorizations lists the grants an owner may revoke.
func (s *Service) Authorizations(ctx context.Context, includeRevoked bool) ([]store.Authorization, error) {
	return s.st.Authorizations(ctx, includeRevoked)
}

// Store exposes the store to `internal/oauth`, which owns the protocol tables
// (clients, enrollment codes, authorization requests, authorization codes)
// while this package owns the credential ones. Sharing one Store rather than
// opening a second is what lets an approval and the token it eventually mints
// be read from the same database with the same single writer.
func (s *Service) Store() *store.Store { return s.st }

// Clock exposes the injected clock, so `internal/oauth` measures every
// timeout on the same one and no test waits anything out.
func (s *Service) Clock() interface{ Now() time.Time } { return s.clk }

// SettingDuration reads one duration setting through the same view this
// package uses, so the OAuth layer cannot end up reading a different settings
// source than the credential layer.
func (s *Service) SettingDuration(ctx context.Context, key string) (time.Duration, error) {
	return s.settings.Duration(ctx, key)
}
