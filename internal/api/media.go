package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/store"
)

// The media flow of spec section 10: three requests to send a file, a ticket
// and two requests to fetch one.
//
// The ticket rules are the load-bearing part and they are stated once, here:
//
//   - a token is typed to **one audience** -- `upload:<upload_id>` or
//     `download:<attachment_id>` -- and **the audience is part of the
//     redemption rather than a check after it**. A token presented at the
//     wrong URL is refused and **not spent**, because the wrong audience
//     never reaches the row that would be spent (section 10.3);
//   - **every refusal is the same message whatever the reason**, so a status
//     code teaches an attacker nothing about which guesses were once valid;
//   - the token is accepted only in the `Authorization` header. There is no
//     `?t=` form, so it cannot be captured from a proxy log or a browser
//     history;
//   - **every redemption re-checks the issuing authorization's scope and
//     revocation state**. A ticket outlives neither, so revoking an
//     authorization immediately kills every ticket it minted.

// Ticket lifetimes and caps (spec section 10.3).
const (
	uploadTokenLife         = 2 * time.Hour
	downloadTokenLife       = 15 * time.Minute
	downloadMaxRedemptions  = 5
	defaultUploadMaxBytes   = int64(104857600) // 100 MiB
	defaultCacheMaxBytes    = int64(2 << 30)   // 2 GiB
	contentSniffPrefixBytes = 512
)

// ticketRefused is the single refusal every ticket failure renders as.
//
// It is one function and one sentence on purpose. A wrong audience, a bad
// signature, an expired ticket, a spent one, a revoked authorization and a
// token that belongs to nobody must be indistinguishable: an answer that told
// them apart would let somebody holding a guessed value learn whether it was
// ever real.
func ticketRefused() *apierr.Error {
	return apierr.InvalidToken("the ticket is not valid for this request")
}

// --- POST /v1/uploads: reserve ----------------------------------------------

type uploadCreateBody struct {
	Filename        string `json:"filename"`
	MimeType        string `json:"mime_type"`
	SizeBytes       *int64 `json:"size_bytes"`
	SHA256          string `json:"sha256"`
	ClientRequestID string `json:"client_request_id"`
}

type uploadLimitsDTO struct {
	MaxBytes         int64   `json:"max_bytes"`
	SizeBytes        int64   `json:"size_bytes"`
	MimeType         string  `json:"mime_type"`
	SHA256           *string `json:"sha256"`
	ExpiresInSeconds int64   `json:"expires_in_seconds"`
}

type uploadDTO struct {
	UploadID      string          `json:"upload_id"`
	UploadURL     string          `json:"upload_url"`
	Method        string          `json:"method"`
	Token         string          `json:"token,omitempty"`
	TokenAudience string          `json:"token_audience"`
	ExpiresAt     *string         `json:"expires_at"`
	State         string          `json:"state"`
	Limits        uploadLimitsDTO `json:"limits"`
	Curl          string          `json:"curl,omitempty"`
}

