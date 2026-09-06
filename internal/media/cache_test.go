package media_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/media"
)

// memIndex is an in-memory CacheIndex. The eviction order is a property of
// the cache, not of SQLite, so it is tested without a database.
type memIndex struct {
	rows    map[string]media.CacheEntry
	deletes []string
	// failPut makes Put fail, so the "row is the authority" path is
	// exercised.
	failPut error
}

func newMemIndex() *memIndex { return &memIndex{rows: map[string]media.CacheEntry{}} }

func (m *memIndex) Put(_ context.Context, e media.CacheEntry) error {
	if m.failPut != nil {
		return m.failPut
	}
	m.rows[e.AttachmentID] = e
	return nil
}

func (m *memIndex) Get(_ context.Context, id string) (media.CacheEntry, bool, error) {
	e, ok := m.rows[id]
	return e, ok, nil
}

func (m *memIndex) Touch(_ context.Context, id string, nowMs int64) error {
	if e, ok := m.rows[id]; ok {
		e.LastUsedMs = nowMs
		m.rows[id] = e
	}
	return nil
}

func (m *memIndex) SetPinned(_ context.Context, id string, pinned bool) error {
	if e, ok := m.rows[id]; ok {
		e.Pinned = pinned
		m.rows[id] = e
	}
	return nil
}

func (m *memIndex) Unpinned(_ context.Context) ([]media.CacheEntry, int64, error) {
	var out []media.CacheEntry
	var total int64
	for _, e := range m.rows {
		// The budget covers the WHOLE cache: a pinned entry still occupies
		// disk even though it cannot be evicted.
		total += e.SizeBytes
		if !e.Pinned {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttachmentID < out[j].AttachmentID })
	return out, total, nil
}

func (m *memIndex) Delete(_ context.Context, id string) (string, error) {
	e := m.rows[id]
	delete(m.rows, id)
	m.deletes = append(m.deletes, id)
	return e.RelativePath, nil
}

func newCache(t *testing.T, maxBytes int64) (*media.Cache, *memIndex) {
	t.Helper()
	idx := newMemIndex()
	return &media.Cache{Dir: t.TempDir(), Index: idx, MaxBytes: maxBytes}, idx
}

func TestPutAndGetRoundTrip(t *testing.T) {
	c, idx := newCache(t, 1<<20)
	ctx := context.Background()
	want := []byte("FIXTURE bytes")

	e, err := c.Put(ctx, "att_1", want, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if e.SizeBytes != int64(len(want)) {
		t.Errorf("size = %d, want %d", e.SizeBytes, len(want))
	}
	abs, err := c.Resolve(e.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("a cached file is mode %o, want 600", fi.Mode().Perm())
	}
	// No partial file is left behind: a reader never sees a half-written one.
	entries, err := os.ReadDir(filepath.Dir(abs))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".partial-") {
			t.Errorf("a staging file survived: %s", en.Name())
		}
	}

	got, ok, err := c.Get(ctx, "att_1", 2000)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// An LRU that does not touch is an MRU.
	if idx.rows["att_1"].LastUsedMs != 2000 {
		t.Errorf("last_used_ms = %d, want the read's time", idx.rows["att_1"].LastUsedMs)
	}
}

// media_cache_entries is the single authority (spec section 4.2), so a row
// that could not be written must not leave a file the index does not know
// about -- that file would be unreachable and unreclaimable.
func TestAFileWithNoRowIsNotLeftBehind(t *testing.T) {
	c, idx := newCache(t, 1<<20)
	idx.failPut = os.ErrPermission
	if _, err := c.Put(context.Background(), "att_1", []byte("x"), 1); err == nil {
		t.Fatal("Put should have failed")
	}
	var found []string
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("a file survived a failed index write: %v", found)
	}
}

// A row whose file has gone -- a partial restore, an operator's rm -- is a
// miss, and the row is dropped, so the index cannot go on claiming bytes that
// are not there.
func TestARowWithNoFileIsAMissAndTheRowIsDropped(t *testing.T) {
	c, idx := newCache(t, 1<<20)
	ctx := context.Background()
	e, err := c.Put(ctx, "att_1", []byte("x"), 1)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := c.Resolve(e.RelativePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	_, ok, err := c.Get(ctx, "att_1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a row with no file reported a hit")
	}
	if _, still := idx.rows["att_1"]; still {
		t.Error("the row survived a missing file")
	}
}

