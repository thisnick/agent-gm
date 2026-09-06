package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func newTestSettings(t *testing.T, env map[string]string) (*Settings, *Memory) {
	t.Helper()
	store := NewMemory()
	lookup := func(k string) string { return env[k] }
	return New(NewRegistry(), store, lookup), store
}

func patch(t *testing.T, s *Settings, body string) ([]Change, error) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("bad test body %s: %v", body, err)
	}
	return s.Patch(context.Background(), m)
}

// TestSourcePrecedence is section 15.1's precedence: database over
// environment over default, reported alongside the value because
// `GET /v1/admin/settings` serves exactly this.
func TestSourcePrecedence(t *testing.T) {
	ctx := context.Background()

	s, _ := newTestSettings(t, nil)
	eff, err := s.Get(ctx, "logging.level")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Source != SourceDefault || eff.Value.Str != "info" {
		t.Fatalf("with nothing set: got %s/%s, want default/info", eff.Source, eff.Value)
	}

	s, store := newTestSettings(t, map[string]string{"AGENT_GM_LOG_LEVEL": "warn"})
	eff, err = s.Get(ctx, "logging.level")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Source != SourceEnvironment || eff.Value.Str != "warn" {
		t.Fatalf("with the environment set: got %s/%s, want environment/warn", eff.Source, eff.Value)
	}

	if err := store.Put(ctx, "logging.level", `"error"`); err != nil {
		t.Fatal(err)
	}
	eff, err = s.Get(ctx, "logging.level")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Source != SourceDatabase || eff.Value.Str != "error" {
		t.Fatalf("with a database row: got %s/%s, want database/error", eff.Source, eff.Value)
	}

	// The same precedence has to hold through the listing route, not just
	// the single-key one.
	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range all {
		if e.Key == "logging.level" {
			seen = true
			if e.Source != SourceDatabase || e.Value.Str != "error" {
				t.Fatalf("All(): got %s/%s, want database/error", e.Source, e.Value)
			}
		}
		if e.Bounds == "" || e.Scope == "" || e.Type == "" {
			t.Errorf("%s: GET /v1/admin/settings must report bounds, scope and type", e.Key)
		}
	}
	if !seen {
		t.Fatal("All() did not include logging.level")
	}
	if len(all) != len(NewRegistry().Keys()) {
		t.Fatalf("All() returned %d rows, registry has %d keys", len(all), len(NewRegistry().Keys()))
	}
}

// TestOutOfBoundsNamesKeyBoundsAndValue: the refusal has to carry all three,
// because an operator who mistyped a bound needs all three to fix it.
func TestOutOfBoundsNamesKeyBoundsAndValue(t *testing.T) {
	s, _ := newTestSettings(t, nil)
	cases := []struct {
		body   string
		key    string
		bounds string
		value  string
	}{
		{`{"backfill.concurrency": 9}`, "backfill.concurrency", "1–8", "9"},
		{`{"backfill.concurrency": 0}`, "backfill.concurrency", "1–8", "0"},
		{`{"accounts.max_concurrent": 33}`, "accounts.max_concurrent", "1–32", "33"},
		{`{"backup.keep": 101}`, "backup.keep", "1–100", "101"},
		{`{"backfill.horizon": "3651d"}`, "backfill.horizon", "7d–3650d", "3651d"},
		{`{"ingest.sweep_interval": "30s"}`, "ingest.sweep_interval", "1m–6h", "30s"},
		{`{"oauth.access_token_ttl": "2m"}`, "oauth.access_token_ttl", "5m–1h", "2m"},
	}
	for _, c := range cases {
		_, err := patch(t, s, c.body)
		if err == nil {
			t.Fatalf("%s: accepted an out-of-bounds value", c.body)
		}
		e, ok := AsError(err)
		if !ok {
			t.Fatalf("%s: error %v is not a settings error", c.body, err)
		}
		if e.Key != c.key {
			t.Errorf("%s: error key %q, want %q", c.body, e.Key, c.key)
		}
		for _, want := range []string{c.key, c.bounds, c.value} {
			if !strings.Contains(e.Error(), want) {
				t.Errorf("%s: refusal %q does not name %q", c.body, e.Error(), want)
			}
		}
		if e.Code() != "invalid_request" {
			t.Errorf("%s: code %q, want invalid_request", c.body, e.Code())
		}
	}
}

