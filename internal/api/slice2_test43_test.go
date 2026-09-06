package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/store"
)

// Section 16 Slice 2 test 43:
//
//	"`POST /v1/admin/backup` produces a file that opens standalone, and
//	 pruning keeps exactly `backup.keep` and audits each removal."
//
// "Opens standalone" is the whole reason the snapshot is taken inside the
// database's own transaction rather than copied. A copy of a WAL-mode
// database WITHOUT its `-wal` log is missing every transaction still in that
// log -- and it opens perfectly well while quietly being out of date, which
// is the worst possible failure for a backup: it is only discovered during a
// restore, by somebody who has already lost the original.
//
// So this test does not merely check that a file appeared. It opens the file
// as a database of its own, in a directory with no `-wal` beside it, and
// reads back the rows that were written just before the snapshot.
func TestSlice2_43_BackupOpensStandaloneAndPruningKeepsExactlyBackupKeep(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	s.seedMessage(accountID, "conv-a", "m0001", s.Clock.Now(), false)
	s.seedMessage(accountID, "conv-a", "m0002", s.Clock.Now(), false)

	wantMessages := countRows(t, s.Store, "SELECT COUNT(*) FROM messages")
	if wantMessages != 2 {
		t.Fatalf("the fixture has %d messages, want 2", wantMessages)
	}

	// --- the snapshot -------------------------------------------------------

	env := s.call("POST", "/v1/admin/backup", nil).ok(t, 200)
	path := getString(t, env.Data, "path")

	if dir := filepath.Dir(path); dir != filepath.Join(s.Dir, store.BackupDirName) {
		t.Errorf("the snapshot is at %s; the caller does not choose the path and it "+
			"belongs under <data_dir>/%s", path, store.BackupDirName)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the snapshot is not on disk: %v", err)
	}
	// No sidecar. A snapshot with a `-wal` beside it is a snapshot that
	// needs the log to be complete, which is the property being ruled out.
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("the snapshot has a %s sidecar; it must open on its own",
				filepath.Base(sidecar))
		}
	}

	// Open it as a database in its own right, in a directory containing
	// nothing else, and read the rows back.
	standalone := t.TempDir()
	copyFile(t, path, filepath.Join(standalone, filepath.Base(s.Store.Path())))
	reopened, err := store.Open(standalone, clock.NewFake())
	if err != nil {
		t.Fatalf("the snapshot does not open standalone: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if got := countRows(t, reopened, "SELECT COUNT(*) FROM messages"); got != wantMessages {
		t.Errorf("the standalone snapshot holds %d messages, want %d; a snapshot that "+
			"opens but is out of date is the failure this test exists for",
			got, wantMessages)
	}
	rows, err := reopened.Accounts(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != accountID {
		t.Errorf("the standalone snapshot's accounts are %v (%v), want just %s",
			rows, err, accountID)
	}

	// The backup itself is audited.
	if len(s.auditRows("admin.backup")) == 0 {
		t.Error("no admin.backup audit row was written")
	}
}

// TestSlice2_43_PruningKeepsExactlyBackupKeepAndAuditsEachRemoval is the
// second half.
//
// **Retention counts calls, not days** (spec section 15.2): an hourly cron
// with `backup.keep=7` keeps seven hours, which is worth knowing before
// relying on it for a week's cover. So the test takes several backups in
// quick succession -- moving the injected clock rather than sleeping -- and
// asserts on the count, not on any age.
func TestSlice2_43_PruningKeepsExactlyBackupKeepAndAuditsEachRemoval(t *testing.T) {
	s := newServer(t)
	s.addAccount(addressA)

	const keep = 2
	s.call("PATCH", "/v1/admin/settings", map[string]any{"backup.keep": keep}).ok(t, 200)

	var paths []string
	for i := range 5 {
		// The snapshot's name carries a second-resolution timestamp, so the
		// clock moves between calls rather than the test sleeping.
		s.Clock.Advance(time.Duration(i+1) * time.Second)
		env := s.call("POST", "/v1/admin/backup", nil).ok(t, 200)
		paths = append(paths, getString(t, env.Data, "path"))
	}

	kept, err := s.Store.ListBackups(s.Dir)
	if err != nil {
		t.Fatalf("listing backups: %v", err)
	}
	if len(kept) != keep {
		t.Fatalf("%d snapshots survive with backup.keep=%d: %v", len(kept), keep, kept)
	}
	// The newest ones are the survivors: the name begins with an ISO-8601
	// basic UTC timestamp, so lexical order is chronological order.
	for _, want := range paths[len(paths)-keep:] {
		if !contains(kept, want) {
			t.Errorf("the newest snapshot %s was pruned", want)
		}
	}
	for _, gone := range paths[:len(paths)-keep] {
		if contains(kept, gone) {
			t.Errorf("the old snapshot %s survived pruning", gone)
		}
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("the old snapshot %s is still on disk: %v", gone, err)
		}
	}

	// Each removal is audited as admin.backup_pruned, individually: a single
	// row saying "pruned 3" would not let an operator answer "which one is
	// gone?" months later.
	pruned := s.auditRows("admin.backup_pruned")
	if len(pruned) != len(paths)-keep {
		t.Fatalf("%d admin.backup_pruned rows for %d removals",
			len(pruned), len(paths)-keep)
	}
	audited := map[string]bool{}
	for _, row := range pruned {
		audited[row.TargetID] = true
	}
	for _, gone := range paths[:len(paths)-keep] {
		if !audited[gone] {
			t.Errorf("the removal of %s was not audited; audited: %v", gone, audited)
		}
	}
}

// TestSlice2_43_PruningNeverTouchesAFileItDidNotWrite is spec section 15.2's
// other rule. A pruner that deleted whatever it found in a directory called
// "backups" would eventually delete something irreplaceable that an operator
// put there by hand.
func TestSlice2_43_PruningNeverTouchesAFileItDidNotWrite(t *testing.T) {
	s := newServer(t)
	s.call("PATCH", "/v1/admin/settings", map[string]any{"backup.keep": 1}).ok(t, 200)

	dir := filepath.Join(s.Dir, store.BackupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the backup directory: %v", err)
	}
	bystanders := []string{
		filepath.Join(dir, "README"),
		filepath.Join(dir, "agent-gm-hand-copied.sqlite3"),
		filepath.Join(dir, "2020-01-01-my-own-backup.sqlite3"),
	}
	for _, p := range bystanders {
		if err := os.WriteFile(p, []byte("not the pruner's business"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}

	for i := range 3 {
		s.Clock.Advance(time.Duration(i+1) * time.Second)
		s.call("POST", "/v1/admin/backup", nil).ok(t, 200)
	}

	for _, p := range bystanders {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("pruning deleted %s, which it did not write: %v", filepath.Base(p), err)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("reading %s: %v", from, err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", to, err)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
