package store

import (
	"context"
	"database/sql"
	"errors"
)

// Upload reservation states (spec section 10.2).
const (
	UploadReserved = "reserved"
	UploadComplete = "complete"
	UploadConsumed = "consumed"
	UploadExpired  = "expired"
)

// Upload is one staged-bytes reservation.
//
// Uploads carry NO account_id, deliberately: an upl_ reservation is bytes
// staged by an authorization, and the account is fixed at send time by the
// conversation named in the send (spec section 10.2). An upload reserved with
// no account in mind therefore sends into any of them, exactly once.
type Upload struct {
	ID              string
	AuthorizationID string
	IdempotencyKey  string
	Filename        string
	MimeType        string
	SizeBytes       int64
	SHA256Declared  string
	State           string
	TokenHash       string
	Redemptions     int
	StagedPath      string
	ExpiresAtMS     int64
	CreatedAtMS     int64
}

// ErrUploadNotFound is returned when no reservation has that ID or token.
var ErrUploadNotFound = errors.New("upload reservation not found")

// ErrUploadState means the reservation is not in the state this step needs --
// a second PUT against a spent token, or a second send of one upload.
var ErrUploadState = errors.New("upload reservation is not in that state")

const uploadColumns = `id, authorization_id, idempotency_key, filename, mime_type,
	size_bytes, sha256_declared, state, token_hash, redemptions, staged_path,
	expires_at_ms, created_at_ms`

func scanUpload(sc interface{ Scan(...any) error }) (Upload, error) {
	var u Upload
	var key, sha, staged sql.NullString
	err := sc.Scan(&u.ID, &u.AuthorizationID, &key, &u.Filename, &u.MimeType,
		&u.SizeBytes, &sha, &u.State, &u.TokenHash, &u.Redemptions, &staged,
		&u.ExpiresAtMS, &u.CreatedAtMS)
	if err != nil {
		return u, err
	}
	u.IdempotencyKey = key.String
	u.SHA256Declared = sha.String
	u.StagedPath = staged.String
	return u, nil
}

// ReserveUpload writes a new reservation in `reserved`. Only the token's hash
// is stored; the value is signed with the ticket key and leaves the process
// once (spec sections 10.3, 12.1).
func (s *Store) ReserveUpload(ctx context.Context, u Upload) (Upload, error) {
	if u.ID == "" {
		u.ID = UploadID()
	}
	u.State = UploadReserved
	u.CreatedAtMS = s.clock.Now().UnixMilli()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO uploads (id, authorization_id, idempotency_key, filename, mime_type,
			    size_bytes, sha256_declared, state, token_hash, redemptions, staged_path,
			    expires_at_ms, created_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,0,NULL,?,?)`,
			u.ID, u.AuthorizationID, nullString(u.IdempotencyKey), u.Filename, u.MimeType,
			u.SizeBytes, nullString(u.SHA256Declared), u.State, u.TokenHash,
			u.ExpiresAtMS, u.CreatedAtMS)
		return err
	})
	return u, err
}

// Upload reads one reservation by its upl_ ID.
func (s *Store) Upload(ctx context.Context, id string) (Upload, error) {
	u, err := scanUpload(s.read.QueryRowContext(ctx,
		`SELECT `+uploadColumns+` FROM uploads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUploadNotFound
	}
	return u, err
}

