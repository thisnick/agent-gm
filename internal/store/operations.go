package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// operations is idempotency and status. It is NOT an outbox (D4): there is no
// queue, so there is no `queued` and no `accepted` state.

// OperationStatus is the closed vocabulary of spec section 6.4.
type OperationStatus string

const (
	// OpRunning means in flight, or the process died mid-call.
	OpRunning OperationStatus = "running"
	// OpSucceeded means the phone accepted it.
	OpSucceeded OperationStatus = "succeeded"
	// OpPending is libgm.ErrPhoneNotResponding and nothing else. The server
	// accepted the request; the phone may still act on it when it wakes.
	// This is not a failure: reporting it as one invites the caller to
	// resend, which sends the message twice when the phone wakes up.
	OpPending OperationStatus = "pending"
	// OpFailed means the phone refused it, or a non-retryable error.
	OpFailed OperationStatus = "failed"
	// OpUnknown means it never settled within pending_timeout, or it was
	// recovered from a crash.
	OpUnknown OperationStatus = "unknown"
)

// ErrCrashRecovered is the error_code written by the startup sweep.
const ErrCrashRecovered = "crash_recovered"

// ErrPendingTimeout is the error_code written by the pending_timeout reaper.
const ErrPendingTimeout = "pending_timeout"

// OperationTerminal derives `terminal` from the status. It is never stored
// independently (spec section 6.5).
func OperationTerminal(s OperationStatus) bool {
	switch s {
	case OpSucceeded, OpFailed, OpUnknown:
		return true
	default:
		return false
	}
}

// OperationTransitionAllowed is spec section 6.4's transition table, exactly:
//
//	running -> succeeded | failed | pending
//	pending -> succeeded | failed          (the echo arrived)
//	pending -> unknown                     (pending_timeout elapsed)
//	running -> unknown                     (crash recovery)
//	unknown -> succeeded | failed          (late authoritative evidence)
//
// Nothing leaves `succeeded` or `failed`: those are the two states a caller is
// entitled to treat as final.
func OperationTransitionAllowed(from, to OperationStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case OpRunning:
		return to == OpSucceeded || to == OpFailed || to == OpPending || to == OpUnknown
	case OpPending:
		return to == OpSucceeded || to == OpFailed || to == OpUnknown
	case OpUnknown:
		return to == OpSucceeded || to == OpFailed
	default:
		return false
	}
}

// ErrOperationTransition means a settle would have moved an operation
// backwards. The stored status is left alone.
var ErrOperationTransition = errors.New("operation status may not move that way")

// ErrOperationNotFound is returned when no operation has that ID or key.
var ErrOperationNotFound = errors.New("operation not found")

// Operation is one mutation's idempotency and status record.
type Operation struct {
	ID                 string
	AccountID          string
	Kind               string
	AuthorizationID    string
	IdempotencyKey     string
	RequestFingerprint string
	ConversationID     string
	MessageID          string
	TmpID              string
	Status             OperationStatus
	Terminal           bool
	TerminalAtMS       int64
	CorrectedAtMS      int64
	ErrorCode          string
	ErrorMessage       string
	ErrorRetryable     bool
	HasErrorRetryable  bool
	GoogleStatusRaw    int32
	RequestPayloadJSON string
	CreatedAtMS        int64
	UpdatedAtMS        int64
}

const operationColumns = `id, account_id, kind, authorization_id, idempotency_key,
	request_fingerprint, conversation_id, message_id, tmp_id, status, terminal,
	terminal_at_ms, corrected_at_ms, error_code, error_message, error_retryable,
	google_status_raw, request_payload_json, created_at_ms, updated_at_ms`

func scanOperation(sc interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	var conv, msg, tmp, code, message sql.NullString
	var terminalAt, correctedAt, retryable, googleRaw sql.NullInt64
	err := sc.Scan(&o.ID, &o.AccountID, &o.Kind, &o.AuthorizationID, &o.IdempotencyKey,
		&o.RequestFingerprint, &conv, &msg, &tmp, &o.Status, &o.Terminal,
		&terminalAt, &correctedAt, &code, &message, &retryable,
		&googleRaw, &o.RequestPayloadJSON, &o.CreatedAtMS, &o.UpdatedAtMS)
	if err != nil {
		return o, err
	}
	o.ConversationID = conv.String
	o.MessageID = msg.String
	o.TmpID = tmp.String
	o.ErrorCode = code.String
	o.ErrorMessage = message.String
	o.ErrorRetryable = retryable.Int64 != 0
	o.HasErrorRetryable = retryable.Valid
	o.TerminalAtMS = terminalAt.Int64
	o.CorrectedAtMS = correctedAt.Int64
	o.GoogleStatusRaw = int32(googleRaw.Int64)
	return o, nil
}

