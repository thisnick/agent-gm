package store

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
)

// The live-gate blocker. `agent-gm serve` against the owner's real Slice 1
// data directory exited 9 with
// "migration 0002 (...): constraint failed: FOREIGN KEY constraint failed",
// because Slice 1's ingest wrote Google's own contact ID into
// `participants.contact_id` -- a column that had no REFERENCES clause then --
// and 0002 gives that column a foreign key to a `contacts` table it has just
// created empty. 47 of 222 participants carried one.
//
// The populated-older-database test missed it because its fixture never had
// contact_id set. This one writes the v1 rows through the REAL ingest path
// against the fake, so the fixture is a database Agent GM made rather than
// one a test invented, and then puts the column into the Slice 1 shape.
//
// Plant: copy contact_id straight across in 0002 instead of resolving it and
// this fails at "migrating a v1 database with contact_id set". Planted
// 2026-09-07.
func TestMigration0002SurvivesAV1DatabaseWithContactIDsSet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	path := buildV1WithRealIngest(t, dir)

	// The Slice 1 shape: Google's own contact IDs, which resolve to nothing.
	db := rawOpen(t, path)
	var participants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&participants); err != nil {
		t.Fatal(err)
	}
	if participants == 0 {
		t.Fatal("the real ingest wrote no participants, so this fixture proves nothing")
	}
	var withContact int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM participants WHERE contact_id IS NOT NULL`).Scan(&withContact); err != nil {
		t.Fatal(err)
	}
	if withContact != participants {
		t.Fatalf("the fixture has %d of %d participants carrying a contact_id",
			withContact, participants)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// This is the line that failed on the owner's machine.
	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatalf("migrating a v1 database with contact_id set: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Every participant survived, and the unresolvable links became NULL
	// rather than taking the migration down with them.
	var after, stillSet int
	if err := st.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM participants`).
		Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != participants {
		t.Errorf("%d participants after the migration, %d before", after, participants)
	}
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM participants WHERE contact_id IS NOT NULL`).Scan(&stillSet); err != nil {
		t.Fatal(err)
	}
	if stillSet != 0 {
		t.Errorf("%d participants kept an unresolvable contact_id", stillSet)
	}

	// foreign_key_check is empty, which is what the constraint was for.
	rows, err := st.ForeignKeyCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("foreign_key_check after the migration: %v", rows)
	}

	// And the dropped links were handed to the startup task, not forgotten.
	value, ok, err := st.Meta(ctx, "pending_reprocess")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "reconcile_participants" {
		t.Errorf("pending_reprocess = %q (present=%v); the dropped contact links "+
			"must be handed to the startup task", value, ok)
	}
}

// buildV1WithRealIngest writes participants through the REAL conversation
// ingest path -- store.UpsertConversation, the function core.Ingester calls --
// and then winds the schema back to version 1.
//
// The ingest has to run at the current version, because today's
// UpsertConversation links participants to `contacts`, a table version 1 does
// not have -- which is itself the difference this whole migration exists to
// bridge. So the ROWS come from the real path and the SCHEMA is then put back
// into Slice 1's shape, including a participants table with no REFERENCES
// clause and Google's own contact IDs in the column, which is exactly what
// the owner's database holds.
func buildV1WithRealIngest(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	path := st.Path()

	const address = "v1-contacts@example.test"
	id := AccountID(address)
	if err := st.UpsertAccount(ctx, Account{
		ID: id, GoogleAccount: address, State: "connected",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		name := string(rune('a' + i))
		if _, err := st.UpsertConversation(ctx, id, gm.Conversation{
			SourceID: "conv-" + name, Name: "Fixture " + name,
			Type: gm.ConversationTypeRCS, SendModeRaw: gm.SendModeAuto,
			Folder: gm.FolderInbox, LastActivity: st.clock.Now(),
			Participants: []gm.Participant{
				{SourceID: "me-" + name, IsMe: true, IsVisible: true},
				{SourceID: "them-" + name, PhoneE164: "+120255501" + name + "0",
					DisplayName: "Fixture", IsVisible: true},
			},
		}); err != nil {
			t.Fatalf("ingesting conversation %s: %v", name, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	windBackToV1(t, path)
	return path
}

// windBackToV1 removes everything migrations 0002 onwards created and
// restores the version 1 `participants` shape: no REFERENCES on contact_id.
func windBackToV1(t *testing.T, path string) {
	t.Helper()
	db := rawOpen(t, path)
	defer func() { _ = db.Close() }()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Foreign keys off for the rebuild, exactly as SQLite's own twelve-step
	// procedure requires.
	exec(`PRAGMA foreign_keys = OFF`)
	exec(`CREATE TABLE participants_v1 (
	          id TEXT PRIMARY KEY,
	          account_id TEXT NOT NULL REFERENCES accounts(id),
	          conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	          source_id TEXT NOT NULL,
	          contact_id TEXT,
	          display_name TEXT, first_name TEXT, phone_e164 TEXT,
	          formatted_number TEXT, identifier_type TEXT,
	          is_me INTEGER NOT NULL DEFAULT 0,
	          is_visible INTEGER NOT NULL DEFAULT 1,
	          UNIQUE (conversation_id, source_id))`)
	// Google's own contact IDs, the way Slice 1's ingest wrote them.
	exec(`INSERT INTO participants_v1
	      SELECT id, account_id, conversation_id, source_id, '3063i' || source_id,
	             display_name, first_name, phone_e164, formatted_number,
	             identifier_type, is_me, is_visible FROM participants`)
	exec(`DROP TABLE participants`)
	exec(`ALTER TABLE participants_v1 RENAME TO participants`)
	exec(`CREATE INDEX participants_phone     ON participants(account_id, phone_e164)`)
	exec(`CREATE INDEX participants_phone_all ON participants(phone_e164)`)
	exec(`CREATE INDEX participants_name      ON participants(display_name COLLATE NOCASE)`)
	exec(`CREATE INDEX participants_me        ON participants(account_id, is_me) WHERE is_me = 1`)
	exec(`CREATE INDEX participants_conv      ON participants(conversation_id)`)

	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS messages_fts_insert`,
		`DROP TRIGGER IF EXISTS messages_fts_delete`,
		`DROP TRIGGER IF EXISTS messages_fts_update`,
		`DROP TABLE IF EXISTS messages_fts`,
		`DROP TABLE IF EXISTS media_cache_entries`,
		`DROP TABLE IF EXISTS download_tickets`,
		`DROP TABLE IF EXISTS uploads`,
		`DROP TABLE IF EXISTS backfill_state`,
		`DROP TABLE IF EXISTS operations`,
		`DROP TABLE IF EXISTS reactions`,
		`DROP TABLE IF EXISTS attachments`,
		`DROP TABLE IF EXISTS contacts`,
		`DROP TABLE IF EXISTS audit_events`,
		`DROP TABLE IF EXISTS settings`,
		`DROP TABLE IF EXISTS authorization_codes`,
		`DROP TABLE IF EXISTS authorization_requests`,
		`DROP TABLE IF EXISTS enrollment_codes`,
		`DROP TABLE IF EXISTS oauth_clients`,
		`DROP TABLE IF EXISTS tokens`,
		`DROP TABLE IF EXISTS authorizations`,
		`DROP TABLE IF EXISTS oauth_attempts`,
		`DELETE FROM server_meta`,
		`PRAGMA user_version = 1`,
		`PRAGMA foreign_keys = ON`,
	} {
		exec(stmt)
	}
}
