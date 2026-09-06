package authz

// The limiters of spec sections 9.8 and 12.3.
//
// Two families, and the difference between them is the whole point:
//
//   - Token buckets for ordinary traffic. In memory, per authorization or per
//     source. A restart forgives an ordinary caller's request debt, which
//     costs nothing.
//   - Durable cooldown metadata for the credential-failure limiters, held in
//     `oauth_attempts`, because that is the limit an attacker would
//     restart-cycle to reset.
//
// Every one of them is measured on the injected clock. Nothing here calls
// time.Now and no test waits a cooldown out.

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// ---------------------------------------------------------------------------
// Token buckets (spec section 12.3's first three rows, plus section 9.8's
// in-memory burst).
// ---------------------------------------------------------------------------

// BucketSpec is one row of spec section 12.3's table.
type BucketSpec struct {
	// Name is the surface, and the prefix of every key in this bucket. Two
	// surfaces never share a bucket, so exhausting reads does not refuse a
	// mutation.
	Name string
	// PerMinute is the sustained refill rate.
	PerMinute float64
	// Burst is the bucket's capacity.
	Burst float64
}

// The buckets of spec section 12.3.
//
// Note what is NOT a parameter anywhere below: an account. Holding several
// scopes does not multiply the allowance, and neither does holding several
// accounts -- the buckets are per authorization and per source, never per
// account (spec section 12.3, D29). One authorization sending to three
// accounts shares one 120/min mutation budget. That is deliberate: the limits
// protect the server and the phones from a runaway agent, and an agent is no
// less runaway for spreading itself across accounts.
var (
	// BucketReads covers every read route and read tool.
	BucketReads = BucketSpec{Name: "reads", PerMinute: 300, Burst: 100}
	// BucketMutations covers send, start, mark read, reactions, uploads and
	// the two delete routes -- messages:write and messages:delete together,
	// one budget, not one each.
	BucketMutations = BucketSpec{Name: "mutations", PerMinute: 120, Burst: 30}
	// BucketAdmin covers /v1/admin/*.
	BucketAdmin = BucketSpec{Name: "admin", PerMinute: 120, Burst: 30}
	// BucketUnauthenticated covers POST /v1/auth/refresh, and in Slice 3 the
	// /oauth/token refresh grant and /oauth/revoke. It is keyed by SOURCE,
	// because the caller is by definition not yet authenticated (spec
	// section 9.8).
	BucketUnauthenticated = BucketSpec{Name: "unauthenticated", PerMinute: 60, Burst: 20}
)

type bucketState struct {
	tokens float64
	last   time.Time
}

// TokenBuckets is the in-memory limiter for ordinary traffic.
type TokenBuckets struct {
	clk clock.Clock
	mu  sync.Mutex
	m   map[string]*bucketState
}

// NewTokenBuckets builds an empty limiter on the injected clock.
func NewTokenBuckets(clk clock.Clock) *TokenBuckets {
	return &TokenBuckets{clk: clk, m: make(map[string]*bucketState)}
}

// Allow consumes one token from the bucket named by spec and key, returning
// nil if the request may proceed and a *RateLimitedError carrying Retry-After
// if it may not.
//
// key is an authorization ID for the first three buckets and a client source
// for the fourth. It is never an account ID and never a scope.
func (b *TokenBuckets) Allow(spec BucketSpec, key string) error {
	now := b.clk.Now()
	full := spec.Name + "|" + key

	b.mu.Lock()
	defer b.mu.Unlock()

	st, ok := b.m[full]
	if !ok {
		st = &bucketState{tokens: spec.Burst, last: now}
		b.m[full] = st
	} else {
		elapsed := now.Sub(st.last)
		if elapsed > 0 {
			st.tokens = math.Min(spec.Burst, st.tokens+elapsed.Seconds()*spec.PerMinute/60)
		}
		st.last = now
	}

	if st.tokens >= 1 {
		st.tokens--
		return nil
	}
	// How long until one whole token exists again.
	deficit := 1 - st.tokens
	wait := time.Duration(deficit / (spec.PerMinute / 60) * float64(time.Second))
	return rateLimited(spec.Name, wait)
}

// ---------------------------------------------------------------------------
// Concurrency limiters (spec section 12.3's last three rows).
// ---------------------------------------------------------------------------

