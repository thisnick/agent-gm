package core

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thisnick/agent-gm/internal/audit"
	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Config is the slice of spec section 15.1's settings table that ingestion
// and operations read. It is a plain struct rather than a live handle on
// internal/settings because core must be drivable from a test with one
// literal, and because a running backfill must not change page size
// underneath itself halfway through a conversation.
//
// Every field here is ScopePerAccount in section 15.1: the value is applied
// to each account independently, so the cost multiplies by account count.
type Config struct {
	// BackfillConcurrency is backfill.concurrency: conversations at a time,
	// per account (default 2, bounds 1-8).
	BackfillConcurrency int
	// ConversationPageSize is backfill.conversation_page_size.
	ConversationPageSize int
	// MessagePageSize is backfill.message_page_size.
	MessagePageSize int
	// MaxMessagesPerConversation is backfill.max_messages_per_conversation.
	MaxMessagesPerConversation int
	// Horizon is backfill.horizon: backfill stops at a message older than
	// this.
	Horizon time.Duration
	// IncludeArchive is backfill.include_archive.
	IncludeArchive bool
	// SweepInterval is ingest.sweep_interval, the per-account timer trigger
	// of the reconciliation sweep.
	SweepInterval time.Duration
	// PendingTimeout is operations.pending_timeout.
	PendingTimeout time.Duration
}

// DefaultConfig is the section 15.1 defaults.
func DefaultConfig() Config {
	return Config{
		BackfillConcurrency:        2,
		ConversationPageSize:       100,
		MessagePageSize:            100,
		MaxMessagesPerConversation: 2000,
		Horizon:                    365 * 24 * time.Hour,
		IncludeArchive:             true,
		SweepInterval:              15 * time.Minute,
		PendingTimeout:             24 * time.Hour,
	}
}

// Account is one account's ingestion and mutation engine: one store, one
// gm.Backend, one clock, one ingester (spec sections 2.4, 3.4, 5.2, 5.3).
//
// Everything on it is scoped to this account. No method here ever reads or
// writes another account's rows, and no event on this account's stream
// changes another account's state -- which is why the two-account tests of
// section 13.2 can assert no cross-talk by construction rather than by
// inspection.
type Account struct {
	ID      string
	Store   *store.Store
	Backend gm.Backend
	Clock   clock.Clock
	Config  Config
	Log     Logger
	// Audit is the section 12.4 writer. It may be nil in a narrow unit test;
	// every call site tolerates that.
	Audit *audit.Writer
	// Source is the resolved client source of section 12.3, stamped on every
	// audit row this engine writes. Ingestion's own source is "system".
	Source string
	// DataKey seals attachment decryption keys at rest. Nil means the
	// attachment row is written without one, which is what a test that does
	// not exercise media wants.
	DataKey *store.DataKey

	ingest atomic.Pointer[Ingester]

	mu sync.Mutex
	// syncPaused is set while a MOBILE_DATABASE_SYNC_STARTED/SYNCING alert
	// from THIS phone is outstanding. One sleepy phone never stalls another
	// account, because this flag lives on this Account (spec section 5.2).
	syncPaused bool
	resume     chan struct{}
	// lastSessionID is the CurrentSessionID last seen, so a BROWSER_ACTIVE
	// alert can be compared against it (spec section 3.4).
	lastSessionID string

	sweeps     atomic.Uint64
	backfillWG sync.WaitGroup

	// crashBefore stands in for the process dying between the operation
	// commit at step 8 of section 6.2 and the library call at step 9. It is
	// nil in production; SetCrashBefore is how the crash-recovery test
	// reproduces that window without killing a real process, and it is
	// placed exactly where the window is rather than anywhere convenient.
	crashBefore func(store.Operation) error
}

// SetCrashBefore installs the step-8/step-9 fault seam. Passing nil removes
// it.
func (a *Account) SetCrashBefore(fn func(store.Operation) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.crashBefore = fn
}

// Ingest returns this account's ingester, creating it on first use.
func (a *Account) Ingest() *Ingester {
	if in := a.ingest.Load(); in != nil {
		return in
	}
	in := &Ingester{Store: a.Store, AccountID: a.ID, Log: a.Log, Account: a}
	if a.ingest.CompareAndSwap(nil, in) {
		return in
	}
	return a.ingest.Load()
}

func (a *Account) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock.Now()
}

// Sweeps is sweeps_total for this account's GET /v1/health block.
func (a *Account) Sweeps() uint64 { return a.sweeps.Load() }

// LastSessionID is the session ID this account last saw.
func (a *Account) LastSessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSessionID
}

// --- the backfill pause (spec sections 3.4, 5.2) ----------------------------

// PauseBackfill defers THIS account's backfill because its phone is resyncing
// its own database. Results during a phone-side sync are unstable.
func (a *Account) PauseBackfill() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.syncPaused {
		return
	}
	a.syncPaused = true
	a.resume = make(chan struct{})
}

// ResumeBackfill lifts the pause on MOBILE_DATABASE_SYNC_COMPLETE.
func (a *Account) ResumeBackfill() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.syncPaused {
		return
	}
	a.syncPaused = false
	if a.resume != nil {
		close(a.resume)
		a.resume = nil
	}
}

// BackfillPaused reports whether this account's backfill is deferred.
func (a *Account) BackfillPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.syncPaused
}

// waitForSync blocks while this phone is resyncing. It is checked between
// conversations and between pages, so a pause takes effect quickly without
// abandoning progress already recorded in backfill_state.
func (a *Account) waitForSync(ctx context.Context) error {
	for {
		a.mu.Lock()
		paused, ch := a.syncPaused, a.resume
		a.mu.Unlock()
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// --- account row helpers ----------------------------------------------------

// Row reads this account's row.
func (a *Account) Row(ctx context.Context) (store.Account, error) {
	return a.Store.Account(ctx, a.ID)
}

// LastEventAt is `accounts.last_event_at_ms` for this account, which is the
// `since` every event-driven trigger in spec section 5.4's table hands the
// sweep. It is read from the row rather than guessed from the clock: the
// whole point of the value is that it is the moment data was last actually
// received, not the moment somebody noticed.
func (a *Account) LastEventAt(ctx context.Context) (time.Time, error) {
	row, err := a.Store.Account(ctx, a.ID)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(row.LastEventAtMS).UTC(), nil
}

// TouchEvent moves last_event_at_ms forward, monotonically. It is stamped
// with the datum's own timestamp rather than the wall clock, so a sweep
// triggered straight after a lossy batch still has a `since` that precedes
// the messages the batch lost.
func (a *Account) TouchEvent(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		at = a.now()
	}
	return a.Store.TouchAccountEvent(ctx, a.ID, at)
}

// setLastSweep records accounts.last_sweep_at_ms for THIS account
// (spec section 5.4 step 4). The store keeps it monotonic, so two overlapping
// sweeps cannot rewind the record.
func (a *Account) setLastSweep(ctx context.Context, at time.Time) error {
	return a.Store.SetAccountLastSweep(ctx, a.ID, at)
}

// setBackfillComplete records accounts.backfill_complete_at_ms for THIS
// account. It is never a server_meta key: two accounts would race on one row
// and the first to finish would mark the whole server complete
// (spec section 5.2 step 7).
func (a *Account) setBackfillComplete(ctx context.Context, at time.Time) error {
	return a.Store.SetAccountBackfillComplete(ctx, a.ID, at)
}
