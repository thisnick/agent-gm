// Package accounts is the account registry and supervisor: which accounts
// exist, their state, one gm.Backend and one ingest goroutine each, and
// start/stop on pair, sign-out and remove (spec section 2.2).
//
// Accounts are independent. One account's phone being asleep, its cookies
// expiring or its backfill running does not block another's.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// DefaultMaxConcurrent is settings.accounts.max_concurrent (spec section
// 15.1, OQ-6). Accounts beyond it are `parked`.
const DefaultMaxConcurrent = 8

// SessionPersistInterval is the 5-minute timer of spec section 3.3. Cookies
// mutate in place with no event, so the timer is the only thing that captures
// a rotation.
const SessionPersistInterval = 5 * time.Minute

// ErrUnknownAccount is returned for an acct_ ID the supervisor does not hold.
var ErrUnknownAccount = errors.New("no such account")

// Account is one supervised Google account: one backend, one ingest
// goroutine, one session file.
type Account struct {
	ID      string
	Address string
	Backend gm.Backend

	sup     *Supervisor
	ingest  *core.Ingester
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
	sessMu  sync.Mutex
	dmCache map[string]bool

	// healthMu guards the cached GET /v1/health values. They are read by an
	// HTTP handler and written by this account's own goroutines.
	healthMu        sync.Mutex
	google          *GoogleHealth
	phoneResponding bool
	sweeps          int
	backfillPaused  bool
}

// Running reports whether this account holds a client and a goroutine. A
// `parked` account holds neither, which is the part of spec section 4.7 worth
// asserting rather than the state string alone.
func (a *Account) Running() bool {
	a.sup.mu.Lock()
	defer a.sup.mu.Unlock()
	return a.running
}

// HasClient reports whether this account's backend is connected.
func (a *Account) HasClient() bool { return a.Backend.IsConnected() }

// Supervisor holds every account.
type Supervisor struct {
	store    *store.Store
	sessions *store.SessionStore
	clock    clock.Clock
	log      core.Logger

	// MaxConcurrent bounds how many accounts run at once. Accounts are
	// connected in last_event_at_ms order, newest first; the rest are parked
	// -- a state of its own, not degraded, because "waiting for a slot" and
	// "retrying a listen error" are different things.
	MaxConcurrent int

	// Settings is the one runtime setting this package reads. A nil Settings
	// means MaxConcurrent is whatever was set on this struct.
	Settings MaxConcurrentSource

	// Audit receives account.state_changed on EVERY transition, plus
	// account.paired, account.resumed, account.signed_out and
	// account.pair_failed. A nil Auditor simply does not audit.
	Audit Auditor

	// Sweep runs the reconciliation sweep for one connected account, on
	// connect -- including after a re-pair, with since = last_event_at_ms --
	// and on that account's own timer.
	Sweep Sweeper

	// Backfill is one backfill worker per connected account.
	Backfill Backfiller

	// PairingTimeout is AGENT_GM_PAIRING_TIMEOUT, measured against the
	// injected clock.
	PairingTimeout time.Duration

	// SweepInterval is settings.ingest.sweep_interval, per account.
	SweepInterval time.Duration

	// ManualIngest stops Start from launching the per-account event
	// goroutine and the per-account sweep timer, so a test can drive Apply
	// itself and never race the loop. Production leaves it false.
	ManualIngest bool

	feed       feed
	goroutines atomic.Int64

	mu          sync.Mutex
	accounts    map[string]*Account
	pairings    map[string]bool
	rebalancing bool
}

// Goroutines is how many per-account goroutines the supervisor is running. A
// `parked` account contributes none, and a test asserts that rather than
// trusting the state string.
func (s *Supervisor) Goroutines() int64 { return s.goroutines.Load() }

// New builds a supervisor.
func New(st *store.Store, sessions *store.SessionStore, clk clock.Clock, log core.Logger) *Supervisor {
	return &Supervisor{
		store:          st,
		sessions:       sessions,
		clock:          clk,
		log:            log,
		MaxConcurrent:  DefaultMaxConcurrent,
		PairingTimeout: PairingTimeoutFromEnv(),
		SweepInterval:  DefaultSweepInterval,
		accounts:       map[string]*Account{},
		pairings:       map[string]bool{},
	}
}

