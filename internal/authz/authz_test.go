package authz_test

// The Slice 2 acceptance tests this package owns (spec section 16): 22, 23
// (comparison_audit_test.go), 24, 25, 29, 30 and 40, plus the exit-3 and
// exit-4 preconditions of test 21 and the refresh-reuse rule of section 9.6.
//
// Everything here runs in the ordinary suite. Nothing needs a phone, Docker or
// a network, and nothing waits: every timeout is advanced on the injected
// clock.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// A fictional admin secret, well over section 12.1's 43-character floor.
const testAdminSecret = "agent-gm-test-admin-secret-0000000000000000000000"

const testPublicURL = "https://gm.example.invalid"

const testSource = "198.51.100.7"

type harness struct {
	t        *testing.T
	dir      string
	clk      *clock.Fake
	settings *authz.MemorySettings
	audit    *authz.RecordingAudit
	st       *store.Store
	svc      *authz.Service
	secret   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		dir:      t.TempDir(),
		clk:      clock.NewFake(),
		settings: authz.NewMemorySettings(),
		secret:   testAdminSecret,
	}
	h.open()
	t.Cleanup(func() {
		if h.st != nil {
			_ = h.st.Close()
		}
	})
	return h
}

func (h *harness) open() {
	h.t.Helper()
	st, err := store.Open(h.dir, h.clk)
	if err != nil {
		h.t.Fatalf("opening store: %v", err)
	}
	h.st = st
	h.audit = authz.NewRecordingAudit()
	svc, err := authz.New(st, h.clk, h.settings, h.audit, authz.Config{
		AdminSecret: h.secret,
		PublicURL:   testPublicURL,
	})
	if err != nil {
		h.t.Fatalf("building authz service: %v", err)
	}
	h.svc = svc
}

// restart closes the database and reopens it, exactly as a process restart
// would. The clock and the data directory survive; every in-memory limiter
// does not. Anything still enforced afterwards is durable by definition.
func (h *harness) restart() {
	h.t.Helper()
	if err := h.st.Close(); err != nil {
		h.t.Fatalf("closing store: %v", err)
	}
	h.open()
}

// restartWithSecret is the section 12.1 rotation: a new process, a new
// AGENT_GM_ADMIN_SECRET, same data directory.
func (h *harness) restartWithSecret(secret string) {
	h.t.Helper()
	h.secret = secret
	h.restart()
}

func (h *harness) mint(scopes []string) *authz.Session {
	h.t.Helper()
	s, err := h.svc.MintAdminSession(context.Background(), h.secret, scopes, testSource)
	if err != nil {
		h.t.Fatalf("minting admin session: %v", err)
	}
	return s
}

func scopeStrings(s authz.ScopeSet) string { return s.String() }

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 24.
// ---------------------------------------------------------------------------

func TestSlice2Test24AdminSessionMintNarrowingAndAudit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	full := h.mint(nil)
	if got, want := scopeStrings(full.Scopes), "admin messages:read messages:write messages:delete"; got != want {
		t.Fatalf("admin bootstrap minted %q, want %q (spec section 9.7)", got, want)
	}
	if full.Narrowed {
		t.Fatal("an unnarrowed mint reported Narrowed")
	}

	narrow := h.mint([]string{"messages:read"})
	if got, want := scopeStrings(narrow.Scopes), "messages:read"; got != want {
		t.Fatalf("narrowed mint gave %q, want %q", got, want)
	}
	if !narrow.Narrowed {
		t.Fatal("a narrowed mint did not report Narrowed")
	}
	// minted_scopes records what THIS session was minted with, which is what a
	// later refresh is measured against.
	if got, want := scopeStrings(narrow.MintedScopes), "messages:read"; got != want {
		t.Fatalf("minted scopes %q, want %q", got, want)
	}

	// A widening beyond the four is invalid_scope.
	if _, err := h.svc.MintAdminSession(ctx, h.secret, []string{"messages:read", "admin:super"}, testSource); !errors.Is(err, authz.ErrInvalidScope) {
		t.Fatalf("widening mint gave %v, want ErrInvalidScope", err)
	}

	// The audit rows.
	kinds, err := h.st.AuditKinds(ctx)
	if err != nil {
		t.Fatalf("reading audit kinds: %v", err)
	}
	if n := countOf(kinds, authz.AuditAdminSessionMinted); n != 2 {
		t.Fatalf("got %d %s rows, want 2", n, authz.AuditAdminSessionMinted)
	}
	minted := h.audit.OfKind(authz.AuditAdminSessionMinted)
	if len(minted) != 2 {
		t.Fatalf("recorder saw %d mint rows, want 2", len(minted))
	}
	if minted[0].Fields["source"] != testSource {
		t.Fatalf("mint audit source %v, want %q", minted[0].Fields["source"], testSource)
	}
	if minted[1].Fields["narrowed"] != true {
		t.Fatal("the narrowed mint's audit row does not say so")
	}
}