// RequestFingerprint is a SHA-256 over the canonically serialised body: keys
// sorted, no insignificant whitespace. Reordered JSON keys are therefore a
// replay and any changed value is not (spec section 6.3).
func RequestFingerprint(body []byte) (string, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if len(bytes.TrimSpace(body)) == 0 {
		v = nil
	} else if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("fingerprinting request body: %w", err)
	}
	var sb strings.Builder
	canonicalJSON(&sb, v)
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:]), nil
}

func canonicalJSON(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeJSONString(sb, k)
			sb.WriteByte(':')
			canonicalJSON(sb, t[k])
		}
		sb.WriteByte('}')
	case []any:
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			canonicalJSON(sb, e)
		}
		sb.WriteByte(']')
	case string:
		writeJSONString(sb, t)
	case json.Number:
		sb.WriteString(t.String())
	case bool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case nil:
		sb.WriteString("null")
	default:
		// Unreachable for json.Decode with UseNumber, but a silent wrong
		// answer here would make two different bodies look like a replay.
		raw, _ := json.Marshal(t)
		sb.Write(raw)
	}
}

func writeJSONString(sb *strings.Builder, s string) {
	raw, _ := json.Marshal(s)
	sb.Write(raw)
}

// InsertOperation writes a new operation in `running`. The row exists before
// the backend call, so a crash between the two is recoverable
// (spec sections 6.2, 6.6).
func (s *Store) InsertOperation(ctx context.Context, o Operation) error {
	now := s.clock.Now().UnixMilli()
	if o.ID == "" {
		o.ID = OperationID()
	}
	if o.RequestPayloadJSON == "" {
		o.RequestPayloadJSON = "{}"
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
			    request_fingerprint, conversation_id, message_id, tmp_id, status, terminal,
			    request_payload_json, created_at_ms, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,'running',0,?,?,?)`,
			o.ID, o.AccountID, o.Kind, o.AuthorizationID, o.IdempotencyKey,
			o.RequestFingerprint, nullString(o.ConversationID), nullString(o.MessageID),
			nullString(o.TmpID), o.RequestPayloadJSON, now, now)
		return err
	})
}

// Operation reads one operation by its op_ ID.
func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+operationColumns+`
		   -- all-accounts: an op_ ID already carries its account (section 4.1).
		   FROM operations WHERE id = ?`, id)
	o, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrOperationNotFound
	}
	return o, err
}

// OperationByKey is the idempotency lookup. Uniqueness is scoped to
// (authorization, account, kind, key): the same key sending to two accounts
// is two operations, because it is two messages to two people
// (spec section 6.3).
func (s *Store) OperationByKey(ctx context.Context, authorizationID, accountID, kind, key string) (Operation, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+operationColumns+` FROM operations
		  WHERE authorization_id = ? AND account_id = ? AND kind = ? AND idempotency_key = ?`,
		authorizationID, accountID, kind, key)
	o, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrOperationNotFound
	}
	return o, err
}

// KeyUsedByAnotherAccount answers the mirror hazard of spec section 6.3.
//
// Because the account is part of the idempotency tuple, reusing a key against
// a *different* account is not a replay -- it is a new operation, and it
// sends a second real message to a real person. That is the safe direction
// for the store and the dangerous one for a caller that "retries" a failed
// send by switching accounts, so both agent-facing surfaces refuse it. This
// query names the account the key was first used with, so the refusal can say
// which one.
func (s *Store) KeyUsedByAnotherAccount(ctx context.Context, authorizationID, kind, key, accountID string) (string, bool, error) {
	var other string
	err := s.read.QueryRowContext(ctx,
		`SELECT account_id FROM operations
		  WHERE authorization_id = ? AND kind = ? AND idempotency_key = ? AND account_id <> ?
		  ORDER BY created_at_ms ASC LIMIT 1`,
		authorizationID, kind, key, accountID).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return other, true, nil
}

// OperationByTmpID correlates the remote echo back to its send attempt,
// within the account (spec section 6.3).
func (s *Store) OperationByTmpID(ctx context.Context, accountID, tmpID string) (Operation, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+operationColumns+` FROM operations
		  WHERE account_id = ? AND tmp_id = ?
		  ORDER BY created_at_ms DESC LIMIT 1`, accountID, tmpID)
	o, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrOperationNotFound
	}
	return o, err
}

// SetOperationTmpID records the bare UUID sent as TmpID, before the library
// call. A retry within one request reuses the same tmp_id, so a retried send
// cannot produce two correlations (spec section 6.3).
func (s *Store) SetOperationTmpID(ctx context.Context, operationID, tmpID string) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE operations SET tmp_id = ?, updated_at_ms = ?
			 -- all-accounts: an op_ ID already carries its account (section 4.1).
			 WHERE id = ?`,
			tmpID, now, operationID)
		return err
	})
}

