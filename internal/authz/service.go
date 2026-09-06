package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// MinAdminSecretLength is spec section 12.1's floor: AGENT_GM_ADMIN_SECRET is
// at least 43 characters and the server refuses shorter. Forty-three base64url
// characters is 256 bits, which is the point.
const MinAdminSecretLength = 43

// Config is what a Service needs from the environment.
type Config struct {
	// AdminSecret is AGENT_GM_ADMIN_SECRET. It lives in the environment only
	// and is never written to the database, a log or an audit payload.
	AdminSecret string
	// PublicURL is AGENT_GM_PUBLIC_URL. Access tokens are audience-bound to
	// PublicURL + "/mcp" (spec section 9.6).
	PublicURL string
	// TrustedProxyCIDRs is AGENT_GM_TRUSTED_PROXY_CIDRS. An invalid value
	// makes New fail, which is spec section 12.3's "refuses to start".
	TrustedProxyCIDRs string
}

// Session is the result of a mint or a refresh. The two token values are
// returned here exactly once and are never stored; only their hashes reach the
// database.
type Session struct {
	AuthorizationID       string
	AccessToken           string
	RefreshToken          string
	Scopes                ScopeSet
	MintedScopes          ScopeSet
	AccessTokenExpiresAt  time.Time
	RefreshTokenExpiresAt time.Time
	// Narrowed reports whether this call narrowed the scope set: at a mint,
	// relative to the full admin bootstrap grant; at a refresh, relative to
	// the session's previous scopes.
	Narrowed bool
}

// Authorization is an authenticated caller, as the rest of the server sees it.
type Authorization struct {
	ID           string
	Kind         string
	Scopes       ScopeSet
	MintedScopes ScopeSet
	Source       string
	ExpiresAt    time.Time
}

// Require is the scope check every route makes. A valid credential on a route
// it does not cover is insufficient_scope (403), never invalid_token and never
// not_found -- this is the exit-4 case of the section 11.2 matrix.
func (a *Authorization) Require(scope Scope) error {
	if a == nil {
		return ErrTokenAbsent
	}
	if a.Scopes.Has(scope) {
		return nil
	}
	return fmt.Errorf("%w: %s required", ErrInsufficientScope, scope)
}

// Service is the credential logic.
type Service struct {
	st       *store.Store
	clk      clock.Clock
	settings Settings
	// audit is an observation sink, not the durable record. Every audit row
	// this package writes is inserted into `audit_events` inside the same
	// transaction as the change it describes, because spec section 9.6
	// requires a rotation, its audit record and a family revocation to commit
	// together. This interface exists so that a caller (and a test) can also
	// see the rows as they are written, without this package depending on the
	// audit subsystem. It may be nil.
	audit AuditWriter

	// adminSecretHash is the fixed-length digest of the configured secret.
	// The secret itself is not retained past construction.
	adminSecretHash Digest
	// generation identifies which secret this process was started with, for
	// the startup revocation of spec section 12.1.
	generation string
	// audience is AGENT_GM_PUBLIC_URL + "/mcp", mixed into every token hash.
	audience string

	Sources   *SourceResolver
	Buckets   *TokenBuckets
	Durable   *DurableLimiter
	Uploads   *ConcurrencyLimiter
	Downloads *ConcurrencyLimiter
}

