package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Store is the persistence this package needs and nothing more. The `settings`
// table of section 4.2 is `(key, value_json, updated_at_ms)`; the store
// implements this interface against it, and this package never imports
// internal/store.
type Store interface {
	// Get returns the stored JSON for a key. ok is false when no row exists.
	Get(ctx context.Context, key string) (valueJSON string, ok bool, err error)
	// Put writes the stored JSON for a key, inserting or replacing.
	Put(ctx context.Context, key, valueJSON string) error
	// All returns every stored row, keyed by setting key.
	All(ctx context.Context) (map[string]string, error)
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory { return &Memory{m: map[string]string{}} }

func (s *Memory) Get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *Memory) Put(_ context.Context, key, valueJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = valueJSON
	return nil
}

func (s *Memory) All(_ context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

// Source is where an effective value came from (section 15.1).
type Source string

const (
	// SourceDefault is the value declared in the registry.
	SourceDefault Source = "default"
	// SourceEnvironment is an environment variable.
	SourceEnvironment Source = "environment"
	// SourceDatabase is a row in the `settings` table.
	SourceDatabase Source = "database"
)

// Effective is one row of `GET /v1/admin/settings`: the value in force, where
// it came from, and what it would take to change it.
type Effective struct {
	Key             string `json:"key"`
	Value           Value  `json:"value"`
	Source          Source `json:"source"`
	Default         Value  `json:"default"`
	Bounds          string `json:"bounds"`
	Scope           Scope  `json:"scope"`
	Type            Kind   `json:"type"`
	Mutable         bool   `json:"mutable"`
	RequiresRestart bool   `json:"requires_restart"`
	Note            string `json:"note,omitempty"`
}

// Error is a settings refusal. Every one of them names the key, so an
// operator reading a rejected PATCH knows which of the keys they sent was the
// problem.
type Error struct {
	Key string
	Msg string
}

func (e *Error) Error() string { return e.Msg }

// Code is the section 7.2 error code every settings refusal maps to.
func (e *Error) Code() string { return "invalid_request" }

func errf(key, format string, args ...any) *Error {
	return &Error{Key: key, Msg: fmt.Sprintf(format, args...)}
}

// Settings resolves effective values over a Store and validates writes.
//
// Precedence is database, then environment, then default: a value an operator
// set through the admin API outranks the environment it was started with,
// which outranks the declaration.
type Settings struct {
	reg   *Registry
	store Store
	env   func(string) string
}

// New builds a Settings over a registry, a store, and an environment lookup.
// env may be nil, in which case no key has an environment source.
func New(reg *Registry, store Store, env func(string) string) *Settings {
	if env == nil {
		env = func(string) string { return "" }
	}
	return &Settings{reg: reg, store: store, env: env}
}

// Registry returns the registry this Settings resolves against.
func (s *Settings) Registry() *Registry { return s.reg }

// resolve computes one effective value given the database row (if any).
func (s *Settings) resolve(d Def, dbJSON string, dbOK bool) (Effective, error) {
	eff := Effective{
		Key:             d.Key,
		Value:           d.Default,
		Source:          SourceDefault,
		Default:         d.Default,
		Bounds:          d.BoundsString(),
		Scope:           d.Scope,
		Type:            d.Kind,
		Mutable:         d.Mutable,
		RequiresRestart: d.RequiresRestart,
		Note:            d.Note,
	}
	if d.Env != "" {
		if raw := s.env(d.Env); raw != "" {
			v, err := parseEnv(d, raw)
			if err != nil {
				return eff, errf(d.Key, "%s=%q is not a valid %s: %s", d.Env, raw, d.Key, err)
			}
			if err := validateAgainstBounds(d, v); err != nil {
				return eff, err
			}
			eff.Value, eff.Source = v, SourceEnvironment
		}
	}
	if dbOK {
		v, err := parseValue(d.Kind, []byte(dbJSON))
		if err != nil {
			return eff, errf(d.Key, "stored value for %q is not a valid %s: %s", d.Key, d.Kind, err)
		}
		if err := validateAgainstBounds(d, v); err != nil {
			return eff, err
		}
		eff.Value, eff.Source = v, SourceDatabase
	}
	return eff, nil
}

// parseEnv reads an environment value, which is a bare string rather than
// JSON: AGENT_GM_LOG_LEVEL is `warn`, not `"warn"`.
func parseEnv(d Def, raw string) (Value, error) {
	switch d.Kind {
	case KindEnum:
		return Enum(raw), nil
	case KindBool, KindInt:
		return parseValue(d.Kind, []byte(raw))
	case KindDuration:
		dur, err := ParseDuration(raw)
		if err != nil {
			return Value{}, err
		}
		return Duration(dur), nil
	}
	return Value{}, fmt.Errorf("unknown setting type %q", d.Kind)
}

// Get returns one effective value.
func (s *Settings) Get(ctx context.Context, key string) (Effective, error) {
	d, ok := s.reg.Def(key)
	if !ok {
		return Effective{}, s.unknown(key)
	}
	raw, found, err := s.store.Get(ctx, key)
	if err != nil {
		return Effective{}, err
	}
	return s.resolve(d, raw, found)
}

// All returns every effective value, sorted by key. This is the body of
// `GET /v1/admin/settings`.
func (s *Settings) All(ctx context.Context) ([]Effective, error) {
	rows, err := s.store.All(ctx)
	if err != nil {
		return nil, err
	}
	keys := s.reg.Keys()
	out := make([]Effective, 0, len(keys))
	for _, k := range keys {
		d, _ := s.reg.Def(k)
		raw, found := rows[k]
		eff, err := s.resolve(d, raw, found)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	return out, nil
}

// unknown builds the refusal for a key that is not declared. A retired key
// lands here too, and names its replacement: setting one must never silently
// write a key that nothing reads (section 15.1).
func (s *Settings) unknown(key string) error {
	if replacement, retired := s.reg.Replacement(key); retired {
		if replacement == "" {
			return errf(key, "setting %q was retired and has no replacement", key)
		}
		return errf(key, "setting %q was retired; use %q instead", key, replacement)
	}
	return errf(key, "unknown setting %q", key)
}

// validateAgainstBounds checks one value against its declaration. The message
// names the key, its bounds and the value given, in that order, because an
// operator who mistyped a bound needs all three to fix it.
func validateAgainstBounds(d Def, v Value) error {
	if v.Kind != d.Kind {
		return errf(d.Key, "setting %q is a %s; got a %s", d.Key, d.Kind, v.Kind)
	}
	if d.Kind == KindEnum {
		for _, allowed := range d.EnumValues {
			if v.Str == allowed {
				return nil
			}
		}
		return errf(d.Key, "setting %q must be one of %s; got %q",
			d.Key, quoteList(d.EnumValues), v.Str)
	}
	if d.Min != nil && v.compare(*d.Min) < 0 {
		return errf(d.Key, "setting %q is out of bounds: bounds are %s; got %s",
			d.Key, d.BoundsString(), v.String())
	}
	if d.Max != nil && v.compare(*d.Max) > 0 {
		return errf(d.Key, "setting %q is out of bounds: bounds are %s; got %s",
			d.Key, d.BoundsString(), v.String())
	}
	return nil
}

// Change is one accepted key in a PATCH.
type Change struct {
	Key  string
	From Value
	To   Value
	// RequiresRestart repeats the declaration, so the response can tell an
	// operator which of the keys they just changed is not yet in force.
	RequiresRestart bool
}

// Patch validates the whole body and then writes it.
//
// Section 7.7: any invalid key rejects the request and changes nothing.
// Validation therefore runs over every key first and returns on the first
// refusal, before a single Put; a body of ten good keys and one bad one
// leaves the database exactly as it was.
func (s *Settings) Patch(ctx context.Context, body map[string]json.RawMessage) ([]Change, error) {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	changes := make([]Change, 0, len(keys))
	for _, k := range keys {
		d, ok := s.reg.Def(k)
		if !ok {
			return nil, s.unknown(k)
		}
		if !d.Mutable {
			return nil, errf(k, "setting %q is not mutable at runtime", k)
		}
		v, err := parseValue(d.Kind, body[k])
		if err != nil {
			return nil, errf(k, "setting %q: %s", k, err)
		}
		if err := validateAgainstBounds(d, v); err != nil {
			return nil, err
		}
		cur, err := s.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		if d.DownwardOnly && v.compare(cur.Value) > 0 {
			return nil, errf(k, "setting %q is mutable downward only: the effective value is %s; got %s",
				k, cur.Value.String(), v.String())
		}
		changes = append(changes, Change{Key: k, From: cur.Value, To: v, RequiresRestart: d.RequiresRestart})
	}

	// Nothing was written above. Only now, with the whole body known good,
	// does anything reach the store.
	for _, c := range changes {
		if err := s.store.Put(ctx, c.Key, c.To.JSON()); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// AsError returns the *Error a settings failure carries, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
