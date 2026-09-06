package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/authz"
	"github.com/thisnick/agent-gm/internal/media"
	"github.com/thisnick/agent-gm/internal/store"
)

// The two routes that carry BYTES rather than JSON, and the one whose body
// fields are not a fixed list.
//
// They are ordinary handlers like every other route. What makes them work is
// three flags on the route itself (internal/api/routes.go):
//
//   - `RawBody` on `PUT /v1/uploads/{id}/content`, so the transport does not
//     read the body into memory under the 1 MiB JSON bound of section 7.2 --
//     an upload runs to `media.upload_max_bytes` and how many bytes are
//     allowed is the RESERVATION's business, which only this handler knows --
//     and does not check a JPEG for unknown JSON fields.
//   - `AltCredential` on `GET /v1/attachments/{id}/content`, so a bearer that
//     is not an access token is not refused at the transport: section 10.3
//     says the route takes an access token **or** a download ticket, and a
//     ticket is not an access token. The transport still tries the bearer,
//     because most callers present one; it just hands the request over with
//     no authorization when that fails, and this handler checks the ticket.
//   - `OpenBody` on `PATCH /v1/admin/settings`, whose body fields ARE the
//     settings keys of section 15.1. They are validated by the settings
//     registry, which checks bounds, type, mutability and the retired-key
//     table -- far more than a name check could -- so enumerating them in the
//     route inventory would be a second copy that could drift from the thing
//     doing the real work.
//
// Putting each exception on the route rather than in a wrapper around the
// server keeps one property that matters: **every route goes through the same
// middleware chain**, so authentication, the scope check, the rate limit, the
// query check and the envelope cannot be skipped by accident on the routes
// that are hardest to reason about.

// --- PUT /v1/uploads/{upload_id}/content ------------------------------------

