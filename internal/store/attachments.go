package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Attachment is one media part of one message.
type Attachment struct {
	ID               string
	AccountID        string
	MessageID        string
	PartIndex        int
	MediaID          string
	ThumbnailMediaID string
	// DecryptionKey is the sealed column value, not the plaintext. Open it
	// with OpenAttachmentKey; it is encrypted at rest under the data key
	// (spec section 4.5).
	DecryptionKey []byte
	Filename      string
	MimeType      string
	MediaFormat   string
	SizeBytes     int64
	Width         int64
	Height        int64
	DownloadState string
	SHA256        string
}

// Download states for attachments.
const (
	DownloadStateAvailable   = "available"
	DownloadStatePending     = "pending"
	DownloadStateFailed      = "failed"
	DownloadStateUnavailable = "unavailable"
)

// ErrAttachmentNotFound is returned when no attachment has that ID.
var ErrAttachmentNotFound = errors.New("attachment not found")

const attachmentColumns = `id, account_id, message_id, part_index, media_id,
	thumbnail_media_id, decryption_key, filename, mime_type, media_format,
	size_bytes, width, height, download_state, sha256`

func scanAttachment(sc interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	var media, thumb, filename, mime, format, sha sql.NullString
	var size, width, height sql.NullInt64
	err := sc.Scan(&a.ID, &a.AccountID, &a.MessageID, &a.PartIndex, &media, &thumb,
		&a.DecryptionKey, &filename, &mime, &format, &size, &width, &height,
		&a.DownloadState, &sha)
	if err != nil {
		return a, err
	}
	a.MediaID = media.String
	a.ThumbnailMediaID = thumb.String
	a.Filename = filename.String
	a.MimeType = mime.String
	a.MediaFormat = format.String
	a.SizeBytes = size.Int64
	a.Width = width.Int64
	a.Height = height.Int64
	a.SHA256 = sha.String
	return a, nil
}

// UpsertAttachment writes one media part under its message, stamping
// account_id on the row so a cross-account attachment listing is
// attributable. sealedKey is the already-sealed decryption key, or nil.
func (s *Store) UpsertAttachment(ctx context.Context, accountID, messageID string, a gm.Attachment, sealedKey []byte, downloadState string) (string, error) {
	id := AttachmentID(messageID, strconv.Itoa(a.PartIndex), a.MediaID)
	if downloadState == "" {
		downloadState = DownloadStatePending
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO attachments (id, account_id, message_id, part_index, media_id,
			    thumbnail_media_id, decryption_key, filename, mime_type, media_format,
			    size_bytes, width, height, download_state, sha256)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
			ON CONFLICT(id) DO UPDATE SET
			    media_id           = excluded.media_id,
			    thumbnail_media_id = excluded.thumbnail_media_id,
			    decryption_key     = COALESCE(excluded.decryption_key, attachments.decryption_key),
			    filename           = excluded.filename,
			    mime_type          = excluded.mime_type,
			    media_format       = excluded.media_format,
			    -- A later echo that carries no size must not erase a size
			    -- already known: Google's echo of our own outgoing media
			    -- carries none at all (migration 0005).
			    size_bytes         = COALESCE(excluded.size_bytes, attachments.size_bytes),
			    width              = excluded.width,
			    height             = excluded.height,
			    download_state     = excluded.download_state`,
			id, accountID, messageID, a.PartIndex, nullString(a.MediaID),
			nullString(a.ThumbnailMediaID), nullBlob(sealedKey), nullString(a.Filename),
			nullString(a.MimeType), nullString(a.MediaFormat), nullInt(a.SizeBytes),
			nullInt(a.Width), nullInt(a.Height), downloadState)
		return err
	})
	return id, err
}

// FillMissingAttachmentSize writes a plaintext size onto this message's
// attachments that have none.
//
// It exists because Google's echo of media WE sent carries a MediaContent
// with no Size, so the attachment row is written sizeless while the byte
// count is sitting on the operation that sent it (migration 0005). A size
// already on the row wins: Google's own number for an incoming attachment is
// never second-guessed, and this is a fill, not a correction.
func (s *Store) FillMissingAttachmentSize(ctx context.Context, messageID string, size int64) error {
	if size <= 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE attachments SET size_bytes = ?
			 -- all-accounts: a msg_ ID already carries its account (section 4.1).
			 WHERE message_id = ? AND (size_bytes IS NULL OR size_bytes = 0)`,
			size, messageID)
		return err
	})
}

