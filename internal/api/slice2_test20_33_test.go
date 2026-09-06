package api_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Section 16 Slice 2 tests 20 and 33, which are two halves of one route.
//
// `DELETE /v1/accounts/{account_id}` is **the only route in Agent GM that
// deletes an account's data** (D30). Everything else -- signing out, a
// RevokePairData from the phone, cookie expiry, `account_changed`, an
// abandoned re-pair, `max_concurrent` parking -- deletes zero rows. So this
// route is where the erasure order has to be right, and it is the only place
// an owner's history can be lost.

// TestSlice2_33_RemovePurgesAndOnlyRemove is test 33:
//
//	"`DELETE /v1/accounts/{a}` without `{"confirm": true}` is
//	 `invalid_request` and deletes nothing. With it, all of `a`'s
//	 conversations, messages, attachments, reactions, contacts, operations and
//	 `backfill_state` are gone, its cached media files are unlinked after the
//	 commit, account `b` is untouched, the response carries the row counts and
//	 the effect sentence, and the `account.removed` audit row **survives**
//	 carrying `account_id`."
//
// The surviving audit row is the subtlest part: it names an account that no
// longer exists, which is exactly the point. "What happened to this account?"
// has to stay answerable after the account is gone (spec section 12.4), so
// `audit_events.account_id` carries no foreign key and the erasure does not
// touch the table.
func TestSlice2_33_RemovePurgesAndOnlyRemove(t *testing.T) {
	s := newServer(t)
	accountA := s.addAccount(addressA)
	accountB := s.addAccount(addressB)

	s.seedConversation(accountA, "conv-a")
	s.seedConversation(accountB, "conv-b")
	s.seedMessage(accountA, "conv-a", "m0001", s.Clock.Now(), false)
	s.seedMessage(accountA, "conv-a", "m0002", s.Clock.Now(), false)
	s.seedMessage(accountB, "conv-b", "m0003", s.Clock.Now(), false)
	s.seedAttachment(accountA, "conv-a", "m-att", []byte("bytes for a"), "image/jpeg")

	// Pull the attachment's bytes through the download path so there is a
	// cached file and a media_cache_entries row to erase.
	attsA := attachmentIDs(t, s, accountA)
	if len(attsA) == 0 {
		t.Fatal("account a has no attachment to cache")
	}
	s.call("GET", "/v1/attachments/"+attsA[0], nil).ok(t, 200)

	countA := func(table string) int64 {
		return countRows(t, s.Store, "SELECT COUNT(*) FROM "+table+" WHERE account_id = ?", accountA)
	}
	beforeA := map[string]int64{
		"conversations": countA("conversations"),
		"messages":      countA("messages"),
		"attachments":   countA("attachments"),
	}
	beforeB := countRows(t, s.Store,
		"SELECT COUNT(*) FROM messages WHERE account_id = ?", accountB)
	if beforeA["messages"] == 0 || beforeB == 0 {
		t.Fatalf("the fixture is empty: a has %d messages, b has %d", beforeA["messages"], beforeB)
	}

	// --- without confirm ----------------------------------------------------

	refused := s.call("DELETE", "/v1/accounts/"+accountA, map[string]any{}).
		refused(t, "invalid_request")
	if got := refused.detail("field"); got != "confirm" {
		t.Errorf("details.field is %v, want \"confirm\"", got)
	}
	// A body with confirm:false is the same refusal: `false` is an answer,
	// and the answer is no.
	s.call("DELETE", "/v1/accounts/"+accountA, map[string]any{"confirm": false}).
		refused(t, "invalid_request")
	// And a request with no body at all.
	s.call("DELETE", "/v1/accounts/"+accountA, nil).refused(t, "invalid_request")

	if got := countA("messages"); got != beforeA["messages"] {
		t.Fatalf("a refused removal deleted rows: %d messages left of %d",
			got, beforeA["messages"])
	}
	if _, err := s.Store.Account(context.Background(), accountA); err != nil {
		t.Fatalf("a refused removal deleted the account row: %v", err)
	}

	// --- with confirm -------------------------------------------------------

	env := s.call("DELETE", "/v1/accounts/"+accountA, map[string]any{"confirm": true}).ok(t, 200)

	if env.Data["effect"] != effectAccountRemove {
		t.Errorf("effect is %q, want the section 4.7 sentence %q",
			env.Data["effect"], effectAccountRemove)
	}
	counts, _ := env.Data["deleted_counts"].(map[string]any)
	if counts == nil {
		t.Fatal("the response carries no deleted_counts")
	}
	for table, want := range beforeA {
		got, _ := counts[table].(float64)
		if int64(got) != want {
			t.Errorf("deleted_counts.%s is %v, want %d", table, counts[table], want)
		}
	}

	for _, table := range []string{
		"conversations", "participants", "messages", "attachments",
		"backfill_state", "operations", "contacts",
	} {
		if got := countA(table); got != 0 {
			t.Errorf("%d rows of %s survive account a's removal", got, table)
		}
	}
	if got := countRows(t, s.Store,
		`SELECT COUNT(*) FROM reactions r JOIN messages m ON m.id = r.message_id
		  WHERE m.account_id = ?`, accountA); got != 0 {
		t.Errorf("%d reactions survive account a's removal", got)
	}
	if _, err := s.Store.Account(context.Background(), accountA); err == nil {
		t.Error("account a's own row survives its removal")
	}

	// Account b is untouched. This is the assertion that catches an erasure
	// with a missing account predicate, which would otherwise look like a
	// perfectly successful removal.
	if got := countRows(t, s.Store,
		"SELECT COUNT(*) FROM messages WHERE account_id = ?", accountB); got != beforeB {
		t.Errorf("account b has %d messages after a's removal, had %d", got, beforeB)
	}
	if _, err := s.Store.Account(context.Background(), accountB); err != nil {
		t.Errorf("account b's row was removed with a's: %v", err)
	}

	// The audit row survives, carrying the account_id of an account that no
	// longer exists.
	found := false
	for _, row := range s.auditRows("account.removed") {
		if row.AccountID == accountA {
			found = true
		}
	}
	if !found {
		t.Errorf("no surviving account.removed audit row carries account_id %s", accountA)
	}
}

