package accounts

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thisnick/agent-gm/internal/store"
)

// State is the account-state vocabulary of spec section 4.7. It is an alias
// of store.AccountState so a state read out of a row and a state written into
// one are the same type -- there is exactly one vocabulary and no conversion
// layer to disagree with itself.
//
// There is no bad_credentials state and no unpaired state; the seven below
// are the whole list.
type State = store.AccountState

const (
	// StatePairing: a pair is in flight and there is no session yet. It is
	// NEVER a resting state -- see Resting.
	StatePairing = store.StatePairing
	// StateConnected: session valid, long poll up. Reads and writes.
	StateConnected = store.StateConnected
	// StateDegraded: a transient listen error is being retried. Reads, and
	// writes that are likely to fail.
	StateDegraded = store.StateDegraded
	// StateError: the supervisor is retrying Reconnect with backoff. Reads;
	// writes refused.
	StateError = store.StateError
	// StateSignedOut: the owner signed this account out, or its cookies
	// died. Reads still work; writes refused with not_signed_in.
	StateSignedOut = store.StateSignedOut
	// StateParked: accounts.max_concurrent is reached, so this account holds
	// no client and no goroutine. Not an error and not transient.
	StateParked = store.StateParked
	// StateAccountChanged: the phone switched Google accounts underneath us.
	StateAccountChanged = store.StateAccountChanged
)

// States returns spec section 4.7's whole vocabulary, in the spec's order.
func States() []State {
	return []State{StatePairing, StateConnected, StateDegraded, StateError,
		StateSignedOut, StateParked, StateAccountChanged}
}

// Resting reports whether a state is one an account may sit in indefinitely.
// `pairing` is the only one that is not: a pairing either completes or its row
// is deleted (spec sections 3.2, 4.7).
func Resting(s State) bool { return s != StatePairing }

// Writable reports whether a write naming an account in this state is
// accepted. Everything else is refused with unsupported_capability /
// not_signed_in, which is a PER-ACCOUNT condition and deliberately not the
// service-level not_paired (spec sections 4.7, 7.8).
//
// A `parked` account is fully readable and is not writable: it holds no
// client to write through.
func Writable(s State) bool { return s == StateConnected || s == StateDegraded }

// Reason is the short machine-readable string every account DTO carries
// beside its state, so `degraded` and `error` are diagnosable without reading
// logs (spec section 4.7).
//
// It is its own type rather than a bare string so that an invented reason is
// a compile error: the vocabulary is closed by the compiler, not by a review.
type Reason string

const (
	// ReasonNone is the empty reason, which serialises as null.
	ReasonNone Reason = ""
	// ReasonCapacity: accounts.max_concurrent is reached. Pairs with
	// `parked`, never with `degraded` -- "waiting for a slot" and "retrying a
	// listen error" are different things.
	ReasonCapacity Reason = "capacity"
	// ReasonListenError: the long poll failed. Pairs with `degraded` while it
	// is being retried, and with `error` once it is fatal.
	ReasonListenError Reason = "listen_error"
	// ReasonCredentials: the credentials are dead, or the owner signed this
	// account out.
	ReasonCredentials Reason = "credentials"
	// ReasonRevokedByPhone: the phone revoked this pairing.
	ReasonRevokedByPhone Reason = "revoked_by_phone"
	// ReasonCookiesExpired: GaiaLoggedOut. The fix is a cookie refresh, not a
	// re-pair.
	ReasonCookiesExpired Reason = "cookies_expired"
	// ReasonAccountSwitched: the phone's active Google account changed.
	ReasonAccountSwitched Reason = "account_switched"
	// ReasonCrashRecovered: the state was reconstructed after a crash.
	ReasonCrashRecovered Reason = "crash_recovered"
)

// Reasons returns spec section 4.7's vocabulary, in the spec's order. The
// empty reason is not in it: it is the absence of one.
func Reasons() []Reason {
	return []Reason{ReasonCapacity, ReasonListenError, ReasonCredentials,
		ReasonRevokedByPhone, ReasonCookiesExpired, ReasonAccountSwitched,
		ReasonCrashRecovered}
}

// Valid reports whether r is the empty reason or one of section 4.7's.
func (r Reason) Valid() bool {
	if r == ReasonNone {
		return true
	}
	for _, k := range Reasons() {
		if k == r {
			return true
		}
	}
	return false
}