// UploadByTokenHash finds the reservation a presented upload token names.
func (s *Store) UploadByTokenHash(ctx context.Context, tokenHash string) (Upload, error) {
	u, err := scanUpload(s.read.QueryRowContext(ctx,
		`SELECT `+uploadColumns+` FROM uploads WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUploadNotFound
	}
	return u, err
}

// UploadByIdempotencyKey finds a caller's earlier reservation for the same
// key, so a repeat returns the same reservation with a fresh token.
func (s *Store) UploadByIdempotencyKey(ctx context.Context, authorizationID, key string) (Upload, error) {
	// A keyless reservation stores NULL and is not addressable by key (D38).
	// The guard is explicit for the same reason it is on OperationByKey: the
	// query below answers "no rows" for the empty key only because `= NULL`
	// is never true, which is right by accident and stops being right the
	// moment somebody rewrites it as `IS ?`.
	if key == "" {
		return Upload{}, ErrUploadNotFound
	}
	u, err := scanUpload(s.read.QueryRowContext(ctx,
		`SELECT `+uploadColumns+` FROM uploads
		  WHERE authorization_id = ? AND idempotency_key = ?`, authorizationID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUploadNotFound
	}
	return u, err
}

// SetUploadToken replaces a reservation's token hash. A repeat reservation
// returns a fresh token because the first token's value left the process and
// cannot be recovered; the new token never outlives the reservation it fills
// (spec section 10.3).
func (s *Store) SetUploadToken(ctx context.Context, uploadID, tokenHash string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE uploads SET token_hash = ? WHERE id = ?`, tokenHash, uploadID)
		return err
	})
}

// CompleteUpload spends the reservation's single redemption and records the
// staged bytes. It moves `reserved` to `complete`, and refuses a second PUT
// with the same token: the redemption is counted whether or not the body was
// acceptable, so a short body, a wrong sha256 or a contradicted content type
// each spend the reservation (spec section 16 test 17). The caller passes
// ok=false for those, which spends the reservation and leaves it `expired`.
func (s *Store) CompleteUpload(ctx context.Context, uploadID, stagedPath string, ok bool) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var state string
		var redemptions int
		err := tx.QueryRowContext(ctx,
			`SELECT state, redemptions FROM uploads WHERE id = ?`, uploadID).Scan(&state, &redemptions)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUploadNotFound
		}
		if err != nil {
			return err
		}
		if state != UploadReserved || redemptions >= 1 {
			return ErrUploadState
		}
		newState := UploadComplete
		var path any = stagedPath
		if !ok {
			newState = UploadExpired
			path = nil
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE uploads SET state = ?, redemptions = redemptions + 1, staged_path = ?
			  WHERE id = ?`, newState, path, uploadID)
		return err
	})
}

// ConsumeUpload moves `complete` to `consumed` when the bytes are sent. A
// second send of the same upload -- into either account -- is refused here,
// which is what makes an account-agnostic reservation safe (section 10.2).
func (s *Store) ConsumeUpload(ctx context.Context, uploadID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE uploads SET state = ? WHERE id = ? AND state = ?`,
			UploadConsumed, uploadID, UploadComplete)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrUploadState
		}
		return nil
	})
}

// ExpireUploads marks every reservation past its expiry. It returns the
// staged paths of the rows it expired so the caller can unlink them after the
// commit, in the erasure order of spec section 10.3.
func (s *Store) ExpireUploads(ctx context.Context) ([]string, error) {
	now := s.clock.Now().UnixMilli()
	var paths []string
	err := s.Write(ctx, func(tx *sql.Tx) error {
		paths = nil
		rows, err := tx.QueryContext(ctx,
			`SELECT staged_path FROM uploads
			  WHERE expires_at_ms <= ? AND state IN ('reserved','complete')
			    AND staged_path IS NOT NULL`, now)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				_ = rows.Close()
				return err
			}
			paths = append(paths, p)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE uploads SET state = ?, staged_path = NULL
			  WHERE expires_at_ms <= ? AND state IN ('reserved','complete')`, UploadExpired, now)
		return err
	})
	return paths, err
}

// --- download tickets --------------------------------------------------------

// DownloadTicket is one minted download authorisation.
//
// Download tickets are stateful because a five-use cap cannot be enforced by a
// signed blob (D24): the counter lives here and is incremented in the same
// transaction that authorises the read (spec section 10.3).
type DownloadTicket struct {
	TokenHash       string
	AttachmentID    string
	AuthorizationID string
	Redemptions     int
	MaxRedemptions  int
	ExpiresAtMS     int64
	CreatedAtMS     int64
}

// ErrTicketNotFound and ErrTicketSpent are the two refusals. The caller turns
// both into the same message: every refusal reads alike, whatever the reason,
// so a status code teaches an attacker nothing (spec section 10.3).
var (
	ErrTicketNotFound = errors.New("download ticket not found")
	ErrTicketSpent    = errors.New("download ticket is spent or expired")
)

// MintDownloadTicket writes a ticket row keyed by the token hash.
func (s *Store) MintDownloadTicket(ctx context.Context, t DownloadTicket) (DownloadTicket, error) {
	if t.MaxRedemptions == 0 {
		t.MaxRedemptions = 5
	}
	t.CreatedAtMS = s.clock.Now().UnixMilli()
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO download_tickets (token_hash, attachment_id, authorization_id,
			    redemptions, max_redemptions, expires_at_ms, created_at_ms)
			VALUES (?,?,?,0,?,?,?)`,
			t.TokenHash, t.AttachmentID, t.AuthorizationID, t.MaxRedemptions,
			t.ExpiresAtMS, t.CreatedAtMS)
		return err
	})
	return t, err
}

