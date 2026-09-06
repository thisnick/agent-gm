package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CacheEntry is one row of `media_cache_entries`, which is the **single
// authority for cached bytes on disk** (spec section 4.2). `attachments`
// carries no cache path and no cached size: the eviction sweep, the LRU
// order, the byte budget and the purge path all read this table and nothing
// else, so there is exactly one row per cached file and "no orphan row" is a
// well-defined assertion.
type CacheEntry struct {
	AttachmentID string
	RelativePath string
	SizeBytes    int64
	// Pinned keeps an in-flight download or an open ticket from being
	// evicted underneath itself.
	Pinned     bool
	LastUsedMs int64
}

// CacheIndex is the persistence the cache needs. It is an interface so this
// package does not hold the store's write lock (spec section 2.2) and so the
// eviction order is testable without a database.
type CacheIndex interface {
	// Put records a cached file. It replaces any row for the same
	// attachment, because there is exactly one row per cached file.
	Put(ctx context.Context, e CacheEntry) error
	// Get reads one row.
	Get(ctx context.Context, attachmentID string) (CacheEntry, bool, error)
	// Touch updates last_used_ms. An LRU that did not is an MRU.
	Touch(ctx context.Context, attachmentID string, nowMs int64) error
	// SetPinned marks an entry unevictable while it is being read or while a
	// ticket for it is outstanding.
	SetPinned(ctx context.Context, attachmentID string, pinned bool) error
	// Unpinned lists evictable entries, oldest use first, with the total
	// bytes of every entry including the pinned ones -- the budget is a
	// budget for the whole cache, and a pinned entry still occupies disk.
	Unpinned(ctx context.Context) (entries []CacheEntry, totalBytes int64, err error)
	// Delete removes one row and returns its relative path, so the caller
	// unlinks the file only after the row is gone.
	Delete(ctx context.Context, attachmentID string) (relativePath string, err error)
}

// Cache is the media cache on disk.
//
// **It is a cache.** The bytes are always re-fetchable from Google, the
// budget is global across accounts rather than per account, and one account's
// backfill may evict another's cached media (spec section 10.3). Nothing here
// is a system of record.
type Cache struct {
	// Dir is <data_dir>/media-cache.
	Dir string
	// Index is the single authority for what is in it.
	Index CacheIndex
	// MaxBytes is `media.cache_max_bytes`, 2 GiB by default.
	MaxBytes int64
}

// ErrOutsideCache means a stored relative path escaped the cache directory.
// A path from the database is not trusted to stay inside it: a restore, a
// hand-edited row or a future bug should not be able to make the eviction
// sweep unlink something outside the cache.
var ErrOutsideCache = errors.New("the cached path is outside the media cache directory")

// relativePathFor spreads files over two levels of fan-out, so a cache with
// a hundred thousand entries does not become one directory with a hundred
// thousand names. The name is derived from the attachment ID, which is
// already a UUIDv5 and carries nothing sensitive.
func relativePathFor(attachmentID string) string {
	sum := sha256.Sum256([]byte(attachmentID))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(h[0:2], h[2:4], h)
}

// Resolve turns a stored relative path into an absolute one, refusing
// anything that escapes the cache directory.
func (c *Cache) Resolve(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", ErrOutsideCache
	}
	abs := filepath.Join(c.Dir, filepath.Clean(relative))
	root := filepath.Clean(c.Dir) + string(os.PathSeparator)
	if !strings.HasPrefix(abs+string(os.PathSeparator), root) {
		return "", ErrOutsideCache
	}
	return abs, nil
}