// TestSlice2_20_ErasureOrderLeavesAnOrphanFileAndNeverAnOrphanRow is test 20:
//
//	"Erasure order, tested through `DELETE /v1/accounts/{id}` specifically,
//	 since section 4.7 makes account removal *the* erasure path: it collects
//	 the cached media paths inside the transaction, commits, then unlinks; a
//	 fault injected between commit and unlink leaves an orphan file and **no**
//	 orphan `media_cache_entries` row."
//
// The order is the opposite of what looks natural, and the reason is that
// only one of the two failure modes is recoverable. The eviction sweep finds
// every file it deletes THROUGH `media_cache_entries`, so:
//
//   - rows first, then files: a crash in between leaves a file nothing
//     references, which an operator can find and delete;
//   - files first, then rows: a crash in between leaves the index claiming
//     bytes that are not on disk, and the cache budget silently stops
//     matching reality for ever.
//
// The fault is injected at exactly the window, through a seam placed where
// the window is rather than anywhere convenient.
func TestSlice2_20_ErasureOrderLeavesAnOrphanFileAndNeverAnOrphanRow(t *testing.T) {
	s := newServer(t)
	accountID := s.addAccount(addressA)
	s.seedConversation(accountID, "conv-a")
	s.seedAttachment(accountID, "conv-a", "m-att", []byte("cached bytes"), "image/jpeg")

	atts := attachmentIDs(t, s, accountID)
	if len(atts) != 1 {
		t.Fatalf("want one attachment, have %d", len(atts))
	}
	// Fetching the metadata computes and caches the digest, which is what
	// pulls the bytes into the media cache and writes the row.
	s.call("GET", "/v1/attachments/"+atts[0], nil).ok(t, 200)

	entry, err := s.Store.MediaCacheEntry(context.Background(), atts[0])
	if err != nil {
		t.Fatalf("the download did not write a media_cache_entries row: %v", err)
	}
	cached := filepath.Join(s.Deps.Cache.Dir, entry.RelativePath)
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("the cached file is not on disk: %v", err)
	}

	// The fault: the transaction has committed and the files are still
	// there. In production nothing runs here.
	injected := errors.New("the process died between the commit and the unlink")
	var collected []string
	s.Deps.AfterErase = func(_ string, paths []string) error {
		collected = append([]string(nil), paths...)
		return injected
	}

	s.call("DELETE", "/v1/accounts/"+accountID, map[string]any{"confirm": true}).
		refused(t, "internal_error")

	// The paths were collected INSIDE the transaction, so they are known
	// after it even though the rows that named them are gone.
	if len(collected) != 1 {
		t.Fatalf("the erasure collected %d cached paths inside the transaction, want 1", len(collected))
	}
	if collected[0] != entry.RelativePath {
		t.Errorf("collected %q, want the cached path %q", collected[0], entry.RelativePath)
	}

	// NO orphan row: the transaction committed, so the row is gone.
	if _, err := s.Store.MediaCacheEntry(context.Background(), atts[0]); err == nil {
		t.Error("a media_cache_entries row survives the committed erasure; " +
			"the rows must go inside the transaction, not after the unlink")
	}
	if got := countRows(t, s.Store, "SELECT COUNT(*) FROM media_cache_entries"); got != 0 {
		t.Errorf("%d media_cache_entries rows survive the erasure", got)
	}
	if got := countRows(t, s.Store,
		"SELECT COUNT(*) FROM attachments WHERE account_id = ?", accountID); got != 0 {
		t.Errorf("%d attachment rows survive the erasure", got)
	}

	// AN orphan file: the unlink never ran. This is the recoverable half, and
	// asserting it is what proves the order rather than merely the outcome.
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the cached file was unlinked despite the fault between the commit "+
			"and the unlink; the order must be rows, commit, files: %v", err)
	}

	// With the fault removed, a well-behaved erasure unlinks. The account is
	// already gone, so this asserts the unlink half on a second account.
	s.Deps.AfterErase = nil
	other := s.addAccount(addressB)
	s.seedConversation(other, "conv-b")
	s.seedAttachment(other, "conv-b", "m-att-b", []byte("more cached bytes"), "image/jpeg")
	otherAtts := attachmentIDs(t, s, other)
	s.call("GET", "/v1/attachments/"+otherAtts[0], nil).ok(t, 200)
	otherEntry, err := s.Store.MediaCacheEntry(context.Background(), otherAtts[0])
	if err != nil {
		t.Fatalf("no cache row for the second account: %v", err)
	}
	otherFile := filepath.Join(s.Deps.Cache.Dir, otherEntry.RelativePath)

	s.call("DELETE", "/v1/accounts/"+other, map[string]any{"confirm": true}).ok(t, 200)
	if _, err := os.Stat(otherFile); !os.IsNotExist(err) {
		t.Errorf("the cached file survives a clean removal: %v", err)
	}
}

// effectAccountRemove is spec section 4.7's exact sentence. It is written out
// here rather than imported so the test would catch a change to the constant
// as well as a change to the handler: the sentence is a promise to an owner,
// and a test that read it from the same place the code does could not tell
// the two apart.
const effectAccountRemove = "permanently deletes Agent GM's copy of this account's conversations, " +
	"messages, attachments and operations; your Google Messages account and the " +
	"messages in it are untouched"

func attachmentIDs(t *testing.T, s *server, accountID string) []string {
	t.Helper()
	rows, err := s.Store.Reader().QueryContext(context.Background(),
		`SELECT id FROM attachments WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		t.Fatalf("listing attachments: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning an attachment id: %v", err)
		}
		out = append(out, id)
	}
	return out
}