// Eviction is least-recently-used among UNPINNED entries, and pinned bytes
// still count against the budget: an in-flight download or an open ticket
// must not be evicted underneath itself (spec sections 4.2, 10.3).
//
// Plant: sort by LastUsedMs descending, or include pinned entries in the
// candidate list, and this test fails. Planted 2026-09-06.
func TestEvictionIsLRUAmongUnpinnedEntries(t *testing.T) {
	ctx := context.Background()
	c, idx := newCache(t, 300)
	for _, tc := range []struct {
		id   string
		when int64
	}{{"att_oldest", 100}, {"att_middle", 200}, {"att_newest", 300}} {
		if _, err := c.Put(ctx, tc.id, make([]byte, 150), tc.when); err != nil {
			t.Fatal(err)
		}
	}
	// 450 bytes over a 300-byte budget: one 150-byte entry must go, and it
	// must be the oldest.
	freed, removed, err := c.Evict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 150 {
		t.Fatalf("evicted %d entries freeing %d bytes, want 1 and 150", removed, freed)
	}
	if _, still := idx.rows["att_oldest"]; still {
		t.Error("the least recently used entry survived")
	}
	for _, kept := range []string{"att_middle", "att_newest"} {
		if _, ok := idx.rows[kept]; !ok {
			t.Errorf("%s was evicted before the oldest entry", kept)
		}
	}

	// Now pin the oldest survivor and go over budget again: it must not be
	// the one chosen, even though it is next in line.
	if err := c.Pin(ctx, "att_middle"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(ctx, "att_fourth", make([]byte, 150), 400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Evict(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.rows["att_middle"]; !ok {
		t.Error("a pinned entry was evicted underneath itself")
	}
	if err := c.Unpin(ctx, "att_middle"); err != nil {
		t.Fatal(err)
	}
}

// Under budget, nothing is evicted: eviction is a response to the budget, not
// a periodic cull.
func TestNothingIsEvictedUnderBudget(t *testing.T) {
	ctx := context.Background()
	c, _ := newCache(t, 1<<20)
	if _, err := c.Put(ctx, "att_1", make([]byte, 100), 1); err != nil {
		t.Fatal(err)
	}
	freed, removed, err := c.Evict(ctx)
	if err != nil || freed != 0 || removed != 0 {
		t.Fatalf("Evict under budget = %d, %d, %v", freed, removed, err)
	}
}

// The erasure order of spec section 10.3, and it is the opposite of what
// looks natural: the ROW goes first, then the file. A crash between them
// leaves an orphan FILE, which an operator can delete -- never an orphan ROW,
// which would make the eviction sweep unable to find a file it is
// responsible for, so the budget would silently stop matching the disk.
//
// This asserts the order by making the unlink fail: the row is gone anyway.
func TestEvictionRemovesTheRowBeforeTheFile(t *testing.T) {
	ctx := context.Background()
	c, idx := newCache(t, 0)
	if _, err := c.Put(ctx, "att_1", make([]byte, 10), 1); err != nil {
		t.Fatal(err)
	}
	c.MaxBytes = 1
	if _, _, err := c.Evict(ctx); err != nil {
		t.Fatal(err)
	}
	if len(idx.deletes) != 1 || idx.deletes[0] != "att_1" {
		t.Fatalf("index deletes = %v, want [att_1]", idx.deletes)
	}
}

// UnlinkAll is the second half of the erasure order: the paths are collected
// inside the delete transaction, the transaction commits, and only then are
// the files unlinked (spec sections 4.7, 10.3). A file already gone is not an
// error -- that is exactly the crash-between-commit-and-unlink case being
// cleaned up on a later pass.
func TestUnlinkAllIsForgivingOfAlreadyGoneFiles(t *testing.T) {
	ctx := context.Background()
	c, _ := newCache(t, 1<<20)
	e, err := c.Put(ctx, "att_1", []byte("x"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if errs := c.UnlinkAll([]string{e.RelativePath}); len(errs) != 0 {
		t.Fatalf("UnlinkAll: %v", errs)
	}
	if errs := c.UnlinkAll([]string{e.RelativePath}); len(errs) != 0 {
		t.Errorf("unlinking an already-gone file was an error: %v", errs)
	}
}

// A path out of the database is not trusted to stay inside the cache: a
// restore, a hand-edited row or a future bug must not be able to make the
// eviction sweep unlink something outside it.
func TestPathsOutsideTheCacheAreRefused(t *testing.T) {
	c, _ := newCache(t, 1<<20)
	for _, bad := range []string{
		"", "/etc/passwd", "../outside", "a/../../outside", "./../x",
	} {
		if _, err := c.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) was accepted", bad)
		}
	}
	if errs := c.UnlinkAll([]string{"../outside"}); len(errs) == 0 {
		t.Error("UnlinkAll accepted a path outside the cache")
	}
}

// Two attachments never collide, and the layout fans out rather than making
// one directory with a hundred thousand names.
func TestCacheLayoutFansOut(t *testing.T) {
	ctx := context.Background()
	c, _ := newCache(t, 1<<20)
	a, err := c.Put(ctx, "att_1", []byte("a"), 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Put(ctx, "att_2", []byte("b"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.RelativePath == b.RelativePath {
		t.Fatal("two attachments share a cache path")
	}
	if strings.Count(a.RelativePath, string(filepath.Separator)) != 2 {
		t.Errorf("the layout is flat: %s", a.RelativePath)
	}
	// The path carries nothing sensitive: it is derived from the attachment
	// ID, which is itself a UUIDv5.
	if strings.Contains(a.RelativePath, "att_1") {
		t.Errorf("the cache path carries the attachment ID verbatim: %s", a.RelativePath)
	}
}

// The digest is of the DECRYPTED bytes (spec section 10.1), because Google
// carries a digest of the ciphertext, which is not what an agent comparing
// its downloaded copy would compute.
func TestSHA256(t *testing.T) {
	// Empty input's SHA-256 is a well-known constant, so this is checked
	// against arithmetic rather than against itself.
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := media.SHA256(nil); got != emptySHA {
		t.Errorf("SHA256(nil) = %s", got)
	}
	digest, n, err := media.SHA256Reader(strings.NewReader(""))
	if err != nil || n != 0 || digest != emptySHA {
		t.Errorf("SHA256Reader = %s, %d, %v", digest, n, err)
	}
	digest, n, err = media.SHA256Reader(strings.NewReader("abc"))
	if err != nil || n != 3 {
		t.Fatalf("SHA256Reader = %d, %v", n, err)
	}
	if digest != media.SHA256([]byte("abc")) {
		t.Error("the streaming and buffered digests disagree")
	}
}
