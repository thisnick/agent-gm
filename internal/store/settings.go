package store

import (
	"context"
	"database/sql"
	"errors"
)

// The settings table is persistence and nothing else. The defaults, the
// bounds and the validation of spec section 15.1 live in the configuration
// package: a value that has never been set has no row here, so "unset" and
// "set to the default" stay distinguishable and a later change of default
// reaches a deployment that never overrode it.

// ErrSettingNotFound is returned when a key has no row.
var ErrSettingNotFound = errors.New("setting not found")

// Setting is one stored override.
type Setting struct {
	Key         string
	ValueJSON   string
	UpdatedAtMS int64
}

// SetSetting writes one key's JSON value.
func (s *Store) SetSetting(ctx context.Context, key, valueJSON string) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value_json, updated_at_ms) VALUES (?,?,?)
			ON CONFLICT(key) DO UPDATE SET
			    value_json    = excluded.value_json,
			    updated_at_ms = excluded.updated_at_ms`, key, valueJSON, now)
		return err
	})
}

// Setting reads one key.
func (s *Store) Setting(ctx context.Context, key string) (Setting, error) {
	var out Setting
	err := s.read.QueryRowContext(ctx,
		`SELECT key, value_json, updated_at_ms FROM settings WHERE key = ?`, key).
		Scan(&out.Key, &out.ValueJSON, &out.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrSettingNotFound
	}
	return out, err
}

// Settings lists every stored override, in key order.
func (s *Store) Settings(ctx context.Context) ([]Setting, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT key, value_json, updated_at_ms FROM settings ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Setting
	for rows.Next() {
		var st Setting
		if err := rows.Scan(&st.Key, &st.ValueJSON, &st.UpdatedAtMS); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// DeleteSetting removes one override, returning that key to its compiled
// default.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	})
}
