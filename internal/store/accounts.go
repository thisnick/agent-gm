package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AccountState is the vocabulary of spec section 4.7. There is no
// bad_credentials state and no unpaired state; this is the whole list.
type AccountState string

const (
	// StatePairing means a pair is in flight and there is no session yet. It
	// is never a resting state.
	StatePairing AccountState = "pairing"
	// StateConnected means the session is valid and the long poll is up.
	StateConnected AccountState = "connected"
	// StateDegraded means a transient listen error is being retried.
	StateDegraded AccountState = "degraded"
	// StateError means the supervisor is retrying Reconnect with backoff.
	StateError AccountState = "error"
	// StateSignedOut means the owner signed this account out, or its cookies
	// died. Reads keep working; writes are refused with not_signed_in.
	StateSignedOut AccountState = "signed_out"
	// StateParked means accounts.max_concurrent is reached, so this account
	// holds no client and no goroutine. Not an error, and not transient.
	StateParked AccountState = "parked"
	// StateAccountChanged means the phone switched Google accounts under us.
	StateAccountChanged AccountState = "account_changed"
)

// State reasons, so degraded and error are diagnosable without reading logs.
const (
	ReasonCapacity        = "capacity"
	ReasonListenError     = "listen_error"
	ReasonCredentials     = "credentials"
	ReasonRevokedByPhone  = "revoked_by_phone"
	ReasonCookiesExpired  = "cookies_expired"
	ReasonAccountSwitched = "account_switched"
	ReasonCrashRecovered  = "crash_recovered"
)

// CountsTowardAmbiguity reports whether a state counts toward the section 7.3
// ambiguity rule, which counts accounts in a usable or recoverable state.
// `pairing` accounts do not: an abandoned pair can never start demanding
// account_id on writes for an account that may never exist.
func CountsTowardAmbiguity(s AccountState) bool {
	switch s {
	case StateConnected, StateDegraded, StateParked, StateError, StateSignedOut, StateAccountChanged:
		return true
	default:
		return false
	}
}

// Account is one Google account.
type Account struct {
	ID                   string
	GoogleAccount        string
	Label                string
	State                AccountState
	StateReason          string
	PhoneID              string
	GaiaDestRegUUID      string
	GaiaDeviceLastSeenMS int64
	SessionPresent       bool
	PairedAtMS           int64
	LastEventAtMS        int64
	LastSweepAtMS        int64
	BackfillCompleteAtMS int64
	CreatedAtMS          int64
	UpdatedAtMS          int64
}

// ErrAccountNotFound is returned when no account has that ID.
var ErrAccountNotFound = errors.New("account not found")

const accountColumns = `id, google_account, label, state, state_reason, phone_id,
	gaia_dest_reg_uuid, gaia_device_last_seen_ms, session_present, paired_at_ms,
	last_event_at_ms, last_sweep_at_ms, backfill_complete_at_ms, created_at_ms, updated_at_ms`

func scanAccount(sc interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var label, reason, phone, destReg sql.NullString
	var lastSeen, paired, lastEvent, lastSweep, backfill sql.NullInt64
	err := sc.Scan(&a.ID, &a.GoogleAccount, &label, &a.State, &reason, &phone,
		&destReg, &lastSeen, &a.SessionPresent, &paired,
		&lastEvent, &lastSweep, &backfill, &a.CreatedAtMS, &a.UpdatedAtMS)
	if err != nil {
		return a, err
	}
	a.Label = label.String
	a.StateReason = reason.String
	a.PhoneID = phone.String
	a.GaiaDestRegUUID = destReg.String
	a.GaiaDeviceLastSeenMS = lastSeen.Int64
	a.PairedAtMS = paired.Int64
	a.LastEventAtMS = lastEvent.Int64
	a.LastSweepAtMS = lastSweep.Int64
	a.BackfillCompleteAtMS = backfill.Int64
	return a, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// UpsertAccount creates the account row or updates the mutable fields of an
// existing one. Re-pairing an existing account resumes it: the acct_ ID is
// derived from the address, so the same Google account -- even on a different
// phone -- lands on the same row (spec sections 3.2, 4.7).
func (s *Store) UpsertAccount(ctx context.Context, a Account) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (`+accountColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
			    google_account           = excluded.google_account,
			    label                    = COALESCE(excluded.label, accounts.label),
			    state                    = excluded.state,
			    state_reason             = excluded.state_reason,
			    phone_id                 = COALESCE(excluded.phone_id, accounts.phone_id),
			    gaia_dest_reg_uuid       = COALESCE(excluded.gaia_dest_reg_uuid, accounts.gaia_dest_reg_uuid),
			    gaia_device_last_seen_ms = COALESCE(excluded.gaia_device_last_seen_ms, accounts.gaia_device_last_seen_ms),
			    session_present          = excluded.session_present,
			    paired_at_ms             = COALESCE(excluded.paired_at_ms, accounts.paired_at_ms),
			    updated_at_ms            = excluded.updated_at_ms`,
			a.ID, a.GoogleAccount, nullString(a.Label), string(a.State), nullString(a.StateReason),
			nullString(a.PhoneID), nullString(a.GaiaDestRegUUID), nullInt(a.GaiaDeviceLastSeenMS),
			a.SessionPresent, nullInt(a.PairedAtMS), nullInt(a.LastEventAtMS),
			nullInt(a.LastSweepAtMS), nullInt(a.BackfillCompleteAtMS), now, now)
		return err
	})
}