func (r Reason) String() string { return string(r) }

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// Auditor is the slice of internal/audit this package needs. It is an
// interface here rather than an import so the supervisor can be tested, and
// reasoned about, without the audit store -- and so that a nil Auditor is a
// supervisor that simply does not audit rather than one that panics.
type Auditor interface {
	Append(ctx context.Context, kind string, fields map[string]any) error
}

// The audit kinds this package writes (spec section 12.4). Every one of them
// carries account_id, which is how "what happened to this account" stays
// answerable after the account is removed.
const (
	// AuditStateChanged carries from, to and state_reason. EVERY transition
	// writes one.
	AuditStateChanged = "account.state_changed"
	// AuditPaired is a brand-new account.
	AuditPaired = "account.paired"
	// AuditResumed is a re-pair onto an existing acct_ row.
	AuditResumed = "account.resumed"
	// AuditSignedOut is the owner signing an account out (spec section 4.7).
	AuditSignedOut = "account.signed_out"
	// AuditPairFailed is a pairing that never completed.
	AuditPairFailed = "account.pair_failed"
)

// ---------------------------------------------------------------------------
// The state-change feed
// ---------------------------------------------------------------------------

// SubscriberBuffer is how many state changes a subscriber may fall behind by
// before it starts dropping them. A slow subscriber must never block the
// supervisor, so publication is a non-blocking send and the overflow is
// counted rather than waited on.
const SubscriberBuffer = 64

// StateChange is one account state transition, and the whole payload of
// GET /v1/accounts/{id}/events. It carries no message data, which is why that
// route needs no replay ring and no cursor (spec section 7.5).
type StateChange struct {
	AccountID string `json:"account_id"`
	From      State  `json:"from"`
	To        State  `json:"to"`
	Reason    Reason `json:"state_reason"`
	// At is truncated to milliseconds. Section 4.4 says every JSON surface
	// renders timestamps as RFC 3339 UTC with millisecond precision, and a
	// stream is a JSON surface: a nanosecond timestamp here made the SSE
	// feed the one place a client saw a different shape.
	At time.Time `json:"at"`
}

// Subscription is one reader of the feed. Close it exactly once; a closed
// subscription's channel is closed too, so a range over it terminates.
type Subscription struct {
	accountID string
	ch        chan StateChange
	feed      *feed
	id        uint64
	dropped   atomic.Uint64
	closeOnce sync.Once
}

// Events is the channel state changes arrive on.
func (s *Subscription) Events() <-chan StateChange { return s.ch }

// Dropped is how many changes this subscriber was too slow to take. It is a
// fact about the subscriber, never a reason to stall the supervisor.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// AccountID is the account this subscription follows, or "" for all of them.
func (s *Subscription) AccountID() string { return s.accountID }

// Close unsubscribes. It is safe to call more than once.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.feed.mu.Lock()
		delete(s.feed.subs, s.id)
		s.feed.mu.Unlock()
		close(s.ch)
	})
}

type feed struct {
	mu   sync.Mutex
	next uint64
	subs map[uint64]*Subscription
}

func (f *feed) subscribe(accountID string) *Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[uint64]*Subscription{}
	}
	f.next++
	sub := &Subscription{
		accountID: accountID,
		ch:        make(chan StateChange, SubscriberBuffer),
		feed:      f,
		id:        f.next,
	}
	f.subs[sub.id] = sub
	return sub
}

// publish fans one change out. The send is non-blocking: a subscriber that is
// not reading loses events and counts them, and the supervisor moves on.
func (f *feed) publish(ev StateChange) {
	f.mu.Lock()
	subs := make([]*Subscription, 0, len(f.subs))
	for _, s := range f.subs {
		if s.accountID == "" || s.accountID == ev.AccountID {
			subs = append(subs, s)
		}
	}
	f.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribe delivers one event per state change, for one account or -- with
// an empty accountID -- for every account, each tagged. It backs
// GET /v1/accounts/{account_id}/events and `agm session --watch`
// (spec section 7.5).
func (s *Supervisor) Subscribe(accountID string) *Subscription {
	return s.feed.subscribe(accountID)
}

// Schedulable reports whether the supervisor will give this account a
// backfill worker and a sweep timer.
//
// `pairing` is included because a pairing in flight is about to become
// `connected`, and reporting `not_started` for it would be true for a second
// and misleading thereafter. Everything that reads but does not write --
// parked, signed out, error, account_changed -- is not schedulable: it holds
// no client, so there is nothing for a worker to walk (spec section 4.7).
func Schedulable(s State) bool {
	switch s {
	case StateConnected, StateDegraded, StatePairing:
		return true
	default:
		return false
	}
}
