// Package clock is the injected time source every timeout in Agent GM is
// measured against (spec section 13.1). No test sleeps, and no test waits out
// a real 24 hours.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source.
type Clock interface {
	Now() time.Time
	// Since is Now().Sub(t).
	Since(t time.Time) time.Duration
}

// Real is the production clock.
type Real struct{}

func (Real) Now() time.Time                  { return time.Now().UTC() }
func (Real) Since(t time.Time) time.Duration { return time.Now().UTC().Sub(t) }

// Fake advances only when a test tells it to. Two backends may share one Fake
// or hold two, and each test says which -- spec section 13.1's sign-out and
// re-pair tests advance one account's clock while the other's stands still,
// which is only expressible with separate clocks.
type Fake struct {
	mu  sync.Mutex
	now time.Time
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

// Advance moves the clock forward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

var _ Clock = Real{}
var _ Clock = (*Fake)(nil)