func TestSlice2Test24WrongSecretCooldownSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const wrong = "agent-gm-test-WRONG-secret-1111111111111111111111"

	// Five wrong secrets: each refused as a bad secret, none rate limited.
	for i := 1; i <= 5; i++ {
		_, err := h.svc.MintAdminSession(ctx, wrong, nil, testSource)
		if !errors.Is(err, authz.ErrInvalidSecret) {
			t.Fatalf("attempt %d gave %v, want ErrInvalidSecret", i, err)
		}
	}

	// The sixth is refused before the secret is examined at all: the section
	// 12.3 limit of 5 per 15 minutes per source has tripped.
	_, err := h.svc.MintAdminSession(ctx, wrong, nil, testSource)
	var limited *authz.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("the sixth wrong secret gave %v, want a rate_limited refusal", err)
	}
	if limited.RetryAfter <= 0 {
		t.Fatalf("rate_limited carried Retry-After %v", limited.RetryAfter)
	}

	// And the RIGHT secret is refused too, which is the point of a cooldown.
	if _, err := h.svc.MintAdminSession(ctx, h.secret, nil, testSource); !errors.Is(err, authz.ErrRateLimited) {
		t.Fatalf("the correct secret during a cooldown gave %v, want rate_limited", err)
	}

	// The cooldown lives in oauth_attempts, so it survives a restart. That is
	// the whole reason it is not in memory (spec section 9.8).
	h.restart()
	if _, err := h.svc.MintAdminSession(ctx, h.secret, nil, testSource); !errors.Is(err, authz.ErrRateLimited) {
		t.Fatalf("after a restart the cooldown gave %v, want rate_limited", err)
	}

	// Another source is unaffected: the limit is per source.
	if _, err := h.svc.MintAdminSession(ctx, h.secret, nil, "203.0.113.4"); err != nil {
		t.Fatalf("a different source was refused: %v", err)
	}

	// Advance past the cooldown and the original source works again.
	h.clk.Advance(2 * time.Minute)
	if _, err := h.svc.MintAdminSession(ctx, h.secret, nil, testSource); err != nil {
		t.Fatalf("after the cooldown elapsed: %v", err)
	}
}