// New builds the Service, validating everything that should stop a start.
func New(st *store.Store, clk clock.Clock, settings Settings, audit AuditWriter, cfg Config) (*Service, error) {
	if st == nil {
		return nil, errors.New("authz: a store is required")
	}
	if clk == nil {
		return nil, errors.New("authz: a clock is required")
	}
	if settings == nil {
		settings = NewMemorySettings()
	}
	if len(cfg.AdminSecret) < MinAdminSecretLength {
		return nil, fmt.Errorf("AGENT_GM_ADMIN_SECRET must be at least %d characters", MinAdminSecretLength)
	}
	public := strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	if len(public) == 0 {
		return nil, errors.New("AGENT_GM_PUBLIC_URL is required")
	}
	resolver, err := NewSourceResolver(cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	return &Service{
		st:              st,
		clk:             clk,
		settings:        settings,
		audit:           audit,
		adminSecretHash: hashSecret(cfg.AdminSecret),
		generation:      secretGeneration(cfg.AdminSecret),
		audience:        public + "/mcp",
		Sources:         resolver,
		Buckets:         NewTokenBuckets(clk),
		Durable:         NewDurableLimiter(st, clk),
		Uploads:         NewConcurrencyLimiter(ConcurrencyUploads),
		Downloads:       NewConcurrencyLimiter(ConcurrencyDownloads),
	}, nil
}

// Audience is the canonical resource every access token is bound to.
func (s *Service) Audience() string { return s.audience }

// RevokeSupersededAdminSessions revokes every admin bootstrap authorization
// minted by a different AGENT_GM_ADMIN_SECRET, and every token belonging to
// them. It runs at startup, before any listener binds.
//
// This is spec section 12.1's "changing it revokes every previous admin
// bootstrap authorization on next start". It is a startup step rather than a
// check at authentication time so that the revocation is recorded once, in the
// audit log, instead of being re-decided on every request.
func (s *Service) RevokeSupersededAdminSessions(ctx context.Context) ([]string, error) {
	return s.st.RevokeAdminAuthorizationsFromOtherSecrets(ctx, s.generation)
}

// MintAdminSession is POST /v1/auth/admin-session (spec sections 9.7, 16
// Slice 2 test 24).
//
// It mints `admin messages:read messages:write messages:delete`, honouring an
// optional narrowing to a subset of those four. A widening beyond them is
// invalid_scope. The presented secret never reaches the database, a log or an
// audit payload.
func (s *Service) MintAdminSession(ctx context.Context, presentedSecret string, requestedScopes []string, source string) (*Session, error) {
	source = normalizeSource(source)

	// The durable limiter is checked BEFORE the secret is examined, so a
	// cooled-down source cannot learn anything by presenting a guess.
	if err := s.Durable.Check(ctx, LimitAdminSecret, source); err != nil {
		return nil, err
	}

	presented := hashSecret(presentedSecret)
	if !equalDigest(presented, s.adminSecretHash) {
		return nil, s.recordAdminSecretFailure(ctx, source)
	}

	granted := AdminBootstrapScopes()
	narrowed := false
	if len(requestedScopes) > 0 {
		req, err := ParseScopes(requestedScopes)
		if err != nil {
			return nil, err
		}
		if req.Empty() {
			return nil, fmt.Errorf("%w: an empty scope list grants nothing", ErrInvalidScope)
		}
		if req.Widens(granted) {
			return nil, fmt.Errorf("%w: %s is wider than the admin bootstrap grant %s",
				ErrInvalidScope, req, granted)
		}
		narrowed = !req.Equal(granted)
		granted = req
	}

	accessTTL, idleTTL, absoluteTTL, err := s.ttls(ctx)
	if err != nil {
		return nil, err
	}

	now := s.clk.Now()
	authID := "authz_" + uuid.NewString()
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
		"source":           source,
		"scopes":           granted.Strings(),
		"narrowed":         narrowed,
	}

	err = s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		if err := t.CreateAuthorization(store.Authorization{
			ID:               authID,
			Kind:             store.AuthKindAdminBootstrap,
			Scopes:           granted.String(),
			MintedScopes:     granted.String(),
			SecretGeneration: s.generation,
			Source:           source,
			ExpiresAtMS:      absoluteDeadline.UnixMilli(),
			CreatedAtMS:      now.UnixMilli(),
		}); err != nil {
			return err
		}
		if err := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindAccess, accessValue),
			AuthorizationID: authID,
			Kind:            store.TokenKindAccess,
			FamilyID:        familyID,
			ExpiresAtMS:     accessExpiry.UnixMilli(),
		}); err != nil {
			return err
		}
		if err := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindRefresh, refreshValue),
			AuthorizationID: authID,
			Kind:            store.TokenKindRefresh,
			FamilyID:        familyID,
			ExpiresAtMS:     refreshExpiry.UnixMilli(),
		}); err != nil {
			return err
		}
		return t.AppendAudit(AuditAdminSessionMinted, "ok", "", authID, source, renderPayload(fields))
	})
	if err != nil {
		return nil, err
	}
	s.observe(ctx, AuditAdminSessionMinted, fields)

	return &Session{
		AuthorizationID:       authID,
		AccessToken:           accessValue,
		RefreshToken:          refreshValue,
		Scopes:                granted,
		MintedScopes:          granted,
		AccessTokenExpiresAt:  accessExpiry,
		RefreshTokenExpiresAt: refreshExpiry,
		Narrowed:              narrowed,
	}, nil
}

