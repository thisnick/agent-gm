package store_test

// Spec sections 4.7, 10.3 and section 16 Slice 2 tests 20 and 33.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// seedFullAccount gives an account one of everything erasure has to remove,
// plus a cached media file on disk.
func seedFullAccount(t *testing.T, st *store.Store, cacheDir, address, convSource string) (accountID, relPath string) {
	t.Helper()
	ctx := context.Background()
	accountID = seedAccount(t, st, address)

	if _, err := st.UpsertContact(ctx, accountID, gm.Contact{
		SourceID: convSource + "-peer", DisplayName: "Alex", PhoneE164: fixturePhoneA,
	}, ""); err != nil {
		t.Fatalf("seeding contact: %v", err)
	}
	f := seedThread(t, st, accountID, convSource, "Alex", fixturePhoneA, false, gm.FolderInbox, "hi", "there")

	attID, err := st.UpsertAttachment(ctx, accountID, f.messages[0], gm.Attachment{
		PartIndex: 0, MediaID: "media-" + convSource, Filename: "IMG_0421.jpg",
		MimeType: "image/jpeg", SizeBytes: 12,
	}, nil, store.DownloadStateAvailable)
	if err != nil {
		t.Fatalf("seeding attachment: %v", err)
	}
	if err := st.ReplaceReactions(ctx, f.messages[0], "", []gm.Reaction{
		{Type: gm.EmojiTypeLike, ParticipantIDs: []string{store.ParticipantID(f.convID, convSource+"-peer")}},
	}); err != nil {
		t.Fatalf("seeding reaction: %v", err)
	}
	if err := st.SetBackfillState(ctx, store.BackfillState{
		ConversationID: f.convID, AccountID: accountID, MessagesDone: 2,
	}); err != nil {
		t.Fatalf("seeding backfill state: %v", err)
	}

	op := store.Operation{
		ID: store.OperationID(), AccountID: accountID, Kind: "send_text",
		AuthorizationID: "auth_1", IdempotencyKey: "k-" + convSource,
		RequestFingerprint: "fp", RequestPayloadJSON: "{}",
	}
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatalf("seeding operation: %v", err)
	}

	relPath = filepath.Join("media", convSource+".jpg")
	full := filepath.Join(cacheDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("creating cache dir: %v", err)
	}
	if err := os.WriteFile(full, []byte("cached bytes"), 0o600); err != nil {
		t.Fatalf("writing cached file: %v", err)
	}
	if err := st.PutMediaCacheEntry(ctx, store.MediaCacheEntry{
		AttachmentID: attID, RelativePath: relPath, SizeBytes: 12,
	}); err != nil {
		t.Fatalf("seeding cache entry: %v", err)
	}
	return accountID, relPath
}

func countRows(t *testing.T, st *store.Store, table, where string, args ...any) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	if err := st.Reader().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// Spec section 16 test 33: remove purges everything of that account's, leaves
// the other account untouched, reports the counts, and the audit row survives.
func TestEraseAccountPurgesOnlyThatAccount(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	cache := t.TempDir()

	one, pathOne := seedFullAccount(t, st, cache, "one@example.com", "c-one")
	two, pathTwo := seedFullAccount(t, st, cache, "two@example.com", "c-two")

	if _, err := st.AppendAudit(ctx, store.AuditEvent{
		Kind: "account.paired", AccountID: one, Result: store.AuditOK,
	}); err != nil {
		t.Fatalf("auditing: %v", err)
	}

	res, err := st.EraseAccount(ctx, one)
	if err != nil {
		t.Fatalf("erasing: %v", err)
	}

	if res.Counts.Conversations != 1 || res.Counts.Messages != 2 ||
		res.Counts.Attachments != 1 || res.Counts.Reactions != 1 ||
		res.Counts.Participants != 2 || res.Counts.BackfillState != 1 ||
		res.Counts.Operations != 1 || res.Counts.Contacts != 1 ||
		res.Counts.MediaCacheEntries != 1 {
		t.Errorf("the reported counts are wrong: %+v", res.Counts)
	}

	// Everything of account one's is gone...
	for _, table := range []string{"conversations", "participants", "messages",
		"attachments", "operations", "contacts", "backfill_state"} {
		if n := countRows(t, st, table, "account_id = ?", one); n != 0 {
			t.Errorf("%d %s rows survived the removal", n, table)
		}
	}
	if n := countRows(t, st, "reactions", ""); n != 1 {
		t.Errorf("%d reaction rows remain, want only account two's", n)
	}
	if _, err := st.Account(ctx, one); !errors.Is(err, store.ErrAccountNotFound) {
		t.Errorf("the account row survived: %v", err)
	}

	// ...and everything of account two's is untouched.
	for _, table := range []string{"conversations", "participants", "messages",
		"attachments", "operations", "contacts", "backfill_state"} {
		if n := countRows(t, st, table, "account_id = ?", two); n == 0 {
			t.Errorf("account two's %s rows were deleted by account one's removal", table)
		}
	}
	if _, err := st.Account(ctx, two); err != nil {
		t.Errorf("account two's row was deleted: %v", err)
	}
	stillCached, err := st.CollectMediaPaths(ctx, two)
	if err != nil {
		t.Fatalf("collecting: %v", err)
	}
	if len(stillCached) != 1 || stillCached[0] != pathTwo {
		t.Errorf("account two's cached media went with account one's: %v", stillCached)
	}

	// The audit row survives, carrying its account_id.
	trail, err := st.ListAuditEvents(ctx, store.AuditQuery{AccountID: one})
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	if len(trail) != 1 || trail[0].AccountID != one {
		t.Fatalf("the audit trail of a removed account did not survive: %+v", trail)
	}

	if len(res.MediaPaths) != 1 || res.MediaPaths[0] != pathOne {
		t.Errorf("the returned media paths are %v, want [%s]", res.MediaPaths, pathOne)
	}
}

