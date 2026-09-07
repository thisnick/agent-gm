package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R-13. Three of the reviewer's plants SURVIVED the whole suite: deleting
// `sup.Backfill = workers`, deleting `sup.Sweep = workers`, and making
// `resumeAccounts` return `0, nil`. Each restored exactly one of the blockers
// this slice was rejected for, and the suite stayed green.
//
// The reason is worth stating, because it generalises: the compiler proves
// `accounts.Workers` SATISFIES the two interfaces, and nothing proved that
// `serve` ASSIGNS them. The wiring lived inside a function that also bound a
// socket and blocked, so no test could reach it. `buildServer` exists so that
// it can.
//
// These tests drive the binary's own constructor -- not a harness that builds
// a similar server, which is what made R-1 invisible in the first place.
func serveEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("AGENT_GM_DATA_DIR", dir)
	t.Setenv("AGENT_GM_DATA_KEY",
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("AGENT_GM_ADMIN_SECRET",
		"a-test-admin-secret-well-over-the-43-character-minimum-0123456789")
	t.Setenv("AGENT_GM_PUBLIC_URL", "http://127.0.0.1:0")
	t.Setenv("AGENT_GM_BACKEND", "fake")
	t.Setenv("AGENT_GM_ALLOW_FAKE", "1")
	t.Setenv("AGENT_GM_LOG_LEVEL", "error")
}

// Plant P11/P14: delete either assignment in serve.go and this fails naming
// the field. Planted 2026-09-07.
func TestServeWiresTheBackfillerAndTheSweeper(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)

	b, code := buildServer(context.Background(), "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	defer b.Close()

	if b.Sup.Backfill == nil {
		t.Error("serve wires no Backfiller: section 5.2's backfill never runs in the " +
			"shipped binary, and every search stays permanently history_incomplete")
	}
	if b.Sup.Sweep == nil {
		t.Error("serve wires no Sweeper: section 5.4's reconciliation sweep never runs, " +
			"and D26 calls it mandatory BECAUSE the library's dedup loses messages")
	}
	// Not merely non-nil: the same value serves as both, which is what
	// accounts.NewWorkers returns and what reaches core.
	if fmt.Sprintf("%T", b.Sup.Backfill) != fmt.Sprintf("%T", b.Sup.Sweep) {
		t.Errorf("Backfill is %T and Sweep is %T; serve builds one Workers for both",
			b.Sup.Backfill, b.Sup.Sweep)
	}
	if got := fmt.Sprintf("%T", b.Sup.Backfill); got != "*accounts.Workers" {
		t.Errorf("Backfill is %s, want *accounts.Workers -- the one that reaches core", got)
	}
}

// Plant P15: make resumeAccounts return 0, nil and this fails at "resumed 0
// accounts". Planted 2026-09-07.
func TestServeResumesAccountsFromTheSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)
	ctx := context.Background()

	// A first process: pair one account through the supervisor the binary
	// itself built, so the session file is written the way production writes
	// it.
	first, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	backend, err := first.Deps.NewBackend()
	if err != nil {
		t.Fatalf("minting a backend: %v", err)
	}
	acct, err := first.Sup.Pair(ctx, backend, fixtureCookies(), 0, func(string) {})
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	id := acct.ID
	if _, err := os.Stat(filepath.Join(dir, "sessions", id+".enc")); err != nil {
		t.Fatalf("no session file was written: %v", err)
	}
	first.Sup.StopAll(ctx)
	first.Close()

	// A second process over the same directory: the account must come back.
	second, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("the second buildServer returned exit %d", code)
	}
	defer second.Close()
	// The listener runs this behind the socket so /healthz answers at once;
	// here it is called directly, which is the same function.
	second.startAccounts(ctx)

	if second.Resumed != 1 {
		t.Fatalf("resumed %d accounts, want 1: without the resume every account row is "+
			"still listed with no backend behind it, so every write fails silently",
			second.Resumed)
	}
	if _, err := second.Sup.Get(id); err != nil {
		t.Errorf("the resumed supervisor does not hold %s: %v", id, err)
	}
}

// The backfill and the sweep actually run against the binary's own wiring,
// not merely against a non-nil field.
func TestServeActuallyBackfillsAndSweeps(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)
	ctx := context.Background()

	b, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	defer b.Close()

	backend, err := b.Deps.NewBackend()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := b.Sup.Pair(ctx, backend, fixtureCookies(), 0, func(string) {})
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	t.Cleanup(func() { b.Sup.StopAll(context.WithoutCancel(ctx)) })

	// The sweep is recorded on the account row, so it survives a restart and
	// an operator can answer "when did this last sweep?" after one.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := b.Store.Account(ctx, acct.ID)
		if err == nil && row.LastSweepAtMS != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := b.Store.Account(ctx, acct.ID)
	t.Fatalf("accounts.last_sweep_at_ms is still %d after connecting: nothing ran the sweep",
		row.LastSweepAtMS)
}

func fixtureCookies() map[string]string {
	out := map[string]string{}
	for _, n := range []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID", "__Secure-1PSIDTS"} {
		out[n] = "FIXTURE-" + n
	}
	return out
}

// Live-gate finding 5: `agent-gm serve` did not listen until resume and the
// reprocess task had finished -- four minutes on the owner's deployment, with
// /healthz unreachable throughout. Anything watching "is the service up"
// concluded it was not, which is exactly backwards.
//
// Section 7.5 says /healthz is ok "whenever the process is serving", and a
// process that has bound its socket is serving. An account that has not
// connected yet is a fact about that account, reported in its own row.
//
// The two things that must NOT move behind the listener are already ahead of
// it: migrations (4.3, before any listener binds) and crash recovery (6.6,
// before any account connects). Those settle the DATABASE; this connects to
// the network.
//
// Plant: call startAccounts before the listen instead of after and this
// fails at "the listener waited for the accounts". Planted 2026-09-07.
func TestHealthAnswersBeforeAccountsAreUp(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)
	ctx := context.Background()

	b, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	defer b.Close()

	// buildServer must NOT have resumed anything: that is the whole point.
	if b.AccountsStarted() {
		t.Error("the listener waited for the accounts; buildServer resumed them itself")
	}

	// And /healthz is answerable from the server it built, with no account
	// connected and none started.
	rec := httptest.NewRecorder()
	b.Server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d before accounts are up, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("/healthz body = %s", got)
	}
}