// recordAdminSecretFailure counts the failure and writes
// `auth.admin_secret_failed` carrying the source and the resulting cooldown
// state -- and, by construction, nothing derived from what was presented.
func (s *Service) recordAdminSecretFailure(ctx context.Context, source string) error {
	attempt, err := s.Durable.RecordFailure(ctx, LimitAdminSecret, source)
	if err != nil {
		return err
	}
	fields := map[string]any{
		"source":             source,
		"failures_in_window": attempt.Failures,
		"cooldown_steps":     attempt.CooldownSteps,
		"cooldown_until_ms":  attempt.CooldownUntilMS,
		"cooling_down":       attempt.CooldownUntilMS > s.clk.Now().UnixMilli(),
	}
	if aerr := s.appendStandalone(ctx, AuditAdminSecretFailed, "refused", "", source, fields); aerr != nil {
		return aerr
	}
	return ErrInvalidSecret
}

// Refresh is POST /v1/auth/refresh (spec sections 9.6, 9.8, 16 Slice 2 tests
// 22 and 25).
//
// Admin refresh tokens rotate on every use, and reuse of a spent one revokes
// the session. An admin refresh may never widen: `scopes` may only narrow,
// relative BOTH to the scopes the session was minted with and to the scopes it
// holds now. Widening -- including back to the full set after a narrowing --
// is invalid_scope, does not spend the presented token, and leaves the
// session's scopes unchanged. Re-widening requires presenting
// AGENT_GM_ADMIN_SECRET again (spec section 9.6).
//
// The two budgets of section 9.8 are checked before the presented token is
// examined at all, so a 429 never distinguishes a real token from a guess.
func (s *Service) Refresh(ctx context.Context, presentedRefreshToken string, requestedScopes []string, source string) (*Session, error) {
	source = normalizeSource(source)

	// Both budgets, before the token is looked at.
	if err := s.Buckets.Allow(BucketUnauthenticated, source); err != nil {
		return nil, err
	}
	if err := s.Durable.Check(ctx, LimitRefreshToken, source); err != nil {
		return nil, err
	}

	// Read-only, before any write transaction opens (spec section 9.8): a
	// caller presenting a value that belongs to nobody must not be able to
	// take the single writer's lock.
	presentedDigest := tokenDigest(s.audience, store.TokenKindRefresh, presentedRefreshToken)
	row, err := s.st.TokenByHash(ctx, hexDigest(presentedDigest))
	if err != nil {
		if errors.Is(err, store.ErrTokenNotFound) {
			return nil, s.recordRefreshFailure(ctx, source, "unknown", ErrTokenUnknown)
		}
		return nil, err
	}
	// The primary-key lookup already matched, but the value that decides
	// whether this is the right credential is compared constant-time, over
	// fixed-length digests, exactly like every other secret comparison.
	if !equalHashHex(presentedDigest, row.TokenHash) {
		return nil, s.recordRefreshFailure(ctx, source, "unknown", ErrTokenUnknown)
	}

	now := s.clk.Now()
	switch {
	case row.Revoked():
		return nil, s.recordRefreshFailure(ctx, source, "revoked", ErrTokenRevoked)
	case now.UnixMilli() >= row.ExpiresAtMS:
		return nil, s.recordRefreshFailure(ctx, source, "expired", ErrTokenExpired)
	case row.Spent():
		return nil, s.detectReuse(ctx, source, row)
	}

	auth, err := s.st.Authorization(ctx, row.AuthorizationID)
	if err != nil {
		return nil, err
	}
	if auth.Revoked() {
		return nil, s.recordRefreshFailure(ctx, source, "revoked", ErrTokenRevoked)
	}
	if auth.ExpiresAtMS != 0 && now.UnixMilli() >= auth.ExpiresAtMS {
		return nil, s.recordRefreshFailure(ctx, source, "expired", ErrTokenExpired)
	}

	current, err := ParseScopeString(auth.Scopes)
	if err != nil {
		return nil, err
	}
	minted, err := ParseScopeString(auth.MintedScopes)
	if err != nil {
		return nil, err
	}

	next := current
	narrowedNow := false
	if len(requestedScopes) > 0 {
		req, perr := ParseScopes(requestedScopes)
		if perr != nil {
			return nil, perr
		}
		if req.Empty() {
			return nil, fmt.Errorf("%w: an empty scope list grants nothing", ErrInvalidScope)
		}
		// Both comparisons, and both matter. Against minted_scopes because
		// that is what section 9.6 measures a narrowing against; against the
		// current scopes because a narrowed session asking for the full
		// minted set back is exactly the widening the rule forbids. Neither
		// path spends the presented token.
		if req.Widens(minted) || req.Widens(current) {
			return nil, fmt.Errorf("%w: %s widens the session, which holds %s and was minted with %s",
				ErrInvalidScope, req, current, minted)
		}
		narrowedNow = !req.Equal(current)
		next = req
	}

	accessTTL, idleTTL, _, err := s.ttls(ctx)
	if err != nil {
		return nil, err
	}
	accessExpiry := now.Add(accessTTL)
	refreshExpiry := now.Add(idleTTL)
	if auth.ExpiresAtMS != 0 {
		refreshExpiry = earlier(refreshExpiry, time.UnixMilli(auth.ExpiresAtMS).UTC())
	}

	accessValue, err := newTokenValue()
	if err != nil {
		return nil, err
	}
	refreshValue, err := newTokenValue()
	if err != nil {
		return nil, err
	}

	refreshedFields := map[string]any{
		"authorization_id": auth.ID,
		"source":           source,
		"scopes_before":    current.Strings(),
		"scopes_after":     next.Strings(),
		"narrowed":         narrowedNow,
	}

	var raced bool
	// One transaction: the rotation, its audit record, and -- if the re-read
	// finds the row spent underneath us -- the family revocation.
	err = s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		cur, terr := t.Token(hexDigest(presentedDigest))
		if terr != nil {
			return terr
		}
		if cur.Spent() || cur.Revoked() {
			raced = true
			return s.revokeFamilyTx(t, auth.ID, cur.FamilyID, source)
		}
		if terr := t.SpendToken(hexDigest(presentedDigest)); terr != nil {
			return terr
		}
		if terr := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindAccess, accessValue),
			AuthorizationID: auth.ID,
			Kind:            store.TokenKindAccess,
			FamilyID:        cur.FamilyID,
			ExpiresAtMS:     accessExpiry.UnixMilli(),
		}); terr != nil {
			return terr
		}
		if terr := t.InsertToken(store.Token{
			TokenHash:       tokenHash(s.audience, store.TokenKindRefresh, refreshValue),
			AuthorizationID: auth.ID,
			Kind:            store.TokenKindRefresh,
			FamilyID:        cur.FamilyID,
			ExpiresAtMS:     refreshExpiry.UnixMilli(),
		}); terr != nil {
			return terr
		}
		if narrowedNow {
			if terr := t.SetAuthorizationScopes(auth.ID, next.String()); terr != nil {
				return terr
			}
			if terr := t.AppendAudit(AuditAdminSessionNarrowed, "ok", "", auth.ID, source,
				renderPayload(map[string]any{
					"authorization_id": auth.ID,
					"source":           source,
					"scopes_before":    current.Strings(),
					"scopes_after":     next.Strings(),
				})); terr != nil {
				return terr
			}
		}
		return t.AppendAudit(AuditAdminSessionRefreshed, "ok", "", auth.ID, source,
			renderPayload(refreshedFields))
	})
	if err != nil {
		return nil, err
	}
	if raced {
		s.observe(ctx, AuditRefreshTokenReuse, map[string]any{
			"authorization_id": auth.ID, "source": source,
		})
		return nil, ErrTokenReused
	}
	if narrowedNow {
		s.observe(ctx, AuditAdminSessionNarrowed, map[string]any{
			"authorization_id": auth.ID, "source": source,
			"scopes_before": current.Strings(), "scopes_after": next.Strings(),
		})
	}
	s.observe(ctx, AuditAdminSessionRefreshed, refreshedFields)

	return &Session{
		AuthorizationID:       auth.ID,
		AccessToken:           accessValue,
		RefreshToken:          refreshValue,
		Scopes:                next,
		MintedScopes:          minted,
		AccessTokenExpiresAt:  accessExpiry,
		RefreshTokenExpiresAt: refreshExpiry,
		Narrowed:              narrowedNow,
	}, nil
}

