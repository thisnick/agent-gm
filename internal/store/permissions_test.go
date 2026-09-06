package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// Live-gate finding, Slice 1: the running deployment had a 0775 data
// directory and a 0644 agent-gm.sqlite3. Only sessions/ and its .enc files
// were tight.
//
// That is not a cosmetic problem. Spec section 4.5 encrypts the session
// envelope and the attachment keys, and says so precisely because the rest is
// NOT encrypted: message text, subjects and conversation names sit in plain
// SQLite. On a shared host the file mode is therefore the entire at-rest
// model for every message the owner has ever sent or received, and a 0644
// database is readable by every account on the machine.
//
// Plant: drop the os.Chmod after MkdirAll in SecureDataDir, or the
// secureDatabaseFiles call after the migration, and this test fails naming
// the path and the mode. Planted 2026-09-06.
func TestOpenTightensTheDataDirectoryAndTheDatabase(t *testing.T) {
	// A data directory that already exists with a loose mode is the case
	// that actually happens: a deployment script, a Docker volume, or
	// `mkdir -p data` in a shell. MkdirAll returns success without touching
	// an existing directory, so only an explicit Chmod fixes it.
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o775); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dataDir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A write, so the -wal and -shm files exist to be checked.
	const address = "perm@example.test"
	if err := st.UpsertAccount(context.Background(), store.Account{
		ID: store.AccountID(address), GoogleAccount: address, State: "connected",
	}); err != nil {
		t.Fatal(err)
	}

	assertMode(t, dataDir, 0o700)
	db := filepath.Join(dataDir, "agent-gm.sqlite3")
	assertMode(t, db, 0o600)
	for _, sidecar := range []string{db + "-wal", db + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			assertMode(t, sidecar, 0o600)
		}
	}

	// Nothing under the data directory is readable by anyone but the owner.
	err = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		if perm&0o077 != 0 {
			t.Errorf("%s is mode %04o: group or other can reach it", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if fi.Mode().Perm() != want {
		t.Errorf("%s is mode %04o, want %04o", path, fi.Mode().Perm(), want)
	}
}

// CheckPermissions warns rather than fails. Agent GM tightens what it
// creates, so a warning means something OUTSIDE Agent GM loosened it -- a
// restore, a bind mount with its own ownership, an operator's chmod -R.
// Refusing to start would turn a fixable disclosure into an outage, and an
// operator who cannot start the server cannot read the message telling them
// why.
func TestCheckPermissionsWarnsAboutWhatItDidNotTighten(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if warnings := store.CheckPermissions(dataDir); len(warnings) != 0 {
		t.Fatalf("a freshly opened data directory warned: %v", warnings)
	}

	// Something outside Agent GM loosens the database after the fact.
	db := filepath.Join(dataDir, "agent-gm.sqlite3")
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	// And a session file, which is one Google account's credential and is
	// checked individually rather than only through its directory.
	sessions := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(sessions, "acct_fixture.enc")
	if err := os.WriteFile(loose, []byte("not a real session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o640); err != nil {
		t.Fatal(err)
	}

	warnings := store.CheckPermissions(dataDir)
	found := map[string]bool{}
	for _, w := range warnings {
		found[w.Path] = true
		if w.String() == "" {
			t.Error("a warning renders as nothing")
		}
	}
	if !found[db] {
		t.Errorf("a 0644 database did not warn: %v", warnings)
	}
	if !found[loose] {
		t.Errorf("a 0640 session file did not warn: %v", warnings)
	}
	// Reopening tightens the database again, so the warning is actionable
	// by restarting as well as by chmod.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(dataDir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	assertMode(t, db, 0o600)
}
