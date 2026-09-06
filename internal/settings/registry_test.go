package settings

import (
	"sort"
	"testing"
	"time"
)

// specRow is one row of spec section 15.1's runtime table (and of section
// 9.6's TTL table), transcribed as literals.
//
// The point of transcribing rather than deriving is that this test fails in
// **both** directions: a key added to the registry without a row here fails,
// a row here without a registry entry fails, and a default, bound or scope
// that drifts on either side fails naming the key. There is no way to change
// a bound in registry.go and have the suite stay green.
type specRow struct {
	key      string
	def      Value
	min, max *Value // the spec's bounds as values, nil where the spec gives none
	scope    Scope
	restart  bool
	// downwardOnly is section 15.1's "mutable downward only" bounds cell.
	downwardOnly bool
}

func specTable() []specRow {
	return []specRow{
		// section 15.1, runtime settings
		{"backfill.concurrency", Int64(2), iv(1), iv(8), ScopePerAccount, true, false},
		{"backfill.conversation_page_size", Int64(100), iv(10), iv(500), ScopePerAccount, true, false},
		{"backfill.message_page_size", Int64(100), iv(10), iv(500), ScopePerAccount, true, false},
		{"backfill.max_messages_per_conversation", Int64(2000), iv(100), iv(100000), ScopePerAccount, true, false},
		{"backfill.horizon", Duration(365 * Day), dv(7 * Day), dv(3650 * Day), ScopePerAccount, true, false},
		{"backfill.include_archive", Bool(true), nil, nil, ScopePerAccount, true, false},
		{"ingest.sweep_interval", Duration(15 * time.Minute), dv(1 * time.Minute), dv(6 * time.Hour), ScopePerAccount, true, false},
		{"accounts.max_concurrent", Int64(8), iv(1), iv(32), ScopeServer, true, false},
		{"operations.send_deadline", Duration(300 * time.Second), dv(60 * time.Second), dv(600 * time.Second), ScopeServer, true, false},
		{"operations.idempotency_ttl", Duration(30 * Day), dv(1 * Day), dv(365 * Day), ScopeServer, true, false},
		{"operations.pending_timeout", Duration(24 * time.Hour), dv(1 * time.Hour), dv(7 * Day), ScopeServer, true, false},
		{"operations.wait_timeout", Duration(60 * time.Second), dv(5 * time.Second), dv(10 * time.Minute), ScopeServer, true, false},
		{"media.upload_max_bytes", Int64(104857600), nil, nil, ScopeServer, true, true},
		{"media.cache_max_bytes", Int64(2 * 1024 * 1024 * 1024), nil, nil, ScopeServer, true, false},
		{"media.inline_mcp_image_max_bytes", Int64(1048576), nil, nil, ScopeServer, true, false},
		{"backup.keep", Int64(7), iv(1), iv(100), ScopeServer, true, false},
		// section 15.1: reloadable without restart, and the only such key.
		{"logging.level", Enum("info"), nil, nil, ScopeServer, false, false},
		// section 9.6, declared now and read in Slice 3
		{"admin.access_token_ttl", Duration(15 * time.Minute), dv(5 * time.Minute), dv(1 * time.Hour), ScopeServer, true, false},
		{"admin.refresh_token_idle_ttl", Duration(30 * Day), dv(1 * Day), dv(90 * Day), ScopeServer, true, false},
		{"admin.refresh_token_absolute_ttl", Duration(90 * Day), dv(7 * Day), dv(365 * Day), ScopeServer, true, false},
		{"oauth.access_token_ttl", Duration(15 * time.Minute), dv(5 * time.Minute), dv(1 * time.Hour), ScopeServer, true, false},
		{"oauth.refresh_token_idle_ttl", Duration(30 * Day), dv(1 * Day), dv(90 * Day), ScopeServer, true, false},
		{"oauth.refresh_token_absolute_ttl", Duration(90 * Day), dv(7 * Day), dv(365 * Day), ScopeServer, true, false},
		{"oauth.authorization_code_ttl", Duration(2 * time.Minute), dv(30 * time.Second), dv(5 * time.Minute), ScopeServer, true, false},
		{"oauth.authorization_request_ttl", Duration(15 * time.Minute), dv(1 * time.Minute), dv(1 * time.Hour), ScopeServer, true, false},
		{"oauth.enrollment_default_ttl", Duration(15 * time.Minute), dv(1 * time.Minute), dv(24 * time.Hour), ScopeServer, true, false},
	}
}