// detectReuse handles a presented refresh token that has already been spent.
// The family revocation, the authorization revocation and the audit row commit
// in one transaction (spec section 9.6).
func (s *Service) detectReuse(ctx context.Context, source string, row store.Token) error {
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		return s.revokeFamilyTx(t, row.AuthorizationID, row.FamilyID, source)
	}); err != nil {
		return err
	}
	if _, err := s.Durable.RecordFailure(ctx, LimitRefreshToken, source); err != nil {
		return err
	}
	s.observe(ctx, AuditRefreshTokenReuse, map[string]any{
		"authorization_id": row.AuthorizationID, "source": source,
	})
	return ErrTokenReused
}

func (s *Service) revokeFamilyTx(t *store.AuthzTx, authorizationID, familyID, source string) error {
	tokens, err := t.RevokeTokenFamily(familyID)
	if err != nil {
		return err
	}
	if err := t.RevokeAuthorization(authorizationID); err != nil {
		return err
	}
	more, err := t.RevokeTokensForAuthorization(authorizationID)
	if err != nil {
		return err
	}
	return t.AppendAudit(AuditRefreshTokenReuse, "refused", "", authorizationID, source,
		renderPayload(map[string]any{
			"authorization_id": authorizationID,
			"source":           source,
			"tokens_revoked":   tokens + more,
		}))
}

