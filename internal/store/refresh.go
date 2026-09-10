package store

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) LastRefresh(ctx context.Context, accountID, scope string) (int64, error) {
	var at int64
	err := s.read.QueryRowContext(ctx, `SELECT refreshed_at_ms FROM refresh_state WHERE account_id = ? AND scope = ?`, accountID, scope).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return at, err
}

func (s *Store) SetLastRefresh(ctx context.Context, accountID, scope string, since, at int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO refresh_state (account_id, scope, refreshed_at_ms, since_at_ms) VALUES (?,?,?,?)
 ON CONFLICT(account_id, scope) DO UPDATE SET refreshed_at_ms = MAX(refresh_state.refreshed_at_ms, excluded.refreshed_at_ms), since_at_ms = MAX(refresh_state.since_at_ms, excluded.since_at_ms)`, accountID, scope, at, since)
		return err
	})
}

func (s *Store) RefreshSince(ctx context.Context, accountID, scope string) (int64, error) {
	var at int64
	err := s.read.QueryRowContext(ctx, `SELECT since_at_ms FROM refresh_state WHERE account_id = ? AND scope = ?`, accountID, scope).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return at, err
}