// Settlement is the outcome being recorded on an operation.
type Settlement struct {
	Status          OperationStatus
	MessageID       string
	ConversationID  string
	ErrorCode       string
	ErrorMessage    string
	ErrorRetryable  bool
	GoogleStatusRaw int32
}

// SettleOperation applies spec section 6.4's transition table.
//
// terminal_at_ms records when the operation *first* became terminal and is
// never cleared or rewritten: a wait that was satisfied by `unknown` is not
// retroactively unsatisfied. corrected_at_ms is set when a late fact moves it
// out of `unknown`.
func (s *Store) SettleOperation(ctx context.Context, operationID string, st Settlement) (Operation, error) {
	now := s.clock.Now().UnixMilli()
	var out Operation
	err := s.Write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+operationColumns+`
			 -- all-accounts: an op_ ID already carries its account (section 4.1).
			 FROM operations WHERE id = ?`, operationID)
		cur, err := scanOperation(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrOperationNotFound
		}
		if err != nil {
			return err
		}
		if !OperationTransitionAllowed(cur.Status, st.Status) {
			return fmt.Errorf("%w: %s -> %s", ErrOperationTransition, cur.Status, st.Status)
		}

		terminal := OperationTerminal(st.Status)
		terminalAt := any(nil)
		if cur.TerminalAtMS != 0 {
			terminalAt = cur.TerminalAtMS
		} else if terminal {
			terminalAt = now
		}
		correctedAt := any(nil)
		if cur.CorrectedAtMS != 0 {
			correctedAt = cur.CorrectedAtMS
		} else if cur.Status == OpUnknown && st.Status != OpUnknown {
			correctedAt = now
		}

		_, err = tx.ExecContext(ctx, `
			-- all-accounts: an op_ ID already carries its account (section 4.1).
			UPDATE operations SET
			    status            = ?,
			    terminal          = ?,
			    terminal_at_ms    = ?,
			    corrected_at_ms   = ?,
			    message_id        = COALESCE(?, message_id),
			    conversation_id   = COALESCE(?, conversation_id),
			    error_code        = ?,
			    error_message     = ?,
			    error_retryable   = ?,
			    google_status_raw = COALESCE(?, google_status_raw),
			    updated_at_ms     = ?
			WHERE id = ?`,
			string(st.Status), terminal, terminalAt, correctedAt,
			nullString(st.MessageID), nullString(st.ConversationID),
			nullString(st.ErrorCode), nullString(st.ErrorMessage),
			nullErrorRetryable(st), nullInt(int64(st.GoogleStatusRaw)), now, operationID)
		if err != nil {
			return err
		}
		row = tx.QueryRowContext(ctx, `SELECT `+operationColumns+`
			 -- all-accounts: an op_ ID already carries its account (section 4.1).
			 FROM operations WHERE id = ?`, operationID)
		out, err = scanOperation(row)
		return err
	})
	return out, err
}

func nullErrorRetryable(st Settlement) any {
	if st.ErrorCode == "" {
		return nil
	}
	return st.ErrorRetryable
}