// ConcurrencySpec is one concurrency row of spec section 12.3.
type ConcurrencySpec struct {
	Name             string
	PerAuthorization int
	Global           int
}

// The concurrency limits of spec section 12.3. Per authorization and global,
// never per account.
var (
	ConcurrencyUploads   = ConcurrencySpec{Name: "uploads", PerAuthorization: 4, Global: 16}
	ConcurrencyDownloads = ConcurrencySpec{Name: "downloads", PerAuthorization: 8, Global: 32}
)

// ConcurrencyLimiter bounds simultaneous work.
type ConcurrencyLimiter struct {
	spec   ConcurrencySpec
	mu     sync.Mutex
	perKey map[string]int
	total  int
}

// NewConcurrencyLimiter builds a limiter for one spec.
func NewConcurrencyLimiter(spec ConcurrencySpec) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{spec: spec, perKey: make(map[string]int)}
}

// Acquire takes one slot for the given authorization. On success it returns a
// release function that must be called exactly once; on refusal it returns a
// *RateLimitedError and a no-op release, so a caller that defers the release
// unconditionally is still correct.
//
// Retry-After on a concurrency refusal is one second: the wait is bounded by
// somebody else finishing, which is not a duration this limiter can know, and
// a second is the smallest honest answer.
func (c *ConcurrencyLimiter) Acquire(key string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total >= c.spec.Global || c.perKey[key] >= c.spec.PerAuthorization {
		return func() {}, rateLimited(c.spec.Name, time.Second)
	}
	c.perKey[key]++
	c.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.perKey[key] > 0 {
				c.perKey[key]--
				if c.perKey[key] == 0 {
					delete(c.perKey, key)
				}
			}
			if c.total > 0 {
				c.total--
			}
		})
	}, nil
}

// InFlight reports the current count for one key, for /v1/health.
func (c *ConcurrencyLimiter) InFlight(key string) (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perKey[key], c.total
}

// ---------------------------------------------------------------------------
// Durable failure limiters (spec sections 9.8, 12.3).
// ---------------------------------------------------------------------------

// DurableLimitSpec is one credential-failure limiter.
type DurableLimitSpec struct {
	// Kind is the `kind` half of oauth_attempts' primary key.
	Kind string
	// Surface names the bucket in a RateLimitedError.
	Surface string
	// PerSource is how many failures inside Window a single source may
	// accumulate before a cooldown starts. The limiter trips ON reaching it,
	// not after exceeding it: spec section 16 Slice 2 test 25 says thirty
	// invalid presented tokens trips the cooldown, so the thirtieth is the one
	// that does.
	PerSource int
	// Global is the same, summed across every source. Zero disables the
	// global half.
	Global int
	// Window is the rolling window failures are counted in.
	Window time.Duration
	// BaseCooldown is the first cooldown; it doubles per further failure in
	// the window and caps at MaxCooldown.
	BaseCooldown time.Duration
	MaxCooldown  time.Duration
}

// The two durable limiters Slice 2 needs.
var (
	// LimitAdminSecret is spec section 12.3's admin-secret failure row: 5 per
	// 15 minutes per source, 20 per 15 minutes globally, exponential
	// cooldown. POST /v1/auth/admin-session is budgeted more tightly than
	// anything else because a success there yields more than a success
	// anywhere else.
	LimitAdminSecret = DurableLimitSpec{
		Kind:         store.AttemptKindAdminSecret,
		Surface:      "admin_secret",
		PerSource:    5,
		Global:       20,
		Window:       15 * time.Minute,
		BaseCooldown: time.Minute,
		MaxCooldown:  24 * time.Hour,
	}
	// LimitRefreshToken is spec section 9.8's durable half for
	// POST /v1/auth/refresh: 30 unknown or invalid presented tokens per 15
	// minutes, cooldown doubling per further failure in the window, capping at
	// 24 hours. It has no global half; section 9.8 states only a per-source
	// figure, and inventing a global one would refuse honest callers on the
	// strength of a number the spec does not give.
	LimitRefreshToken = DurableLimitSpec{
		Kind:         store.AttemptKindRefresh,
		Surface:      "refresh_token",
		PerSource:    30,
		Global:       0,
		Window:       15 * time.Minute,
		BaseCooldown: time.Minute,
		MaxCooldown:  24 * time.Hour,
	}
)

