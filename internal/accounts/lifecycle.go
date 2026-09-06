package accounts

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// DefaultPairingTimeout is AGENT_GM_PAIRING_TIMEOUT (spec section 15.1): how
// long a started pairing may sit unconfirmed before the half-built AuthData is
// discarded and the `pairing` row deleted. It is measured against the injected
// clock, so no test waits it out.
const DefaultPairingTimeout = 5 * time.Minute

// DefaultSweepInterval is settings.ingest.sweep_interval (spec section 15.1),
// per account. It is also how often the cached `google` block of
// GET /v1/health is refreshed, so that route never blocks on a phone.
const DefaultSweepInterval = 15 * time.Minute

// PairingTimeoutFromEnv reads AGENT_GM_PAIRING_TIMEOUT, falling back to
// DefaultPairingTimeout. An unparseable value is the default: a malformed
// timeout must not silently mean "never expire".
func PairingTimeoutFromEnv() time.Duration {
	raw := os.Getenv("AGENT_GM_PAIRING_TIMEOUT")
	if raw == "" {
		return DefaultPairingTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultPairingTimeout
	}
	return d
}

// MaxConcurrentSource is the one runtime setting this package reads:
// settings.accounts.max_concurrent (spec sections 4.7, 15.1). It is a
// one-method interface rather than an import of internal/settings so that the
// supervisor depends on the value and not on the registry.
type MaxConcurrentSource interface {
	AccountsMaxConcurrent() int
}

// Sweeper runs the reconciliation sweep of spec section 5.4 for one account,
// with `since` taken from that account's last_event_at_ms. One timer per
// connected account; a sweep for one account never touches another's rows.
type Sweeper interface {
	Sweep(ctx context.Context, accountID string, since time.Time) error
}

// Backfiller is one backfill worker per connected account (spec section 5.2).
// Pause and Resume are driven by THAT account's
// MOBILE_DATABASE_SYNC_STARTED / MOBILE_DATABASE_SYNC_COMPLETE alerts, so one
// sleepy phone never stalls another account.
type Backfiller interface {
	Start(ctx context.Context, accountID string) error
	Stop(accountID string)
	Pause(accountID string)
	Resume(accountID string)
	Progress(accountID string) BackfillHealth
}

// ---------------------------------------------------------------------------
// Parking, and re-evaluating it
// ---------------------------------------------------------------------------

// SetMaxConcurrent changes accounts.max_concurrent and re-evaluates parking
// immediately, so raising the setting connects a parked account WITHOUT a
// restart (spec section 4.7, section 16 Slice 2 test 37).
func (s *Supervisor) SetMaxConcurrent(ctx context.Context, n int) {
	s.mu.Lock()
	s.MaxConcurrent = n
	s.mu.Unlock()
	s.rebalance(ctx)
}

// ReloadMaxConcurrent re-reads the setting from its source and re-evaluates
// parking. It is what a settings change calls.
func (s *Supervisor) ReloadMaxConcurrent(ctx context.Context) {
	if s.Settings == nil {
		s.rebalance(ctx)
		return
	}
	s.SetMaxConcurrent(ctx, s.Settings.AccountsMaxConcurrent())
}