// Get returns one supervised account.
func (s *Supervisor) Get(id string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, fmt.Errorf("%s: %w", id, ErrUnknownAccount)
	}
	return a, nil
}

// List returns every supervised account, ordered by acct_ ID.
func (s *Supervisor) List() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Pair adds an account, or resumes one. It is always explicit; Agent GM never
// pairs implicitly (spec section 3.2).
//
// backend is a fresh gm.Backend for this pairing. cookies are the seven
// captured cookies, which go from the capture into this call and nowhere
// else. emoji is handed the one emoji character to show the owner.
func (s *Supervisor) Pair(ctx context.Context, backend gm.Backend, cookies map[string]string, deviceIndex int, emoji func(string)) (*Account, error) {
	return s.PairAs(ctx, "", backend, cookies, deviceIndex, emoji)
}

// PairAs is Pair with the account the caller expects. `agm pair --account
// <id>` and POST /v1/pairing/start's account_id make the intent explicit: a
// signed-in address that hashes to a different acct_ ID is refused with
// pairing_wrong_account and changes nothing (spec sections 3.2, 4.7).
//
// An empty expectedID accepts whichever account signs in, which is how a
// first pairing and an ordinary re-pair work.
func (s *Supervisor) PairAs(ctx context.Context, expectedID string, backend gm.Backend, cookies map[string]string, deviceIndex int, emoji func(string)) (*Account, error) {
	persister, ok := backend.(gm.SessionPersister)
	if !ok {
		return nil, errors.New("backend cannot persist a session")
	}
	// Cleanup runs on a context the caller cannot cancel: a pairing abandoned
	// BY a cancellation still has to delete its row.
	cleanupCtx := context.WithoutCancel(ctx)

	// The emoji callback fires between StartGaiaPairing and FinishGaiaPairing,
	// by which time AuthData.Mobile is populated. That is the moment the
	// account identity is known but the pairing is not yet confirmed, and it
	// is where a new account's `pairing` row is created or an existing
	// account is resumed (spec section 3.2).
	var pendingID string
	var pendingNew bool
	var wrongAccount string
	var resumed bool
	wrapped := func(e string) {
		address := persister.AccountAddress()
		if gm.PlausibleAccountAddress(address) {
			id := store.AccountID(address)
			// The expected account is checked the moment the identity is
			// known and BEFORE any row is created, so a refusal really does
			// change nothing.
			if expectedID != "" && id != expectedID {
				wrongAccount = id
				if emoji != nil {
					emoji(e)
				}
				return
			}
			if existing, err := s.store.Account(ctx, id); err != nil {
				// A brand-new account starts life in `pairing`. Only a new
				// account passes through it.
				if errors.Is(err, store.ErrAccountNotFound) {
					now := s.clock.Now().UnixMilli()
					_ = s.store.UpsertAccount(ctx, store.Account{
						ID: id, GoogleAccount: address, State: store.StatePairing,
						CreatedAtMS: now, UpdatedAtMS: now,
					})
					pendingID, pendingNew = id, true
					s.markPairing(id, true)
					s.announce(ctx, id, "", StatePairing, ReasonNone)
				}
			} else {
				// Re-pairing an existing account does not move it to
				// `pairing`: it keeps its current state for the whole of the
				// new pairing and flips straight to connected on success, so
				// a caller polling never sees it become less usable than it
				// already was.
				pendingID, resumed = existing.ID, true
			}
		}
		if emoji != nil {
			emoji(e)
		}
	}

	// A row that has never been connected is deleted; a row that had been
	// connected reverts to its previous state, which is what not touching it
	// achieves. Every failure path below goes through this.
	abandon := func(reason string) {
		s.markPairing(pendingID, false)
		if pendingNew && pendingID != "" {
			_ = s.store.DeletePairingAccount(cleanupCtx, pendingID)
			s.audit(cleanupCtx, AuditPairFailed, map[string]any{
				"account_id": pendingID, "outcome": reason,
			})
		}
	}

	dev, err := backend.StartGooglePairing(ctx, cookies, deviceIndex, wrapped)
	if err != nil {
		abandon("pairing_failed")
		return nil, err
	}
	if wrongAccount != "" {
		// Nothing was created for the wrong account, and the expected
		// account's own row was never touched.
		abandon("pairing_wrong_account")
		if p, ok := backend.(gm.SessionPersister); ok {
			p.Shred()
		}
		return nil, gm.Classify(gm.ErrWrongAccount)
	}
	// A pairing the caller gave up on -- DELETE /v1/pairing/{id}, or a
	// cancelled request -- leaves no row and no session file. The library may
	// well have finished by now; that is exactly why this is checked.
	if cerr := ctx.Err(); cerr != nil {
		abandon("cancelled")
		if p, ok := backend.(gm.SessionPersister); ok {
			p.Shred()
		}
		return nil, gm.Classify(gm.ErrPairingCancelled)
	}
	// The address a backend hands back is validated here as well as inside
	// the adapter: an implausible one creates no account row and no session
	// file, because acct_ is derived from it (spec 3.2).
	address, err := gm.AccountAddressFromPairing(dev.AccountAddress)
	if err != nil {
		abandon("no_account_address")
		return nil, err
	}

	id := store.AccountID(address)
	if expectedID != "" && id != expectedID {
		abandon("pairing_wrong_account")
		return nil, gm.Classify(gm.ErrWrongAccount)
	}
	now := s.clock.Now().UnixMilli()
	acct := store.Account{
		ID:              id,
		GoogleAccount:   address,
		State:           store.StateConnected,
		PhoneID:         dev.PhoneID,
		GaiaDestRegUUID: dev.DestRegUUID,
		SessionPresent:  true,
		PairedAtMS:      now,
	}
	if !dev.DeviceLastSeen.IsZero() {
		acct.GaiaDeviceLastSeenMS = dev.DeviceLastSeen.UnixMilli()
	}
	prev := State("")
	if row, rerr := s.store.Account(ctx, id); rerr == nil {
		prev = row.State
	}
	if err := s.store.UpsertAccount(ctx, acct); err != nil {
		abandon("store_error")
		return nil, err
	}
	s.markPairing(id, false)
	s.announce(ctx, id, prev, StateConnected, ReasonNone)
	kind := AuditPaired
	if resumed {
		// A re-pair resumes the same acct_ row: no second account, no
		// duplicated history (spec section 4.7).
		kind = AuditResumed
	}
	// The payload names the device by INDEX and not by phone ID. A backend's
	// phone ID embeds the Google account address (the fake's is
	// `<address>/<index>`, and libgm's is derived the same way), and section
	// 12.2 forbids that address in an audit payload -- so the redactor
	// refuses it. It refuses the whole ROW, not the field: carrying the phone
	// ID here meant that once `serve` finally had an Auditor at all, a
	// pairing still wrote nothing and said so only in a warning.
	s.audit(ctx, kind, map[string]any{
		"account_id":   id,
		"device_index": dev.DeviceIndex,
		"device_count": dev.DeviceCount,
		"resumed":      resumed,
	})

	// A re-pair replaces this account's client, and the previous one may
	// still have a goroutine draining ITS events. That goroutine reads
	// a.Backend, so swapping the field under it is a data race -- and
	// leaving it running would be worse than the race: Start below is a
	// no-op on an account that is already running, so the new client would
	// never be connected and the old one would go on being drained. Stopping
	// first makes the swap happen with no reader and lets Start bring the
	// new client up.
	if prev, err := s.Get(id); err == nil {
		s.Stop(prev)
	}
	a := s.register(id, address, backend)
	if err := a.PersistSession(ctx); err != nil {
		return nil, err
	}
	if err := s.Start(ctx, a); err != nil {
		return a, err
	}
	return a, nil
}