// DurableLimiter is the `oauth_attempts`-backed limiter.
type DurableLimiter struct {
	st  *store.Store
	clk clock.Clock
}

// NewDurableLimiter builds one.
func NewDurableLimiter(st *store.Store, clk clock.Clock) *DurableLimiter {
	return &DurableLimiter{st: st, clk: clk}
}

// Check reports whether a source is currently cooled down, READ-ONLY.
//
// It runs on the read pool and opens no write transaction, which is spec
// section 9.8's rule: a caller presenting a value that belongs to nobody must
// not be able to take the single writer's lock and queue every other writer
// behind itself.
func (l *DurableLimiter) Check(ctx context.Context, spec DurableLimitSpec, source string) error {
	now := l.clk.Now()
	keys := []string{source}
	if spec.Global > 0 {
		keys = append(keys, store.AttemptGlobalSource)
	}
	for _, k := range keys {
		a, ok, err := l.st.AttemptRow(ctx, spec.Kind, k)
		if err != nil {
			return err
		}
		if !ok || a.CooldownUntilMS == 0 {
			continue
		}
		until := time.UnixMilli(a.CooldownUntilMS).UTC()
		if now.Before(until) {
			return rateLimited(spec.Surface, until.Sub(now))
		}
	}
	return nil
}

// RecordFailure counts one credential failure against the source and, where
// the spec gives one, against the global budget. It returns the source's
// resulting row so the caller can audit the cooldown state (spec section 12.4
// requires `auth.admin_secret_failed` to carry it).
//
// A successful presentation deliberately does NOT clear the counter (spec
// section 9.8): there is no RecordSuccess.
func (l *DurableLimiter) RecordFailure(ctx context.Context, spec DurableLimitSpec, source string) (store.Attempt, error) {
	now := l.clk.Now()
	out, err := l.st.UpdateAttempt(ctx, spec.Kind, source, func(a store.Attempt) store.Attempt {
		return advanceAttempt(a, now, spec, spec.PerSource)
	})
	if err != nil {
		return out, err
	}
	if spec.Global > 0 {
		if _, err := l.st.UpdateAttempt(ctx, spec.Kind, store.AttemptGlobalSource, func(a store.Attempt) store.Attempt {
			return advanceAttempt(a, now, spec, spec.Global)
		}); err != nil {
			return out, err
		}
	}
	return out, nil
}

// advanceAttempt is the whole cooldown rule, in one place and on the injected
// clock.
//
// The window is rolling in the cheap sense: a failure arriving more than
// Window after the window opened starts a new one. The cooldown does NOT reset
// with the window while it is still running -- otherwise an attacker would
// wait out the window rather than the cooldown, and the escalation would never
// bite. Once the cooldown has elapsed, a new window starts the escalation over.
func advanceAttempt(a store.Attempt, now time.Time, spec DurableLimitSpec, limit int) store.Attempt {
	nowMS := now.UnixMilli()
	coolingDown := a.CooldownUntilMS != 0 && nowMS < a.CooldownUntilMS

	if a.WindowStartMS == 0 || now.Sub(time.UnixMilli(a.WindowStartMS).UTC()) >= spec.Window {
		a.WindowStartMS = nowMS
		a.Failures = 0
		if !coolingDown {
			a.CooldownSteps = 0
		}
	}
	a.Failures++

	if limit > 0 && a.Failures >= limit {
		a.CooldownSteps++
		a.CooldownUntilMS = nowMS + int64(cooldownFor(spec, a.CooldownSteps)/time.Millisecond)
	}
	return a
}

// cooldownFor is BaseCooldown doubled once per step, capped at MaxCooldown.
// The shift is bounded before it is taken, so a long-lived attacker cannot
// overflow the exponent into a short cooldown.
func cooldownFor(spec DurableLimitSpec, steps int) time.Duration {
	if steps < 1 {
		steps = 1
	}
	d := spec.BaseCooldown
	for i := 1; i < steps; i++ {
		if d >= spec.MaxCooldown {
			break
		}
		d *= 2
	}
	if d > spec.MaxCooldown || d <= 0 {
		d = spec.MaxCooldown
	}
	return d
}
