package media

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisnick/agent-gm/internal/store"
)

// StoreIndex is the CacheIndex backed by `media_cache_entries`, which is the
// **single authority for cached bytes on disk** (spec section 4.2).
//
// It exists because Cache is deliberately written against an interface rather
// than against the store: the eviction order, the LRU tiebreak and the
// erasure order are properties worth testing without a database, and
// internal/media must not hold the store's write lock (spec section 2.2).
// This is the one adapter that closes that seam in production.
//
// The two methods with no counterpart on *store.Store -- Unpinned and Delete
// -- run through Store.Write and Store.Reader, which are the store's own
// exported handles. Nothing here interprets a row; it is transport between
// one table and one interface.
type StoreIndex struct{ Store *store.Store }

// NewStoreIndex builds the adapter.
func NewStoreIndex(s *store.Store) *StoreIndex { return &StoreIndex{Store: s} }

// Put records a cached file, replacing any row for the same attachment: there
// is exactly one row per cached file, which is what makes "no orphan row" a
// well-defined assertion.
func (i *StoreIndex) Put(ctx context.Context, e CacheEntry) error {
	return i.Store.PutMediaCacheEntry(ctx, store.MediaCacheEntry{
		AttachmentID: e.AttachmentID,
		RelativePath: e.RelativePath,
		SizeBytes:    e.SizeBytes,
		Pinned:       e.Pinned,
		LastUsedMS:   e.LastUsedMs,
	})
}

// Get reads one row.
func (i *StoreIndex) Get(ctx context.Context, attachmentID string) (CacheEntry, bool, error) {
	row, err := i.Store.MediaCacheEntry(ctx, attachmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, err
	}
	return CacheEntry{
		AttachmentID: row.AttachmentID,
		RelativePath: row.RelativePath,
		SizeBytes:    row.SizeBytes,
		Pinned:       row.Pinned,
		LastUsedMs:   row.LastUsedMS,
	}, true, nil
}

// Touch moves an entry to the head of the LRU. An LRU that did not touch
// would be an MRU.
func (i *StoreIndex) Touch(ctx context.Context, attachmentID string, _ int64) error {
	return i.Store.TouchMediaCacheEntry(ctx, attachmentID)
}

// SetPinned marks an entry unevictable while it is being read or while a
// ticket for it is outstanding.
func (i *StoreIndex) SetPinned(ctx context.Context, attachmentID string, pinned bool) error {
	return i.Store.SetMediaCachePinned(ctx, attachmentID, pinned)
}

// Unpinned lists evictable entries oldest-use-first, with the total bytes of
// **every** entry including the pinned ones: the budget is a budget for the
// whole cache, and a pinned entry still occupies disk.
func (i *StoreIndex) Unpinned(ctx context.Context) ([]CacheEntry, int64, error) {
	total, err := i.Store.MediaCacheBytes(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := i.Store.Reader().QueryContext(ctx,
		`SELECT attachment_id, relative_path, size_bytes, last_used_ms
		   FROM media_cache_entries WHERE pinned = 0
		  ORDER BY last_used_ms ASC, attachment_id ASC`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.AttachmentID, &e.RelativePath, &e.SizeBytes, &e.LastUsedMs); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Delete removes one row and returns its relative path.
//
// The path is read **before** the delete and inside the same transaction, so
// the caller can unlink the file only after the row is gone. That order is
// the erasure order of spec section 10.3, and it is the opposite of what
// looks natural: deleting the file first and then failing to delete the row
// leaves the index claiming bytes that are not there, while deleting the row
// first and then failing to unlink leaves a file nothing references, which an
// operator can delete. Only the second is recoverable.
func (i *StoreIndex) Delete(ctx context.Context, attachmentID string) (string, error) {
	var rel string
	err := i.Store.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT relative_path FROM media_cache_entries WHERE attachment_id = ?`,
			attachmentID).Scan(&rel)
		if errors.Is(err, sql.ErrNoRows) {
			rel = ""
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM media_cache_entries WHERE attachment_id = ?`, attachmentID)
		return err
	})
	return rel, err
}

// StoreIndex satisfies CacheIndex. The assertion is here rather than in a
// test so a signature change is a compile error at the source.
var _ CacheIndex = (*StoreIndex)(nil)