func (s *Supervisor) markPairing(id string, live bool) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pairings == nil {
		s.pairings = map[string]bool{}
	}
	if live {
		s.pairings[id] = true
	} else {
		delete(s.pairings, id)
	}
}

// Adopt registers an already-paired backend, for a restart where the session
// file reloaded. It does not pair.
func (s *Supervisor) Adopt(ctx context.Context, id, address string, backend gm.Backend) *Account {
	return s.register(id, address, backend)
}

func (s *Supervisor) register(id, address string, backend gm.Backend) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.accounts[id]; ok {
		existing.Backend = backend
		existing.Address = address
		return existing
	}
	a := &Account{
		ID:              id,
		Address:         address,
		Backend:         backend,
		sup:             s,
		dmCache:         map[string]bool{},
		phoneResponding: true,
		ingest: &core.Ingester{
			Store:     s.store,
			AccountID: id,
			Log:       s.log,
		},
	}
	s.accounts[id] = a
	return a
}

// Start connects one account and begins draining its events. It is a no-op if
// the account is already running.
//
// Every connect issues one ListConversations for this account's client:
// the first call on a Client sends BUGLE_ANNOTATION and later calls
// BUGLE_MESSAGE, and the flag lives on the Client, of which there is one per
// account. Satisfying the rule for one account leaves another account's
// conversation events untrustworthy (spec section 3.7).
func (s *Supervisor) Start(ctx context.Context, a *Account) error {
	err := s.start(ctx, a)
	// Parking is re-evaluated whenever an account connects or disconnects
	// (spec section 4.7).
	s.rebalance(ctx)
	return err
}

