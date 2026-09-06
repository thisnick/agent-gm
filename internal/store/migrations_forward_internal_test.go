package store

// These tests are in-package because they need `migrations` itself: the point
// is to build a database at an *older* version with the shipped statements
// for that version, not with a hand-copied approximation that could drift
// from what a real 0001 database looks like.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thisnick/agent-gm/internal/clock"
)

func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func applyMigration(t *testing.T, db *sql.DB, m migration) {
	t.Helper()
	for _, stmt := range m.stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("migration %04d (%s) statement failed: %v\n%s", m.version, m.name, err, stmt)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		t.Fatalf("stamping user_version: %v", err)
	}
}

func foreignKeyCheck(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scanning foreign_key_check: %v", err)
		}
		out = append(out, fmt.Sprintf("%s row %d -> %s (fk %d)", table, rowid.Int64, parent, fkid))
	}
	return out
}

// snapshot reads every row of a table as a stable, comparable string, so
// "survives byte for byte" is a real assertion and not a row count.
func snapshot(t *testing.T, db *sql.DB, table string, columns []string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT ` + strings.Join(columns, ", ") + ` FROM ` + table)
	if err != nil {
		t.Fatalf("snapshotting %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		cells := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scanning %s: %v", table, err)
		}
		var sb strings.Builder
		for i, c := range cells {
			fmt.Fprintf(&sb, "%s=%#v|", columns[i], c)
		}
		out = append(out, sb.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	sort.Strings(out)
	return out
}

// The columns migration 0001 shipped. Named explicitly so that a later
// migration adding a column cannot make the survival assertion weaker by
// quietly changing what SELECT * means.
var v1Columns = map[string][]string{
	"server_meta": {"key", "value"},
	"accounts": {"id", "google_account", "label", "state", "state_reason", "phone_id",
		"gaia_dest_reg_uuid", "gaia_device_last_seen_ms", "session_present", "paired_at_ms",
		"last_event_at_ms", "last_sweep_at_ms", "backfill_complete_at_ms",
		"created_at_ms", "updated_at_ms"},
	"conversations": {"id", "account_id", "source_id", "name", "is_group", "conversation_type",
		"send_mode_raw", "folder", "unread", "pinned", "read_only", "force_rcs_eligible",
		"default_outgoing_id", "latest_message_id", "last_activity_ms", "group_avatar_url",
		"sim_payload_json", "deleted_at_ms", "created_at_ms", "updated_at_ms"},
	"participants": {"id", "account_id", "conversation_id", "source_id", "contact_id",
		"display_name", "first_name", "phone_e164", "formatted_number", "identifier_type",
		"is_me", "is_visible"},
	"messages": {"id", "account_id", "conversation_id", "source_id", "kind", "direction",
		"sender_participant", "text", "subject", "delivery_state", "delivery_state_raw",
		"delivery_error", "reply_to_message_id", "operation_id", "tmp_id", "is_deleted",
		"sent_at_ms", "ingested_at_ms", "updated_at_ms", "content_hash"},
}

// populateV1 fills every table migration 0001 declares: two accounts, a
// conversation and participants each, and messages that share a millisecond
// so the tiebreaker is exercised on the far side of the migration too.
func populateV1(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seeding: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO server_meta(key, value) VALUES ('upstream_commit', 'be48a58')`)

	for i, address := range []string{"owner-one@example.com", "owner-two@example.com"} {
		acct := AccountID(address)
		exec(`INSERT INTO accounts (id, google_account, label, state, state_reason, phone_id,
		          gaia_dest_reg_uuid, gaia_device_last_seen_ms, session_present, paired_at_ms,
		          last_event_at_ms, last_sweep_at_ms, backfill_complete_at_ms,
		          created_at_ms, updated_at_ms)
		      VALUES (?,?,?,'connected',NULL,?,?,?,1,?,?,?,?,?,?)`,
			acct, address, fmt.Sprintf("label-%d", i), fmt.Sprintf("%s/%d", address, i),
			fmt.Sprintf("dest-%d", i), 1700000000000+int64(i), 1700000000000+int64(i),
			1700000001000+int64(i), 1700000002000+int64(i), 1700000003000+int64(i),
			1700000000000+int64(i), 1700000000000+int64(i))

		conv := ConversationID(acct, fmt.Sprintf("goog-conv-%d", i))
		exec(`INSERT INTO conversations (id, account_id, source_id, name, is_group,
		          conversation_type, send_mode_raw, folder, unread, pinned, read_only,
		          force_rcs_eligible, default_outgoing_id, latest_message_id, last_activity_ms,
		          group_avatar_url, sim_payload_json, deleted_at_ms, created_at_ms, updated_at_ms)
		      VALUES (?,?,?,?,0,'rcs','SEND_MODE_AUTO','active',1,0,0,1,NULL,NULL,?,NULL,NULL,NULL,?,?)`,
			conv, acct, fmt.Sprintf("goog-conv-%d", i), fmt.Sprintf("Thread %d", i),
			1700000010000+int64(i), 1700000000000, 1700000000000)

		part := ParticipantID(conv, fmt.Sprintf("goog-part-%d", i))
		exec(`INSERT INTO participants (id, account_id, conversation_id, source_id, contact_id,
		          display_name, first_name, phone_e164, formatted_number, identifier_type,
		          is_me, is_visible)
		      VALUES (?,?,?,?,NULL,?,?,?,?,'phone',0,1)`,
			part, acct, conv, fmt.Sprintf("goog-part-%d", i),
			fmt.Sprintf("Person %d", i), "Person",
			// A fictional 555 number, never a real one.
			fmt.Sprintf("+120255510%02d", i), fmt.Sprintf("(202) 555-10%02d", i))

		// Two messages sharing one millisecond: spec section 5.4's collision.
		for j := 0; j < 2; j++ {
			msg := MessageID(acct, fmt.Sprintf("goog-conv-%d", i), fmt.Sprintf("goog-msg-%d-%d", i, j))
			exec(`INSERT INTO messages (id, account_id, conversation_id, source_id, kind,
			          direction, sender_participant, text, subject, delivery_state,
			          delivery_state_raw, delivery_error, reply_to_message_id, operation_id,
			          tmp_id, is_deleted, sent_at_ms, ingested_at_ms, updated_at_ms, content_hash)
			      VALUES (?,?,?,?,'message','incoming',?,?,NULL,'received',100,NULL,NULL,NULL,
			          NULL,0,?,?,?,?)`,
				msg, acct, conv, fmt.Sprintf("goog-msg-%d-%d", i, j), part,
				fmt.Sprintf("marmalade sandwich number %d", j),
				1700000010000, 1700000011000, 1700000011000,
				fmt.Sprintf("hash-%d-%d", i, j))
		}
	}
}