// uploadsCreate reserves. It answers `201` with a ticket.
//
// `mime_type` is validated **at reservation time** with `gm.SupportedMIME`,
// which follows upstream's own table and upstream's own type-prefix fallback,
// so a reservation that would be accepted here and refused at send time -- or
// the reverse -- is impossible. An unsupported type is
// `media_unsupported_type` (415) and **reserves nothing**: a caller should not
// have to clean up after a refusal.
func (d *HandlerDeps) uploadsCreate(r *Request) (*Response, error) {
	var body uploadCreateBody
	if e := r.DecodeBody(&body); e != nil {
		return nil, e
	}
	key, e := idempotency(r, body.ClientRequestID)
	if e != nil {
		return nil, e
	}
	if body.Filename == "" {
		return nil, apierr.MissingParameter("filename")
	}
	if body.MimeType == "" {
		return nil, apierr.MissingParameter("mime_type")
	}
	if body.SizeBytes == nil {
		return nil, apierr.MissingParameter("size_bytes")
	}
	if *body.SizeBytes <= 0 {
		return nil, apierr.WrongTypeForField("size_bytes", "a positive number of bytes")
	}
	if body.SHA256 != "" && !isHexDigest(body.SHA256) {
		return nil, apierr.WrongTypeForField("sha256", "a 64-character hex SHA-256 digest")
	}
	// Text normalisation is never silent (spec section 7.1): a filename over
	// its bound or carrying control characters is cleaned and the cleaning is
	// reported.
	filename, warnings := apierr.NormalizeFilename(body.Filename)
	r.Warn(warnings...)

	if !gm.SupportedMIME(body.MimeType) {
		return nil, apierr.MediaUnsupportedType(body.MimeType)
	}
	maxBytes := d.settingInt(r.Ctx, "media.upload_max_bytes", defaultUploadMaxBytes)
	if *body.SizeBytes > maxBytes {
		return nil, apierr.PayloadTooLarge("the reserved upload", maxBytes)
	}

	expiresAt := d.now().Add(uploadTokenLife)

	// `client_request_id` makes the reservation idempotent, and **each
	// attempt returns a fresh token**: the first token's value left the
	// process and cannot be recovered. A token minted on a repeat never
	// outlives the reservation it fills, which is what
	// `limits.expires_in_seconds` counts down to.
	existing, err := d.Store.UploadByIdempotencyKey(r.Ctx, r.Auth.ID, key)
	if err == nil {
		value, hash, mintErr := d.Signer.Mint(media.KindUpload, existing.ID)
		if mintErr != nil {
			return nil, mintErr
		}
		if err := d.Store.SetUploadToken(r.Ctx, existing.ID, hash); err != nil {
			return nil, err
		}
		return &Response{Status: http.StatusCreated,
			Data: d.uploadDTOFor(existing, value)}, nil
	}
	if !errors.Is(err, store.ErrUploadNotFound) {
		return nil, err
	}

	id := store.UploadID()
	value, hash, err := d.Signer.Mint(media.KindUpload, id)
	if err != nil {
		return nil, err
	}
	reservation, err := d.Store.ReserveUpload(r.Ctx, store.Upload{
		ID:              id,
		AuthorizationID: r.Auth.ID,
		IdempotencyKey:  key,
		Filename:        filename,
		MimeType:        body.MimeType,
		SizeBytes:       *body.SizeBytes,
		SHA256Declared:  strings.ToLower(body.SHA256),
		TokenHash:       hash,
		ExpiresAtMS:     expiresAt.UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	return &Response{Status: http.StatusCreated, Data: d.uploadDTOFor(reservation, value)}, nil
}

// uploadDTOFor renders a reservation. The token is included only when one was
// just minted: it leaves the process once, and `GET /v1/uploads/{id}` must
// not be a way to read it back.
func (d *HandlerDeps) uploadDTOFor(u store.Upload, token string) uploadDTO {
	url := d.uploadURL(u.ID)
	expiresIn := int64(0)
	if remaining := time.UnixMilli(u.ExpiresAtMS).Sub(d.now()); remaining > 0 {
		expiresIn = int64(remaining.Seconds())
	}
	out := uploadDTO{
		UploadID:      u.ID,
		UploadURL:     url,
		Method:        http.MethodPut,
		Token:         token,
		TokenAudience: media.Audience(media.KindUpload, u.ID),
		ExpiresAt:     rfc3339(u.ExpiresAtMS),
		State:         u.State,
		Limits: uploadLimitsDTO{
			MaxBytes:         defaultUploadMaxBytes,
			SizeBytes:        u.SizeBytes,
			MimeType:         u.MimeType,
			SHA256:           nullable(u.SHA256Declared),
			ExpiresInSeconds: expiresIn,
		},
	}
	if token != "" {
		out.Curl = media.CurlUpload(url, token, u.MimeType)
	}
	return out
}

// uploadURL and downloadURL are built from AGENT_GM_PUBLIC_URL and NEVER from
// the request's Host or X-Forwarded-Host (spec sections 10.3, 12.3). They
// take no request, so there is nothing for a hostile header to reach.
func (d *HandlerDeps) uploadURL(uploadID string) string {
	return d.url("/v1/uploads/" + escapePathSegment(uploadID) + "/content")
}

func (d *HandlerDeps) downloadURL(attachmentID string) string {
	return d.url("/v1/attachments/" + escapePathSegment(attachmentID) + "/content")
}

// --- GET and DELETE /v1/uploads/{upload_id} ---------------------------------

// ownUpload reads the caller's own reservation. **Another authorization's is
// `not_found`**, byte-identical to one that never existed, so an `upl_` ID
// cannot be probed for existence.
func (d *HandlerDeps) ownUpload(r *Request) (store.Upload, *apierr.Error) {
	id, e := pathID(r, "upload_id", store.PrefixUpload)
	if e != nil {
		return store.Upload{}, e
	}
	u, err := d.Store.Upload(r.Ctx, id)
	if errors.Is(err, store.ErrUploadNotFound) {
		return store.Upload{}, apierr.NotFound("upload")
	}
	if err != nil {
		return store.Upload{}, apierr.From(err)
	}
	if u.AuthorizationID != r.Auth.ID {
		return store.Upload{}, apierr.NotFound("upload")
	}
	return u, nil
}

func (d *HandlerDeps) uploadsGet(r *Request) (*Response, error) {
	u, e := d.ownUpload(r)
	if e != nil {
		return nil, e
	}
	return &Response{Data: d.uploadDTOFor(u, "")}, nil
}

// uploadsDelete drops the reservation and its staged bytes.
//
// The order is the erasure order of spec section 10.3: the staged path is
// read, the row goes, and only then is the file unlinked -- so a crash in
// between leaves a file an operator can delete rather than a row pointing at
// nothing.
func (d *HandlerDeps) uploadsDelete(r *Request) (*Response, error) {
	u, e := d.ownUpload(r)
	if e != nil {
		return nil, e
	}
	// A row delete, not business logic: internal/store has no DeleteUpload
	// and Write is its exported escape hatch for exactly this.
	err := d.Store.Write(r.Ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Ctx,
			`DELETE FROM uploads WHERE id = ? AND authorization_id = ?`, u.ID, r.Auth.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if u.StagedPath != "" {
		_ = os.Remove(u.StagedPath)
	}
	return &Response{Status: http.StatusNoContent, Raw: func(http.ResponseWriter) {}}, nil
}

// --- the send-time half of an upload ----------------------------------------

// consumeUploadForSend reads a filled reservation, spends it, and uploads its
// bytes to Google.
//
// **Uploads are deliberately account-agnostic** (spec section 10.2). An
// `upl_` reservation carries no `account_id`: it is bytes staged by an
// authorization, and the account is fixed HERE, at send time, by the
// conversation named in the send. So the same upload can be sent into any
// account the caller may write to -- though only **once**, so it cannot be
// fanned out.
//
// The reservation is consumed BEFORE the bytes leave for Google, not after.
// That ordering is the safe one: consuming first means a send that fails
// half-way cannot be "retried" into a second real message, and a caller that
// genuinely wants to try again reserves again -- which is the same rule
// section 10.2 already states for a `PUT` that failed verification.
func (d *HandlerDeps) consumeUploadForSend(r *Request, uploadID string, eng *core.Account) (gm.MediaRef, *apierr.Error) {
	if e := apierr.CheckIDPrefix(uploadID, "upload_ids", store.PrefixUpload); e != nil {
		return gm.MediaRef{}, e
	}
	u, err := d.Store.Upload(r.Ctx, uploadID)
	if errors.Is(err, store.ErrUploadNotFound) {
		return gm.MediaRef{}, apierr.NotFound("upload")
	}
	if err != nil {
		return gm.MediaRef{}, apierr.From(err)
	}
	// Only the authorization that owns an upload may send it; another's is
	// not_found rather than a refusal that confirms it exists.
	if u.AuthorizationID != r.Auth.ID {
		return gm.MediaRef{}, apierr.NotFound("upload")
	}
	if u.State != store.UploadComplete || u.StagedPath == "" {
		return gm.MediaRef{}, apierr.New(apierr.CodeInvalidRequest,
			"that upload has no verified bytes to send: reserve one, PUT the bytes, then send it; "+
				"an upload can be sent once")
	}
	data, readErr := os.ReadFile(u.StagedPath)
	if readErr != nil {
		return gm.MediaRef{}, apierr.New(apierr.CodeInvalidRequest,
			"that upload's staged bytes are gone; reserve again")
	}
	if err := d.Store.ConsumeUpload(r.Ctx, u.ID); err != nil {
		if errors.Is(err, store.ErrUploadState) {
			return gm.MediaRef{}, apierr.New(apierr.CodeInvalidRequest,
				"that upload has already been sent; an upload can be sent once, into one conversation")
		}
		return gm.MediaRef{}, apierr.From(err)
	}
	ref, err := eng.Backend.Upload(r.Ctx, data, u.Filename, u.MimeType)
	if err != nil {
		return gm.MediaRef{}, apierr.From(err)
	}
	_ = os.Remove(u.StagedPath)
	return ref, nil
}

// --- GET /v1/attachments/{attachment_id} ------------------------------------

type attachmentDTO struct {
	AttachmentID string  `json:"attachment_id"`
	AccountID    string  `json:"account_id"`
	Filename     *string `json:"filename"`
	MimeType     string  `json:"mime_type"`
	Size         int64   `json:"size"`
	SHA256       *string `json:"sha256"`
	// SHA256Available is false when the digest could not be computed, and
	// SHA256UnavailableReason then says which of the three reasons it was.
	SHA256Available         bool    `json:"sha256_available"`
	SHA256UnavailableReason *string `json:"sha256_unavailable_reason,omitempty"`
	Width                   *int64  `json:"width"`
	Height                  *int64  `json:"height"`
	DownloadState           string  `json:"download_state"`
	Inline                  bool    `json:"inline"`
	ResourceURI             string  `json:"resource_uri"`
	DownloadURL             string  `json:"download_url"`
	Token                   string  `json:"token"`
	TokenAudience           string  `json:"token_audience"`
	ExpiresAt               *string `json:"expires_at"`
	MaxRedemptions          int     `json:"max_redemptions"`
	Curl                    string  `json:"curl"`
}

func (d *HandlerDeps) attachment(r *Request) (store.Attachment, *apierr.Error) {
	id, e := pathID(r, "attachment_id", store.PrefixAttachment)
	if e != nil {
		return store.Attachment{}, e
	}
	a, err := d.Store.Attachment(r.Ctx, id)
	if errors.Is(err, store.ErrAttachmentNotFound) || errors.Is(err, sql.ErrNoRows) {
		return store.Attachment{}, apierr.NotFound("attachment")
	}
	if err != nil {
		return store.Attachment{}, apierr.From(err)
	}
	return a, nil
}

// attachmentsGet returns metadata **and a download ticket** (spec section
// 10.1).
func (d *HandlerDeps) attachmentsGet(r *Request) (*Response, error) {
	a, e := d.attachment(r)
	if e != nil {
		return nil, e
	}
	digest, reason := d.attachmentDigest(r, a)
	if reason != "" {
		// The reason also appears in warnings, so a caller reading only the
		// envelope still learns that a field it might have compared is
		// absent for a reason rather than by accident.
		r.Warn(media.SHA256UnavailableWarning(reason))
	}

	value, hash, err := d.Signer.Mint(media.KindDownload, a.ID)
	if err != nil {
		return nil, err
	}
	expiresAt := d.now().Add(downloadTokenLife)
	if _, err := d.Store.MintDownloadTicket(r.Ctx, store.DownloadTicket{
		TokenHash:       hash,
		AttachmentID:    a.ID,
		AuthorizationID: r.Auth.ID,
		MaxRedemptions:  downloadMaxRedemptions,
		ExpiresAtMS:     expiresAt.UnixMilli(),
	}); err != nil {
		return nil, err
	}

	url := d.downloadURL(a.ID)
	inlineMax := d.settingInt(r.Ctx, "media.inline_mcp_image_max_bytes", 1<<20)
	var w, h *int64
	if a.Width != 0 {
		v := a.Width
		w = &v
	}
	if a.Height != 0 {
		v := a.Height
		h = &v
	}
	expiresMS := expiresAt.UnixMilli()
	return &Response{Data: attachmentDTO{
		AttachmentID:            a.ID,
		AccountID:               a.AccountID,
		Filename:                nullable(a.Filename),
		MimeType:                a.MimeType,
		Size:                    a.SizeBytes,
		SHA256:                  nullable(digest),
		SHA256Available:         digest != "",
		SHA256UnavailableReason: nullable(reason),
		Width:                   w,
		Height:                  h,
		DownloadState:           a.DownloadState,
		Inline:                  strings.HasPrefix(a.MimeType, "image/") && a.SizeBytes <= inlineMax,
		ResourceURI:             "agm://attachments/" + a.ID,
		DownloadURL:             url,
		Token:                   value,
		TokenAudience:           media.Audience(media.KindDownload, a.ID),
		ExpiresAt:               rfc3339(expiresMS),
		MaxRedemptions:          downloadMaxRedemptions,
		Curl:                    media.CurlDownload(url, value, a.Filename),
	}}, nil
}

// attachmentDigest is the SHA-256 of the **decrypted** bytes, computed on
// demand and cached on the attachment row (spec section 10.1).
//
// Google carries a digest of the ciphertext, which is not what an agent
// comparing its downloaded copy would compute, so serving Google's value
// would look like an answer and never match. When the plaintext digest cannot
// be had, the answer is null with a named reason rather than a wrong number.
func (d *HandlerDeps) attachmentDigest(r *Request, a store.Attachment) (digest, reason string) {
	if a.SHA256 != "" {
		return a.SHA256, ""
	}
	if a.DownloadState != store.DownloadStateAvailable {
		return "", media.ReasonBytesUnavailable
	}
	cacheMax := d.settingInt(r.Ctx, "media.cache_max_bytes", defaultCacheMaxBytes)
	if a.SizeBytes > cacheMax {
		// Hashing it would mean holding, outside the cache, an object the
		// cache budget exists to bound.
		return "", media.ReasonLargerThanCacheBudget
	}
	data, e := d.attachmentBytes(r, a)
	if e != nil {
		if e.Code == apierr.CodeRateLimited {
			return "", media.ReasonDownloadBudgetExhausted
		}
		return "", media.ReasonBytesUnavailable
	}
	sum := media.SHA256(data)
	// Cached on the row so the next reader does not re-download.
	if err := d.Store.SetAttachmentDownloadState(r.Ctx, a.ID, a.DownloadState, sum); err != nil && d.Log != nil {
		d.Log.Warn("caching an attachment digest failed", "attachment_id", a.ID, "error", err.Error())
	}
	return sum, ""
}

// attachmentBytes reads the decrypted bytes, from the media cache if present
// and from Google otherwise, caching what it fetches.
func (d *HandlerDeps) attachmentBytes(r *Request, a store.Attachment) ([]byte, *apierr.Error) {
	if d.Cache != nil {
		if data, ok, err := d.Cache.Get(r.Ctx, a.ID, d.now().UnixMilli()); err == nil && ok {
			return data, nil
		}
	}
	if a.DownloadState != store.DownloadStateAvailable {
		return nil, apierr.UnsupportedCapability(apierr.ReasonMediaPending,
			a.ID, "download", "download_state", a.DownloadState)
	}
	authorizationID := ""
	if r.Auth != nil {
		authorizationID = r.Auth.ID
	}
	release, err := d.Authz.Downloads.Acquire(authorizationID)
	if err != nil {
		return nil, translateAuthzError(err)
	}
	defer release()

	eng, err := d.engine(a.AccountID, r.Source)
	if err != nil {
		return nil, apierr.From(err)
	}
	var key []byte
	if len(a.DecryptionKey) > 0 {
		key, err = store.OpenAttachmentKey(d.DataKey, a.ID, a.DecryptionKey)
		if err != nil {
			return nil, apierr.From(err)
		}
	}
	data, err := eng.Backend.Download(r.Ctx, a.MediaID, key)
	if err != nil {
		return nil, apierr.From(err)
	}
	if d.Cache != nil {
		if _, err := d.Cache.Put(r.Ctx, a.ID, data, d.now().UnixMilli()); err != nil && d.Log != nil {
			d.Log.Warn("caching attachment bytes failed", "attachment_id", a.ID, "error", err.Error())
		}
	}
	return data, nil
}

// --- ticket redemption ------------------------------------------------------

// checkIssuingAuthorization is the re-check every redemption performs.
//
// **A ticket outlives neither its authorization's scope nor its revocation**
// (spec section 10.3), so revoking an authorization, or narrowing it to drop
// the scope, immediately kills every ticket it minted. The check reads the
// row rather than anything carried in the token: a signed blob cannot know it
// has been revoked since it was signed.
func (d *HandlerDeps) checkIssuingAuthorization(ctx context.Context, authorizationID string, required authz.Scope) bool {
	row, err := d.Store.Authorization(ctx, authorizationID)
	if err != nil || row.Revoked() {
		return false
	}
	if row.ExpiresAtMS != 0 && row.ExpiresAtMS <= d.now().UnixMilli() {
		return false
	}
	for _, s := range store.SplitScopes(row.Scopes) {
		if authz.Scope(s) == required {
			return true
		}
	}
	return false
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// sameHash compares two stored token hashes in constant time. They are equal
// length by construction, and comparing them with `==` would leak a prefix
// match through timing (spec section 12.1).
func sameHash(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// digest is a streaming SHA-256, so a body is hashed as it arrives rather
// than after it is all in memory: the point of the size bound is not to hold
// 100 MiB, and holding it to hash it would give that back.
type digest struct{ h hash.Hash }

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) Write(p []byte) { _, _ = d.h.Write(p) }

func (d *digest) hex() string { return hex.EncodeToString(d.h.Sum(nil)) }