// start is Start without the rebalance, so the rebalance itself can call it
// without recursing.
func (s *Supervisor) start(ctx context.Context, a *Account) error {
	s.mu.Lock()
	if a.running {
		s.mu.Unlock()
		return nil
	}
	if s.MaxConcurrent > 0 && s.runningCountLocked() >= s.MaxConcurrent {
		s.mu.Unlock()
		// Not an error, and not degraded: waiting for a slot is its own
		// state, and a parked account is fully readable. It holds no client
		// and no goroutine, which is why nothing below this line runs.
		return s.transition(ctx, a.ID, StateParked, ReasonCapacity)
	}
	a.running = true
	s.mu.Unlock()

	if err := a.Backend.Connect(ctx); err != nil {
		s.mu.Lock()
		a.running = false
		s.mu.Unlock()
		reason := ReasonListenError
		state := StateError
		var ge *gm.Error
		if errors.As(err, &ge) && ge.SignsOutAccount {
			state, reason = StateSignedOut, ReasonCredentials
		}
		_ = s.transition(ctx, a.ID, state, reason)
		return err
	}
	// Persist on every successful Connect (spec section 3.3).
	if err := a.PersistSession(ctx); err != nil {
		return err
	}

	if !s.ManualIngest {
		loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		a.cancel = cancel
		a.done = make(chan struct{})
		s.goroutines.Add(1)
		go a.loop(loopCtx)
		// One sweep timer per connected account, when there is a sweeper to
		// run. It also refreshes the cached `google` block.
		if s.Sweep != nil && s.SweepInterval > 0 {
			s.goroutines.Add(1)
			go a.sweepTimer(loopCtx)
		}
	}

	if _, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 100); err != nil {
		return err
	}

	// The cached `google` block is refreshed on connect, so GET /v1/health
	// answers from memory rather than waiting on a phone (spec section 7.5).
	a.refreshGoogle(ctx)

	// A re-pair or a restart resumes: a full reconciliation sweep runs with
	// since = accounts.last_event_at_ms (spec sections 4.7, 5.4). A brand-new
	// account has no last event, so `since` is the zero time and the sweep
	// covers everything.
	if s.Sweep != nil {
		var since time.Time
		if row, err := s.store.Account(ctx, a.ID); err == nil && row.LastEventAtMS > 0 {
			since = time.UnixMilli(row.LastEventAtMS).UTC()
		}
		if err := s.Sweep.Sweep(ctx, a.ID, since); err != nil {
			a.warn("the reconciliation sweep failed", err)
		} else {
			a.noteSweep(ctx, s.clock.Now())
		}
	}
	if s.Backfill != nil {
		if err := s.Backfill.Start(ctx, a.ID); err != nil {
			a.warn("starting backfill failed", err)
		}
	}
	return s.transition(ctx, a.ID, StateConnected, ReasonNone)
}

