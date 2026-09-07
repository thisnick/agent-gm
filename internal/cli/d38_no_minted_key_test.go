package cli_test

import (
	"strings"
	"testing"
)

// D38: `agm` no longer mints an idempotency key.
//
// It used to mint a UUID per invocation, which read like protection and was
// not: a fresh key on every run is byte-for-byte a run with no key, plus one
// more header. Worse, it made the docs say "omit it and agm mints one", which
// invited a reader to believe a re-run was somehow guarded.
//
// The behaviour that matters is on the WIRE, so this asserts the header the
// stub actually received rather than the flag the runner parsed.
//
// Plant: restore the mint in runner.idempotencyKey and this fails on the
// first subtest. Planted 2026-09-07.
func TestD38TheCLISendsNoIdempotencyKeyUnlessAsked(t *testing.T) {
	t.Run("an ordinary send carries no key", func(t *testing.T) {
		s := newStub(t)
		got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq", "--text", "hello")
		if got.code != 0 {
			t.Fatalf("the send exited %d\nstderr: %s", got.code, got.stderr)
		}
		seen := s.seen()
		var sends int
		for _, r := range seen {
			if r.Method != "POST" || !strings.HasSuffix(r.Path, "/messages") {
				continue
			}
			sends++
			if r.IdempotencyKey != "" {
				t.Errorf("the CLI sent Idempotency-Key %q on an ordinary send; "+
					"a key it invented is a key that protects nothing", r.IdempotencyKey)
			}
			// And it must not have moved to the body either.
			if strings.Contains(r.Body, "client_request_id") {
				t.Errorf("the CLI put client_request_id in the body: %s", r.Body)
			}
		}
		if sends == 0 {
			t.Fatal("no send reached the stub, so the assertion proves nothing")
		}
	})

	t.Run("--idempotency-key sets the header", func(t *testing.T) {
		s := newStub(t)
		got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq", "--text", "hello",
			"--idempotency-key", "automation-key-1")
		if got.code != 0 {
			t.Fatalf("the send exited %d\nstderr: %s", got.code, got.stderr)
		}
		var found bool
		for _, r := range s.seen() {
			if r.Method == "POST" && strings.HasSuffix(r.Path, "/messages") {
				found = true
				if r.IdempotencyKey != "automation-key-1" {
					t.Errorf("Idempotency-Key = %q, want the flag's value", r.IdempotencyKey)
				}
				if strings.Contains(r.Body, "client_request_id") {
					t.Errorf("the flag reached the body as well as the header: %s", r.Body)
				}
			}
		}
		if !found {
			t.Fatal("no send reached the stub")
		}
	})
}