func TestSlice2Test24NoAuditRowContainsThePresentedValue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const wrong = "agent-gm-test-WRONG-secret-2222222222222222222222"

	session := h.mint(nil)
	if _, err := h.svc.MintAdminSession(ctx, wrong, nil, testSource); !errors.Is(err, authz.ErrInvalidSecret) {
		t.Fatalf("wrong secret gave %v", err)
	}

	sentinels := []string{h.secret, wrong, session.AccessToken, session.RefreshToken}

	for _, kind := range []string{authz.AuditAdminSessionMinted, authz.AuditAdminSecretFailed} {
		payloads, err := h.st.AuditPayloads(ctx, kind)
		if err != nil {
			t.Fatalf("reading %s payloads: %v", kind, err)
		}
		if len(payloads) == 0 {
			t.Fatalf("no %s rows were written", kind)
		}
		for _, p := range payloads {
			for _, s := range sentinels {
				if strings.Contains(p, s) {
					t.Fatalf("%s payload contains a presented value: %s", kind, p)
				}
			}
		}
	}

	// And the same for the observation sink.
	blob, err := h.audit.JSON()
	if err != nil {
		t.Fatalf("rendering audit records: %v", err)
	}
	for _, s := range sentinels {
		if strings.Contains(blob, s) {
			t.Fatal("an audit record contains a presented value")
		}
	}

	// The failure row does carry the cooldown state, which section 12.4
	// requires.
	failed := h.audit.OfKind(authz.AuditAdminSecretFailed)
	if len(failed) != 1 {
		t.Fatalf("got %d admin_secret_failed rows, want 1", len(failed))
	}
	if _, ok := failed[0].Fields["cooldown_until_ms"]; !ok {
		t.Fatal("admin_secret_failed does not carry the cooldown state")
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 22 -- the load-bearing one.
// ---------------------------------------------------------------------------

func TestSlice2Test22NarrowedAdminSessionCannotReWiden(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	full := h.mint(nil)

	// Narrow to two scopes on the first refresh.
	narrowed, err := h.svc.Refresh(ctx, full.RefreshToken, []string{"admin", "messages:read"}, testSource)
	if err != nil {
		t.Fatalf("narrowing refresh: %v", err)
	}
	if got, want := scopeStrings(narrowed.Scopes), "admin messages:read"; got != want {
		t.Fatalf("after narrowing, scopes %q, want %q", got, want)
	}

	// Now try to widen back to the full set. This is the case section 9.6
	// calls out by name.
	_, err = h.svc.Refresh(ctx, narrowed.RefreshToken,
		[]string{"admin", "messages:read", "messages:write", "messages:delete"}, testSource)
	if !errors.Is(err, authz.ErrInvalidScope) {
		t.Fatalf("re-widening gave %v, want ErrInvalidScope", err)
	}

	// The session's scopes are unchanged.
	auth, err := h.svc.Authenticate(ctx, narrowed.AccessToken)
	if err != nil {
		t.Fatalf("authenticating after the refused widening: %v", err)
	}
	if got, want := scopeStrings(auth.Scopes), "admin messages:read"; got != want {
		t.Fatalf("the refused widening changed the session's scopes to %q, want %q", got, want)
	}

	// The refused attempt did NOT spend the presented refresh token -- proven
	// by using it successfully now.
	again, err := h.svc.Refresh(ctx, narrowed.RefreshToken, nil, testSource)
	if err != nil {
		t.Fatalf("the refused widening spent the refresh token: %v", err)
	}
	if got, want := scopeStrings(again.Scopes), "admin messages:read"; got != want {
		t.Fatalf("after the successful refresh, scopes %q, want %q", got, want)
	}

	// And the out-of-scope route is still exit 4.
	after, err := h.svc.Authenticate(ctx, again.AccessToken)
	if err != nil {
		t.Fatalf("authenticating the rotated access token: %v", err)
	}
	if err := after.Require(authz.ScopeMessagesWrite); !errors.Is(err, authz.ErrInsufficientScope) {
		t.Fatalf("a narrowed session on a write route gave %v, want ErrInsufficientScope", err)
	}

	// Re-widening requires presenting the admin secret again -- and that
	// works, producing a NEW session at the full grant.
	fresh := h.mint(nil)
	if got, want := scopeStrings(fresh.Scopes), "admin messages:read messages:write messages:delete"; got != want {
		t.Fatalf("re-minting with the secret gave %q, want %q", got, want)
	}
}

func TestRefreshNarrowingIsRecordedAndCannotWidenBeyondMinted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A session MINTED narrow cannot refresh into anything it never held,
	// even though those scopes are inside the admin bootstrap grant.
	minted := h.mint([]string{"messages:read", "messages:write"})
	_, err := h.svc.Refresh(ctx, minted.RefreshToken, []string{"admin"}, testSource)
	if !errors.Is(err, authz.ErrInvalidScope) {
		t.Fatalf("refreshing into a scope outside minted_scopes gave %v, want ErrInvalidScope", err)
	}

	narrowed, err := h.svc.Refresh(ctx, minted.RefreshToken, []string{"messages:read"}, testSource)
	if err != nil {
		t.Fatalf("narrowing refresh: %v", err)
	}
	if got, want := scopeStrings(narrowed.MintedScopes), "messages:read messages:write"; got != want {
		t.Fatalf("minted scopes changed to %q; they must stay %q", got, want)
	}
	if n := len(h.audit.OfKind(authz.AuditAdminSessionNarrowed)); n != 1 {
		t.Fatalf("got %d %s rows, want 1", n, authz.AuditAdminSessionNarrowed)
	}
	refreshed := h.audit.OfKind(authz.AuditAdminSessionRefreshed)
	if len(refreshed) != 1 {
		t.Fatalf("got %d %s rows, want 1", len(refreshed), authz.AuditAdminSessionRefreshed)
	}
	before, _ := json.Marshal(refreshed[0].Fields["scopes_before"])
	after, _ := json.Marshal(refreshed[0].Fields["scopes_after"])
	if string(before) != `["messages:read","messages:write"]` || string(after) != `["messages:read"]` {
		t.Fatalf("refresh audit recorded before=%s after=%s", before, after)
	}
}

// ---------------------------------------------------------------------------
// Refresh rotation and reuse (spec section 9.6).
// ---------------------------------------------------------------------------