// sweepTimer is one account's sweep timer: it refreshes the cached `google`
// block and runs the reconciliation sweep every settings.ingest.sweep_interval
// (spec sections 5.4, 7.5). One timer per connected account; a slow phone on
// one account never delays another's.
func (a *Account) sweepTimer(ctx context.Context) {
	defer a.sup.goroutines.Add(-1)
	interval := a.sup.SweepInterval
	if interval <= 0 {
		return
	}
	for {
		if err := a.sup.clock.Sleep(ctx, interval); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		a.refreshGoogle(ctx)
		if a.sup.Sweep != nil {
			var since time.Time
			if row, err := a.sup.store.Account(ctx, a.ID); err == nil && row.LastEventAtMS > 0 {
				since = time.UnixMilli(row.LastEventAtMS).UTC()
			}
			if err := a.sup.Sweep.Sweep(ctx, a.ID, since); err != nil {
				a.warn("the reconciliation sweep failed", err)
			} else {
				a.noteSweep(ctx, a.sup.clock.Now())
			}
		}
	}
}

func (s *Supervisor) runningCountLocked() int {
	n := 0
	for _, a := range s.accounts {
		if a.running {
			n++
		}
	}
	return n
}

// Stop disconnects one account and stops its ingest goroutine. It deletes
// nothing.
func (s *Supervisor) Stop(a *Account) {
	s.mu.Lock()
	if !a.running {
		s.mu.Unlock()
		return
	}
	a.running = false
	s.mu.Unlock()

	a.Backend.Disconnect()
	if a.cancel != nil {
		a.cancel()
	}
	if a.done != nil {
		<-a.done
	}
	a.cancel, a.done = nil, nil
	// The account is no longer connected, so its cached Google block is no
	// longer a fact about a live phone: GET /v1/health serves null.
	a.clearGoogle()
	if s.Backfill != nil {
		s.Backfill.Stop(a.ID)
	}
}

// StopAll persists every session and stops every account, which is what
// graceful shutdown does.
func (s *Supervisor) StopAll(ctx context.Context) {
	for _, a := range s.List() {
		_ = a.PersistSession(ctx)
		s.Stop(a)
	}
}

// SignOut disconnects the account, stops its ingest goroutine, shreds its
// session file, zeroes the in-memory AuthData, sets state='signed_out' and
// session_present=0.
//
// It deletes no conversation, message, attachment, reaction, contact or
// operation. Everything stays readable and searchable (spec section 4.7, D30).
func (s *Supervisor) SignOut(ctx context.Context, a *Account) error {
	s.Stop(a)
	if p, ok := a.Backend.(gm.SessionPersister); ok {
		p.Shred()
	}
	if err := s.sessions.Shred(a.ID); err != nil {
		return err
	}
	if err := s.store.SetSessionPresent(ctx, a.ID, false); err != nil {
		return err
	}
	if err := s.transition(ctx, a.ID, StateSignedOut, ReasonCredentials); err != nil {
		return err
	}
	s.audit(ctx, AuditSignedOut, map[string]any{
		"account_id": a.ID, "session_shredded": true,
	})
	// Signing out frees a slot, so parking is re-evaluated.
	s.rebalance(ctx)
	return nil
}

// PersistSession writes this account's session file. It is called on
// PairSuccessful, on AuthTokenRefreshed, on every successful Connect, on
// graceful shutdown, and on the 5-minute dirty timer (spec section 3.3).
func (a *Account) PersistSession(ctx context.Context) error {
	p, ok := a.Backend.(gm.SessionPersister)
	if !ok {
		return nil
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	data, err := p.MarshalSession()
	if err != nil {
		return fmt.Errorf("marshalling session for %s: %w", a.ID, err)
	}
	if err := a.sup.sessions.Save(a.ID, data); err != nil {
		return fmt.Errorf("saving session for %s: %w", a.ID, err)
	}
	return a.sup.store.SetSessionPresent(ctx, a.ID, true)
}

// Ingester exposes this account's ingester, for tests and for the spike.
func (a *Account) Ingester() *core.Ingester { return a.ingest }

// loop drains this account's events. One goroutine per account, applying each
// event as a store write and stamping account_id on every row.
func (a *Account) loop(ctx context.Context) {
	defer a.sup.goroutines.Add(-1)
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-a.Backend.Events():
			if !ok {
				return
			}
			a.Apply(ctx, ev)
		}
	}
}