func TestRegistryMatchesSpecTable(t *testing.T) {
	reg := NewRegistry()
	for _, row := range specTable() {
		t.Run(row.key, func(t *testing.T) {
			d, ok := reg.Def(row.key)
			if !ok {
				t.Fatalf("spec section 15.1 declares %q but the registry does not", row.key)
			}
			if !d.Default.Equal(row.def) {
				t.Errorf("%s: default is %s, spec says %s", row.key, d.Default, row.def)
			}
			if !sameBound(d.Min, row.min) {
				t.Errorf("%s: lower bound is %s, spec says %s", row.key, showBound(d.Min), showBound(row.min))
			}
			if !sameBound(d.Max, row.max) {
				t.Errorf("%s: upper bound is %s, spec says %s", row.key, showBound(d.Max), showBound(row.max))
			}
			if d.DownwardOnly != row.downwardOnly {
				t.Errorf("%s: downward-only is %v, spec says %v", row.key, d.DownwardOnly, row.downwardOnly)
			}
			if d.Scope != row.scope {
				t.Errorf("%s: scope is %q, spec says %q — getting this wrong is how a "+
					"two-account deployment does twice the work it was configured for",
					row.key, d.Scope, row.scope)
			}
			if d.RequiresRestart != row.restart {
				t.Errorf("%s: requires_restart is %v, spec says %v", row.key, d.RequiresRestart, row.restart)
			}
			if row.key == "logging.level" {
				want := []string{"debug", "info", "warn", "error"}
				if len(d.EnumValues) != len(want) {
					t.Fatalf("logging.level values = %v, spec says %v", d.EnumValues, want)
				}
				for i := range want {
					if d.EnumValues[i] != want[i] {
						t.Fatalf("logging.level values = %v, spec says %v", d.EnumValues, want)
					}
				}
			}
			if !d.Mutable {
				t.Errorf("%s: every key in the runtime table is mutable through PATCH", row.key)
			}
		})
	}
}

func TestRegistryDeclaresNothingTheSpecDoesNot(t *testing.T) {
	want := map[string]bool{}
	for _, row := range specTable() {
		want[row.key] = true
	}
	for _, k := range NewRegistry().Keys() {
		if !want[k] {
			t.Errorf("the registry declares %q, which is in no spec table; "+
				"add the spec row or drop the key", k)
		}
	}
	if got, wantN := len(NewRegistry().Keys()), len(want); got != wantN {
		t.Errorf("registry has %d keys, spec table has %d", got, wantN)
	}
}

// TestLoggingLevelIsTheOnlyHotReloadableKey pins section 15.1's one honest
// exception: logging.level reloads, and every other key says a restart is
// required rather than implying a reload nothing implements.
func TestLoggingLevelIsTheOnlyHotReloadableKey(t *testing.T) {
	var hot []string
	reg := NewRegistry()
	for _, k := range reg.Keys() {
		if d, _ := reg.Def(k); !d.RequiresRestart {
			hot = append(hot, k)
		}
	}
	sort.Strings(hot)
	if len(hot) != 1 || hot[0] != "logging.level" {
		t.Fatalf("keys claiming no restart = %v, want [logging.level]", hot)
	}
}

// TestPerAccountScopeIsExactlyTheBackfillAndIngestKeys guards the scope column
// as a set, not key by key: a new backfill key that quietly lands as `server`
// gets the whole deployment's budget rather than each account's.
func TestPerAccountScopeIsExactlyTheBackfillAndIngestKeys(t *testing.T) {
	want := []string{
		"backfill.concurrency",
		"backfill.conversation_page_size",
		"backfill.horizon",
		"backfill.include_archive",
		"backfill.max_messages_per_conversation",
		"backfill.message_page_size",
		"ingest.sweep_interval",
	}
	var got []string
	reg := NewRegistry()
	for _, k := range reg.Keys() {
		if d, _ := reg.Def(k); d.Scope == ScopePerAccount {
			got = append(got, k)
		}
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("per-account keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("per-account keys = %v, want %v", got, want)
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	cases := map[string]time.Duration{
		"365d": 365 * Day,
		"7d":   7 * Day,
		"15m":  15 * time.Minute,
		"6h":   6 * time.Hour,
		"30s":  30 * time.Second,
		"5m":   300 * time.Second,
		"1h":   time.Hour,
	}
	for text, want := range cases {
		got, err := ParseDuration(text)
		if err != nil {
			t.Fatalf("ParseDuration(%q): %v", text, err)
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", text, got, want)
		}
		if back := formatDuration(got); back != text {
			t.Errorf("formatDuration(%v) = %q, want %q", got, back, text)
		}
	}
	// The trim bug this formatter exists to avoid: ninety minutes must not
	// come back as "1h3".
	if got := formatDuration(90 * time.Minute); got != "90m" {
		t.Errorf("formatDuration(90m) = %q, want %q", got, "90m")
	}
}

func sameBound(a, b *Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func showBound(v *Value) string {
	if v == nil {
		return "(none)"
	}
	return v.String()
}