func TestRefreshTokenReuseRevokesFamilyAndAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first := h.mint(nil)
	second, err := h.svc.Refresh(ctx, first.RefreshToken, nil, testSource)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("the refresh token did not rotate")
	}

	// Reuse of the spent token.
	if _, err := h.svc.Refresh(ctx, first.RefreshToken, nil, testSource); !errors.Is(err, authz.ErrTokenReused) {
		t.Fatalf("reuse gave %v, want ErrTokenReused", err)
	}

	// The whole family is gone, and so is the authorization.
	if _, err := h.svc.Authenticate(ctx, second.AccessToken); !errors.Is(err, authz.ErrTokenRevoked) {
		t.Fatalf("the rotated access token after reuse gave %v, want ErrTokenRevoked", err)
	}
	if _, err := h.svc.Refresh(ctx, second.RefreshToken, nil, testSource); !errors.Is(err, authz.ErrTokenRevoked) {
		t.Fatalf("the live refresh token after reuse gave %v, want ErrTokenRevoked", err)
	}

	auth, err := h.st.Authorization(ctx, first.AuthorizationID)
	if err != nil {
		t.Fatalf("reading the authorization: %v", err)
	}
	if !auth.Revoked() {
		t.Fatal("reuse did not revoke the authorization")
	}

	// The reuse detection, the family revocation and the audit row committed
	// together: the audit row exists precisely because the revocation did.
	payloads, err := h.st.AuditPayloads(ctx, authz.AuditRefreshTokenReuse)
	if err != nil {
		t.Fatalf("reading reuse payloads: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("got %d reuse audit rows, want 1", len(payloads))
	}
	if !strings.Contains(payloads[0], first.AuthorizationID) {
		t.Fatalf("the reuse audit row does not name the authorization: %s", payloads[0])
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 25.
// ---------------------------------------------------------------------------

func TestSlice2Test25RefreshDurableBudgetSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	live := h.mint(nil)

	// Thirty invalid presented tokens inside fifteen minutes. The clock moves
	// a second between them so the in-memory 60/min burst of section 9.8 --
	// which is a different limit with a different lifetime -- is not what
	// refuses them.
	for i := 1; i <= 30; i++ {
		_, err := h.svc.Refresh(ctx, "not-a-token-"+strings.Repeat("x", i), nil, testSource)
		if !errors.Is(err, authz.ErrTokenUnknown) {
			t.Fatalf("invalid presentation %d gave %v, want ErrTokenUnknown", i, err)
		}
		h.clk.Advance(time.Second)
	}

	// The thirty-first is refused before the token is examined -- and so is a
	// presentation of the REAL token. The two refusals must be
	// indistinguishable.
	_, guessErr := h.svc.Refresh(ctx, "a-guess-that-belongs-to-nobody", nil, testSource)
	_, realErr := h.svc.Refresh(ctx, live.RefreshToken, nil, testSource)

	var guessLimited, realLimited *authz.RateLimitedError
	if !errors.As(guessErr, &guessLimited) {
		t.Fatalf("a guess after the budget gave %v, want rate_limited", guessErr)
	}
	if !errors.As(realErr, &realLimited) {
		t.Fatalf("the real token after the budget gave %v, want rate_limited", realErr)
	}
	if *guessLimited != *realLimited {
		t.Fatalf("a 429 distinguishes a real token (%+v) from a guess (%+v)", realLimited, guessLimited)
	}
	if guessLimited.Error() != realLimited.Error() {
		t.Fatalf("the two 429s render differently: %q vs %q", realLimited.Error(), guessLimited.Error())
	}
	if guessLimited.RetryAfter <= 0 {
		t.Fatal("a rate_limited refusal must carry Retry-After")
	}

	// The real token was NOT spent by the refused presentation.
	// (Proven after the cooldown elapses, below.)

	// Durable: it survives a restart.
	h.restart()
	if _, err := h.svc.Refresh(ctx, live.RefreshToken, nil, testSource); !errors.Is(err, authz.ErrRateLimited) {
		t.Fatalf("after a restart the cooldown gave %v, want rate_limited", err)
	}

	// After the cooldown, the real token still works: a 429 spends nothing.
	h.clk.Advance(2 * time.Minute)
	if _, err := h.svc.Refresh(ctx, live.RefreshToken, nil, testSource); err != nil {
		t.Fatalf("the real refresh token after the cooldown: %v", err)
	}
}

func TestRefreshSuccessDoesNotClearTheFailureCounter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	live := h.mint(nil)

	for i := 0; i < 10; i++ {
		if _, err := h.svc.Refresh(ctx, "nope-"+strings.Repeat("y", i), nil, testSource); !errors.Is(err, authz.ErrTokenUnknown) {
			t.Fatalf("invalid presentation %d gave %v", i, err)
		}
		h.clk.Advance(time.Second)
	}
	if _, err := h.svc.Refresh(ctx, live.RefreshToken, nil, testSource); err != nil {
		t.Fatalf("a valid refresh: %v", err)
	}
	// Spec section 9.8: "A successful presentation does not clear the failure
	// counter."
	row, ok, err := h.st.AttemptRow(ctx, store.AttemptKindRefresh, testSource)
	if err != nil || !ok {
		t.Fatalf("reading the attempt row: %v (found=%v)", err, ok)
	}
	if row.Failures != 10 {
		t.Fatalf("the failure counter is %d after a success, want 10", row.Failures)
	}
}

func TestRefreshInMemoryBurstIsCheckedBeforeTheToken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	live := h.mint(nil)

	// Burst 20 on a frozen clock: the twenty-first request is refused whatever
	// it presents.
	for i := 0; i < 20; i++ {
		_, _ = h.svc.Refresh(ctx, "burn-"+strings.Repeat("z", i), nil, testSource)
	}
	if _, err := h.svc.Refresh(ctx, live.RefreshToken, nil, testSource); !errors.Is(err, authz.ErrRateLimited) {
		t.Fatalf("the 21st request in one instant gave %v, want rate_limited", err)
	}
}

// ---------------------------------------------------------------------------
// Exit 3 and exit 4 preconditions (section 16 Slice 2 test 21).
// ---------------------------------------------------------------------------

