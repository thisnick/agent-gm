package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/thisnick/agent-gm/internal/clock"
)

// Store is Agent GM's SQLite database.
//
// One goroutine writes, for the whole process: every write from every account
// goes through Writer, a single goroutine consuming a channel of write
// closures. Accounts are concurrent at the network and at the ingest
// boundary, serialised at the database. Reads use a separate read-only pool
// (spec section 2.4).
type Store struct {
	path  string
	write *sql.DB
	read  *sql.DB
	clock clock.Clock

	jobs   chan job
	closed chan struct{}
	done   chan struct{}
}

type job struct {
	fn     func(*sql.Tx) error
	result chan error
}

// ErrSchemaTooNew means the database was written by a newer binary.
type ErrSchemaTooNew struct {
	Database int
	Binary   int
}

func (e ErrSchemaTooNew) Error() string {
	return fmt.Sprintf("database schema version %d is newer than this binary understands (%d); "+
		"there is no down-migration, so run a build that knows version %d",
		e.Database, e.Binary, e.Database)
}

// Open opens (and migrates) the database under the data directory.
func Open(dataDir string, clk clock.Clock) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, "agent-gm.sqlite3")

	// journal_mode=WAL, busy_timeout=5000, foreign_keys=on,
	// synchronous=NORMAL (spec sections 2.4, 4.2).
	const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)"

	write, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, err
	}
	// The writer is a single goroutine, so a single connection is correct and
	// makes "one writer" true at the driver level too.
	write.SetMaxOpenConns(1)

	read, err := sql.Open("sqlite", path+pragmas+"&mode=ro")
	if err != nil {
		_ = write.Close()
		return nil, err
	}

	s := &Store{
		path:   path,
		write:  write,
		read:   read,
		clock:  clk,
		jobs:   make(chan job),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}

	if err := s.migrate(); err != nil {
		_ = s.close()
		return nil, err
	}
	go s.run()
	return s, nil
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Reader is the read-only pool. Every query it serves must carry an
// account_id predicate or an explicit all-accounts marker (spec section 13.2).
func (s *Store) Reader() *sql.DB { return s.read }

func (s *Store) run() {
	defer close(s.done)
	for {
		select {
		case <-s.closed:
			return
		case j := <-s.jobs:
			j.result <- s.apply(j.fn)
		}
	}
}

func (s *Store) apply(fn func(*sql.Tx) error) error {
	tx, err := s.write.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ErrStoreClosed is returned by Write after Close.
var ErrStoreClosed = errors.New("store is closed")

// Write runs fn on the single writer goroutine, inside one transaction.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	result := make(chan error, 1)
	select {
	case <-s.closed:
		return ErrStoreClosed
	case <-ctx.Done():
		return ctx.Err()
	case s.jobs <- job{fn: fn, result: result}:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result:
		return err
	}
}

// Close stops the writer and closes both pools.
func (s *Store) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	<-s.done
	return s.close()
}

func (s *Store) close() error {
	var first error
	if err := s.write.Close(); err != nil {
		first = err
	}
	if err := s.read.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// migrate runs each pending migration inside one transaction, before any
// listener binds. PRAGMA user_version is the version.
func (s *Store) migrate() error {
	var version int
	if err := s.write.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version > SchemaVersion() {
		return ErrSchemaTooNew{Database: version, Binary: SchemaVersion()}
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if err := s.apply(func(tx *sql.Tx) error {
			for _, stmt := range m.stmts {
				if _, err := tx.Exec(stmt); err != nil {
					return fmt.Errorf("migration %04d (%s): %w", m.version, m.name, err)
				}
			}
			// PRAGMA cannot be parameterised.
			if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// Version reports the database's current schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.read.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// ForeignKeyCheck runs PRAGMA foreign_key_check and returns the offending
// rows. It must be empty after every migration (spec section 4.3).
func (s *Store) ForeignKeyCheck(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s row %d -> %s (fk %d)", table, rowid.Int64, parent, fkid))
	}
	return out, rows.Err()
}

// SetMeta writes one server_meta key. Nothing account-shaped lives here:
// every per-account fact is a column on accounts, because two accounts would
// otherwise race on one row (spec section 4.2).
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO server_meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	})
}

// Meta reads one server_meta key.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM server_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}