// Spec section 16 Slice 2 test 42, and the migration-forward half of section
// 4.3: an older database with real rows in it comes forward intact.
func TestMigrationForwardOnAPopulatedV1Database(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-gm.sqlite3")

	db := rawOpen(t, path)
	applyMigration(t, db, migration0001)
	populateV1(t, db)

	tables := []string{"server_meta", "accounts", "conversations", "participants", "messages"}
	before := map[string][]string{}
	for _, table := range tables {
		before[table] = snapshot(t, db, table, v1Columns[table])
		if len(before[table]) == 0 {
			t.Fatalf("%s was not populated, so the survival assertion would be vacuous", table)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the v1 database: %v", err)
	}

	// Now open it with the current binary.
	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatalf("opening the v1 database with the current binary: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	version, err := st.Version()
	if err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != SchemaVersion() || version < 2 {
		t.Fatalf("user_version = %d, want %d (migration 0002 did not run)", version, SchemaVersion())
	}

	after := rawOpen(t, path)
	for _, table := range tables {
		got := snapshot(t, after, table, v1Columns[table])
		want := before[table]
		if len(got) != len(want) {
			t.Errorf("%s: %d rows after the migration, %d before", table, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s row changed across the migration:\n before %s\n  after %s",
					table, want[i], got[i])
			}
		}
	}

	if violations := foreignKeyCheck(t, after); len(violations) != 0 {
		t.Errorf("foreign_key_check after migrating a populated database: %v", violations)
	}

	// The migration backfilled the pre-existing rows into messages_fts, so
	// history written before search existed is searchable.
	ctx := context.Background()
	results, err := st.SearchMessages(ctx, SearchQuery{Q: "marmalade", AllAccounts: true})
	if err != nil {
		t.Fatalf("searching migrated rows: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("search found %d pre-existing messages, want 4 -- migration 0002 did not backfill messages_fts", len(results))
	}
}

// Spec section 4.3 and section 16 test 42: refusing a database from the
// future, naming both numbers.
func TestDatabaseAtAHigherUserVersionRefusesToOpenNamingBoth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-gm.sqlite3")
	db := rawOpen(t, path)
	future := SchemaVersion() + 7
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		t.Fatalf("stamping a future user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	_, err := Open(dir, clock.NewFake())
	if err == nil {
		t.Fatal("a database from the future opened; it must refuse")
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprint(future), fmt.Sprint(SchemaVersion())} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %s: %s", want, msg)
		}
	}
}

// Spec section 4.3: PRAGMA foreign_key_check is empty after EVERY migration,
// asserted per migration rather than once at the end -- a violation
// introduced by 0002 and repaired by 0003 would otherwise never be seen.
func TestForeignKeyCheckIsEmptyAfterEachMigration(t *testing.T) {
	dir := t.TempDir()
	db := rawOpen(t, filepath.Join(dir, "per-migration.sqlite3"))
	for _, m := range migrations {
		applyMigration(t, db, m)
		if m.version == 1 {
			// Rows first, so the later migrations have something to violate.
			populateV1(t, db)
		}
		if violations := foreignKeyCheck(t, db); len(violations) != 0 {
			t.Errorf("migration %04d (%s) left foreign key violations: %v",
				m.version, m.name, violations)
		}
		var v int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatalf("reading user_version: %v", err)
		}
		if v != m.version {
			t.Errorf("after migration %04d user_version = %d", m.version, v)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "per-migration.sqlite3")); err != nil {
		t.Fatalf("the database file is missing: %v", err)
	}
}