func TestSlice2Exit3ExpiredAdminAccessTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.mint(nil)
	if _, err := h.svc.Authenticate(ctx, session.AccessToken); err != nil {
		t.Fatalf("a fresh access token: %v", err)
	}

	// Advanced past admin.access_token_ttl on the injected clock, not waited
	// out.
	ttl, err := h.settings.Duration(ctx, authz.SettingAdminAccessTokenTTL)
	if err != nil {
		t.Fatalf("reading the TTL: %v", err)
	}
	if ttl != 15*time.Minute {
		t.Fatalf("admin.access_token_ttl defaults to %s, want 15m (spec section 15.1)", ttl)
	}
	h.clk.Advance(ttl)
	if _, err := h.svc.Authenticate(ctx, session.AccessToken); !errors.Is(err, authz.ErrTokenExpired) {
		t.Fatalf("an expired access token gave %v, want ErrTokenExpired", err)
	}

	// The refresh token has a much longer life, so the session recovers.
	if _, err := h.svc.Refresh(ctx, session.RefreshToken, nil, testSource); err != nil {
		t.Fatalf("refreshing after the access token expired: %v", err)
	}
}

func TestSlice2Exit4NarrowedSessionIsInsufficientScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.mint([]string{"messages:read"})
	auth, err := h.svc.Authenticate(ctx, session.AccessToken)
	if err != nil {
		t.Fatalf("authenticating: %v", err)
	}
	if err := auth.Require(authz.ScopeMessagesRead); err != nil {
		t.Fatalf("a read route with messages:read: %v", err)
	}
	for _, sc := range []authz.Scope{authz.ScopeMessagesWrite, authz.ScopeMessagesDelete, authz.ScopeAdmin} {
		if err := auth.Require(sc); !errors.Is(err, authz.ErrInsufficientScope) {
			t.Fatalf("%s gave %v, want ErrInsufficientScope", sc, err)
		}
	}
}

func TestAuthenticateTypedRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.svc.Authenticate(ctx, ""); !errors.Is(err, authz.ErrTokenAbsent) {
		t.Fatalf("an absent bearer gave %v, want ErrTokenAbsent", err)
	}
	if _, err := h.svc.Authenticate(ctx, "  "); !errors.Is(err, authz.ErrTokenAbsent) {
		t.Fatalf("a blank bearer gave %v, want ErrTokenAbsent", err)
	}
	if _, err := h.svc.Authenticate(ctx, "belongs-to-nobody"); !errors.Is(err, authz.ErrTokenUnknown) {
		t.Fatalf("an unknown bearer gave %v, want ErrTokenUnknown", err)
	}

	session := h.mint(nil)
	// A refresh token presented as an access token is unknown: the kind is
	// inside the hash.
	if _, err := h.svc.Authenticate(ctx, session.RefreshToken); !errors.Is(err, authz.ErrTokenUnknown) {
		t.Fatalf("a refresh token at an access-token check gave %v, want ErrTokenUnknown", err)
	}
}

func TestAccessTokensAreAudienceBound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if got, want := h.svc.Audience(), testPublicURL+"/mcp"; got != want {
		t.Fatalf("audience %q, want %q", got, want)
	}
	session := h.mint(nil)

	// A second service over the SAME database under a different public URL:
	// section 15.6's "every token minted under the previous origin is refused".
	other, err := authz.New(h.st, h.clk, h.settings, nil, authz.Config{
		AdminSecret: h.secret,
		PublicURL:   "https://gm.moved.invalid",
	})
	if err != nil {
		t.Fatalf("building the second service: %v", err)
	}
	if _, err := other.Authenticate(ctx, session.AccessToken); !errors.Is(err, authz.ErrTokenUnknown) {
		t.Fatalf("a token from the previous origin gave %v, want ErrTokenUnknown", err)
	}
}

// ---------------------------------------------------------------------------
// Section 12.1: changing the admin secret revokes previous sessions.
// ---------------------------------------------------------------------------

func TestChangingTheAdminSecretRevokesPreviousSessionsOnNextStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	old := h.mint(nil)
	if _, err := h.svc.Authenticate(ctx, old.AccessToken); err != nil {
		t.Fatalf("the session before the rotation: %v", err)
	}

	h.restartWithSecret("agent-gm-test-admin-secret-9999999999999999999999")
	revoked, err := h.svc.RevokeSupersededAdminSessions(ctx)
	if err != nil {
		t.Fatalf("revoking superseded sessions: %v", err)
	}
	if len(revoked) != 1 || revoked[0] != old.AuthorizationID {
		t.Fatalf("revoked %v, want [%s]", revoked, old.AuthorizationID)
	}
	if _, err := h.svc.Authenticate(ctx, old.AccessToken); !errors.Is(err, authz.ErrTokenRevoked) {
		t.Fatalf("the old access token gave %v, want ErrTokenRevoked", err)
	}
	if _, err := h.svc.Refresh(ctx, old.RefreshToken, nil, testSource); !errors.Is(err, authz.ErrTokenRevoked) {
		t.Fatalf("the old refresh token gave %v, want ErrTokenRevoked", err)
	}

	// The new secret mints, and a second start with the SAME secret revokes
	// nothing.
	fresh := h.mint(nil)
	h.restart()
	again, err := h.svc.RevokeSupersededAdminSessions(ctx)
	if err != nil {
		t.Fatalf("second startup revocation: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("an unchanged secret revoked %v", again)
	}
	if _, err := h.svc.Authenticate(ctx, fresh.AccessToken); err != nil {
		t.Fatalf("the current session after an unchanged restart: %v", err)
	}
}

