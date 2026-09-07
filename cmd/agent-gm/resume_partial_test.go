package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/store"
)

// T-2. One account whose session cannot be opened must not take the accounts
// AFTER it down with it.
//
// resumeAccounts used to `return` on the first undecryptable session, which
// was a refusal to start while the caller treated it as fatal. It stopped
// being fatal when the resume moved behind the listener (section 4.3), and
// the return then did something nobody chose: it abandoned the walk. Every
// account later in row order never resumed, held no client, refused writes
// with `not_signed_in`, and carried whatever state it had when the process
// last stopped -- so which of three accounts worked depended on which one had
// the bad session file.
//
// This pairs two accounts, re-seals the FIRST one in row order under a
// different data key, and restarts.
//
// Plant P31: turn the `continue` in resumeAccounts back into a `return` and
// this fails at "resumed 0 accounts". Planted 2026-09-07.
func TestOneUnreadableSessionDoesNotStopTheAccountsBehindIt(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)
	ctx := context.Background()

	first, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	for i := 0; i < 2; i++ {
		backend, err := first.Deps.NewBackend()
		if err != nil {
			t.Fatalf("minting a backend: %v", err)
		}
		if _, err := first.Sup.Pair(ctx, backend, fixtureCookies(), 0, func(string) {}); err != nil {
			t.Fatalf("pairing: %v", err)
		}
	}
	rows, err := first.Store.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("paired %d accounts, want 2", len(rows))
	}
	// The FIRST in the order resumeAccounts walks, which is the order that
	// used to decide whether the other one worked.
	broken, healthy := rows[0].ID, rows[1].ID
	first.Sup.StopAll(ctx)
	first.Close()

	// A genuinely undecryptable envelope: the same file, sealed with a key
	// this process does not have. Writing garbage would test a different
	// error; section 15.4's runbook row is about a key that does not match.
	otherKey, err := store.ParseDataKey(
		"202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f")
	if err != nil {
		t.Fatal(err)
	}
	otherSessions, err := store.NewSessionStore(dir, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherSessions.Save(broken, []byte(`{"address":"nobody@example.test"}`)); err != nil {
		t.Fatalf("re-sealing the session: %v", err)
	}

	second, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("the second buildServer returned exit %d", code)
	}
	defer second.Close()

	// Directly, so the error is readable; startAccounts calls exactly this.
	resumed, resumeErr := resumeAccounts(ctx, second.cfg, second.Store,
		second.Sessions, second.Sup, second.libLog)

	if resumed != 1 {
		t.Fatalf("resumed %d accounts, want 1: one unreadable session must not "+
			"abandon the walk, or whether an account works depends on row order",
			resumed)
	}
	if _, err := second.Sup.Get(healthy); err != nil {
		t.Errorf("the account behind the broken one did not resume: %v", err)
	}
	if _, err := second.Sup.Get(broken); err == nil {
		t.Error("the account with the unreadable session was adopted anyway")
	}

	// The failure is still loud, and still names the fix.
	if resumeErr == nil {
		t.Error("resumeAccounts reported no error at all; an undecryptable session " +
			"is section 15.4's first runbook row and must not pass silently")
	} else {
		msg := resumeErr.Error()
		for _, want := range []string{broken, "cannot be decrypted", "AGENT_GM_DATA_KEY"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the error does not mention %q: %s", want, msg)
			}
		}
	}

	// And the account SAYS so, rather than sitting at whatever it held when
	// the last process stopped.
	row, err := second.Store.Account(ctx, broken)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateSignedOut {
		t.Errorf("the unresumable account is %q, want signed_out: it holds no client, "+
			"so every write naming it is refused -- and the state is where an "+
			"operator reads why", row.State)
	}
	if row.StateReason != store.ReasonCredentials {
		t.Errorf("the unresumable account's state_reason is %q, want %q",
			row.StateReason, store.ReasonCredentials)
	}

	// The healthy one is untouched by its neighbour's failure.
	good, err := second.Store.Account(ctx, healthy)
	if err != nil {
		t.Fatal(err)
	}
	if good.State == store.StateSignedOut {
		t.Error("the healthy account was marked signed_out too")
	}

	// The session file itself is left alone: the fix is to restore the key,
	// and deleting the evidence would make that impossible.
	if _, err := os.Stat(filepath.Join(dir, "sessions", broken+".enc")); err != nil {
		t.Errorf("the unreadable session file was removed: %v", err)
	}
}