type uploadFilledDTO struct {
	UploadID  string `json:"upload_id"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// serveUploadContent streams the bytes into a reservation.
//
// It is authenticated by the **upload token**, not the access token, and its
// redemption re-checks the issuing authorization's `messages:write` scope and
// revocation state -- **a token outlives neither** (spec section 10.3).
//
// Two properties are easy to state and easy to get wrong, so they are
// enforced in a fixed order here:
//
//   - **a token presented at another upload's URL is refused and NOT spent.**
//     The audience is inside the MAC, so the verification against THIS
//     upload's ID fails before any row is read, let alone written. That is
//     what makes "learning a token value does not let anyone destroy it"
//     true, and it is why the audience is part of the redemption rather than
//     a check after it.
//   - **a reservation that fails verification is spent.** A short body, a
//     long body, a wrong `sha256` and a contradicted content type each
//     consume the single redemption, so the caller reserves again rather
//     than retrying into a half-written file. The redemption is counted
//     whether or not the body was acceptable.
func (d *HandlerDeps) uploadContent(req *Request) (*Response, error) {
	// The prefix rule first, because it reads nothing and reveals nothing: a
	// `msg_` where an `upl_` belongs is a caller mistake with a fix, and
	// `not_found` would send them looking for a missing reservation.
	uploadID := req.Path["upload_id"]
	if e := apierr.CheckIDPrefix(uploadID, "upload_id", store.PrefixUpload); e != nil {
		return nil, e
	}

	token := req.Bearer
	if token == "" {
		return nil, ticketRefused()
	}
	// Verified against THIS upload's audience. A token minted for another
	// upload never reaches the row below.
	hash, err := d.Signer.Verify(token, media.KindUpload, uploadID)
	if err != nil {
		return nil, ticketRefused()
	}

	u, err := d.Store.Upload(req.Ctx, uploadID)
	if err != nil || !sameHash(u.TokenHash, hash) {
		return nil, ticketRefused()
	}
	if u.State != store.UploadReserved || u.Redemptions >= 1 ||
		u.ExpiresAtMS <= d.now().UnixMilli() {
		return nil, ticketRefused()
	}
	if !d.checkIssuingAuthorization(req.Ctx, u.AuthorizationID, authz.ScopeMessagesWrite) {
		return nil, ticketRefused()
	}

	release, err := d.Authz.Uploads.Acquire(u.AuthorizationID)
	if err != nil {
		return nil, translateAuthzError(err)
	}
	defer release()

	// spend marks the reservation used and returns the refusal. Every
	// refusal below spends it: a reservation that fails verification is
	// spent, so the caller reserves again rather than retrying into a
	// half-written file (spec section 10.2).
	spend := func(e *apierr.Error) error {
		if err := d.Store.CompleteUpload(req.Ctx, u.ID, "", false); err != nil && d.Log != nil {
			d.Log.Warn("spending a failed upload reservation", "upload_id", u.ID, "error", err.Error())
		}
		return e
	}

	dir := d.stagingDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, apierr.Internal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, apierr.Internal(err)
	}
	tmp, err := os.CreateTemp(dir, "upl-*.part")
	if err != nil {
		return nil, apierr.Internal(err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	// **The stream is refused the moment it exceeds the reserved length.**
	// One byte over is read so that "exactly the reserved length" and "longer
	// than it" are distinguishable, and nothing beyond that is ever written
	// to disk.
	limited := io.LimitReader(req.RawBody, u.SizeBytes+1)
	hasher := newDigest()
	var sniff []byte
	written := int64(0)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			written += int64(n)
			if written > u.SizeBytes {
				cleanup()
				return nil, spend(apierr.PayloadTooLarge("the uploaded body", u.SizeBytes))
			}
			if len(sniff) < contentSniffPrefixBytes {
				take := min(contentSniffPrefixBytes-len(sniff), len(chunk))
				sniff = append(sniff, chunk[:take]...)
			}
			hasher.Write(chunk)
			if _, err := tmp.Write(chunk); err != nil {
				cleanup()
				return nil, apierr.Internal(err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			cleanup()
			return nil, spend(apierr.New(apierr.CodeInvalidRequest, "the uploaded body could not be read"))
		}
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return nil, apierr.Internal(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, apierr.Internal(err)
	}

	// Completion verifies the byte count, the declared digest and the
	// detected content type -- in that order, so the cheapest and most
	// specific answer comes first.
	if written != u.SizeBytes {
		_ = os.Remove(tmpName)
		e := apierr.Newf(apierr.CodeInvalidRequest,
			"the reservation named %d bytes and the body carried %d; the reservation is spent, so reserve again",
			u.SizeBytes, written)
		e.Details = map[string]any{"field": "size_bytes", "reserved": u.SizeBytes, "received": written}
		return nil, spend(e)
	}
	digest := hasher.hex()
	if u.SHA256Declared != "" && !sameHash(u.SHA256Declared, digest) {
		_ = os.Remove(tmpName)
		e := apierr.New(apierr.CodeInvalidRequest,
			"the body's SHA-256 is not the one the reservation declared; the reservation is spent, so reserve again")
		e.Details = map[string]any{"field": "sha256"}
		return nil, spend(e)
	}
	if media.ContentTypeContradicted(u.MimeType, http.DetectContentType(sniff)) {
		_ = os.Remove(tmpName)
		e := apierr.New(apierr.CodeInvalidRequest,
			"the body is not the content type the reservation declared; the reservation is spent, so reserve again")
		e.Details = map[string]any{"field": "mime_type", "declared": u.MimeType}
		return nil, spend(e)
	}

	staged := filepath.Join(dir, u.ID)
	if err := os.Rename(tmpName, staged); err != nil {
		_ = os.Remove(tmpName)
		return nil, apierr.Internal(err)
	}
	if err := d.Store.CompleteUpload(req.Ctx, u.ID, staged, true); err != nil {
		_ = os.Remove(staged)
		if errors.Is(err, store.ErrUploadState) {
			// A second PUT with the same token, racing the first.
			return nil, ticketRefused()
		}
		return nil, apierr.Internal(err)
	}

	return &Response{Data: uploadFilledDTO{
		UploadID:  u.ID,
		State:     store.UploadComplete,
		SizeBytes: written,
		SHA256:    digest,
	}}, nil
}

// --- GET /v1/attachments/{attachment_id}/content ----------------------------

// serveAttachmentContent serves the decrypted bytes.
//
// It accepts **either** a `messages:read` access token **or** a download
// ticket, and the ticket is accepted only in the `Authorization` header --
// there is no `?t=` form, so a token cannot be captured from a proxy log or a
// browser history.
//
// Three headers travel with every success, and each is load-bearing on its
// own: `Content-Disposition: attachment` so a browser saves rather than
// renders, `X-Content-Type-Options: nosniff` so it does not overrule the
// declared type, and a sandboxing CSP so anything that is rendered anyway is
// inert. And **SVG, HTML and every unrecognised type are served as
// `application/octet-stream`, never with their own type** -- a message from
// anybody can carry an attachment, so repeating the sender's content type
// would be a stored cross-site scripting hole on the owner's own origin.
func (d *HandlerDeps) attachmentContent(req *Request) (*Response, error) {
	attachmentID := req.Path["attachment_id"]
	if e := apierr.CheckIDPrefix(attachmentID, "attachment_id", store.PrefixAttachment); e != nil {
		return nil, e
	}
	if req.Bearer == "" {
		return nil, apierr.InvalidToken("no Authorization header")
	}

	// Either credential: the transport has already accepted an access token
	// if there was one (req.Auth), and has deliberately NOT refused a bearer
	// that was not one, because it may be a download ticket (section 10.3).
	authorizationID, e := d.authoriseDownload(req, attachmentID)
	if e != nil {
		return nil, e
	}

	a, err := d.Store.Attachment(req.Ctx, attachmentID)
	if err != nil {
		return nil, apierr.NotFound("attachment")
	}
	// An attachment whose bytes are not downloaded yet is
	// unsupported_capability with reason media_pending, not an empty body.
	if a.DownloadState != store.DownloadStateAvailable {
		return nil, apierr.UnsupportedCapability(apierr.ReasonMediaPending,
			a.ID, "download", "download_state", a.DownloadState)
	}

	// The bytes are fetched as the ISSUING authorization, which on the
	// ticket path is not the caller: a ticket is attributed to the session
	// that minted it, so revoking that session kills the ticket.
	byteReq := &Request{
		Ctx:    req.Ctx,
		Source: req.Source,
		Auth:   &authz.Authorization{ID: authorizationID},
	}
	data, byteErr := d.attachmentBytes(byteReq, a)
	if byteErr != nil {
		return nil, byteErr
	}

	// Three headers, each load-bearing on its own, and the content type is
	// the fourth: SVG, HTML and every unrecognised type are served as
	// application/octet-stream, never with their own (media.ServedContentType).
	return &Response{
		Header: http.Header{
			"Content-Type":            {media.ServedContentType(a.MimeType)},
			"Content-Disposition":     {contentDisposition(a.Filename)},
			"Content-Security-Policy": {media.ContentSecurityPolicy},
			"Content-Length":          {strconv.FormatInt(int64(len(data)), 10)},
		},
		Raw: func(w http.ResponseWriter) { _, _ = w.Write(data) },
	}, nil
}

// authoriseDownload accepts an access token or a download ticket and returns
// the authorization the read is attributed to.
//
// The ticket path re-reads the issuing authorization BEFORE spending a
// redemption, so a ticket whose session has been narrowed to drop
// `messages:read` fails without burning one of its five uses -- the check is
// still on every redemption, which is what section 10.3 requires, and the
// counter still means what it says.
func (d *HandlerDeps) authoriseDownload(req *Request, attachmentID string) (authorizationID string, e *apierr.Error) {
	ctx, token := req.Ctx, req.Bearer

	if kind, ok := media.KindOf(token); ok && kind == media.KindDownload {
		hash, err := d.Signer.Verify(token, media.KindDownload, attachmentID)
		if err != nil {
			// Wrong audience: refused, and NOT spent.
			return "", ticketRefused()
		}
		ticket, err := d.Store.DownloadTicketByHash(ctx, hash)
		if err != nil {
			return "", ticketRefused()
		}
		if !d.checkIssuingAuthorization(ctx, ticket.AuthorizationID, authz.ScopeMessagesRead) {
			return "", ticketRefused()
		}
		if _, err := d.Store.RedeemDownloadTicket(ctx, hash); err != nil {
			return "", ticketRefused()
		}
		return ticket.AuthorizationID, nil
	}

	auth, err := d.Authz.Authenticate(ctx, token)
	if err != nil {
		return "", apierr.InvalidToken("the token was refused")
	}
	if err := auth.Require(authz.ScopeMessagesRead); err != nil {
		return "", apierr.InsufficientScope(string(authz.ScopeMessagesRead))
	}
	if err := d.Authz.AllowRequest(auth, authz.BucketReads); err != nil {
		return "", translateAuthzError(err)
	}
	return auth.ID, nil
}

// contentDisposition renders the header. The filename is quoted and its
// quotes and control characters are stripped: a filename arriving from Google
// is a remote party's choice, and header injection is exactly what a
// remote party's choice should never be able to do.
func contentDisposition(filename string) string {
	clean := strings.Map(func(rune_ rune) rune {
		if rune_ < 0x20 || rune_ == 0x7f || rune_ == '"' || rune_ == '\\' {
			return -1
		}
		return rune_
	}, filename)
	if clean == "" {
		return media.ContentDisposition
	}
	return media.ContentDisposition + `; filename="` + clean + `"`
}

// --- PATCH /v1/admin/settings -----------------------------------------------

// settingsPatch is an ordinary handler. Its body's field names ARE the
// settings keys (spec section 7.7), which is why its route is declared
// `OpenBody`: the inventory cannot enumerate them, and it should not try,
// because the settings registry validates far more than a name -- bounds,
// type, mutability, downward-only for `media.upload_max_bytes`, and the
// retired-key table that answers naming a key's replacement rather than
// writing a key nothing reads.
//
// The strictness section 7.1 asks for is therefore delivered rather than
// lost: a misspelled key is refused, and the whole body is validated before
// anything is written, so an invalid key rejects the request and changes
// nothing.
func (d *HandlerDeps) settingsPatch(req *Request) (*Response, error) {
	return d.adminSettingsPatch(req)
}