func TestShortAdminSecretRefusesToStart(t *testing.T) {
	h := newHarness(t)
	_, err := authz.New(h.st, h.clk, h.settings, nil, authz.Config{
		AdminSecret: strings.Repeat("a", authz.MinAdminSecretLength-1),
		PublicURL:   testPublicURL,
	})
	if err == nil {
		t.Fatal("a secret shorter than 43 characters was accepted (spec section 12.1)")
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 tests 30 and 40.
// ---------------------------------------------------------------------------

func TestSlice2Test30OneAllowancePerSurfaceNotPerScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.mint(nil)
	auth, err := h.svc.Authenticate(ctx, session.AccessToken)
	if err != nil {
		t.Fatalf("authenticating: %v", err)
	}
	if auth.Scopes.Len() != 4 {
		t.Fatalf("the session holds %d scopes, want 4", auth.Scopes.Len())
	}

	// The clock is frozen, so exactly the burst is available and no more.
	reads := allowUntilRefused(t, h, auth, authz.BucketReads)
	if reads != int(authz.BucketReads.Burst) {
		t.Fatalf("the read bucket allowed %d, want %d -- four scopes must not multiply one allowance",
			reads, int(authz.BucketReads.Burst))
	}

	// A different surface is a different bucket, still one allowance.
	mutations := allowUntilRefused(t, h, auth, authz.BucketMutations)
	if mutations != int(authz.BucketMutations.Burst) {
		t.Fatalf("the mutation bucket allowed %d, want %d", mutations, int(authz.BucketMutations.Burst))
	}

	admin := allowUntilRefused(t, h, auth, authz.BucketAdmin)
	if admin != int(authz.BucketAdmin.Burst) {
		t.Fatalf("the admin bucket allowed %d, want %d", admin, int(authz.BucketAdmin.Burst))
	}

	// The refusal is rate_limited with Retry-After.
	err = h.svc.AllowRequest(auth, authz.BucketReads)
	var limited *authz.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("exceeding a bucket gave %v, want rate_limited", err)
	}
	if limited.RetryAfter <= 0 {
		t.Fatal("rate_limited must carry Retry-After")
	}

	// And the bucket refills on the injected clock.
	h.clk.Advance(time.Second)
	if err := h.svc.AllowRequest(auth, authz.BucketReads); err != nil {
		t.Fatalf("after a second of refill: %v", err)
	}
}

func allowUntilRefused(t *testing.T, h *harness, auth *authz.Authorization, spec authz.BucketSpec) int {
	t.Helper()
	n := 0
	for i := 0; i < int(spec.Burst)+50; i++ {
		if err := h.svc.AllowRequest(auth, spec); err != nil {
			return n
		}
		n++
	}
	t.Fatalf("the %s bucket never refused", spec.Name)
	return n
}

func TestSlice2Test40RateLimitsDoNotMultiplyWithAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := h.mint(nil)
	auth, err := h.svc.Authenticate(ctx, session.AccessToken)
	if err != nil {
		t.Fatalf("authenticating: %v", err)
	}

	// One authorization sending to two accounts. The account is not a
	// parameter of the limiter at all -- it cannot be, by the signature -- so
	// the two accounts share one mutation budget.
	accounts := []string{"acct_one", "acct_two"}
	allowed := 0
	for i := 0; i < int(authz.BucketMutations.Burst)+20; i++ {
		_ = accounts[i%len(accounts)] // the account a caller is sending to
		if err := h.svc.AllowRequest(auth, authz.BucketMutations); err != nil {
			break
		}
		allowed++
	}
	if allowed != int(authz.BucketMutations.Burst) {
		t.Fatalf("two accounts got %d mutations, want %d -- adding an account must divide the "+
			"per-account send rate, not add to it (spec section 12.3, D29)",
			allowed, int(authz.BucketMutations.Burst))
	}

	// A second authorization has its own budget: the bucket is per
	// authorization.
	second := h.mint(nil)
	auth2, err := h.svc.Authenticate(ctx, second.AccessToken)
	if err != nil {
		t.Fatalf("authenticating the second session: %v", err)
	}
	if err := h.svc.AllowRequest(auth2, authz.BucketMutations); err != nil {
		t.Fatalf("a second authorization was refused on the first's budget: %v", err)
	}
}

