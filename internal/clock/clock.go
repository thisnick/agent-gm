// Package clock is the injected time source every timeout in Agent GM is
// measured against (spec section 13.1). No test sleeps, and no test waits out
// a real 24 hours.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock is the time source.
type Clock interface {
	Now() time.Time
	// Since is Now().Sub(t).
	Since(t time.Time) time.Duration
	// Sleep blocks for d as this clock measures it, or until ctx ends, in
	// which case it returns ctx.Err(). Every wait in Agent GM goes through
	// it -- the [3s, 8s, 20s] send backoff, ticket expiry, the pending
	// timeout -- so no test sleeps and no test waits out a real 24 hours
	// (spec section 13.1).
	Sleep(ctx context.Context, d time.Duration) error
}

// Real is the production clock.
type Real struct{}

func (Real) Now() time.Time                  { return time.Now().UTC() }
func (Real) Since(t time.Time) time.Duration { return time.Now().UTC().Sub(t) }

func (Real) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Fake advances only when a test tells it to. Two backends may share one Fake
// or hold two, and each test says which -- spec section 13.1's sign-out and
// re-pair tests advance one account's clock while the other's stands still,
// which is only expressible with separate clocks.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	frozen bool
}

// NewFake starts a fake clock at a fixed, deterministic instant.
func NewFake() *Fake {
	return &Fake{now: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
}

// NewFakeAt starts a fake clock at a chosen instant.
func NewFakeAt(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration {
	return f.Now().Sub(t)
}

// Sleep advances this clock by d and records the wait, rather than blocking.
//
// A fake clock that blocked until another goroutine advanced it would make
// every single-goroutine test a deadlock waiting to happen, and would make
// the thing under test -- "it backed off for 3s and then 8s" -- unobservable.
// Advancing and recording makes the schedule itself the assertion: see
// Sleeps.
//
// A frozen clock (FreezeSleep) does not advance, for the tests that need to
// hold time still across a wait.
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.slept = append(f.slept, d)
	if !f.frozen && d > 0 {
		f.now = f.now.Add(d)
	}
	f.mu.Unlock()
	return nil
}

// Sleeps reports every duration Sleep was asked for, in order. It is how a
// test asserts a backoff schedule without waiting for it.
func (f *Fake) Sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

// FreezeSleep makes Sleep record without advancing.
func (f *Fake) FreezeSleep(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = v
}

// Advance moves the clock forward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

var _ Clock = Real{}
var _ Clock = (*Fake)(nil)