// RedeemDownloadTicket increments `redemptions` in the same transaction that
// authorises the read, and refuses the redemption past the cap or past the
// expiry. The counter therefore survives a restart, which is what makes "the
// sixth fails" true across one (spec section 16 test 18).
func (s *Store) RedeemDownloadTicket(ctx context.Context, tokenHash string) (DownloadTicket, error) {
	now := s.clock.Now().UnixMilli()
	var t DownloadTicket
	err := s.Write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT token_hash, attachment_id, authorization_id, redemptions,
			        max_redemptions, expires_at_ms, created_at_ms
			   FROM download_tickets WHERE token_hash = ?`, tokenHash).
			Scan(&t.TokenHash, &t.AttachmentID, &t.AuthorizationID, &t.Redemptions,
				&t.MaxRedemptions, &t.ExpiresAtMS, &t.CreatedAtMS)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if t.ExpiresAtMS <= now || t.Redemptions >= t.MaxRedemptions {
			return ErrTicketSpent
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE download_tickets SET redemptions = redemptions + 1 WHERE token_hash = ?`,
			tokenHash); err != nil {
			return err
		}
		t.Redemptions++
		return nil
	})
	return t, err
}

// DownloadTicketByHash reads a ticket without spending it.
func (s *Store) DownloadTicketByHash(ctx context.Context, tokenHash string) (DownloadTicket, error) {
	var t DownloadTicket
	err := s.read.QueryRowContext(ctx,
		`SELECT token_hash, attachment_id, authorization_id, redemptions,
		        max_redemptions, expires_at_ms, created_at_ms
		   FROM download_tickets WHERE token_hash = ?`, tokenHash).
		Scan(&t.TokenHash, &t.AttachmentID, &t.AuthorizationID, &t.Redemptions,
			&t.MaxRedemptions, &t.ExpiresAtMS, &t.CreatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrTicketNotFound
	}
	return t, err
}

// SweepExpiredTickets deletes tickets past their expiry.
func (s *Store) SweepExpiredTickets(ctx context.Context) (int64, error) {
	now := s.clock.Now().UnixMilli()
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM download_tickets WHERE expires_at_ms <= ?`, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// --- media cache -------------------------------------------------------------

// MediaCacheEntry is one cached file.
//
// media_cache_entries is the single authority for cached bytes: attachments
// carries no cache path and no cached size, so there is exactly one row per
// cached file and "no orphan row" is a well-defined assertion
// (spec section 4.2).
type MediaCacheEntry struct {
	AttachmentID string
	RelativePath string
	SizeBytes    int64
	Pinned       bool
	LastUsedMS   int64
}

// PutMediaCacheEntry inserts or replaces one cached file's row.
func (s *Store) PutMediaCacheEntry(ctx context.Context, e MediaCacheEntry) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO media_cache_entries (attachment_id, relative_path, size_bytes,
			    pinned, last_used_ms)
			VALUES (?,?,?,?,?)
			ON CONFLICT(attachment_id) DO UPDATE SET
			    relative_path = excluded.relative_path,
			    size_bytes    = excluded.size_bytes,
			    pinned        = excluded.pinned,
			    last_used_ms  = excluded.last_used_ms`,
			e.AttachmentID, e.RelativePath, e.SizeBytes, e.Pinned, now)
		return err
	})
}

