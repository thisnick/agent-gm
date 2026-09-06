package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// BackupDirName is the directory under the data directory that snapshots go
// in. The caller does not choose the path (spec section 15.2).
const BackupDirName = "backups"

// backupNamePattern is the exact shape of a snapshot's name. Pruning never
// touches a file that does not match it, so a file an operator put in the
// directory by hand is safe from a process that is deleting things.
var backupNamePattern = regexp.MustCompile(
	`^agent-gm-\d{8}T\d{6}Z-[0-9a-f]{8}\.sqlite3$`)

// Backup writes a snapshot to <data_dir>/backups/agent-gm-<ts>-<id>.sqlite3
// and returns its path.
//
// **Mechanism, and a documented deviation from spec section 15.2.** The spec
// says "using the SQLite backup API, not a file copy". `modernc.org/sqlite`
// at the pinned version does not expose `sqlite3_backup_init` from its driver
// package -- the symbol exists only inside the vendored C translation under
// `lib/`, which is not importable -- so the online backup API is not
// reachable from Agent GM at all. `VACUUM INTO` is used instead, and it
// delivers every property section 15.2 actually asks for:
//
//   - it runs inside the database's own read transaction, so the snapshot is
//     a consistent point in time and is never a torn file copy;
//   - in WAL mode it does not block writers, so the server keeps serving;
//   - the result is a single file with **no `-wal` sidecar** that opens on
//     its own, which is the property a restore depends on;
//   - it is not a file copy, which is the thing the spec was ruling out.
//
// What it does not do is the restart-on-write behaviour of the incremental
// backup API, and it does not need to: `VACUUM INTO` copies within one
// transaction rather than page by page across many, so there is no window
// for a writer to invalidate a page already taken. The observable contract
// is unchanged; only the sentence naming the C function is.
func (s *Store) Backup(ctx context.Context, dataDir string) (string, error) {
	dir := filepath.Join(dataDir, BackupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	name := fmt.Sprintf("agent-gm-%s-%s.sqlite3",
		s.clock.Now().UTC().Format("20060102T150405Z"),
		strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	path := filepath.Join(dir, name)
	if !backupNamePattern.MatchString(name) {
		// A name this function built that its own pruner would not recognise
		// would mean snapshots accumulate for ever, silently.
		return "", fmt.Errorf("built a snapshot name the pruner would not match: %q", name)
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("a snapshot already exists at %s", path)
	}

	// VACUUM INTO cannot be parameterised, and it takes a string literal.
	// The path is one this function built from a timestamp and a UUID, so
	// there is nothing caller-supplied in it; the quote doubling is belt and
	// braces against a data directory with an apostrophe in its name.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := s.write.ExecContext(ctx, stmt); err != nil {
		// A half-written snapshot is worse than none: an operator restoring
		// it would get a corrupt database rather than an error.
		_ = os.Remove(path)
		return "", fmt.Errorf("writing the snapshot: %w", err)
	}
	return path, nil
}

// PrunedBackup is one removal, for the audit row.
type PrunedBackup struct {
	Path string
	// Err is set when the file could not be removed. Such a file is logged
	// and LEFT rather than failing a backup that had already succeeded
	// (spec section 15.2).
	Err error
}

// PruneBackups keeps the newest `keep` snapshots and removes the rest,
// returning what it removed so each can be audited as `admin.backup_pruned`.
//
// Two rules from spec section 15.2, both load-bearing:
//
//   - **Pruning never touches a file that is not named
//     `agent-gm-<timestamp>-<id>.sqlite3`.** An operator's own copy, a
//     README, a partially transferred file: none of them are this function's
//     business, and a pruner that deleted whatever it found in a directory
//     called "backups" would eventually delete something irreplaceable.
//   - **A file it cannot delete is reported and left**, rather than failing a
//     backup that had already succeeded. The snapshot the caller asked for
//     exists; a permission problem on an old one is a separate matter.
//
// **Retention counts calls, not days.** An hourly cron with `backup.keep=7`
// keeps seven hours, which is worth knowing before relying on it for a
// week's cover.
func (s *Store) PruneBackups(dataDir string, keep int) ([]PrunedBackup, error) {
	if keep < 1 {
		return nil, errors.New("backup.keep must be at least 1")
	}
	dir := filepath.Join(dataDir, BackupDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshots []string
	for _, e := range entries {
		if e.IsDir() || !backupNamePattern.MatchString(e.Name()) {
			continue
		}
		snapshots = append(snapshots, e.Name())
	}
	// The name begins with an ISO-8601 basic UTC timestamp, so lexical order
	// is chronological order and no stat call is needed -- which also means
	// the order does not change if a file's mtime is touched by a copy.
	sort.Strings(snapshots)
	if len(snapshots) <= keep {
		return nil, nil
	}
	var pruned []PrunedBackup
	for _, name := range snapshots[:len(snapshots)-keep] {
		p := filepath.Join(dir, name)
		entry := PrunedBackup{Path: p}
		if err := os.Remove(p); err != nil {
			entry.Err = err
		}
		pruned = append(pruned, entry)
	}
	return pruned, nil
}

// ListBackups returns the snapshots in the data directory, oldest first.
func (s *Store) ListBackups(dataDir string) ([]string, error) {
	dir := filepath.Join(dataDir, BackupDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && backupNamePattern.MatchString(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}