// rebalance fills every free slot with the newest parked account.
//
// "Newest" is last_event_at_ms order, newest first (spec section 4.7), which
// store.Accounts already returns. A running account is never preempted: a
// slot opens when an account connects or disconnects, and that is when this
// runs.
func (s *Supervisor) rebalance(ctx context.Context) {
	s.mu.Lock()
	if s.rebalancing {
		s.mu.Unlock()
		return
	}
	s.rebalancing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.rebalancing = false
		s.mu.Unlock()
	}()

	tried := map[string]bool{}
	for {
		s.mu.Lock()
		free := s.MaxConcurrent <= 0 || s.runningCountLocked() < s.MaxConcurrent
		s.mu.Unlock()
		if !free {
			return
		}
		rows, err := s.store.Accounts(ctx)
		if err != nil {
			return
		}
		var next *Account
		for _, row := range rows { // newest last_event_at_ms first
			if row.State != StateParked || tried[row.ID] {
				continue
			}
			s.mu.Lock()
			a, ok := s.accounts[row.ID]
			running := ok && a.running
			s.mu.Unlock()
			if !ok || running {
				continue
			}
			next = a
			break
		}
		if next == nil {
			return
		}
		tried[next.ID] = true
		if err := s.start(ctx, next); err != nil {
			s.warn("connecting a parked account failed", next.ID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Abandoned pairings
// ---------------------------------------------------------------------------

// SweepPairings deletes every `pairing` row with no live pairing behind it,
// and every live one older than the pairing timeout. It runs at startup --
// where the live set is empty, so a process killed mid-pairing leaves nothing
// behind -- and on a timer.
//
// It returns the IDs it deleted. A `pairing` row does not count toward the
// section 7.3 ambiguity rule even before this runs (store.CountsTowardAmbiguity),
// so an abandoned pairing can never start demanding account_id on writes.
func (s *Supervisor) SweepPairings(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	live := make(map[string]bool, len(s.pairings))
	for id := range s.pairings {
		live[id] = true
	}
	timeout := s.PairingTimeout
	s.mu.Unlock()
	if timeout <= 0 {
		timeout = DefaultPairingTimeout
	}
	deleted, err := s.store.SweepAbandonedPairings(ctx, live, timeout)
	for _, id := range deleted {
		s.audit(ctx, AuditPairFailed, map[string]any{
			"account_id": id, "outcome": "abandoned",
		})
	}
	return deleted, err
}

// ---------------------------------------------------------------------------
// Choosing an account, and refusing a write
// ---------------------------------------------------------------------------

// ChooseWriteAccount applies spec section 7.3 to a write. The rule itself
// lives in core.ResolveAccount, which is the single implementation of it; this
// supplies the candidate list from the store and hands back the chosen row.
//
// Only accounts in a usable or recoverable state are candidates. That is what
// keeps an abandoned `pairing` row from making a single-account deployment
// ambiguous (spec section 16 Slice 2 test 38); a `signed_out` or `parked`
// account IS a candidate, because refusing its write for the right reason
// tells the caller more than pretending it does not exist.
func (s *Supervisor) ChooseWriteAccount(ctx context.Context, requested string) (store.Account, error) {
	rows, err := s.store.Accounts(ctx)
	if err != nil {
		return store.Account{}, apierr.Internal(err)
	}
	byID := make(map[string]store.Account, len(rows))
	refs := make([]core.AccountRef, 0, len(rows))
	for _, r := range rows {
		if !store.CountsTowardAmbiguity(r.State) {
			continue
		}
		byID[r.ID] = r
		refs = append(refs, core.AccountRef{
			ID: r.ID, GoogleAccount: r.GoogleAccount, State: string(r.State),
		})
	}
	res, err := core.ResolveAccount(requested, refs, core.Write)
	if err != nil {
		return store.Account{}, err
	}
	return byID[res.AccountID], nil
}

// CheckWritable refuses a write naming an account that cannot make one. The
// refusal is unsupported_capability with details.reason = "not_signed_in": a
// per-account condition, never the service-level not_paired (spec sections
// 4.7, 7.8).
func (s *Supervisor) CheckWritable(ctx context.Context, accountID string) error {
	row, err := s.store.Account(ctx, accountID)
	if errors.Is(err, store.ErrAccountNotFound) {
		return apierr.NotFound("account " + accountID)
	}
	if err != nil {
		return apierr.Internal(err)
	}
	if Writable(row.State) {
		return nil
	}
	return apierr.UnsupportedCapability(apierr.ReasonNotSignedIn,
		accountID, "write to", "account_state", string(row.State))
}

// ---------------------------------------------------------------------------
// Health (spec section 7.5)
// ---------------------------------------------------------------------------

// GoogleHealth is the per-account `google` block. It is served from a cache
// refreshed on connect and every sweep interval, so GET /v1/health never
// blocks on a phone, and it is nil -- null on the wire -- for an account that
// is not `connected`.
type GoogleHealth struct {
	ConfigVersionLive string `json:"config_version_live"`
	// ConfigVersionStale is informational and is not a fault (D32).
	ConfigVersionStale bool `json:"config_version_stale"`
	IsDefaultSMSApp    bool `json:"is_default_sms_app"`
}

// The closed `backfill.state` vocabulary of spec section 7.5.
const (
	// BackfillComplete: accounts.backfill_complete_at_ms is set, so an empty
	// result means there is nothing rather than not yet.
	BackfillComplete = "complete"
	// BackfillRunning: a worker is walking this account now.
	BackfillRunning = "running"
	// BackfillPaused: THIS account's phone reported a database sync, so its
	// walk is waiting (section 5.2). Another account's phone does not cause
	// it.
	BackfillPaused = "paused"
	// BackfillPending: scheduled, not started yet.
	BackfillPending = "pending"
	// BackfillNotStarted: this account will NOT be scheduled -- parked,
	// signed out, error or account_changed -- so no worker exists for it.
	BackfillNotStarted = "not_started"
)

// BackfillStates is the closed vocabulary, for the test that keeps it closed.
func BackfillStates() []string {
	return []string{
		BackfillComplete, BackfillRunning, BackfillPaused,
		BackfillPending, BackfillNotStarted,
	}
}

// progressFor asks the worker for this account's progress, telling it the
// account's state so it can distinguish `pending` (scheduled, not started)
// from `not_started` (will not be scheduled at all).
func (s *Supervisor) progressFor(row store.Account) BackfillHealth {
	if w, ok := s.Backfill.(interface {
		ProgressFor(string, State) BackfillHealth
	}); ok {
		return w.ProgressFor(row.ID, State(row.State))
	}
	return s.Backfill.Progress(row.ID)
}

// BackfillHealth is the per-account `backfill` block.
type BackfillHealth struct {
	State              string     `json:"state"`
	ConversationsDone  int        `json:"conversations_done"`
	ConversationsTotal int        `json:"conversations_total"`
	CompletedAt        *time.Time `json:"completed_at"`
}

// SweepHealth is the per-account `sweep` block.
type SweepHealth struct {
	// LastSweepAt is accounts.last_sweep_at_ms, so it survives a restart:
	// "when did this account last sweep?" is exactly the question an operator
	// asks after one.
	LastSweepAt *time.Time `json:"last_sweep_at"`
	// SweepsTotal is a PROCESS-LIFETIME counter, not a stored total. A small
	// number right after a restart means the process is young, not that the
	// sweeper has stalled -- read LastSweepAt for that.
	SweepsTotal int `json:"sweeps_total"`
}

// Counters is the per-account `counters` block.
type Counters struct {
	DroppedEvents     uint64 `json:"dropped_events"`
	UnknownEvents     uint64 `json:"unknown_events"`
	PendingOperations int    `json:"pending_operations"`
}

// AccountHealth is the per-account object GET /v1/health embeds and
// GET /v1/accounts/{id} returns (spec section 7.5).
type AccountHealth struct {
	AccountID     string `json:"account_id"`
	GoogleAccount string `json:"google_account"`
	// Label and StateReason are POINTERS so that "no label" and "no reason"
	// encode as null, exactly as GET /v1/accounts encodes them. Section 7.5
	// says the health block is "the same per-account object", and serving ""
	// here against null there made the same account two shapes depending on
	// which route a client asked.
	Label           *string        `json:"label"`
	State           State          `json:"state"`
	StateReason     *string        `json:"state_reason"`
	PhoneResponding bool           `json:"phone_responding"`
	LastEventAt     *time.Time     `json:"last_event_at"`
	Google          *GoogleHealth  `json:"google"`
	Backfill        BackfillHealth `json:"backfill"`
	Sweep           SweepHealth    `json:"sweep"`
	Counters        Counters       `json:"counters"`
}

// Health is every account's health block, newest event first. It reads
// cached Google values and never calls the phone.
func (s *Supervisor) Health(ctx context.Context) ([]AccountHealth, error) {
	rows, err := s.store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AccountHealth, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.healthFor(ctx, row))
	}
	return out, nil
}

// HealthFor is one account's health block.
func (s *Supervisor) HealthFor(ctx context.Context, accountID string) (AccountHealth, error) {
	row, err := s.store.Account(ctx, accountID)
	if err != nil {
		return AccountHealth{}, err
	}
	return s.healthFor(ctx, row), nil
}

func (s *Supervisor) healthFor(ctx context.Context, row store.Account) AccountHealth {
	s.mu.Lock()
	a := s.accounts[row.ID]
	s.mu.Unlock()

	h := AccountHealth{
		AccountID:       row.ID,
		GoogleAccount:   row.GoogleAccount,
		Label:           nullableString(row.Label),
		State:           row.State,
		StateReason:     nullableString(row.StateReason),
		PhoneResponding: true,
		LastEventAt:     msToTime(row.LastEventAtMS),
	}
	// backfill.completed_at is accounts.backfill_complete_at_ms -- the column,
	// never a server_meta key: two accounts would race on one row and the
	// first to finish would mark the whole server complete (spec section 5.2).
	h.Backfill = BackfillHealth{State: "pending", CompletedAt: msToTime(row.BackfillCompleteAtMS)}
	if row.BackfillCompleteAtMS > 0 {
		h.Backfill.State = "complete"
	}
	if s.Backfill != nil {
		// A worker knows the conversation counts and whether it is running;
		// the completion instant is still the stored one, so it survives a
		// restart the worker does not.
		progress := s.progressFor(row)
		progress.CompletedAt = h.Backfill.CompletedAt
		// A stored completion turns `pending`/`not_started` into `complete`:
		// a signed-out account that finished before it was signed out still
		// has its history indexed, which is the whole point of section 4.7's
		// "signing out keeps everything", and an agent must not read its
		// empty result as "not yet".
		//
		// It does NOT override a worker that is running or paused right now.
		// A re-backfill after a re-pair is exactly that case, and "it is
		// walking at this moment" is the more actionable fact than "it
		// finished once before".
		if progress.CompletedAt != nil &&
			(progress.State == BackfillPending || progress.State == BackfillNotStarted) {
			progress.State = BackfillComplete
		}
		h.Backfill = progress
	}
	h.Sweep = SweepHealth{LastSweepAt: msToTime(row.LastSweepAtMS)}

	if a != nil {
		a.healthMu.Lock()
		h.PhoneResponding = a.phoneResponding
		if a.backfillPaused {
			// THIS account's phone is mid-database-sync. Another account's
			// backfill is unaffected.
			h.Backfill.State = "paused"
		}
		h.Sweep.SweepsTotal = a.sweeps
		// The google block is null for an account that is not connected:
		// stale cached values on a signed-out account would read as live
		// facts about a phone nobody is talking to.
		if row.State == StateConnected && a.google != nil {
			g := *a.google
			h.Google = &g
		}
		a.healthMu.Unlock()

		if c, ok := a.Backend.(gm.DroppedEventCounter); ok {
			h.Counters.DroppedEvents = c.DroppedEvents()
			h.Counters.UnknownEvents = c.UnknownEvents()
		}
	}
	notTerminal := false
	if ops, err := s.store.ListOperations(ctx, store.OperationQuery{
		AccountID: row.ID, Terminal: &notTerminal,
	}); err == nil {
		h.Counters.PendingOperations = len(ops)
	}
	return h
}

func msToTime(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// refreshGoogle caches FetchConfig and IsDefaultSMSApp for this account. It
// runs on connect and on every sweep tick; GET /v1/health only ever reads the
// cache. A phone that does not answer leaves the previous cached value alone
// rather than blanking the block.
func (a *Account) refreshGoogle(ctx context.Context) {
	cfg, err := a.Backend.FetchConfig(ctx)
	if err != nil {
		a.warn("fetching the Google config failed", err)
		return
	}
	isDefault, err := a.Backend.IsDefaultSMSApp(ctx)
	if err != nil {
		a.warn("asking whether we are the default SMS app failed", err)
		return
	}
	compiled := a.Backend.CompiledConfigVersion()
	a.healthMu.Lock()
	a.google = &GoogleHealth{
		ConfigVersionLive: cfg.Live.String(),
		// The staleness rule is the date diff alone (spec section 3.7), and
		// it is informational: a live version differing from the compiled one
		// is not a fault (D32).
		ConfigVersionStale: !cfg.Live.SameDate(compiled),
		IsDefaultSMSApp:    isDefault,
	}
	a.healthMu.Unlock()
}

// clearGoogle drops the cached Google block, which is what disconnecting
// does: the account is no longer connected, so the block is null.
func (a *Account) clearGoogle() {
	a.healthMu.Lock()
	a.google = nil
	a.healthMu.Unlock()
}

// noteSweep records that one sweep finished. last_sweep_at_ms is stored so it
// outlives the process; the total is a process-lifetime counter.
//
// The column write is monotonic in the store, because two sweeps for one
// account can overlap and the slower one finishing second must not rewind the
// record.
func (a *Account) noteSweep(ctx context.Context, at time.Time) {
	a.healthMu.Lock()
	a.sweeps++
	a.healthMu.Unlock()
	if err := a.sup.store.SetAccountLastSweep(ctx, a.ID, at); err != nil {
		a.warn("recording last_sweep_at_ms failed", err)
	}
}

func (a *Account) setPhoneResponding(v bool) {
	a.healthMu.Lock()
	a.phoneResponding = v
	a.healthMu.Unlock()
}

// nullableString encodes an absent value as JSON null rather than as "".
// Section 4.7 says state_reason is "a short machine-readable string ... or
// null", and section 7.5 says the health block is the same object
// /v1/accounts serves -- so the two routes must agree, and "" is not null.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