// Spec section 16 test 20 and the erasure order of section 10.3: the paths are
// collected inside the transaction and returned; a fault injected between the
// commit and the unlink leaves an orphan FILE and NO orphan
// media_cache_entries row.
func TestErasureCollectsPathsInsideTheTransactionAndUnlinksAfterTheCommit(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	cache := t.TempDir()
	one, rel := seedFullAccount(t, st, cache, "one@example.com", "c-one")
	full := filepath.Join(cache, rel)

	res, err := st.EraseAccount(ctx, one)
	if err != nil {
		t.Fatalf("erasing: %v", err)
	}

	// The fault: the process dies here, between the commit and the unlink.
	// Nothing else runs.

	if len(res.MediaPaths) != 1 || res.MediaPaths[0] != rel {
		t.Fatalf("the paths were not collected inside the transaction: %v", res.MediaPaths)
	}
	if _, err := os.Stat(full); err != nil {
		t.Errorf("the cached file was unlinked before the commit: %v", err)
	}
	if n := countRows(t, st, "media_cache_entries", ""); n != 0 {
		t.Errorf("%d media_cache_entries rows survived the commit; the row must go with the transaction", n)
	}
	// The orphan is a FILE, which an operator can delete, and never a row --
	// the eviction sweep finds every file it deletes through those rows, so
	// an orphan row would make the byte budget silently stop matching disk.
	all, err := st.CollectMediaPaths(ctx, "")
	if err != nil {
		t.Fatalf("collecting: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("an orphan media_cache_entries row remains: %v", all)
	}

	// And when the process does not die, the caller unlinks after the commit.
	for _, p := range res.MediaPaths {
		if err := os.Remove(filepath.Join(cache, p)); err != nil {
			t.Fatalf("unlinking after the commit: %v", err)
		}
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("the cached file survived the unlink: %v", err)
	}
}

// The removal cascades in an order that does not violate a foreign key:
// participants reference contacts, so contacts go after conversations.
func TestErasureLeavesNoForeignKeyViolations(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	cache := t.TempDir()
	one, _ := seedFullAccount(t, st, cache, "one@example.com", "c-one")
	seedFullAccount(t, st, cache, "two@example.com", "c-two")
	if err := st.RelinkAccountContacts(ctx, one); err != nil {
		t.Fatalf("relinking: %v", err)
	}

	if _, err := st.EraseAccount(ctx, one); err != nil {
		t.Fatalf("erasing: %v", err)
	}
	violations, err := st.ForeignKeyCheck(ctx)
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if len(violations) != 0 {
		t.Errorf("removal left foreign key violations: %v", violations)
	}
}

// The effect sentence is defined once, so the CLI can print the route's own
// string byte for byte (spec sections 4.7, 16 test 28).
func TestEffectSentenceIsTheSpecWording(t *testing.T) {
	const want = "permanently deletes Agent GM's copy of this account's conversations, " +
		"messages, attachments and operations; your Google Messages account and the " +
		"messages in it are untouched"
	if store.EffectSentence != want {
		t.Errorf("EffectSentence =\n %q\nwant\n %q", store.EffectSentence, want)
	}
}
