package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 43, first half: the snapshot opens STANDALONE.
//
// That is the property a restore depends on, and it is why the mechanism
// matters: a file copy of a WAL-mode database without its `-wal` sidecar is
// missing every committed transaction still in the log, and it opens
// perfectly well while quietly being out of date.
//
// Plant: replace VACUUM INTO with an io.Copy of the database file and this
// test fails at "the snapshot is missing rows". Planted 2026-09-06.
func TestBackupOpensStandaloneAndCarriesTheRowsAtTheTimeItWasTaken(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	clk := clock.NewFake()
	st, err := store.Open(dataDir, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const address = "backup@example.test"
	id := store.AccountID(address)
	if err := st.UpsertAccount(ctx, store.Account{
		ID: id, GoogleAccount: address, State: "connected",
	}); err != nil {
		t.Fatal(err)
	}

	path, err := st.Backup(ctx, dataDir)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(dataDir, store.BackupDirName) {
		t.Errorf("the snapshot went to %s; the caller does not choose the path", path)
	}

	// A snapshot is a full copy of every message the owner has ever sent or
	// received, and message text is not encrypted at rest, so its mode is
	// its whole at-rest protection. It is also the file most likely to be
	// copied somewhere else, carrying that mode with it.
	//
	// Plant: drop the Chmod after VACUUM INTO and this fails at "the
	// snapshot is mode 0644". Planted 2026-09-07.
	assertMode(t, path, 0o600)
	if warnings := store.CheckPermissions(dataDir); len(warnings) != 0 {
		t.Errorf("a freshly taken backup warned: %v", warnings)
	}

	// No -wal or -shm sidecar: the snapshot has to open on its own.
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("the snapshot has a %s sidecar", filepath.Base(sidecar))
		}
	}

	// Opened read-only and with no pragmas, so nothing this test does can
	// repair a snapshot that was not already complete.
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got string
	if err := db.QueryRow(`SELECT google_account FROM accounts WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("the snapshot is missing rows: %v", err)
	}
	if got != address {
		t.Errorf("the snapshot holds %q, want %q", got, address)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != store.SchemaVersion() {
		t.Errorf("the snapshot is at schema version %d, want %d", version, store.SchemaVersion())
	}

	// The server keeps serving: a write after the snapshot succeeds and is
	// absent from it, which is what "a point in time" means.
	if err := st.SetAccountLabel(ctx, id, "after the snapshot"); err != nil {
		t.Fatalf("the store stopped serving after a backup: %v", err)
	}
	var label sql.NullString
	if err := db.QueryRow(`SELECT label FROM accounts WHERE id = ?`, id).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label.Valid {
		t.Errorf("the snapshot carries a write made after it was taken: %q", label.String)
	}
}

// Section 16 Slice 2 test 43, second half: pruning keeps exactly backup.keep
// and reports each removal so it can be audited.
//
// Retention counts CALLS, not days: an hourly cron with keep=7 keeps seven
// hours.
func TestPruningKeepsExactlyBackupKeepAndReportsEachRemoval(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	clk := clock.NewFake()
	st, err := store.Open(dataDir, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var taken []string
	for i := 0; i < 5; i++ {
		// Distinct timestamps, so lexical order is chronological order.
		clk.Advance(90 * 1e9)
		p, err := st.Backup(ctx, dataDir)
		if err != nil {
			t.Fatal(err)
		}
		taken = append(taken, p)
	}

	const keep = 2
	pruned, err := st.PruneBackups(dataDir, keep)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != len(taken)-keep {
		t.Fatalf("pruned %d snapshots, want %d", len(pruned), len(taken)-keep)
	}
	for _, p := range pruned {
		if p.Err != nil {
			t.Errorf("removing %s: %v", p.Path, p.Err)
		}
		if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
			t.Errorf("%s was reported pruned but is still there", p.Path)
		}
	}
	left, err := st.ListBackups(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != keep {
		t.Fatalf("%d snapshots survived, want exactly %d", len(left), keep)
	}
	// The newest are the ones kept, not whichever the directory listed first.
	for i, want := range taken[len(taken)-keep:] {
		if left[i] != want {
			t.Errorf("kept %s, want %s", left[i], want)
		}
	}

	// Pruning again is a no-op rather than an error.
	again, err := st.PruneBackups(dataDir, keep)
	if err != nil || len(again) != 0 {
		t.Errorf("a second prune removed %d and gave %v", len(again), err)
	}
	if _, err := st.PruneBackups(dataDir, 0); err == nil {
		t.Error("keep=0 was accepted; it would delete every snapshot")
	}
}

// Pruning never touches a file that is not named
// agent-gm-<timestamp>-<id>.sqlite3. An operator's own copy, a README, a
// partially transferred file: a pruner that deleted whatever it found in a
// directory called "backups" would eventually delete something
// irreplaceable.
//
// Plant: prune every entry in the directory rather than matching the name
// pattern, and this test fails at "pruning deleted ...". Planted 2026-09-06.
func TestPruningNeverTouchesAFileItDidNotWrite(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.Backup(ctx, dataDir); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, store.BackupDirName)
	bystanders := []string{
		"README.txt",
		"agent-gm-before-the-migration.sqlite3",
		"agent-gm-20260906T120000Z-deadbeef.sqlite3.part",
		"my-own-copy.sqlite3",
	}
	for _, name := range bystanders {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// keep=1 with one real snapshot means nothing of ours is due for
	// removal, so anything deleted was a bystander.
	if _, err := st.PruneBackups(dataDir, 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range bystanders {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			t.Errorf("pruning deleted %s, which it did not write", name)
		}
	}
	// And they are not counted as snapshots either, or keep would be wrong.
	list, err := st.ListBackups(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("ListBackups found %d snapshots among the bystanders: %v", len(list), list)
	}
}

// Two snapshots in the same second do not collide: the name carries a random
// suffix as well as a timestamp, which is why spec section 15.2 names both.
func TestTwoSnapshotsInTheSameSecondDoNotCollide(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(dataDir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	first, err := st.Backup(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Backup(ctx, dataDir)
	if err != nil {
		t.Fatalf("a second snapshot in the same second failed: %v", err)
	}
	if first == second {
		t.Fatal("two snapshots in the same second share a path")
	}
	if !strings.HasSuffix(first, ".sqlite3") || !strings.HasSuffix(second, ".sqlite3") {
		t.Errorf("snapshot names are %s and %s", first, second)
	}
}
