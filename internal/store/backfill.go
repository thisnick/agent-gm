package store

import (
	"context"
	"database/sql"
	"errors"
)

// BackfillState is one conversation's backfill progress (spec section 5.2).
// It carries account_id as well as conversation_id: the cursor is Google's
// own and is only meaningful within the account that issued it.
type BackfillState struct {
	ConversationID string
	AccountID      string
	CursorItemID   string
	// CursorTimestampUS is gmproto.Cursor.lastItemTimestamp, in Google's own
	// microseconds, carried back verbatim. It is the one place a microsecond
	// value is stored, because it is an opaque token rather than a timestamp
	// Agent GM ever reads (spec section 4.4).
	CursorTimestampUS int64
	OldestSeenMS      int64
	MessagesDone      int64
	Complete          bool
	UpdatedAtMS       int64
}

// ErrBackfillStateNotFound is returned when a conversation has no row.
var ErrBackfillStateNotFound = errors.New("backfill state not found")

// SetBackfillState writes one conversation's progress.
func (s *Store) SetBackfillState(ctx context.Context, b BackfillState) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO backfill_state (conversation_id, account_id, cursor_item_id,
			    cursor_ts_us, oldest_seen_ms, messages_done, complete, updated_at_ms)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(conversation_id) DO UPDATE SET
			    cursor_item_id = excluded.cursor_item_id,
			    cursor_ts_us   = excluded.cursor_ts_us,
			    oldest_seen_ms = excluded.oldest_seen_ms,
			    messages_done  = excluded.messages_done,
			    complete       = excluded.complete,
			    updated_at_ms  = excluded.updated_at_ms`,
			b.ConversationID, b.AccountID, nullString(b.CursorItemID),
			nullInt(b.CursorTimestampUS), nullInt(b.OldestSeenMS),
			b.MessagesDone, b.Complete, now)
		return err
	})
}

// BackfillState reads one conversation's progress.
func (s *Store) BackfillState(ctx context.Context, conversationID string) (BackfillState, error) {
	var b BackfillState
	var item sql.NullString
	var ts, oldest sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT conversation_id, account_id, cursor_item_id, cursor_ts_us,
		        oldest_seen_ms, messages_done, complete, updated_at_ms
		   -- all-accounts: scoped by conversation_id, which carries the account (section 4.1).
		   FROM backfill_state WHERE conversation_id = ?`, conversationID).
		Scan(&b.ConversationID, &b.AccountID, &item, &ts, &oldest,
			&b.MessagesDone, &b.Complete, &b.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrBackfillStateNotFound
	}
	if err != nil {
		return b, err
	}
	b.CursorItemID = item.String
	b.CursorTimestampUS = ts.Int64
	b.OldestSeenMS = oldest.Int64
	return b, nil
}

// BackfillProgress counts one account's conversations and how many of them
// have finished backfilling, for the per-account `backfill` block of
// GET /v1/health (spec section 7.5).
func (s *Store) BackfillProgress(ctx context.Context, accountID string) (done, total int64, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(complete), 0), COUNT(*)
		   FROM backfill_state WHERE account_id = ?`, accountID).Scan(&done, &total)
	return done, total, err
}