func TestConcurrencyLimitsArePerAuthorizationAndGlobal(t *testing.T) {
	uploads := authz.NewConcurrencyLimiter(authz.ConcurrencyUploads)

	var releases []func()
	for i := 0; i < authz.ConcurrencyUploads.PerAuthorization; i++ {
		rel, err := uploads.Acquire("authz_a")
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
		releases = append(releases, rel)
	}
	if _, err := uploads.Acquire("authz_a"); !errors.Is(err, authz.ErrRateLimited) {
		t.Fatalf("the 5th concurrent upload gave %v, want rate_limited", err)
	}
	// Another authorization still has its own four.
	rel, err := uploads.Acquire("authz_b")
	if err != nil {
		t.Fatalf("a second authorization was refused: %v", err)
	}
	rel()

	releases[0]()
	if _, err := uploads.Acquire("authz_a"); err != nil {
		t.Fatalf("after a release: %v", err)
	}

	// The global ceiling.
	global := authz.NewConcurrencyLimiter(authz.ConcurrencyDownloads)
	held := 0
	for i := 0; i < authz.ConcurrencyDownloads.Global+10; i++ {
		key := "authz_" + string(rune('a'+i/authz.ConcurrencyDownloads.PerAuthorization))
		if _, err := global.Acquire(key); err != nil {
			break
		}
		held++
	}
	if held != authz.ConcurrencyDownloads.Global {
		t.Fatalf("the global download limiter held %d, want %d", held, authz.ConcurrencyDownloads.Global)
	}
}

// ---------------------------------------------------------------------------
// Section 16 Slice 2 test 29.
// ---------------------------------------------------------------------------

func TestSlice2Test29TrustedProxyResolution(t *testing.T) {
	const trusted = "10.0.0.0/8, 127.0.0.1"

	cases := []struct {
		name     string
		cidrs    string
		peer     string
		xff      []string
		want     string
		wantMode string
	}{
		{
			name:     "no trusted proxies ignores forwarded headers entirely",
			cidrs:    "",
			peer:     "10.0.0.5:44321",
			xff:      []string{"1.2.3.4"},
			want:     "10.0.0.5",
			wantMode: authz.SourceModeSocketPeer,
		},
		{
			name:     "a forged header from an untrusted peer is ignored",
			cidrs:    trusted,
			peer:     "203.0.113.9:1234",
			xff:      []string{"1.2.3.4"},
			want:     "203.0.113.9",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "the rightmost non-trusted entry wins",
			cidrs:    trusted,
			peer:     "10.0.0.5:1234",
			xff:      []string{"1.2.3.4, 198.51.100.9, 10.0.0.7"},
			want:     "198.51.100.9",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "an unparseable entry stops the walk at the TCP peer",
			cidrs:    trusted,
			peer:     "10.0.0.5:1234",
			xff:      []string{"1.2.3.4, not-an-address, 10.0.0.7"},
			want:     "10.0.0.5",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "every entry trusted falls back to the peer",
			cidrs:    trusted,
			peer:     "10.0.0.5:1234",
			xff:      []string{"10.1.1.1, 10.2.2.2"},
			want:     "10.0.0.5",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "an IPv4-mapped IPv6 peer matches an IPv4 CIDR",
			cidrs:    trusted,
			peer:     "[::ffff:127.0.0.1]:9999",
			xff:      []string{"198.51.100.22"},
			want:     "198.51.100.22",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "several headers are one list",
			cidrs:    trusted,
			peer:     "10.0.0.5:1234",
			xff:      []string{"1.2.3.4", "198.51.100.30, 10.0.0.9"},
			want:     "198.51.100.30",
			wantMode: authz.SourceModeTrustedProxy,
		},
		{
			name:     "an unparseable RemoteAddr still resolves to something",
			cidrs:    "",
			peer:     "not-an-address-at-all",
			want:     authz.SourceUnknown,
			wantMode: authz.SourceModeSocketPeer,
		},
		{
			name:     "an entry carrying a port is read",
			cidrs:    trusted,
			peer:     "10.0.0.5:1234",
			xff:      []string{"198.51.100.44:5555, 10.0.0.9"},
			want:     "198.51.100.44",
			wantMode: authz.SourceModeTrustedProxy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := authz.NewSourceResolver(tc.cidrs)
			if err != nil {
				t.Fatalf("building the resolver: %v", err)
			}
			h := http.Header{}
			for _, v := range tc.xff {
				h.Add("X-Forwarded-For", v)
			}
			got := r.Resolve(tc.peer, h)
			if got.Value != tc.want {
				t.Fatalf("resolved %q, want %q", got.Value, tc.want)
			}
			if got.Mode != tc.wantMode {
				t.Fatalf("mode %q, want %q", got.Mode, tc.wantMode)
			}
			if got.Value == "" {
				t.Fatal("the resolved source must never be the empty string")
			}
		})
	}
}

func TestSlice2Test29InvalidCIDRListRefusesToStart(t *testing.T) {
	for _, bad := range []string{"10.0.0.0/99", "not-a-cidr", "10.0.0.0/8, garbage", "10.0.0.0/8,,300.1.1.1"} {
		if _, err := authz.NewSourceResolver(bad); err == nil {
			t.Fatalf("%q was accepted as a trusted-proxy list", bad)
		}
	}
	// And the Service refuses to start with one.
	h := newHarness(t)
	if _, err := authz.New(h.st, h.clk, h.settings, nil, authz.Config{
		AdminSecret:       h.secret,
		PublicURL:         testPublicURL,
		TrustedProxyCIDRs: "10.0.0.0/8, nonsense",
	}); err == nil {
		t.Fatal("the service started with an invalid AGENT_GM_TRUSTED_PROXY_CIDRS (spec section 12.3)")
	}
}