// Apply handles one event. It is exported so a test can drive it
// deterministically without a goroutine.
func (a *Account) Apply(ctx context.Context, ev gm.Event) {
	st := a.sup.store
	switch e := ev.(type) {
	case *gm.EventClientReady:
		for _, c := range e.Conversations {
			if _, err := a.ingest.IngestConversation(ctx, c); err != nil {
				a.warn("ingest conversation failed", err)
			}
		}
		a.setState(ctx, StateConnected, ReasonNone)

	case *gm.EventAuthTokenRefreshed:
		if err := a.PersistSession(ctx); err != nil {
			a.warn("persisting session failed", err)
		}

	case *gm.EventListenTemporaryError:
		a.setState(ctx, StateDegraded, ReasonListenError)

	case *gm.EventListenRecovered:
		a.setState(ctx, StateConnected, ReasonNone)

	case *gm.EventListenFatalError:
		// The match is on the error value, never the string: a 403 in the
		// retry branch loops forever on dead credentials.
		if e.CredentialsDead {
			a.setState(ctx, StateSignedOut, ReasonCredentials)
		} else {
			a.setState(ctx, StateError, ReasonListenError)
		}

	case *gm.EventPingFailed:
		switch {
		case e.EntityNotFound:
			// The phone no longer knows this pairing.
			a.setState(ctx, StateSignedOut, ReasonRevokedByPhone)
		case e.ErrorCount > 1:
			// Upstream deliberately ignores the first failure.
			a.setState(ctx, StateError, ReasonListenError)
		}

	case *gm.EventPairSuccessful:
		if err := a.PersistSession(ctx); err != nil {
			a.warn("persisting session failed", err)
		}

	case *gm.EventGaiaLoggedOut:
		// Cookies dead. Reads keep working; writes are refused with
		// not_signed_in. This deletes nothing, and the fix is a cookie
		// refresh, not a re-pair.
		a.setState(ctx, StateSignedOut, ReasonCookiesExpired)

	case *gm.EventRevokePairData:
		a.setState(ctx, StateSignedOut, ReasonRevokedByPhone)

	case *gm.EventAccountChange:
		// IsFake means it was synthesised at startup, not a real change.
		if !e.IsFake {
			a.setState(ctx, StateAccountChanged, ReasonAccountSwitched)
		}

	case *gm.EventConversation:
		if _, err := a.ingest.IngestConversation(ctx, e.Conversation); err != nil {
			a.warn("ingest conversation failed", err)
		}
		a.dmCache[e.Conversation.SourceID] = !e.Conversation.IsGroup
		a.touch(ctx)

	case *gm.EventMessage:
		isDM := a.isDM(ctx, e.Message.ConversationID)
		if _, err := a.ingest.IngestMessage(ctx, e.Message, isDM, e.IsOld); err != nil {
			a.warn("ingest message failed", err)
		}
		if !e.IsOld {
			a.touch(ctx)
		}

	case *gm.EventUserAlert:
		// THIS account's backfill is paused while a phone-side database sync
		// is outstanding and resumes on that phone's SYNC_COMPLETE. One
		// sleepy phone never stalls another account (spec section 5.2).
		switch {
		case e.Alert.PausesBackfill():
			a.healthMu.Lock()
			a.backfillPaused = true
			a.healthMu.Unlock()
			if a.sup.Backfill != nil {
				a.sup.Backfill.Pause(a.ID)
			}
		case e.Alert == gm.AlertMobileDatabaseSyncComplete:
			a.healthMu.Lock()
			a.backfillPaused = false
			a.healthMu.Unlock()
			if a.sup.Backfill != nil {
				a.sup.Backfill.Resume(a.ID)
			}
		}
		a.touch(ctx)

	case *gm.EventPhoneNotResponding:
		// phone_responding is a health fact, not a state: a phone asleep is
		// not an account fault (spec section 7.5).
		a.setPhoneResponding(false)

	case *gm.EventPhoneRespondingAgain:
		a.setPhoneResponding(true)

	case *gm.EventNoDataReceived, *gm.EventHackySetActiveMayFail,
		*gm.EventSettings, *gm.EventTyping:
		// Ephemeral signals; none of them moves this account's state.

	case *gm.EventUnknown:
		a.ingest.CountUnknown()

	default:
		a.ingest.CountUnknown()
	}
	_ = st
}