// TestPatchIsAllOrNothing is section 7.7: a PATCH validates the whole body,
// and any invalid key rejects the request and changes nothing.
func TestPatchIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s, store := newTestSettings(t, nil)

	bodies := []string{
		// good, good, out of bounds
		`{"backfill.concurrency": 4, "backup.keep": 3, "accounts.max_concurrent": 99}`,
		// good, unknown key
		`{"backfill.concurrency": 4, "backfill.speed": 4}`,
		// good, wrong type
		`{"backfill.concurrency": 4, "backfill.include_archive": "yes"}`,
	}
	for _, body := range bodies {
		if _, err := patch(t, s, body); err == nil {
			t.Fatalf("%s: accepted", body)
		}
		rows, err := store.All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s: rejected the request but wrote %v", body, rows)
		}
		eff, err := s.Get(ctx, "backfill.concurrency")
		if err != nil {
			t.Fatal(err)
		}
		if eff.Source != SourceDefault || eff.Value.Int != 2 {
			t.Fatalf("%s: the valid key in a rejected body took effect anyway (%s/%s)",
				body, eff.Source, eff.Value)
		}
	}

	// The same body without the bad key writes everything in it.
	changes, err := patch(t, s, `{"backfill.concurrency": 4, "backup.keep": 3}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes))
	}
	rows, _ := store.All(ctx)
	if len(rows) != 2 {
		t.Fatalf("stored %v, want two rows", rows)
	}
	if changes[0].Key != "backfill.concurrency" || changes[0].From.Int != 2 || changes[0].To.Int != 4 {
		t.Errorf("change 0 = %+v", changes[0])
	}
}

// TestUploadMaxBytesIsDownwardOnly: raising the ceiling at runtime would let a
// caller re-open a limit an operator closed.
func TestUploadMaxBytesIsDownwardOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSettings(t, nil)

	if _, err := patch(t, s, `{"media.upload_max_bytes": 52428800}`); err != nil {
		t.Fatalf("lowering the ceiling was refused: %v", err)
	}
	eff, _ := s.Get(ctx, "media.upload_max_bytes")
	if eff.Value.Int != 52428800 || eff.Source != SourceDatabase {
		t.Fatalf("after lowering: %s/%s", eff.Source, eff.Value)
	}

	// Back up to the default is still up.
	_, err := patch(t, s, `{"media.upload_max_bytes": 104857600}`)
	if err == nil {
		t.Fatal("raising media.upload_max_bytes back to the default was accepted")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error %v is not a settings error", err)
	}
	if !strings.Contains(e.Error(), "media.upload_max_bytes") ||
		!strings.Contains(e.Error(), "downward only") ||
		!strings.Contains(e.Error(), "52428800") {
		t.Errorf("refusal %q must name the key, the rule and the effective value", e.Error())
	}
	eff, _ = s.Get(ctx, "media.upload_max_bytes")
	if eff.Value.Int != 52428800 {
		t.Fatalf("a refused raise changed the value to %s", eff.Value)
	}

	// Equal is not an increase.
	if _, err := patch(t, s, `{"media.upload_max_bytes": 52428800}`); err != nil {
		t.Fatalf("rewriting the same value was refused: %v", err)
	}
	// And a further reduction still works.
	if _, err := patch(t, s, `{"media.upload_max_bytes": 1024}`); err != nil {
		t.Fatalf("lowering again was refused: %v", err)
	}
}

// TestRetiredKeyIsAnUnknownKey is section 15.1: a retired key becomes an
// unknown key, and setting one answers naming its replacement rather than
// silently writing a key nothing reads.
func TestRetiredKeyIsAnUnknownKey(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()
	reg.Retire("backfill.page_size", "backfill.message_page_size")
	reg.Retire("ingest.legacy_poll", "")
	store := NewMemory()
	s := New(reg, store, nil)

	_, err := patch(t, s, `{"backfill.page_size": 100}`)
	if err == nil {
		t.Fatal("a retired key was accepted")
	}
	if !strings.Contains(err.Error(), "backfill.page_size") ||
		!strings.Contains(err.Error(), "retired") ||
		!strings.Contains(err.Error(), "backfill.message_page_size") {
		t.Errorf("refusal %q must name the retired key and its replacement", err)
	}
	if rows, _ := store.All(ctx); len(rows) != 0 {
		t.Fatalf("a retired key was written: %v", rows)
	}
	if _, err := s.Get(ctx, "backfill.page_size"); err == nil {
		t.Fatal("a retired key is still readable")
	}

	// A retirement with no replacement still says so rather than falling
	// back to the generic unknown-key wording.
	_, err = patch(t, s, `{"ingest.legacy_poll": true}`)
	if err == nil || !strings.Contains(err.Error(), "no replacement") {
		t.Errorf("refusal for a replacement-less retirement = %v", err)
	}

	// And an ordinary unknown key is still an unknown key.
	_, err = patch(t, s, `{"backfill.nonsense": 1}`)
	if err == nil || !strings.Contains(err.Error(), `unknown setting "backfill.nonsense"`) {
		t.Errorf("unknown-key refusal = %v", err)
	}
}

func TestRetiringALiveKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("retiring a key that is still declared was allowed")
		}
	}()
	NewRegistry().Retire("backup.keep", "backup.retain")
}

func TestEnumAndTypeRefusals(t *testing.T) {
	s, _ := newTestSettings(t, nil)
	for _, body := range []string{
		`{"logging.level": "verbose"}`,
		`{"logging.level": 3}`,
		`{"backfill.concurrency": "4"}`,
		`{"backfill.concurrency": 1.5}`,
		`{"backfill.horizon": 365}`,
		`{"backfill.include_archive": 1}`,
	} {
		if _, err := patch(t, s, body); err == nil {
			t.Errorf("%s: accepted", body)
		}
	}
}

// TestInvalidEnvironmentValueIsReported: an environment value out of bounds
// is not silently ignored in favour of the default.
func TestInvalidEnvironmentValueIsReported(t *testing.T) {
	s, _ := newTestSettings(t, map[string]string{"AGENT_GM_LOG_LEVEL": "chatty"})
	_, err := s.Get(context.Background(), "logging.level")
	if err == nil {
		t.Fatal("an invalid AGENT_GM_LOG_LEVEL was accepted")
	}
	if !strings.Contains(err.Error(), "logging.level") || !strings.Contains(err.Error(), "chatty") {
		t.Errorf("refusal %q must name the key and the value", err)
	}
}