// SetAttachmentDownloadState records the outcome of a fetch, with the SHA-256
// of the bytes when there are any.
func (s *Store) SetAttachmentDownloadState(ctx context.Context, attachmentID, state, sha256hex string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE attachments SET download_state = ?, sha256 = COALESCE(?, sha256)
			 -- all-accounts: an att_ ID already carries its account (section 4.1).
			 WHERE id = ?`, state, nullString(sha256hex), attachmentID)
		return err
	})
}

// Attachment reads one attachment by its att_ ID.
func (s *Store) Attachment(ctx context.Context, id string) (Attachment, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+attachmentColumns+`
		   -- all-accounts: an att_ ID already carries its account (section 4.1).
		   FROM attachments WHERE id = ?`, id)
	a, err := scanAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrAttachmentNotFound
	}
	return a, err
}

// AttachmentsForMessage lists one message's media parts in part order.
func (s *Store) AttachmentsForMessage(ctx context.Context, messageID string) ([]Attachment, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+attachmentColumns+`
		   -- all-accounts: scoped by message_id, which carries the account (section 4.1).
		   FROM attachments WHERE message_id = ? ORDER BY part_index ASC`, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SealAttachmentKey encrypts a media decryption key for the
// attachments.decryption_key column, under the data key derived for
// agent-gm/attachment-key/v1 with the att_ ID as associated data, so a key
// blob copied onto another attachment's row fails to open (spec section 4.5).
func SealAttachmentKey(dk DataKey, attachmentID string, raw []byte) ([]byte, error) {
	aead, err := attachmentAEAD(dk)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, raw, []byte(InfoAttachmentKey+"|"+attachmentID))...), nil
}

// ErrAttachmentKeyUndecryptable means the data key differs from the one that
// sealed this column, exactly as for a session envelope.
var ErrAttachmentKeyUndecryptable = errors.New("attachment key cannot be decrypted")

// OpenAttachmentKey reverses SealAttachmentKey.
func OpenAttachmentKey(dk DataKey, attachmentID string, sealed []byte) ([]byte, error) {
	if len(sealed) < chacha20poly1305.NonceSizeX {
		return nil, ErrAttachmentKeyUndecryptable
	}
	aead, err := attachmentAEAD(dk)
	if err != nil {
		return nil, err
	}
	out, err := aead.Open(nil, sealed[:chacha20poly1305.NonceSizeX],
		sealed[chacha20poly1305.NonceSizeX:], []byte(InfoAttachmentKey+"|"+attachmentID))
	if err != nil {
		return nil, ErrAttachmentKeyUndecryptable
	}
	return out, nil
}

func attachmentAEAD(dk DataKey) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}, error) {
	sub, err := dk.Derive(InfoAttachmentKey)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(sub)
}

func nullBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// DownloadStateFor decides an attachment's download_state from the
// attachment's own facts, rather than from a constant a writer chose.
//
// It is a FACT ABOUT REACHABILITY, not about the cache: `available` means
// Agent GM can produce the bytes on demand -- from `media_cache_entries` if
// they are there and from `libgm.DownloadMedia` otherwise -- and the cache is
// its own table for exactly that reason (spec section 4.2).
//
// Writing `pending` unconditionally at ingest, as this used to, made
// `GET /v1/attachments/{id}/content` unreachable for EVERY real attachment:
// the route answers `unsupported_capability` / `media_pending` for anything
// not `available`, `sha256` was therefore always null, and every download
// ticket minted could never be redeemed. Nothing in production could move an
// attachment out of `pending`, so the bytes a caller had just uploaded came
// back as not-yet-downloaded.
//
// The three cases are section 10.1's:
//
//   - a media ID → `available`. Google holds the bytes and the decryption key
//     is on the row.
//   - no media ID but a thumbnail → `pending`: `GetFullSizeImage` has to run
//     first, and while that is outstanding the route says `media_pending`,
//     which is what section 10.1 asks for.
//   - neither → `unavailable`. There is nothing to fetch and never will be;
//     saying `pending` would promise bytes that are not coming.
func DownloadStateFor(mediaID, thumbnailMediaID string) string {
	return DownloadStateForMessage(mediaID, thumbnailMediaID, "")
}

// DownloadStateForMessage is DownloadStateFor with the owning message's
// delivery state, which can veto the other two.
//
// Section 4.4 maps `INCOMING_DOWNLOAD_FAILED(106)` and its eight siblings to
// a message delivery_state of `download_failed`, and such a message can still
// carry a media ID -- Google told us the media exists and then told us the
// download did not work. Reading the media ID alone would serve that
// attachment as `available`, and the redemption would fail against Google
// instead of the route saying `failed` up front.
//
// `failed` was a declared value of section 4.2's closed vocabulary that
// nothing produced, which is the worst kind: a caller branches on it and
// never exercises the branch.
func DownloadStateForMessage(mediaID, thumbnailMediaID, deliveryState string) string {
	if deliveryState == string(gm.DeliveryStateDownloadFailed) {
		return DownloadStateFailed
	}
	switch {
	case mediaID != "":
		return DownloadStateAvailable
	case thumbnailMediaID != "":
		return DownloadStatePending
	default:
		return DownloadStateUnavailable
	}
}