// Put writes bytes into the cache and records the row.
//
// The file is written to a temporary name and renamed into place, so a reader
// never sees a half-written file, and the row is written **after** the rename:
// a row pointing at a file that is not there yet would make the cache lie
// about what it holds, while a file with no row is merely an orphan an
// operator can delete.
func (c *Cache) Put(ctx context.Context, attachmentID string, data []byte, nowMs int64) (CacheEntry, error) {
	rel := relativePathFor(attachmentID)
	abs, err := c.Resolve(rel)
	if err != nil {
		return CacheEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return CacheEntry{}, fmt.Errorf("creating the cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".partial-*")
	if err != nil {
		return CacheEntry{}, fmt.Errorf("staging a cached file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return CacheEntry{}, fmt.Errorf("writing a cached file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return CacheEntry{}, err
	}
	if err := tmp.Close(); err != nil {
		return CacheEntry{}, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return CacheEntry{}, fmt.Errorf("publishing a cached file: %w", err)
	}
	e := CacheEntry{
		AttachmentID: attachmentID,
		RelativePath: rel,
		SizeBytes:    int64(len(data)),
		LastUsedMs:   nowMs,
	}
	if err := c.Index.Put(ctx, e); err != nil {
		// The row is the authority. Without it the file is unreachable and
		// unreclaimable, so it is removed rather than left behind.
		_ = os.Remove(abs)
		return CacheEntry{}, err
	}
	return e, nil
}

// Get reads cached bytes and touches the entry. A row whose file has gone --
// a partial restore, an operator's `rm` -- is treated as a miss and the row
// is dropped, so the index cannot go on claiming bytes that are not there.
func (c *Cache) Get(ctx context.Context, attachmentID string, nowMs int64) ([]byte, bool, error) {
	e, ok, err := c.Index.Get(ctx, attachmentID)
	if err != nil || !ok {
		return nil, false, err
	}
	abs, err := c.Resolve(e.RelativePath)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		if _, delErr := c.Index.Delete(ctx, attachmentID); delErr != nil {
			return nil, false, delErr
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := c.Index.Touch(ctx, attachmentID, nowMs); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Pin and Unpin guard an entry that is being read or that has an outstanding
// ticket, so the eviction sweep cannot delete it underneath itself.
func (c *Cache) Pin(ctx context.Context, attachmentID string) error {
	return c.Index.SetPinned(ctx, attachmentID, true)
}

// Unpin releases the guard.
func (c *Cache) Unpin(ctx context.Context, attachmentID string) error {
	return c.Index.SetPinned(ctx, attachmentID, false)
}

// Evict brings the cache under its byte budget by removing the
// least-recently-used **unpinned** entries.
//
// The order of the two removals is the erasure order of spec section 10.3,
// and it is the opposite of what looks natural: the ROW goes first, then the
// file. Deleting the file first and then failing to delete the row leaves the
// index claiming bytes that are not there; deleting the row first and then
// failing to unlink leaves a file nothing references, which an operator can
// delete. Only the second of those is recoverable.
func (c *Cache) Evict(ctx context.Context) (freed int64, removed int, err error) {
	entries, total, err := c.Index.Unpinned(ctx)
	if err != nil {
		return 0, 0, err
	}
	if c.MaxBytes <= 0 || total <= c.MaxBytes {
		return 0, 0, nil
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].LastUsedMs != entries[j].LastUsedMs {
			return entries[i].LastUsedMs < entries[j].LastUsedMs
		}
		// A stable tiebreak, so two entries used in the same millisecond are
		// evicted in a defined order rather than whichever the index
		// happened to return first.
		return entries[i].AttachmentID < entries[j].AttachmentID
	})
	for _, e := range entries {
		if total-freed <= c.MaxBytes {
			break
		}
		rel, err := c.Index.Delete(ctx, e.AttachmentID)
		if err != nil {
			return freed, removed, err
		}
		abs, err := c.Resolve(rel)
		if err != nil {
			return freed, removed, err
		}
		// A file that will not unlink is logged by the caller and left,
		// rather than failing an eviction that has already freed the budget
		// as far as the index is concerned.
		_ = os.Remove(abs)
		freed += e.SizeBytes
		removed++
	}
	return freed, removed, nil
}

// UnlinkAll removes files by relative path, after the transaction that
// deleted their rows has committed.
//
// This is the second half of the erasure order (spec sections 4.7, 10.3): the
// paths are collected INSIDE the delete transaction, the transaction commits,
// and only then are the files unlinked. A crash between the commit and the
// unlink leaves a file nothing references, which an operator can delete; the
// other order would leave the eviction sweep unable to find a file it is
// responsible for, and the cache budget would silently stop matching the
// disk.
func (c *Cache) UnlinkAll(paths []string) []error {
	var errs []error
	for _, rel := range paths {
		abs, err := c.Resolve(rel)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errs
}

// SHA256 is the digest of the **decrypted** bytes (spec section 10.1),
// because Google carries a digest of the ciphertext, which is not what an
// agent comparing its downloaded copy would compute.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256Reader is SHA256 over a stream, for a body being verified as it
// arrives rather than after it is all in memory.
func SHA256Reader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
