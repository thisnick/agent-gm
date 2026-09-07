package store

import (
	"context"
	"testing"

	"github.com/thisnick/agent-gm/internal/clock"
)

// R-12. The R-4 fix changed what `messages.sender_participant` and
// `reactions.participant_id` hold, and shipped without a migration. A
// database written before it -- the owner's own Slice 1 data directory is one
// -- keeps Google's raw participant IDs there, and nothing announces it:
// `sender=me` returns an empty page, which is a valid answer, and a stored
// `react_` ID disagrees with the one re-derived from the same reaction.
//
// This builds exactly that database at user_version 3, opens it with the
// current binary, and asserts the rewrite.
//
// Plant: drop either UPDATE from migration0004 and this fails naming the
// column that still holds a raw ID. Planted 2026-09-07.
func TestMigration0004RewritesRawParticipantIDs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A v3 database, populated the way the pre-fix code populated one.
	db := rawOpen(t, buildV3WithRawParticipants(t, dir))
	defer func() { _ = db.Close() }()

	var rawSender, rawReaction string
	if err := db.QueryRow(`SELECT sender_participant FROM messages WHERE id = 'msg-1'`).
		Scan(&rawSender); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT participant_id FROM reactions WHERE id = 'react-1'`).
		Scan(&rawReaction); err != nil {
		t.Fatal(err)
	}
	if rawSender != "them" || rawReaction != "them" {
		t.Fatalf("the fixture is not the pre-fix shape: %q / %q", rawSender, rawReaction)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatalf("opening the v3 database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	want := ParticipantID("conv-1", "them")

	var gotSender, gotReaction string
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT sender_participant FROM messages WHERE id = 'msg-1'`).Scan(&gotSender); err != nil {
		t.Fatal(err)
	}
	if gotSender != want {
		t.Errorf("messages.sender_participant = %q, want the derived %q -- a raw Google "+
			"participant ID here makes every sender filter return an empty page",
			gotSender, want)
	}
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT participant_id FROM reactions WHERE id = 'react-1'`).Scan(&gotReaction); err != nil {
		t.Fatal(err)
	}
	if gotReaction != want {
		t.Errorf("reactions.participant_id = %q, want the derived %q", gotReaction, want)
	}

	// A participant that was never ingested has nothing to join against, so
	// its message keeps the raw value -- and the migration says so through
	// pending_reprocess rather than pretending it converted everything.
	var orphan string
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT sender_participant FROM messages WHERE id = 'msg-2'`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if orphan != "never-ingested" {
		t.Errorf("an unresolvable sender became %q; it must be left for the sweep", orphan)
	}
	pending, ok, err := st.Meta(ctx, "pending_reprocess")
	if err != nil || !ok {
		t.Fatalf("pending_reprocess is not set: %v (ok=%v)", err, ok)
	}
	if pending != "reconcile_participants" {
		t.Errorf("pending_reprocess = %q", pending)
	}
}

// buildV3WithRawParticipants writes a database at user_version 3 holding the
// raw participant IDs the pre-fix code wrote, and returns its path.
//
// It writes the rows with raw SQL rather than through the store's own upsert
// functions, because those now DERIVE the ID -- using them would produce the
// post-fix shape and the test would pass without the migration doing
// anything.
func buildV3WithRawParticipants(t *testing.T, dir string) string {
	t.Helper()

	// Migrate to the current version first, then wind user_version back to 3
	// and rewrite the two columns to the pre-fix values. That keeps the
	// schema real -- the fixture is a database this binary made -- while the
	// DATA is the old shape, which is exactly the situation an upgrade meets.
	st, err := Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	db := rawOpen(t, path)
	defer func() { _ = db.Close() }()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO accounts (id, google_account, state, created_at_ms, updated_at_ms)
	      VALUES ('acct-1','fixture@example.test','connected',1,1)`)
	exec(`INSERT INTO conversations (id, account_id, source_id, conversation_type,
	          send_mode_raw, last_activity_ms, created_at_ms, updated_at_ms)
	      VALUES ('conv-1','acct-1','conv-src','rcs','SEND_MODE_AUTO',1,1,1)`)
	exec(`INSERT INTO participants (id, account_id, conversation_id, source_id, phone_e164, is_me)
	      VALUES (?, 'acct-1','conv-1','them','+12025550123',0)`, ParticipantID("conv-1", "them"))
	// The pre-fix shape: Google's own participant ID in both columns.
	exec(`INSERT INTO messages (id, account_id, conversation_id, source_id, kind, direction,
	          sender_participant, delivery_state, delivery_state_raw, sent_at_ms,
	          ingested_at_ms, updated_at_ms, content_hash)
	      VALUES ('msg-1','acct-1','conv-1','msg-src-1','message','incoming','them',
	              'received',100,1,1,1,'h1')`)
	exec(`INSERT INTO messages (id, account_id, conversation_id, source_id, kind, direction,
	          sender_participant, delivery_state, delivery_state_raw, sent_at_ms,
	          ingested_at_ms, updated_at_ms, content_hash)
	      VALUES ('msg-2','acct-1','conv-1','msg-src-2','message','incoming','never-ingested',
	              'received',100,2,1,1,'h2')`)
	exec(`INSERT INTO reactions (id, message_id, participant_id, emoji, emoji_type, updated_at_ms)
	      VALUES ('react-1','msg-1','them','👍','like',1)`)
	// A v3 database genuinely lacks every column a later migration adds, and
	// winding the stamp back without winding the SCHEMA back would make
	// migration 0005 fail on a duplicate column -- which is the fixture
	// lying, not the migration.
	exec(`ALTER TABLE operations DROP COLUMN media_size_bytes`)
	exec(`PRAGMA user_version = 3`)
	return path
}
