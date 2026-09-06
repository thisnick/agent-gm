package settings

import (
	"fmt"
	"sort"
	"time"
)

// Scope says whether a value is one server-wide budget or a server-wide value
// applied to each account independently (spec section 15.1).
//
// This is the field most worth getting right. `ScopePerAccount` on
// `backfill.concurrency` means the worst case is that value multiplied by
// `accounts.max_concurrent`; reading it as server-wide is how a two-account
// deployment does half the work it was configured for, and reading a
// server-wide key as per-account is how it does twice.
type Scope string

const (
	// ScopeServer is one budget for the whole server, however many accounts
	// it holds.
	ScopeServer Scope = "server"
	// ScopePerAccount is a server-wide value applied to each account
	// independently: the cost multiplies by account count.
	ScopePerAccount Scope = "per account"
)

// Def is the declaration of one runtime setting.
type Def struct {
	// Key is the wire name, as it appears in `PATCH /v1/admin/settings`.
	Key string
	// Kind is the type.
	Kind Kind
	// Default is the value with no environment and no database row.
	Default Value
	// Min and Max are inclusive bounds. Both nil means unbounded.
	Min *Value
	Max *Value
	// EnumValues constrains a KindEnum setting.
	EnumValues []string
	// Scope is section 15.1's scope column.
	Scope Scope
	// Mutable says whether `PATCH /v1/admin/settings` may write the key at
	// all.
	Mutable bool
	// DownwardOnly says a new value may only be less than or equal to the
	// current effective one. `media.upload_max_bytes` is the one key with
	// this property: raising the ceiling at runtime would let a caller
	// re-open a limit an operator closed.
	DownwardOnly bool
	// RequiresRestart is honest rather than aspirational. Section 15.1
	// singles out `logging.level` as reloadable without restart; every other
	// key says a restart is required rather than claim a hot reload nothing
	// implements.
	RequiresRestart bool
	// Env is the environment variable that overrides the default, or "" if
	// the key has none. Only declared names are ever read, so no key gains
	// an environment override by accident.
	Env string
	// Note is the section 15.1 remark, carried so the admin surface can
	// explain the multiplication an operator is signing up for.
	Note string
}

// BoundsString renders the bounds the way an error message needs them.
func (d Def) BoundsString() string {
	switch {
	case d.DownwardOnly:
		return "mutable downward only"
	case d.Kind == KindEnum && len(d.EnumValues) > 0:
		return "one of " + quoteList(d.EnumValues)
	case d.Min != nil && d.Max != nil:
		return d.Min.String() + "–" + d.Max.String()
	case d.Min != nil:
		return "at least " + d.Min.String()
	case d.Max != nil:
		return "at most " + d.Max.String()
	}
	return "unbounded"
}

func quoteList(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ", "
		}
		out += `"` + s + `"`
	}
	return out
}

func iv(n int64) *Value         { v := Int64(n); return &v }
func dv(d time.Duration) *Value { v := Duration(d); return &v }

// Registry is the set of declared settings plus the retired-key table.
type Registry struct {
	defs    map[string]Def
	order   []string
	retired map[string]string
}

// Retired is the retired-key table of section 15.1. The value is the
// replacement key, or "" for a key with no replacement.
//
// It is empty today because nothing has been retired yet. The mechanism
// exists anyway, and is tested: a retired key must become an *unknown* key
// that answers with its replacement, never a key that is silently written and
// then read by nothing.
var Retired = map[string]string{}