func TestForwardedProtoAndHostAreRecordedNeverUsed(t *testing.T) {
	r, err := authz.NewSourceResolver("10.0.0.0/8")
	if err != nil {
		t.Fatalf("building the resolver: %v", err)
	}
	h := http.Header{}
	h.Set("X-Forwarded-Proto", "http")
	h.Set("X-Forwarded-Host", "evil.example")
	h.Set("X-Forwarded-For", "198.51.100.5")
	got := r.Resolve("10.0.0.5:1234", h)
	if got.ForwardedProto != "http" || got.ForwardedHost != "evil.example" {
		t.Fatalf("forwarded metadata not recorded: %+v", got)
	}
	// The Source type carries no URL and builds none. The only thing derived
	// from a request here is the source itself.
	if got.Value != "198.51.100.5" {
		t.Fatalf("source %q", got.Value)
	}
	if strings.Contains(got.Value, "evil.example") {
		t.Fatal("X-Forwarded-Host reached the source")
	}
}

// ---------------------------------------------------------------------------
// Scopes and settings.
// ---------------------------------------------------------------------------

func TestAdminScopeIsNeverEnrollable(t *testing.T) {
	if authz.ScopeAdmin.Enrollable() {
		t.Fatal("admin must never be enrollable (spec section 9.7)")
	}
	for _, sc := range []authz.Scope{authz.ScopeMessagesRead, authz.ScopeMessagesWrite, authz.ScopeMessagesDelete} {
		if !sc.Enrollable() {
			t.Fatalf("%s should be enrollable", sc)
		}
	}
}

func TestScopeSetNarrowingAndWidening(t *testing.T) {
	full := authz.AdminBootstrapScopes()
	narrow := authz.NewScopeSet(authz.ScopeMessagesRead)
	if !narrow.Subset(full) {
		t.Fatal("messages:read should be a subset of the admin bootstrap grant")
	}
	if narrow.Widens(full) {
		t.Fatal("a narrowing was reported as a widening")
	}
	if !full.Widens(narrow) {
		t.Fatal("the full set does widen a narrowed one")
	}
	if got, want := full.String(), "admin messages:read messages:write messages:delete"; got != want {
		t.Fatalf("canonical rendering %q, want %q", got, want)
	}
	if _, err := authz.ParseScopes([]string{"messages:reed"}); !errors.Is(err, authz.ErrInvalidScope) {
		t.Fatalf("a misspelled scope gave %v, want ErrInvalidScope", err)
	}
}

func TestSettingsDefaultsAndBounds(t *testing.T) {
	s := authz.NewMemorySettings()
	ctx := context.Background()
	for key, want := range map[string]time.Duration{
		authz.SettingAdminAccessTokenTTL:         15 * time.Minute,
		authz.SettingAdminRefreshTokenIdleTTL:    30 * 24 * time.Hour,
		authz.SettingAdminRefreshTokenAbsoluteTL: 90 * 24 * time.Hour,
	} {
		got, err := s.Duration(ctx, key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Fatalf("%s defaults to %s, want %s (spec sections 9.6, 15.1)", key, got, want)
		}
	}
	if err := s.Set(authz.SettingAdminAccessTokenTTL, time.Minute); err == nil {
		t.Fatal("1m is below the 5m bound for admin.access_token_ttl")
	}
	if err := s.Set(authz.SettingAdminAccessTokenTTL, 2*time.Hour); err == nil {
		t.Fatal("2h is above the 1h bound for admin.access_token_ttl")
	}
	if err := s.Set(authz.SettingAdminAccessTokenTTL, 30*time.Minute); err != nil {
		t.Fatalf("30m is inside the bounds: %v", err)
	}
}

func TestAccessTokenTTLComesFromSettings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.settings.Set(authz.SettingAdminAccessTokenTTL, 5*time.Minute); err != nil {
		t.Fatalf("setting the TTL: %v", err)
	}
	session := h.mint(nil)
	h.clk.Advance(4 * time.Minute)
	if _, err := h.svc.Authenticate(ctx, session.AccessToken); err != nil {
		t.Fatalf("inside the configured TTL: %v", err)
	}
	h.clk.Advance(2 * time.Minute)
	if _, err := h.svc.Authenticate(ctx, session.AccessToken); !errors.Is(err, authz.ErrTokenExpired) {
		t.Fatalf("past the configured TTL gave %v, want ErrTokenExpired", err)
	}
}

func countOf(items []string, want string) int {
	n := 0
	for _, i := range items {
		if i == want {
			n++
		}
	}
	return n
}
