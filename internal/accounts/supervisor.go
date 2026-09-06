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

	sup      *Supervisor
	ingest   *core.Ingester
	cancel   context.CancelFunc
	done     chan struct{}
	running  bool
	sessMu  sync.Mutex
	dmCache map[string]bool
}

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

	// ManualIngest stops Start from launching the per-account event
	// goroutine, so a test can drive Apply itself and never race the loop.
	// Production leaves it false.
	ManualIngest bool

	mu       sync.Mutex
	accounts map[string]*Account
}

// New builds a supervisor.
func New(st *store.Store, sessions *store.SessionStore, clk clock.Clock, log core.Logger) *Supervisor {
	return &Supervisor{
		store:         st,
		sessions:      sessions,
		clock:         clk,
		log:           log,
		MaxConcurrent: DefaultMaxConcurrent,
		accounts:      map[string]*Account{},
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
	persister, ok := backend.(gm.SessionPersister)
	if !ok {
		return nil, errors.New("backend cannot persist a session")
	}

	// The emoji callback fires between StartGaiaPairing and FinishGaiaPairing,
	// by which time AuthData.Mobile is populated. That is the moment the
	// account identity is known but the pairing is not yet confirmed, and it
	// is where a new account's `pairing` row is created or an existing
	// account is resumed (spec section 3.2).
	var pendingID string
	var pendingNew bool
	wrapped := func(e string) {
		address := persister.AccountAddress()
		if gm.PlausibleAccountAddress(address) {
			id := store.AccountID(address)
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
				}
			} else {
				// Re-pairing an existing account does not move it to
				// `pairing`: it keeps its current state for the whole of the
				// new pairing and flips straight to connected on success, so
				// a caller polling never sees it become less usable than it
				// already was.
				pendingID = existing.ID
			}
		}
		if emoji != nil {
			emoji(e)
		}
	}

	dev, err := backend.StartGooglePairing(ctx, cookies, deviceIndex, wrapped)
	if err != nil {
		// A row that has never been connected is deleted; a row that had been
		// connected reverts to its previous state, which is what not touching
		// it achieves.
		if pendingNew && pendingID != "" {
			_ = s.store.DeletePairingAccount(ctx, pendingID)
		}
		return nil, err
	}
	// The address a backend hands back is validated here as well as inside
	// the adapter: an implausible one creates no account row and no session
	// file, because acct_ is derived from it (spec 3.2).
	address, err := gm.AccountAddressFromPairing(dev.AccountAddress)
	if err != nil {
		if pendingNew && pendingID != "" {
			_ = s.store.DeletePairingAccount(ctx, pendingID)
		}
		return nil, err
	}

	id := store.AccountID(address)
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
	if err := s.store.UpsertAccount(ctx, acct); err != nil {
		return nil, err
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
		ID:      id,
		Address: address,
		Backend: backend,
		sup:     s,
		dmCache: map[string]bool{},
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
	s.mu.Lock()
	if a.running {
		s.mu.Unlock()
		return nil
	}
	if s.MaxConcurrent > 0 && s.runningCountLocked() >= s.MaxConcurrent {
		s.mu.Unlock()
		// Not an error, and not degraded: waiting for a slot is its own
		// state, and a parked account is fully readable.
		return s.store.SetAccountState(ctx, a.ID, store.StateParked, store.ReasonCapacity)
	}
	a.running = true
	s.mu.Unlock()

	if err := a.Backend.Connect(ctx); err != nil {
		s.mu.Lock()
		a.running = false
		s.mu.Unlock()
		reason := store.ReasonListenError
		state := store.StateError
		var ge *gm.Error
		if errors.As(err, &ge) && ge.SignsOutAccount {
			state, reason = store.StateSignedOut, store.ReasonCredentials
		}
		_ = s.store.SetAccountState(ctx, a.ID, state, reason)
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
		go a.loop(loopCtx)
	}

	if _, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 100); err != nil {
		return err
	}
	return s.store.SetAccountState(ctx, a.ID, store.StateConnected, "")
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
	return s.store.SetAccountState(ctx, a.ID, store.StateSignedOut, store.ReasonCredentials)
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
		a.setState(ctx, store.StateConnected, "")

	case *gm.EventAuthTokenRefreshed:
		if err := a.PersistSession(ctx); err != nil {
			a.warn("persisting session failed", err)
		}

	case *gm.EventListenTemporaryError:
		a.setState(ctx, store.StateDegraded, store.ReasonListenError)

	case *gm.EventListenRecovered:
		a.setState(ctx, store.StateConnected, "")

	case *gm.EventListenFatalError:
		// The match is on the error value, never the string: a 403 in the
		// retry branch loops forever on dead credentials.
		if e.CredentialsDead {
			a.setState(ctx, store.StateSignedOut, store.ReasonCredentials)
		} else {
			a.setState(ctx, store.StateError, store.ReasonListenError)
		}

	case *gm.EventPingFailed:
		switch {
		case e.EntityNotFound:
			// The phone no longer knows this pairing.
			a.setState(ctx, store.StateSignedOut, store.ReasonRevokedByPhone)
		case e.ErrorCount > 1:
			// Upstream deliberately ignores the first failure.
			a.setState(ctx, store.StateError, store.ReasonListenError)
		}

	case *gm.EventPairSuccessful:
		if err := a.PersistSession(ctx); err != nil {
			a.warn("persisting session failed", err)
		}

	case *gm.EventGaiaLoggedOut:
		// Cookies dead. Reads keep working; writes are refused with
		// not_signed_in. This deletes nothing, and the fix is a cookie
		// refresh, not a re-pair.
		a.setState(ctx, store.StateSignedOut, store.ReasonCookiesExpired)

	case *gm.EventRevokePairData:
		a.setState(ctx, store.StateSignedOut, store.ReasonRevokedByPhone)

	case *gm.EventAccountChange:
		// IsFake means it was synthesised at startup, not a real change.
		if !e.IsFake {
			a.setState(ctx, store.StateAccountChanged, store.ReasonAccountSwitched)
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
		a.touch(ctx)

	case *gm.EventPhoneNotResponding, *gm.EventPhoneRespondingAgain,
		*gm.EventNoDataReceived, *gm.EventHackySetActiveMayFail,
		*gm.EventSettings, *gm.EventTyping:
		// Health and ephemeral signals. The health document and the
		// reconciliation sweep they feed arrive with Slice 2.

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

func (a *Account) setState(ctx context.Context, st store.AccountState, reason string) {
	if err := a.sup.store.SetAccountState(ctx, a.ID, st, reason); err != nil &&
		!errors.Is(err, store.ErrAccountNotFound) {
		a.warn("setting account state failed", err)
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