// RecoverRunningOperations is the crash sweep of spec section 6.6. It runs at
// startup, before the listener binds, and it is process-wide rather than
// per-account on purpose: a crash is a property of the process, so every
// account's in-flight operations are settled at once.
//
// It returns the rows it settled, each carrying its account_id, so the caller
// can write one operation.crash_recovered audit row per row. Nothing is
// retried: if the send did reach Google, the remote echo will arrive on
// reconnect and correct the operation to succeeded, which is exactly why the
// correction out of `unknown` exists.
//
// A `pending` operation is left alone here; the reaper settles it.
func (s *Store) RecoverRunningOperations(ctx context.Context) ([]Operation, error) {
	now := s.clock.Now().UnixMilli()
	var out []Operation
	err := s.Write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx,
			`SELECT `+operationColumns+`
			   -- all-accounts: crash recovery is process-wide, not per account (section 6.6).
			   FROM operations WHERE status = 'running'`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			o, err := scanOperation(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, o.ID)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			-- all-accounts: crash recovery is process-wide, not per account (section 6.6).
			UPDATE operations
			   SET status = 'unknown', terminal = 1,
			       terminal_at_ms = COALESCE(terminal_at_ms, ?),
			       error_code = ?, error_retryable = 0, updated_at_ms = ?
			 WHERE status = 'running'`, now, ErrCrashRecovered, now); err != nil {
			return err
		}
		for _, id := range ids {
			o, err := scanOperation(tx.QueryRowContext(ctx,
				`SELECT `+operationColumns+`
				 -- all-accounts: crash recovery is process-wide (section 6.6).
				 FROM operations WHERE id = ?`, id))
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// ReapPendingOperations settles every `pending` operation older than the
// timeout, measured from created_at_ms, to `unknown`. Like crash recovery it
// is process-wide (spec section 6.6).
func (s *Store) ReapPendingOperations(ctx context.Context, timeoutMS int64) ([]Operation, error) {
	now := s.clock.Now().UnixMilli()
	cutoff := now - timeoutMS
	var out []Operation
	err := s.Write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx,
			`SELECT `+operationColumns+`
			   -- all-accounts: the pending_timeout reaper is process-wide (section 6.6).
			   FROM operations WHERE status = 'pending' AND created_at_ms <= ?`, cutoff)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			o, err := scanOperation(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, o.ID)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				-- all-accounts: the pending_timeout reaper is process-wide (section 6.6).
				UPDATE operations
				   SET status = 'unknown', terminal = 1,
				       terminal_at_ms = COALESCE(terminal_at_ms, ?),
				       error_code = ?, error_retryable = 0, updated_at_ms = ?
				 WHERE id = ?`, now, ErrPendingTimeout, now, id); err != nil {
				return err
			}
			o, err := scanOperation(tx.QueryRowContext(ctx,
				`SELECT `+operationColumns+`
				 -- all-accounts: the pending_timeout reaper is process-wide (section 6.6).
				 FROM operations WHERE id = ?`, id))
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// SweepIdempotencyKeys deletes operation rows past the retention window. It
// never deletes a row that is not terminal (spec section 6.6).
func (s *Store) SweepIdempotencyKeys(ctx context.Context, ttlMS int64) (int64, error) {
	cutoff := s.clock.Now().UnixMilli() - ttlMS
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM operations
			   -- all-accounts: the idempotency sweep is process-wide (section 6.6).
			   WHERE terminal = 1 AND created_at_ms <= ?`, cutoff)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// OperationQuery is the parameter set of GET /v1/operations. A caller sees
// only its own operations, keyed on the authorization that created them
// (spec section 6.5), so AuthorizationID is the caller's scope, not a filter
// the caller chooses.
type OperationQuery struct {
	AuthorizationID string
	AllAccounts     bool
	AccountID       string
	Kind            string
	Status          OperationStatus
	Terminal        *bool
	AfterMS         int64
	BeforeMS        int64
	Cursor          *Cursor
	Limit           int
}

func (q OperationQuery) sql() (string, []any) {
	var b builder
	if q.AuthorizationID != "" {
		b.and("o.authorization_id = ?", q.AuthorizationID)
	}
	if q.AccountID != "" {
		b.and("o.account_id = ?", q.AccountID)
	}
	if q.Kind != "" {
		b.and("o.kind = ?", q.Kind)
	}
	if q.Status != "" {
		b.and("o.status = ?", string(q.Status))
	}
	if q.Terminal != nil {
		b.and("o.terminal = ?", *q.Terminal)
	}
	if q.AfterMS > 0 {
		b.and("o.created_at_ms >= ?", q.AfterMS)
	}
	if q.BeforeMS > 0 {
		b.and("o.created_at_ms <= ?", q.BeforeMS)
	}
	b.after("o.created_at_ms", "o.id", q.Cursor)
	sql := `SELECT ` + prefixed(operationColumns, "o") + `
	  -- all-accounts: section 7.3 makes omitting account_id the default for
	  -- reads; an operations listing is scoped by authorization, not account.
	  FROM operations o
	 WHERE 1=1` + b.String() + `
	 ORDER BY o.created_at_ms DESC, o.id DESC
	 LIMIT ?`
	return sql, append(b.args, clampLimit(q.Limit))
}

// ListOperations serves GET /v1/operations in both forms.
func (s *Store) ListOperations(ctx context.Context, q OperationQuery) ([]Operation, error) {
	if q.AccountID == "" && !q.AllAccounts {
		return nil, ErrNoAccountPredicate
	}
	sql, args := q.sql()
	rows, err := s.read.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		o, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CursorPosition is the operation's position in the newest-first order.
func (o Operation) CursorPosition() Cursor { return Cursor{SentAtMS: o.CreatedAtMS, ID: o.ID} }
