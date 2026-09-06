package store_test

// Spec section 10: upload reservations, download tickets and the media cache.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func TestUploadReservationLifecycleIsAccountAgnostic(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)

	u, err := st.ReserveUpload(ctx, store.Upload{
		AuthorizationID: "auth_1", IdempotencyKey: "k1", Filename: "IMG_0421.jpg",
		MimeType: "image/jpeg", SizeBytes: 12, SHA256Declared: "abc",
		TokenHash: "hash-1", ExpiresAtMS: clk.Now().Add(2 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("reserving: %v", err)
	}
	if !store.HasPrefix(u.ID, store.PrefixUpload) {
		t.Errorf("the reservation ID is %s", u.ID)
	}

	// The reservation carries no account: the account is fixed at send time
	// by the conversation named in the send (spec section 10.2).
	var n int
	if err := st.Reader().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('uploads') WHERE name = 'account_id'`).Scan(&n); err != nil {
		t.Fatalf("inspecting uploads: %v", err)
	}
	if n != 0 {
		t.Error("uploads carries an account_id column; it deliberately must not")
	}

	if err := st.CompleteUpload(ctx, u.ID, "staged/abc.jpg", true); err != nil {
		t.Fatalf("completing: %v", err)
	}
	got, err := st.Upload(ctx, u.ID)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got.State != store.UploadComplete || got.Redemptions != 1 || got.StagedPath != "staged/abc.jpg" {
		t.Errorf("after the PUT: %+v", got)
	}

	// A second PUT with the same token fails.
	if err := st.CompleteUpload(ctx, u.ID, "staged/again.jpg", true); !errors.Is(err, store.ErrUploadState) {
		t.Errorf("a second PUT gave %v, want ErrUploadState", err)
	}

	// The send consumes it, and a second send -- into EITHER account -- is
	// refused.
	if err := st.ConsumeUpload(ctx, u.ID); err != nil {
		t.Fatalf("consuming: %v", err)
	}
	if err := st.ConsumeUpload(ctx, u.ID); !errors.Is(err, store.ErrUploadState) {
		t.Errorf("a second send gave %v, want ErrUploadState", err)
	}

	byToken, err := st.UploadByTokenHash(ctx, "hash-1")
	if err != nil || byToken.ID != u.ID {
		t.Errorf("token lookup gave %+v, %v", byToken, err)
	}
	byKey, err := st.UploadByIdempotencyKey(ctx, "auth_1", "k1")
	if err != nil || byKey.ID != u.ID {
		t.Errorf("idempotency lookup gave %+v, %v", byKey, err)
	}
	if _, err := st.UploadByTokenHash(ctx, "nope"); !errors.Is(err, store.ErrUploadNotFound) {
		t.Errorf("an unknown token gave %v", err)
	}
}

// A refused body spends the reservation: a short body, a long body, a wrong
// sha256 and a contradicted content type are each refused AND spend it
// (spec section 16 test 17).
func TestARefusedUploadBodyStillSpendsTheReservation(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	u, err := st.ReserveUpload(ctx, store.Upload{
		AuthorizationID: "auth_1", Filename: "f", MimeType: "image/jpeg", SizeBytes: 12,
		TokenHash: "hash-2", ExpiresAtMS: clk.Now().Add(2 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("reserving: %v", err)
	}
	if err := st.CompleteUpload(ctx, u.ID, "", false); err != nil {
		t.Fatalf("recording the refusal: %v", err)
	}
	got, err := st.Upload(ctx, u.ID)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got.Redemptions != 1 || got.State != store.UploadExpired {
		t.Errorf("a refused body left the reservation %+v", got)
	}
	if err := st.CompleteUpload(ctx, u.ID, "staged/x", true); !errors.Is(err, store.ErrUploadState) {
		t.Errorf("the spent reservation accepted a retry: %v", err)
	}
}

// Spec section 16 test 18: a download ticket redeems 5 times and the 6th
// fails, proven against the counter and ACROSS A PROCESS RESTART.
func TestDownloadTicketRedeemsFiveTimesAcrossARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := clock.NewFake()

	st, err := store.Open(dir, clk)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	acct := seedAccount(t, st, "one@example.com")
	f := seedThread(t, st, acct, "c-a", "Alex", fixturePhoneA, false, gm.FolderInbox, "hi")
	attID, err := st.UpsertAttachment(ctx, acct, f.messages[0], gm.Attachment{
		PartIndex: 0, MediaID: "media-1", MimeType: "image/jpeg", SizeBytes: 12,
	}, nil, store.DownloadStateAvailable)
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	if _, err := st.MintDownloadTicket(ctx, store.DownloadTicket{
		TokenHash: "dt-hash", AttachmentID: attID, AuthorizationID: "auth_1",
		ExpiresAtMS: clk.Now().Add(15 * time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("minting: %v", err)
	}

	for i := 1; i <= 3; i++ {
		got, err := st.RedeemDownloadTicket(ctx, "dt-hash")
		if err != nil {
			t.Fatalf("redemption %d: %v", i, err)
		}
		if got.Redemptions != i {
			t.Errorf("redemption %d recorded %d", i, got.Redemptions)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	// Restart. The counter is a row, not a signed blob, so it survives.
	st, err = store.Open(dir, clk)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i := 4; i <= 5; i++ {
		got, err := st.RedeemDownloadTicket(ctx, "dt-hash")
		if err != nil {
			t.Fatalf("redemption %d after the restart: %v", i, err)
		}
		if got.Redemptions != i {
			t.Errorf("redemption %d recorded %d; the counter did not survive the restart", i, got.Redemptions)
		}
	}
	if _, err := st.RedeemDownloadTicket(ctx, "dt-hash"); !errors.Is(err, store.ErrTicketSpent) {
		t.Errorf("the sixth redemption gave %v, want ErrTicketSpent", err)
	}
	after, err := st.DownloadTicketByHash(ctx, "dt-hash")
	if err != nil {
		t.Fatalf("reading the ticket: %v", err)
	}
	if after.Redemptions != 5 {
		t.Errorf("the refused redemption still incremented the counter: %d", after.Redemptions)
	}

	// Expiry refuses too, and an unknown token is not found.
	clk.Advance(time.Hour)
	if _, err := st.MintDownloadTicket(ctx, store.DownloadTicket{
		TokenHash: "dt-expired", AttachmentID: attID, AuthorizationID: "auth_1",
		ExpiresAtMS: clk.Now().Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("minting an expired ticket: %v", err)
	}
	if _, err := st.RedeemDownloadTicket(ctx, "dt-expired"); !errors.Is(err, store.ErrTicketSpent) {
		t.Errorf("an expired ticket gave %v", err)
	}
	if _, err := st.RedeemDownloadTicket(ctx, "never-minted"); !errors.Is(err, store.ErrTicketNotFound) {
		t.Errorf("an unknown token gave %v", err)
	}
}

// The cache budget is global across accounts, and eviction is LRU among
// UNPINNED entries: an in-flight download is not evicted under itself
// (spec sections 4.2, 10.3).
func TestMediaCacheEvictsLRUAmongUnpinnedOnly(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	one := seedAccount(t, st, "one@example.com")
	two := seedAccount(t, st, "two@example.com")

	type entry struct{ att, path string }
	var entries []entry
	for i, acct := range []string{one, one, two, two} {
		source := fmt.Sprintf("c-%d", i)
		f := seedThread(t, st, acct, source, "Alex", fixturePhoneA, false, gm.FolderInbox, "hi")
		att, err := st.UpsertAttachment(ctx, acct, f.messages[0], gm.Attachment{
			PartIndex: 0, MediaID: "media-" + source, MimeType: "image/jpeg", SizeBytes: 100,
		}, nil, store.DownloadStateAvailable)
		if err != nil {
			t.Fatalf("attaching: %v", err)
		}
		path := source + ".jpg"
		if err := st.PutMediaCacheEntry(ctx, store.MediaCacheEntry{
			AttachmentID: att, RelativePath: path, SizeBytes: 100,
		}); err != nil {
			t.Fatalf("caching: %v", err)
		}
		entries = append(entries, entry{att, path})
		clk.Advance(time.Second)
	}

	total, err := st.MediaCacheBytes(ctx)
	if err != nil {
		t.Fatalf("sizing: %v", err)
	}
	if total != 400 {
		t.Fatalf("cache holds %d bytes, want 400", total)
	}

	// Pin the oldest, then evict down to 200 bytes. The pinned entry survives
	// even though it is the least recently used.
	if err := st.SetMediaCachePinned(ctx, entries[0].att, true); err != nil {
		t.Fatalf("pinning: %v", err)
	}
	// Touching the second-oldest moves it to the head of the LRU.
	clk.Advance(time.Minute)
	if err := st.TouchMediaCacheEntry(ctx, entries[1].att); err != nil {
		t.Fatalf("touching: %v", err)
	}

	evicted, err := st.EvictMediaCacheLRU(ctx, 200)
	if err != nil {
		t.Fatalf("evicting: %v", err)
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %v, want two entries", evicted)
	}
	// One account's backfill can evict another's: the budget is global.
	if evicted[0] != entries[2].path || evicted[1] != entries[3].path {
		t.Errorf("evicted %v, want the two least recently used unpinned entries", evicted)
	}
	if _, err := st.MediaCacheEntry(ctx, entries[0].att); err != nil {
		t.Errorf("the pinned entry was evicted: %v", err)
	}
	if _, err := st.MediaCacheEntry(ctx, entries[1].att); err != nil {
		t.Errorf("the touched entry was evicted: %v", err)
	}

	left, err := st.MediaCacheBytes(ctx)
	if err != nil {
		t.Fatalf("sizing: %v", err)
	}
	if left != 200 {
		t.Errorf("the cache holds %d bytes after eviction, want 200", left)
	}
}

// The attachment decryption key is encrypted at rest and bound to its
// attachment, so a blob copied onto another row fails to open
// (spec section 4.5).
func TestAttachmentKeySealing(t *testing.T) {
	key := testKey(t)
	raw := []byte("a media decryption key")
	sealed, err := store.SealAttachmentKey(key, "att_one", raw)
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	opened, err := store.OpenAttachmentKey(key, "att_one", sealed)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if string(opened) != string(raw) {
		t.Errorf("round trip gave %q", opened)
	}
	if _, err := store.OpenAttachmentKey(key, "att_two", sealed); !errors.Is(err, store.ErrAttachmentKeyUndecryptable) {
		t.Errorf("a key blob opened on another attachment's row: %v", err)
	}
	if string(sealed) == string(raw) {
		t.Error("the key is stored in the clear")
	}
}

func TestExpiredUploadsAreSweptAndReturnTheirStagedPaths(t *testing.T) {
	ctx := context.Background()
	st, clk := newStore(t)
	u, err := st.ReserveUpload(ctx, store.Upload{
		AuthorizationID: "auth_1", Filename: "f", MimeType: "image/jpeg", SizeBytes: 1,
		TokenHash: "hash-3", ExpiresAtMS: clk.Now().Add(2 * time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("reserving: %v", err)
	}
	if err := st.CompleteUpload(ctx, u.ID, "staged/expiring.bin", true); err != nil {
		t.Fatalf("completing: %v", err)
	}

	clk.Advance(3 * time.Hour)
	paths, err := st.ExpireUploads(ctx)
	if err != nil {
		t.Fatalf("expiring: %v", err)
	}
	if len(paths) != 1 || paths[0] != "staged/expiring.bin" {
		t.Fatalf("the sweep returned %v", paths)
	}
	got, err := st.Upload(ctx, u.ID)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got.State != store.UploadExpired || got.StagedPath != "" {
		t.Errorf("the expired reservation is %+v", got)
	}
}