// recordRefreshFailure counts one unknown or invalid presented token against
// the durable budget and returns the caller's refusal unchanged. Note what is
// NOT in the audit payload: anything derived from the presented value.
func (s *Service) recordRefreshFailure(ctx context.Context, source, reason string, refusal error) error {
	attempt, err := s.Durable.RecordFailure(ctx, LimitRefreshToken, source)
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
	if aerr := s.appendStandalone(ctx, AuditRefreshTokenFailed, "refused", "", source, fields); aerr != nil {
		return aerr
	}
	return refusal
}

// Authenticate resolves a presented bearer to its authorization, or to a typed
// refusal: absent, unknown, expired or revoked.
//
// Access tokens are audience-bound to <AGENT_GM_PUBLIC_URL>/mcp. The binding
// is inside the hash, so a token minted under a previous origin resolves to
// nothing and is refused as unknown (spec sections 9.6, 15.6).
func (s *Service) Authenticate(ctx context.Context, presentedAccessToken string) (*Authorization, error) {
	if len(strings.TrimSpace(presentedAccessToken)) == 0 {
		return nil, ErrTokenAbsent
	}
	presentedDigest := tokenDigest(s.audience, store.TokenKindAccess, presentedAccessToken)
	row, err := s.st.TokenByHash(ctx, hexDigest(presentedDigest))
	if err != nil {
		if errors.Is(err, store.ErrTokenNotFound) {
			return nil, ErrTokenUnknown
		}
		return nil, err
	}
	if !equalHashHex(presentedDigest, row.TokenHash) {
		return nil, ErrTokenUnknown
	}
	if row.Revoked() {
		return nil, ErrTokenRevoked
	}
	now := s.clk.Now()
	if now.UnixMilli() >= row.ExpiresAtMS {
		return nil, ErrTokenExpired
	}

	auth, err := s.st.Authorization(ctx, row.AuthorizationID)
	if err != nil {
		if errors.Is(err, store.ErrAuthorizationNotFound) {
			return nil, ErrTokenUnknown
		}
		return nil, err
	}
	if auth.Revoked() {
		return nil, ErrTokenRevoked
	}
	if auth.ExpiresAtMS != 0 && now.UnixMilli() >= auth.ExpiresAtMS {
		return nil, ErrTokenExpired
	}

	scopes, err := ParseScopeString(auth.Scopes)
	if err != nil {
		return nil, err
	}
	minted, err := ParseScopeString(auth.MintedScopes)
	if err != nil {
		return nil, err
	}
	out := &Authorization{
		ID:           auth.ID,
		Kind:         auth.Kind,
		Scopes:       scopes,
		MintedScopes: minted,
		Source:       auth.Source,
	}
	if auth.ExpiresAtMS != 0 {
		out.ExpiresAt = time.UnixMilli(auth.ExpiresAtMS).UTC()
	}
	return out, nil
}

