package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// The durable per-source budget of spec section 9.8 also guards the
// `authorization_code` grant, not only the refresh grant and /oauth/revoke.
//
// Section 9.8 names the budget for "the presented token", and a reader who
// takes that literally would leave the code grant unbudgeted -- which would
// hand an attacker an unmetered oracle for guessing a code, the one presented
// value that is short-lived enough to be worth guessing quickly.
// ExchangeAuthorizationCode consults LimitOAuthToken before it looks at the
// presented value at all, and this is the test that says so.
//
// It lives here rather than in cmd/agent-gm's HTTP suite for the reason
// TestDurableTokenBudgetSurvivesARestart already gives: the in-memory bucket
// of section 9.8 (60 a minute, burst 20) would refuse the caller long before
// thirty guesses were possible over HTTP, and a test may not wait a minute
// out. Here the clock is injected, so the bucket is kept full by advancing it
// a second between attempts and the refusal under test is unambiguously the
// durable one -- which the assertion on Surface pins down.
//
// Plant: delete the `s.Durable.Check(ctx, LimitOAuthToken, source)` call from
// ExchangeAuthorizationCode and the thirty-first exchange answers
// invalid_grant instead of rate_limited.
func TestCodeGrantIsBudgetedPerSource(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake()
	st, svc := newBudgetService(t, t.TempDir(), clk)
	t.Cleanup(func() { _ = st.Close() })

	const guesser = "203.0.113.9"
	const bystander = "198.51.100.11"

	// Thirty failed exchanges from one source. Each one presents a code that
	// belongs to nobody, which is the cheapest failure the grant has, and
	// each records one durable failure.
	for i := 0; i < 30; i++ {
		clk.Advance(time.Second) // keep the in-memory burst bucket full
		_, err := svc.ExchangeAuthorizationCode(ctx, exchangeOfAnUnknownCode(i, guesser))
		if !errors.Is(err, authz.ErrInvalidGrant) {
			t.Fatalf("guess %d: got %v, want invalid_grant", i, err)
		}
		var limited *authz.RateLimitedError
		if errors.As(err, &limited) {
			t.Fatalf("guess %d was refused by a limiter before thirty were spent: %v", i, err)
		}
	}

	// The thirty-first is refused by the budget, before the presented value
	// is examined at all.
	clk.Advance(time.Second)
	_, err := svc.ExchangeAuthorizationCode(ctx, exchangeOfAnUnknownCode(30, guesser))
	var limited *authz.RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("the thirty-first code exchange from one source was not rate limited: %v", err)
	}
	if limited.Surface != authz.LimitOAuthToken.Surface {
		t.Errorf("the refusal came from the %q limiter, not the durable %q budget",
			limited.Surface, authz.LimitOAuthToken.Surface)
	}
	if limited.RetryAfter <= 0 {
		t.Error("a 429 carries Retry-After or it tells the client to guess")
	}

	// The cooldown is per source: an unrelated caller is untouched, and
	// reaches invalid_grant rather than a 429.
	clk.Advance(time.Second)
	_, other := svc.ExchangeAuthorizationCode(ctx, exchangeOfAnUnknownCode(0, bystander))
	if errors.As(other, &limited) {
		t.Fatalf("one source's cooldown refused another source: %v", other)
	}
	if !errors.Is(other, authz.ErrInvalidGrant) {
		t.Fatalf("an unrelated source got %v, want invalid_grant", other)
	}

	// And it ends on the clock rather than on a wall clock a test would wait
	// out.
	clk.Advance(25 * time.Hour)
	_, after := svc.ExchangeAuthorizationCode(ctx, exchangeOfAnUnknownCode(31, guesser))
	if errors.As(after, &limited) {
		t.Errorf("the cooldown outlived its own deadline: %v", after)
	}
}

// exchangeOfAnUnknownCode is one code exchange whose code hash belongs to
// nobody. Verify is set because the production caller always sets it, and a
// budget that only bit when it was nil would be no budget at all.
func exchangeOfAnUnknownCode(n int, source string) authz.CodeExchange {
	return authz.CodeExchange{
		CodeHash: "no-such-code-hash-" + string(rune('a'+n%26)) + "-" + source,
		ClientID: "client_never_registered",
		Source:   source,
		Verify:   func(store.AuthorizationCode) error { return nil },
	}
}