// SetAccountState moves one account's state, with its machine-readable
// reason. No event from one account ever changes another's state.
func (s *Store) SetAccountState(ctx context.Context, accountID string, state AccountState, reason string) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE accounts SET state = ?, state_reason = ?, updated_at_ms = ? WHERE id = ?`,
			string(state), nullString(reason), now, accountID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrAccountNotFound
		}
		return nil
	})
}

// SetSessionPresent records whether sessions/<id>.enc is on disk.
func (s *Store) SetSessionPresent(ctx context.Context, accountID string, present bool) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts SET session_present = ?, updated_at_ms = ? WHERE id = ?`,
			present, now, accountID)
		return err
	})
}

// TouchAccountEvent moves an account's last_event_at_ms forward. It is a
// column on accounts, never a server_meta key: two accounts would race on one
// row (spec section 4.2).
func (s *Store) TouchAccountEvent(ctx context.Context, accountID string, at time.Time) error {
	ms := at.UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts
			    SET last_event_at_ms = MAX(COALESCE(last_event_at_ms, 0), ?),
			        updated_at_ms = ?
			  WHERE id = ?`, ms, ms, accountID)
		return err
	})
}

// SetAccountLastSweep records when this account's reconciliation sweep last
// ran (spec section 5.4 step 4).
//
// It moves only forward, like TouchAccountEvent: two sweeps for one account
// can overlap -- a timer sweep and a BROWSER_ACTIVE sweep, say -- and the
// slower one finishing second must not rewind the record and cause the next
// sweep to re-walk ground the faster one already covered.
//
// It is a column on accounts and never a server_meta key: two accounts would
// race on one row and the first to finish would mark the whole server swept
// (spec sections 4.2, 5.2).
func (s *Store) SetAccountLastSweep(ctx context.Context, accountID string, at time.Time) error {
	ms := at.UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts
			    SET last_sweep_at_ms = MAX(COALESCE(last_sweep_at_ms, 0), ?),
			        updated_at_ms = ?
			  WHERE id = ?`, ms, ms, accountID)
		return err
	})
}

// SetAccountBackfillComplete records when THIS account finished its initial
// backfill (spec section 5.2 step 7).
//
// Unlike the sweep timestamp this is set once and is not monotonic: a
// re-backfill after a re-pair legitimately re-stamps it, and an operator
// asking "when did this account last finish walking its history?" wants the
// latest answer rather than the first. Passing the zero time clears it, which
// is what re-opening a backfill does.
func (s *Store) SetAccountBackfillComplete(ctx context.Context, accountID string, at time.Time) error {
	now := s.clock.Now().UnixMilli()
	var value any
	if !at.IsZero() {
		value = at.UnixMilli()
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts SET backfill_complete_at_ms = ?, updated_at_ms = ? WHERE id = ?`,
			value, now, accountID)
		return err
	})
}

// SetAccountLabel sets the owner's human label. It is never an ID.
func (s *Store) SetAccountLabel(ctx context.Context, accountID, label string) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts SET label = ?, updated_at_ms = ? WHERE id = ?`,
			nullString(label), now, accountID)
		return err
	})
}

// Account reads one account.
func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	row := s.read.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAccountNotFound
	}
	return a, err
}

// Accounts lists every account, newest event first -- the order accounts are
// connected in when accounts.max_concurrent bounds how many run at once.
//
// This is the explicit all-accounts query: it has no account_id predicate
// because listing accounts is exactly the all-accounts case.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts
		  ORDER BY COALESCE(last_event_at_ms, 0) DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeletePairingAccount removes a `pairing` row that has never been connected.
// A startup sweep deletes every such row with no live pairing behind it:
// `pairing` is never a resting state (spec section 3.2).
func (s *Store) DeletePairingAccount(ctx context.Context, accountID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM accounts WHERE id = ? AND state = 'pairing' AND paired_at_ms IS NULL`,
			accountID)
		return err
	})
}

// SweepAbandonedPairings deletes every `pairing` row older than the timeout
// whose ID is not in live. It returns the IDs it deleted.
func (s *Store) SweepAbandonedPairings(ctx context.Context, live map[string]bool, olderThan time.Duration) ([]string, error) {
	cutoff := s.clock.Now().Add(-olderThan).UnixMilli()
	rows, err := s.read.QueryContext(ctx,
		`SELECT id FROM accounts WHERE state = 'pairing' AND paired_at_ms IS NULL`)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var deleted []string
	for _, id := range candidates {
		a, err := s.Account(ctx, id)
		if err != nil {
			return nil, err
		}
		if live[id] && a.UpdatedAtMS >= cutoff {
			continue
		}
		if err := s.DeletePairingAccount(ctx, id); err != nil {
			return nil, err
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}
