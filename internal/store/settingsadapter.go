package store

import (
	"context"
	"errors"
)

// SettingsStore adapts the `settings` table to the persistence interface
// `internal/settings` declares.
//
// The adapter exists because `internal/settings` deliberately does not import
// `internal/store`: the defaults, the bounds, the scope column and the
// validation of spec section 15.1 are a table of facts about the
// specification, not about SQLite, and a package that owned both would make
// the registry impossible to test without a database. So the registry
// declares a three-method interface and this is the one implementation of it
// that talks to a real database.
type SettingsStore struct{ store *Store }

// NewSettingsStore adapts a store.
func NewSettingsStore(s *Store) *SettingsStore { return &SettingsStore{store: s} }

// Get returns the stored JSON for a key. ok is false when no row exists,
// which is how the registry tells "the operator set this" from "the default
// applies" -- and therefore how GET /v1/admin/settings reports `source`.
func (a *SettingsStore) Get(ctx context.Context, key string) (string, bool, error) {
	row, err := a.store.Setting(ctx, key)
	if errors.Is(err, ErrSettingNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.ValueJSON, true, nil
}

// Put writes the stored JSON for a key.
func (a *SettingsStore) Put(ctx context.Context, key, valueJSON string) error {
	return a.store.SetSetting(ctx, key, valueJSON)
}

// All returns every stored row.
func (a *SettingsStore) All(ctx context.Context) (map[string]string, error) {
	rows, err := a.store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.ValueJSON
	}
	return out, nil
}