// NewRegistry builds the registry of section 15.1's runtime table plus the OAuth and
// admin TTLs of section 9.6.
//
// The defaults, bounds and scopes here are the specification's, transcribed
// once. A table test in this package asserts them against the same literals
// read out of the spec, in both directions, so neither a new key nor a
// changed bound can drift without a named failure.
func NewRegistry() *Registry {
	r := &Registry{defs: map[string]Def{}, retired: map[string]string{}}
	for k, v := range Retired {
		r.retired[k] = v
	}
	add := func(d Def) {
		if _, dup := r.defs[d.Key]; dup {
			panic("settings: duplicate key " + d.Key)
		}
		if d.Kind == "" {
			d.Kind = d.Default.Kind
		}
		r.defs[d.Key] = d
		r.order = append(r.order, d.Key)
	}

	// --- backfill (section 15.1) ---------------------------------------
	add(Def{
		Key: "backfill.concurrency", Default: Int64(2), Min: iv(1), Max: iv(8),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
		Note: "worst case is this multiplied by accounts.max_concurrent",
	})
	add(Def{
		Key: "backfill.conversation_page_size", Default: Int64(100), Min: iv(10), Max: iv(500),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "backfill.message_page_size", Default: Int64(100), Min: iv(10), Max: iv(500),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "backfill.max_messages_per_conversation", Default: Int64(2000), Min: iv(100), Max: iv(100000),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "backfill.horizon", Default: Duration(365 * Day), Min: dv(7 * Day), Max: dv(3650 * Day),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
		Note: "cost multiplies by account count",
	})
	add(Def{
		Key: "backfill.include_archive", Default: Bool(true),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
	})

	// --- ingest ---------------------------------------------------------
	add(Def{
		Key: "ingest.sweep_interval", Default: Duration(15 * time.Minute),
		Min: dv(time.Minute), Max: dv(6 * time.Hour),
		Scope: ScopePerAccount, Mutable: true, RequiresRestart: true,
	})

	// --- accounts -------------------------------------------------------
	add(Def{
		Key: "accounts.max_concurrent", Default: Int64(8), Min: iv(1), Max: iv(32),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
		Note: "section 4.7",
	})

	// --- operations -----------------------------------------------------
	add(Def{
		Key: "operations.send_deadline", Default: Duration(300 * time.Second),
		Min: dv(60 * time.Second), Max: dv(600 * time.Second),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "operations.idempotency_ttl", Default: Duration(30 * Day),
		Min: dv(1 * Day), Max: dv(365 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "operations.pending_timeout", Default: Duration(24 * time.Hour),
		Min: dv(time.Hour), Max: dv(7 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "operations.wait_timeout", Default: Duration(60 * time.Second),
		Min: dv(5 * time.Second), Max: dv(10 * time.Minute),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})

	// --- media ----------------------------------------------------------
	add(Def{
		Key: "media.upload_max_bytes", Default: Int64(104857600),
		Scope: ScopeServer, Mutable: true, DownwardOnly: true, RequiresRestart: true,
		Note: "mutable downward only",
	})
	add(Def{
		Key: "media.cache_max_bytes", Default: Int64(2 * 1024 * 1024 * 1024),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
		Note: "the LRU is cross-account (section 10.3)",
	})
	add(Def{
		Key: "media.inline_mcp_image_max_bytes", Default: Int64(1024 * 1024),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})

	// --- admin and oauth token TTLs (section 9.6) ------------------------
	// Declared now; Slice 3 reads them.
	add(Def{
		Key: "admin.access_token_ttl", Default: Duration(15 * time.Minute),
		Min: dv(5 * time.Minute), Max: dv(time.Hour),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "admin.refresh_token_idle_ttl", Default: Duration(30 * Day),
		Min: dv(1 * Day), Max: dv(90 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "admin.refresh_token_absolute_ttl", Default: Duration(90 * Day),
		Min: dv(7 * Day), Max: dv(365 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.access_token_ttl", Default: Duration(15 * time.Minute),
		Min: dv(5 * time.Minute), Max: dv(time.Hour),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.refresh_token_idle_ttl", Default: Duration(30 * Day),
		Min: dv(1 * Day), Max: dv(90 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.refresh_token_absolute_ttl", Default: Duration(90 * Day),
		Min: dv(7 * Day), Max: dv(365 * Day),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.authorization_code_ttl", Default: Duration(2 * time.Minute),
		Min: dv(30 * time.Second), Max: dv(5 * time.Minute),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.authorization_request_ttl", Default: Duration(15 * time.Minute),
		Min: dv(time.Minute), Max: dv(time.Hour),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "oauth.enrollment_default_ttl", Default: Duration(15 * time.Minute),
		Min: dv(time.Minute), Max: dv(24 * time.Hour),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})

	// --- backup and logging ---------------------------------------------
	add(Def{
		Key: "backup.keep", Default: Int64(7), Min: iv(1), Max: iv(100),
		Scope: ScopeServer, Mutable: true, RequiresRestart: true,
	})
	add(Def{
		Key: "logging.level", Kind: KindEnum, Default: Enum("info"),
		EnumValues: []string{"debug", "info", "warn", "error"},
		Scope:      ScopeServer, Mutable: true, RequiresRestart: false,
		Env:  "AGENT_GM_LOG_LEVEL",
		Note: "reloadable without restart",
	})

	sort.Strings(r.order)
	return r
}

// Def returns the declaration for a key.
func (r *Registry) Def(key string) (Def, bool) {
	d, ok := r.defs[key]
	return d, ok
}

// Keys returns every declared key, sorted.
func (r *Registry) Keys() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Retire records a retired key and its replacement. A retired key is an
// unknown key: it can never be written again, and the refusal names the
// replacement.
func (r *Registry) Retire(key, replacement string) {
	if _, live := r.defs[key]; live {
		panic(fmt.Sprintf("settings: %q is a live key and cannot be retired while declared", key))
	}
	r.retired[key] = replacement
}

// Replacement reports the replacement for a retired key.
func (r *Registry) Replacement(key string) (string, bool) {
	v, ok := r.retired[key]
	return v, ok
}