func (a *Account) isDM(ctx context.Context, convSourceID string) bool {
	if v, ok := a.dmCache[convSourceID]; ok {
		return v
	}
	c, err := a.sup.store.Conversation(ctx, store.ConversationID(a.ID, convSourceID))
	if err != nil {
		return false
	}
	a.dmCache[convSourceID] = !c.IsGroup
	return !c.IsGroup
}

func (a *Account) setState(ctx context.Context, st State, reason Reason) {
	if err := a.sup.transition(ctx, a.ID, st, reason); err != nil &&
		!errors.Is(err, store.ErrAccountNotFound) {
		a.warn("setting account state failed", err)
	}
}

// MarkUnresumable records on the row that an account could not be brought up
// at startup, so the state and state_reason say so instead of the row sitting
// at whatever it held when the process last stopped.
//
// It exists because the only two ways an account's state changed were "the
// supervisor is driving it" and "the owner acted", and there is a third: a
// session file the process cannot open. That account has no client, so every
// write naming it is refused -- and until this existed the refusal came with
// a state and a reason describing the LAST run, which is the one thing an
// operator reads to find out what is wrong now.
//
// `signed_out` is the state, not `error`: `error` means the supervisor is
// retrying with backoff, and nothing is retrying here. Section 15.4's runbook
// row already says an undecryptable data key leaves "every account
// `signed_out` with its history intact", which is exactly what this writes.
func (s *Supervisor) MarkUnresumable(ctx context.Context, accountID string, reason Reason) error {
	return s.transition(ctx, accountID, StateSignedOut, reason)
}

// transition moves one account's state and, when the state or the reason
// actually changed, writes account.state_changed and publishes to the feed.
//
// Every transition is audited with from, to and state_reason, which is what
// makes `degraded` and `error` diagnosable after the fact without reading
// logs (spec sections 4.7, 12.4).
func (s *Supervisor) transition(ctx context.Context, accountID string, to State, reason Reason) error {
	from := State("")
	fromReason := ReasonNone
	if row, err := s.store.Account(ctx, accountID); err == nil {
		from, fromReason = row.State, Reason(row.StateReason)
	}
	if err := s.store.SetAccountState(ctx, accountID, to, string(reason)); err != nil {
		return err
	}
	if from == to && fromReason == reason {
		// Not a transition: re-asserting a state is not a state change, and
		// an SSE subscriber must not see an event for one.
		return nil
	}
	s.announce(ctx, accountID, from, to, reason)
	return nil
}

// announce writes the audit row and publishes to the state-change feed. It is
// separate from transition because creating a `pairing` row is a state change
// with no previous row to read.
func (s *Supervisor) announce(ctx context.Context, accountID string, from, to State, reason Reason) {
	s.audit(ctx, AuditStateChanged, map[string]any{
		"account_id":   accountID,
		"from":         string(from),
		"to":           string(to),
		"state_reason": string(reason),
	})
	s.feed.publish(StateChange{
		AccountID: accountID, From: from, To: to, Reason: reason,
		// Truncated, not rounded: section 4.4 fixes millisecond precision on
		// every JSON surface, and a stream is one.
		At: s.clock.Now().Truncate(time.Millisecond),
	})
}

func (s *Supervisor) audit(ctx context.Context, kind string, fields map[string]any) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Append(ctx, kind, fields); err != nil {
		s.warn("writing an audit row failed", fmt.Sprint(fields["account_id"]), err)
	}
}

func (s *Supervisor) warn(msg, accountID string, err error) {
	if s.log != nil {
		// Logs carry the acct_ ID, never the Google account address.
		s.log.Warn(msg, "account_id", accountID, "error", err.Error())
	}
}

func (a *Account) touch(ctx context.Context) {
	if err := a.sup.store.TouchAccountEvent(ctx, a.ID, a.sup.clock.Now()); err != nil {
		a.warn("touching last_event_at_ms failed", err)
	}
}

func (a *Account) warn(msg string, err error) {
	if a.sup.log != nil {
		// Logs carry the acct_ ID, never the Google account address.
		a.sup.log.Warn(msg, "account_id", a.ID, "error", err.Error())
	}
}