// AllowRequest applies the ordinary-traffic bucket for one route class.
//
// There is deliberately no account parameter and no scope parameter: the
// buckets are per authorization, so holding several scopes does not multiply
// the allowance and neither does holding several accounts (spec section 12.3,
// section 16 Slice 2 tests 30 and 40). Adding a parameter here is how that
// invariant would be lost, so the signature is the guardrail.
func (s *Service) AllowRequest(auth *Authorization, spec BucketSpec) error {
	if auth == nil {
		return ErrTokenAbsent
	}
	return s.Buckets.Allow(spec, auth.ID)
}

// ttls reads the three admin TTL settings.
func (s *Service) ttls(ctx context.Context) (access, idle, absolute time.Duration, err error) {
	if access, err = s.settings.Duration(ctx, SettingAdminAccessTokenTTL); err != nil {
		return
	}
	if idle, err = s.settings.Duration(ctx, SettingAdminRefreshTokenIdleTTL); err != nil {
		return
	}
	absolute, err = s.settings.Duration(ctx, SettingAdminRefreshTokenAbsoluteTL)
	return
}

// appendStandalone writes an audit row that has no change to commit with, in
// its own transaction.
func (s *Service) appendStandalone(ctx context.Context, kind, result, accountID, source string, fields map[string]any) error {
	if err := s.st.AuthzTx(ctx, func(t *store.AuthzTx) error {
		return t.AppendAudit(kind, result, accountID, "", source, renderPayload(fields))
	}); err != nil {
		return err
	}
	s.observe(ctx, kind, fields)
	return nil
}

func (s *Service) observe(ctx context.Context, kind string, fields map[string]any) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Append(ctx, kind, fields)
}

// normalizeSource guarantees spec section 12.3's rule that a resolved source
// is never the empty string, at the one place that would otherwise let an
// empty string become its own limiter bucket.
func normalizeSource(source string) string {
	source = strings.TrimSpace(source)
	if len(source) == 0 {
		return SourceUnknown
	}
	return source
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
