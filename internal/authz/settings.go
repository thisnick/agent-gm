package authz

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The three TTL settings this package reads, with the defaults of spec section
// 15.1 and the bounds of section 9.6.
//
// Admin sessions expire on the same schedule as OAuth ones. The credential
// that outranks everything else does not get a longer life than the ones it
// outranks (spec section 9.6).
const (
	SettingAdminAccessTokenTTL         = "admin.access_token_ttl"
	SettingAdminRefreshTokenIdleTTL    = "admin.refresh_token_idle_ttl"
	SettingAdminRefreshTokenAbsoluteTL = "admin.refresh_token_absolute_ttl"

	// The OAuth grant's own three, with the same defaults and the same
	// bounds. They are separate keys because an owner may want a connector's
	// session to outlive (or not outlive) their own admin session, and one
	// key for both would make that choice impossible to express.
	SettingOAuthAccessTokenTTL          = "oauth.access_token_ttl"
	SettingOAuthRefreshTokenIdleTTL     = "oauth.refresh_token_idle_ttl"
	SettingOAuthRefreshTokenAbsoluteTTL = "oauth.refresh_token_absolute_ttl"

	// The three lifetimes of the authorization flow itself (section 9.6).
	SettingOAuthAuthorizationCodeTTL    = "oauth.authorization_code_ttl"
	SettingOAuthAuthorizationRequestTTL = "oauth.authorization_request_ttl"
	SettingOAuthEnrollmentDefaultTTL    = "oauth.enrollment_default_ttl"
)

type ttlBound struct {
	def, min, max time.Duration
}

var ttlBounds = map[string]ttlBound{
	SettingAdminAccessTokenTTL:         {def: 15 * time.Minute, min: 5 * time.Minute, max: time.Hour},
	SettingAdminRefreshTokenIdleTTL:    {def: 30 * 24 * time.Hour, min: 24 * time.Hour, max: 90 * 24 * time.Hour},
	SettingAdminRefreshTokenAbsoluteTL: {def: 90 * 24 * time.Hour, min: 7 * 24 * time.Hour, max: 365 * 24 * time.Hour},

	SettingOAuthAccessTokenTTL:          {def: 15 * time.Minute, min: 5 * time.Minute, max: time.Hour},
	SettingOAuthRefreshTokenIdleTTL:     {def: 30 * 24 * time.Hour, min: 24 * time.Hour, max: 90 * 24 * time.Hour},
	SettingOAuthRefreshTokenAbsoluteTTL: {def: 90 * 24 * time.Hour, min: 7 * 24 * time.Hour, max: 365 * 24 * time.Hour},

	SettingOAuthAuthorizationCodeTTL:    {def: 2 * time.Minute, min: 30 * time.Second, max: 5 * time.Minute},
	SettingOAuthAuthorizationRequestTTL: {def: 15 * time.Minute, min: time.Minute, max: time.Hour},
	SettingOAuthEnrollmentDefaultTTL:    {def: 15 * time.Minute, min: time.Minute, max: 24 * time.Hour},
}

// Settings is the minimal view of the runtime settings table this package
// needs (spec section 15.1). It is an interface, and a small one, so that
// `internal/settings` supplies it without this package depending on the shape
// of the settings subsystem, and so that a test can move a TTL without a
// database.
type Settings interface {
	// Duration returns the effective value of one duration setting. An
	// unknown key is an error, never a zero duration: a TTL that silently
	// became zero would expire every credential the instant it was minted.
	Duration(ctx context.Context, key string) (time.Duration, error)
}

// MemorySettings is the in-memory implementation, seeded with the defaults of
// spec section 15.1.
type MemorySettings struct {
	mu sync.RWMutex
	v  map[string]time.Duration
}

// NewMemorySettings returns the documented defaults: 15m, 30d, 90d.
func NewMemorySettings() *MemorySettings {
	m := &MemorySettings{v: make(map[string]time.Duration, len(ttlBounds))}
	for k, b := range ttlBounds {
		m.v[k] = b.def
	}
	return m
}

// Set overrides one setting, refusing a value outside spec section 9.6's
// bounds. Bounds are enforced here rather than at read time so a bad value is
// refused when it is written, not silently clamped when it is used.
func (m *MemorySettings) Set(key string, d time.Duration) error {
	b, ok := ttlBounds[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	if d < b.min || d > b.max {
		return fmt.Errorf("%s must be between %s and %s, got %s", key, b.min, b.max, d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.v[key] = d
	return nil
}

// Duration implements Settings.
func (m *MemorySettings) Duration(_ context.Context, key string) (time.Duration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.v[key]
	if !ok {
		return 0, fmt.Errorf("unknown setting %q", key)
	}
	return d, nil
}

var _ Settings = (*MemorySettings)(nil)
