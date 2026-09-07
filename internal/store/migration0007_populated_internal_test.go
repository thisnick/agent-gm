package store

// Reviewer test, slice 4 (D38). Migration 0007 rebuilds `operations` with a
// rename-copy-drop, and the ONLY test that existed for it opened a fresh
// store -- which runs 0001..0007 against an empty table, so the
// `INSERT INTO operations ... SELECT ... FROM operations_pre0007` copy never
// moved a single row. That copy is the half that matters on the day this
// slice is actually used: the cutover runbook migrates the owner's LIVE data
// directory, which has operations rows in it. A column dropped or transposed
// in the twenty-column SELECT would lose or corrupt every operation ever
// recorded, and every test in the tree would still be green.
//
// So: build a database at version 6 with the shipped statements, put real
// rows in `operations`, open it with the current binary, and assert the rows
// came through byte for byte -- and that the table is now the D38 one.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// The columns migration 0003 shipped on `operations`, named explicitly so a
// later migration adding one cannot weaken the survival assertion.
var v6OperationColumns = []string{
	"id", "account_id", "kind", "authorization_id", "idempotency_key",
	"request_fingerprint", "conversation_id", "message_id", "tmp_id", "status",
	"terminal", "terminal_at_ms", "corrected_at_ms", "error_code", "error_message",
	"error_retryable", "google_status_raw", "request_payload_json",
	"media_size_bytes", "created_at_ms", "updated_at_ms",
}

func TestMigration0007CarriesAPopulatedOperationsTableForward(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-gm.sqlite3")

	db := rawOpen(t, path)
	for _, m := range migrations {
		if m.version > 6 {
			break
		}
		applyMigration(t, db, m)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seeding: %v\n%s", err, q)
		}
	}
	acct := AccountID("owner-one@example.com")
	exec(`INSERT INTO accounts (id, google_account, state, created_at_ms, updated_at_ms)
	      VALUES (?, 'owner-one@example.com', 'connected', 1700000000000, 1700000000000)`, acct)

	// Three rows that between them touch every nullable column, both terminal
	// states, an error, a media size and a tmp_id -- so a transposed column
	// in the copy shows up as a value in the wrong place rather than as a
	// row count that still matches.
	for i, r := range []struct {
		kind, status string
		terminal     int
		errCode      any
		tmp          any
		media        any
	}{
		{"send_text", "running", 0, nil, "tmp-1", nil},
		{"send_media", "succeeded", 1, nil, nil, int64(4096)},
		{"mark_read", "failed", 1, "phone_not_responding", nil, nil},
	} {
		exec(`INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
		          request_fingerprint, conversation_id, message_id, tmp_id, status, terminal,
		          terminal_at_ms, corrected_at_ms, error_code, error_message, error_retryable,
		          google_status_raw, request_payload_json, media_size_bytes,
		          created_at_ms, updated_at_ms)
		      VALUES (?,?,?,'auth_legacy',?,?,'conv_x','msg_x',?,?,?,?,NULL,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("op_legacy_%d", i), acct, r.kind, fmt.Sprintf("legacy-key-%d", i),
			fmt.Sprintf("fingerprint-%d", i), r.tmp, r.status, r.terminal,
			1700000100000+int64(i), r.errCode, "the phone did not answer", 1, 503,
			`{"text":"marmalade"}`, r.media, 1700000000000+int64(i), 1700000050000+int64(i))
	}

	before := snapshot(t, db, "operations", v6OperationColumns)
	if len(before) != 3 {
		t.Fatalf("seeded %d operations, want 3; the survival assertion would be vacuous", len(before))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the v6 database: %v", err)
	}

	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatalf("opening the v6 database with the current binary: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if v, err := st.Version(); err != nil {
		t.Fatalf("reading user_version: %v", err)
	} else if v != SchemaVersion() {
		t.Fatalf("user_version = %d, want %d", v, SchemaVersion())
	}

	after := rawOpen(t, path)
	got := snapshot(t, after, "operations", v6OperationColumns)
	if len(got) != len(before) {
		t.Fatalf("after 0007 there are %d operations, before there were %d; the copy lost rows",
			len(got), len(before))
	}
	for i := range before {
		if got[i] != before[i] {
			t.Errorf("operation row %d changed across migration 0007:\n before %s\n after  %s",
				i, before[i], got[i])
		}
	}

	// The scaffolding is gone, not left behind holding a second copy of every
	// operation ever recorded.
	var leftover int
	if err := after.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name = 'operations_pre0007'`).Scan(&leftover); err != nil {
		t.Fatalf("looking for the scaffolding table: %v", err)
	}
	if leftover != 0 {
		t.Error("operations_pre0007 survived the migration")
	}

	// And the table is now the D38 one on a database that had rows in it:
	// a keyless insert is accepted, twice, alongside the legacy keyed rows.
	if _, err := after.Exec(
		`INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
		     request_fingerprint, status, terminal, request_payload_json,
		     created_at_ms, updated_at_ms)
		 VALUES ('op_new_1', ?, 'send_text', 'auth_legacy', NULL, 'fp', 'running', 0, '{}', 1, 1)`,
		acct); err != nil {
		t.Fatalf("a keyless operation was refused on a MIGRATED database: %v", err)
	}
	if _, err := after.Exec(
		`INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
		     request_fingerprint, status, terminal, request_payload_json,
		     created_at_ms, updated_at_ms)
		 VALUES ('op_new_2', ?, 'send_text', 'auth_legacy', NULL, 'fp', 'running', 0, '{}', 1, 1)`,
		acct); err != nil {
		t.Fatalf("a SECOND keyless operation was refused on a migrated database: %v\n"+
			"On the owner's live data directory this is a dropped message.", err)
	}
	// The legacy key still collides, so uniqueness came through the rebuild.
	if _, err := after.Exec(
		`INSERT INTO operations (id, account_id, kind, authorization_id, idempotency_key,
		     request_fingerprint, status, terminal, request_payload_json,
		     created_at_ms, updated_at_ms)
		 VALUES ('op_new_3', ?, 'send_text', 'auth_legacy', 'legacy-key-0', 'fp', 'running', 0, '{}', 1, 1)`,
		acct); err == nil {
		t.Error("a legacy idempotency key was accepted a second time after 0007; " +
			"uniqueness did not survive the rebuild and every replay is now a second message")
	}

	if bad := foreignKeyCheck(t, after); len(bad) > 0 {
		t.Errorf("foreign_key_check after 0007: %v", bad)
	}
	var integrity string
	if err := after.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q", integrity)
	}
}