// TouchMediaCacheEntry moves an entry to the head of the LRU.
func (s *Store) TouchMediaCacheEntry(ctx context.Context, attachmentID string) error {
	now := s.clock.Now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE media_cache_entries SET last_used_ms = ? WHERE attachment_id = ?`,
			now, attachmentID)
		return err
	})
}

// SetMediaCachePinned pins or unpins an entry, so an in-flight download or an
// open ticket is not evicted underneath itself (spec section 4.2).
func (s *Store) SetMediaCachePinned(ctx context.Context, attachmentID string, pinned bool) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE media_cache_entries SET pinned = ? WHERE attachment_id = ?`, pinned, attachmentID)
		return err
	})
}

// MediaCacheEntry reads one entry.
func (s *Store) MediaCacheEntry(ctx context.Context, attachmentID string) (MediaCacheEntry, error) {
	var e MediaCacheEntry
	err := s.read.QueryRowContext(ctx,
		`SELECT attachment_id, relative_path, size_bytes, pinned, last_used_ms
		   FROM media_cache_entries WHERE attachment_id = ?`, attachmentID).
		Scan(&e.AttachmentID, &e.RelativePath, &e.SizeBytes, &e.Pinned, &e.LastUsedMS)
	return e, err
}

// MediaCacheBytes is the cache's current size. The budget is global across
// accounts, not per account: one account's backfill can evict another's
// cached media, and that is fine because the cache is a cache
// (spec section 10.3).
func (s *Store) MediaCacheBytes(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT SUM(size_bytes) FROM media_cache_entries`).Scan(&n)
	return n.Int64, err
}

// EvictMediaCacheLRU deletes least-recently-used unpinned entries until the
// cache fits maxBytes, and returns the relative paths it removed rows for.
//
// The rows go inside the transaction and the files are unlinked by the caller
// after the commit: the eviction sweep finds every file it deletes through
// those rows, so deleting a row without its file leaks a byte budget that
// silently stops matching the disk (spec section 10.3).
func (s *Store) EvictMediaCacheLRU(ctx context.Context, maxBytes int64) ([]string, error) {
	var paths []string
	err := s.Write(ctx, func(tx *sql.Tx) error {
		paths = nil
		var total sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT SUM(size_bytes) FROM media_cache_entries`).Scan(&total); err != nil {
			return err
		}
		remaining := total.Int64
		if remaining <= maxBytes {
			return nil
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT attachment_id, relative_path, size_bytes
			   FROM media_cache_entries WHERE pinned = 0
			  ORDER BY last_used_ms ASC, attachment_id ASC`)
		if err != nil {
			return err
		}
		type victim struct {
			id, path string
			size     int64
		}
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.path, &v.size); err != nil {
				_ = rows.Close()
				return err
			}
			victims = append(victims, v)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, v := range victims {
			if remaining <= maxBytes {
				break
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM media_cache_entries WHERE attachment_id = ?`, v.id); err != nil {
				return err
			}
			remaining -= v.size
			paths = append(paths, v.path)
		}
		return nil
	})
	return paths, err
}

// CollectMediaPaths returns the cached relative paths belonging to one
// account, or to every account when accountID is empty. Erasure calls the
// transaction-scoped form so the paths are collected before the rows go.
func (s *Store) CollectMediaPaths(ctx context.Context, accountID string) ([]string, error) {
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return collectMediaPaths(ctx, tx, accountID)
}

// queryer is what collectMediaPaths needs: it runs on the read pool for a
// plain listing and on the write transaction during erasure.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func collectMediaPaths(ctx context.Context, q queryer, accountID string) ([]string, error) {
	sqlText := `SELECT e.relative_path
	              -- all-accounts: the media cache budget is global across accounts (section 10.3).
	              FROM media_cache_entries e
	              JOIN attachments a ON a.id = e.attachment_id`
	var args []any
	if accountID != "" {
		sqlText += ` WHERE a.account_id = ?`
		args = append(args, accountID)
	}
	sqlText += ` ORDER BY e.relative_path ASC`
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
