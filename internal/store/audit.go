package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// audit_events is append-only. There is no update and no delete in this file,
// and there never will be one: an audit row records what happened at the time
// it happened, and a migration does not rewrite it either (spec sections 4.3,
// 12.4). Account removal deletes the account's rows and leaves its audit
// trail standing, which is how "what happened to this account" stays
// answerable afterwards.

// AuditResult is the outcome vocabulary.
const (
	AuditOK      = "ok"
	AuditRefused = "refused"
	AuditFailed  = "failed"
)

// AuditEvent is one recorded fact.
//
// Payloads carry IDs, counts, codes and outcomes -- never message text, never
// a full phone number, never a token, never a cookie, never the data key.
// Redaction happens before the row reaches this package.
type AuditEvent struct {
	ID              string
	Kind            string
	AccountID       string
	AuthorizationID string
	TargetType      string
	TargetID        string
	Result          string
	Source          string
	PayloadJSON     string
	CreatedAtMS     int64
}

const auditColumns = `id, kind, account_id, authorization_id, target_type, target_id,
	result, source, payload_json, created_at_ms`

func scanAuditEvent(sc interface{ Scan(...any) error }) (AuditEvent, error) {
	var e AuditEvent
	var account, auth, targetType, targetID, source sql.NullString
	err := sc.Scan(&e.ID, &e.Kind, &account, &auth, &targetType, &targetID,
		&e.Result, &source, &e.PayloadJSON, &e.CreatedAtMS)
	if err != nil {
		return e, err
	}
	e.AccountID = account.String
	e.AuthorizationID = auth.String
	e.TargetType = targetType.String
	e.TargetID = targetID.String
	e.Source = source.String
	return e, nil
}

// AppendAudit writes one audit row. Every row carries account_id where the
// event belongs to an account and NULL where it does not, such as an OAuth or
// settings event (spec section 12.4).
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	if e.ID == "" {
		e.ID = AuditID()
	}
	if e.Result == "" {
		e.Result = AuditOK
	}
	if e.PayloadJSON == "" {
		e.PayloadJSON = "{}"
	}
	e.CreatedAtMS = s.clock.Now().UnixMilli()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO audit_events (`+auditColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.Kind, nullString(e.AccountID), nullString(e.AuthorizationID),
			nullString(e.TargetType), nullString(e.TargetID), e.Result,
			nullString(e.Source), e.PayloadJSON, e.CreatedAtMS)
		return err
	})
	return e, err
}

// AppendAuditOnce appends one audit row unless a row of the same kind about
// the same target is already there, and reports whether this call was the one
// that wrote it.
//
// It exists for the two `*.expired` kinds of section 12.4. Expiry is derived
// at read time rather than swept (see internal/oauth's StatusExpired), so the
// moment it becomes true is the first read that observes it -- and every
// later read observes it again. Without a condition, "audit the expiry" would
// mean one row per poll of the waiting page.
//
// This is still append-only: the guard is a WHERE NOT EXISTS on the INSERT,
// not an UPDATE. Nothing here rewrites a row, and the whole statement runs on
// the single writer, so two concurrent observers cannot both win.
//
// TargetType and TargetID are the identity of the thing that expired and are
// required: they are what makes "already recorded" a question with an answer.
func (s *Store) AppendAuditOnce(ctx context.Context, e AuditEvent) (bool, error) {
	if e.TargetType == "" || e.TargetID == "" {
		return false, errors.New("store: AppendAuditOnce needs a target_type and a target_id")
	}
	if e.ID == "" {
		e.ID = AuditID()
	}
	if e.Result == "" {
		e.Result = AuditOK
	}
	if e.PayloadJSON == "" {
		e.PayloadJSON = "{}"
	}
	e.CreatedAtMS = s.clock.Now().UnixMilli()
	var wrote bool
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO audit_events (`+auditColumns+`)
			SELECT ?,?,?,?,?,?,?,?,?,?
			 WHERE NOT EXISTS (
			     SELECT 1 FROM audit_events
			      WHERE kind = ? AND target_type = ? AND target_id = ?)`,
			e.ID, e.Kind, nullString(e.AccountID), nullString(e.AuthorizationID),
			e.TargetType, e.TargetID, e.Result,
			nullString(e.Source), e.PayloadJSON, e.CreatedAtMS,
			e.Kind, e.TargetType, e.TargetID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		wrote = n == 1
		return err
	})
	return wrote, err
}

// AuditQuery is the filter set of GET /v1/admin/audit and
// `agm admin audit list` (spec section 12.4).
type AuditQuery struct {
	Kind            string
	KindPrefix      string
	AccountID       string
	AuthorizationID string
	AfterMS         int64
	BeforeMS        int64
	Cursor          *Cursor
	Limit           int
}

func (q AuditQuery) sql() (string, []any) {
	var b builder
	if q.Kind != "" {
		b.and("e.kind = ?", q.Kind)
	}
	if q.KindPrefix != "" {
		b.and(`e.kind LIKE ? ESCAPE '\'`, escapeLike(q.KindPrefix)+"%")
	}
	if q.AccountID != "" {
		b.and("e.account_id = ?", q.AccountID)
	}
	if q.AuthorizationID != "" {
		b.and("e.authorization_id = ?", q.AuthorizationID)
	}
	if q.AfterMS > 0 {
		b.and("e.created_at_ms >= ?", q.AfterMS)
	}
	if q.BeforeMS > 0 {
		b.and("e.created_at_ms <= ?", q.BeforeMS)
	}
	b.after("e.created_at_ms", "e.id", q.Cursor)
	sqlText := `SELECT ` + prefixed(auditColumns, "e") + `
	  -- all-accounts: an audit row survives the account it names, and the
	  -- server-wide events carry no account at all (section 12.4).
	  FROM audit_events e
	 WHERE 1=1` + b.String() + `
	 ORDER BY e.created_at_ms DESC, e.id DESC
	 LIMIT ?`
	return sqlText, append(b.args, clampLimit(q.Limit))
}

// ListAuditEvents serves the audit listing.
func (s *Store) ListAuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, error) {
	sqlText, args := q.sql()
	rows, err := s.read.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CursorPosition is the audit row's position in the newest-first order.
func (e AuditEvent) CursorPosition() Cursor { return Cursor{SentAtMS: e.CreatedAtMS, ID: e.ID} }

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
