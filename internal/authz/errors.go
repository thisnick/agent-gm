package authz

import (
	"errors"
	"fmt"
	"time"
)

// The typed refusals of this package. Each maps to exactly one spec section
// 7.2 error code, named in its comment, so the HTTP layer translates rather
// than decides.
var (
	// ErrInvalidScope is spec section 7.2 `invalid_request`-adjacent
	// `invalid_scope`: a requested scope set that widens, or names a scope
	// outside the four of section 9.7.
	ErrInvalidScope = errors.New("invalid_scope")

	// ErrTokenAbsent is `invalid_token` (401): no bearer was presented.
	ErrTokenAbsent = errors.New("invalid_token: absent")
	// ErrTokenUnknown is `invalid_token` (401): the presented value resolves
	// to no row. A token minted under a previous AGENT_GM_PUBLIC_URL lands
	// here, because the audience is inside the hash (spec sections 9.6, 15.6).
	ErrTokenUnknown = errors.New("invalid_token: unknown")
	// ErrTokenExpired is `invalid_token` (401), measured on the injected clock.
	ErrTokenExpired = errors.New("invalid_token: expired")
	// ErrTokenRevoked is `invalid_token` (401).
	ErrTokenRevoked = errors.New("invalid_token: revoked")
	// ErrTokenReused is `invalid_token` (401) and, uniquely, has a side
	// effect: presenting a spent refresh token revokes the whole family and
	// its authorization (spec section 9.6).
	ErrTokenReused = errors.New("invalid_token: refresh token reused")

	// ErrInvalidSecret is the wrong AGENT_GM_ADMIN_SECRET. It is deliberately
	// indistinguishable from ErrTokenUnknown at the HTTP surface.
	ErrInvalidSecret = errors.New("invalid_token: admin secret")

	// ErrInsufficientScope is `insufficient_scope` (403): a valid credential
	// on a route it does not cover. This is the exit-4 case of the section
	// 11.2 matrix.
	ErrInsufficientScope = errors.New("insufficient_scope")
)

// RateLimitedError is spec section 7.2 `rate_limited` (429). It always carries
// RetryAfter, and it carries nothing else: a 429 must never distinguish a real
// token from a guess (spec section 9.8), so two RateLimitedError values raised
// for the same surface and the same wait are indistinguishable by
// construction, whatever provoked them.
type RateLimitedError struct {
	// Surface names the bucket, for logs and metrics. It is the same string
	// for a real token and for a guess.
	Surface string
	// RetryAfter is rounded up to whole seconds, the granularity the
	// Retry-After header has.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate_limited: %s, retry after %s", e.Surface, e.RetryAfter)
}

// Is makes errors.Is(err, ErrRateLimited) work.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }

// ErrRateLimited is the sentinel every RateLimitedError matches.
var ErrRateLimited = errors.New("rate_limited")

func rateLimited(surface string, wait time.Duration) *RateLimitedError {
	if wait < 0 {
		wait = 0
	}
	// Round up: answering "retry after 0" for a live cooldown would invite an
	// immediate retry that is refused again.
	secs := (wait + time.Second - 1) / time.Second * time.Second
	if secs == 0 {
		secs = time.Second
	}
	return &RateLimitedError{Surface: surface, RetryAfter: secs}
}
