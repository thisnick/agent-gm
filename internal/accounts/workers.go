package accounts

import (
	"context"
	"sync"
	"time"

	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// Workers is the real backfill and the real reconciliation sweep, in the
// shape `internal/accounts` supervises them.
//
// **Why this type exists at all.** `internal/accounts` declares `Backfiller`
// and `Sweeper` so that the supervisor can start, stop, pause and resume the
// per-account work without importing `internal/core` and creating an import
// cycle. Before this type there was no implementation of either outside a
// test double, and `agent-gm serve` passed neither -- so §5.2's backfill and
// §5.4's sweep, the sweep D26 calls mandatory *because the event stream loses
// messages*, never ran in the shipped binary while both were "tested" against
// injected fakes.
//
// The lesson is in the wiring test that goes with this file, not here: a
// worker interface with exactly one implementation, and that implementation
// only in tests, is a worker nothing runs.
type Workers struct {
	// Engine builds the per-account engine. It is a function rather than a
	// map because an account's backend is only known once the supervisor has
	// connected it.
	Engine func(accountID string) (*core.Account, error)
	// Store is read directly by Progress.
	//
	// Progress used to build an engine just to reach the store, which meant
	// asking about an account that has no backend -- a parked one, a
	// signed-out one, one that has not connected yet -- did work and could
	// fail for a reason that has nothing to do with the question. The
	// backfill counts are rows in `backfill_state`; reading them needs a
	// database and nothing else.
	Store *store.Store
	// Log is optional.
	Log core.Logger

	mu    sync.Mutex
	state map[string]*workerState
}

type workerState struct {
	cancel context.CancelFunc
	// paused is set while THIS account's phone has an outstanding
	// MOBILE_DATABASE_SYNC_STARTED/SYNCING alert. Results during a
	// phone-side sync are unstable, so that account's backfill waits --
	// and one sleepy phone never stalls another account, which is why this
	// is per account and not a process-wide flag (spec section 5.2).
	paused  bool
	resume  chan struct{}
	started bool
	done    bool
	total   int
	doneN   int
	err     error
}

// NewWorkers builds the adapter.
func NewWorkers(st *store.Store, engine func(string) (*core.Account, error), log core.Logger) *Workers {
	return &Workers{Store: st, Engine: engine, Log: log, state: map[string]*workerState{}}
}

// Start begins this account's backfill, once. Calling it again while one is
// running is a no-op rather than a second walk of the same history.
func (w *Workers) Start(ctx context.Context, accountID string) error {
	w.mu.Lock()
	st, existing := w.state[accountID]
	if existing && st.started && !st.done {
		w.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if !existing {
		// A Pause that arrived before ClientReady already made this row, and
		// its paused flag is the whole reason Pause does that.
		st = &workerState{resume: make(chan struct{}, 1)}
		w.state[accountID] = st
	}
	st.cancel = cancel
	st.started = true
	st.done = false
	w.mu.Unlock()

	go w.run(runCtx, accountID, st)
	return nil
}

func (w *Workers) run(ctx context.Context, accountID string, st *workerState) {
	defer func() {
		w.mu.Lock()
		st.done = true
		w.mu.Unlock()
	}()

	// The pause is checked before the walk starts as well as inside it: an
	// account whose phone was already syncing when it connected must not
	// begin, or the first page it reads is the unstable one.
	if err := w.waitWhilePaused(ctx, st); err != nil {
		return
	}

	engine, err := w.Engine(accountID)
	if err != nil {
		w.note(accountID, "backfill could not build an engine", err)
		w.fail(st, err)
		return
	}
	// The conversations ClientReady carried are not available here, so the
	// walk starts from ListConversations, which is step 2 and is also what
	// arms the library's BUGLE_MESSAGE mode (spec sections 5.2, 3.7).
	if err := engine.Backfill(ctx, nil); err != nil {
		w.note(accountID, "backfill failed", err)
		w.fail(st, err)
		return
	}
}

func (w *Workers) fail(st *workerState, err error) {
	w.mu.Lock()
	st.err = err
	w.mu.Unlock()
}

func (w *Workers) waitWhilePaused(ctx context.Context, st *workerState) error {
	for {
		w.mu.Lock()
		paused := st.paused
		w.mu.Unlock()
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-st.resume:
		}
	}
}

// Stop ends this account's backfill. It is called when an account
// disconnects, is signed out, is parked or is removed.
func (w *Workers) Stop(accountID string) {
	w.mu.Lock()
	st, ok := w.state[accountID]
	if ok {
		delete(w.state, accountID)
	}
	w.mu.Unlock()
	if ok && st.cancel != nil {
		st.cancel()
	}
}

// Pause holds THIS account's backfill while its phone reports a database
// sync. Results during a phone-side sync are unstable (spec section 5.2).
//
// It records the pause even when no backfill is running yet, and that is not
// defensive coding: a MOBILE_DATABASE_SYNC_STARTED alert can arrive BEFORE
// ClientReady, so an account can legitimately be told to pause before it has
// anything to pause. Ignoring that -- which is what "only pause a running
// worker" does -- means the first page the walk reads is the unstable one,
// which is the exact failure section 5.2 pauses to avoid.
func (w *Workers) Pause(accountID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.state[accountID]
	if !ok {
		st = &workerState{resume: make(chan struct{}, 1)}
		w.state[accountID] = st
	}
	st.paused = true
}

// Resume releases it on that phone's MOBILE_DATABASE_SYNC_COMPLETE.
func (w *Workers) Resume(accountID string) {
	w.mu.Lock()
	st, ok := w.state[accountID]
	if ok {
		st.paused = false
	}
	w.mu.Unlock()
	if ok {
		select {
		case st.resume <- struct{}{}:
		default:
		}
	}
}

// Sweep runs the reconciliation sweep for one account.
//
// It is synchronous: the supervisor calls it on connect and on that account's
// timer, and a sweep that ran in the background would make "the sweep ran"
// unobservable to the thing that asked for it.
func (w *Workers) Sweep(ctx context.Context, accountID string, since time.Time) error {
	engine, err := w.Engine(accountID)
	if err != nil {
		return err
	}
	return engine.Reconcile(ctx, since)
}

func (w *Workers) note(accountID, msg string, err error) {
	if w.Log == nil {
		return
	}
	// The acct_ ID, never the Google address (spec section 12.2).
	w.Log.Warn(msg, "account_id", accountID, "error", err.Error())
}

// Progress reports what GET /v1/health's per-account `backfill` block says.
//
// `conversations_done` and `conversations_total` come from `backfill_state`
// rather than from this process's memory, so they survive a restart and a
// resumed backfill reports where it really is rather than starting from zero.
func (w *Workers) Progress(accountID string) BackfillHealth {
	w.mu.Lock()
	st, running := w.state[accountID]
	state := "pending"
	if running && st.started {
		switch {
		case st.done:
			state = "complete"
		case st.paused:
			state = "paused"
		default:
			state = "running"
		}
	}
	w.mu.Unlock()

	out := BackfillHealth{State: state}
	if w.Store == nil {
		return out
	}
	done, total, err := w.Store.BackfillProgress(context.Background(), accountID)
	if err != nil {
		return out
	}
	out.ConversationsDone = int(done)
	out.ConversationsTotal = int(total)
	return out
}

// The whole point of this file: the supervisor's two worker interfaces have a
// real implementation, and the compiler says so. Before this, both had only
// test doubles and `agent-gm serve` passed neither.
var (
	_ Sweeper    = (*Workers)(nil)
	_ Backfiller = (*Workers)(nil)
)

// Ready is the conversation list ClientReady carried, for a caller that has
// one. It exists so a supervisor that does hold them can pass them rather
// than making the walk re-fetch (spec section 5.2 step 1).
func (w *Workers) StartWith(ctx context.Context, accountID string, ready []gm.Conversation) error {
	engine, err := w.Engine(accountID)
	if err != nil {
		return err
	}
	engine.KickBackfill(ctx, ready)
	return nil
}
