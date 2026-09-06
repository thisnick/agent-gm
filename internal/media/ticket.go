// Package media holds the upload and download tickets, the media cache on
// disk, content sniffing and the size limits of spec section 10. It never
// holds the store's write lock.
package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/thisnick/agent-gm/internal/store"
)

// Token prefixes (spec section 10.3). They are visible in the value so an
// owner reading a curl line can tell an upload ticket from a download one,
// and so a token presented at the wrong kind of endpoint is refused before
// anything else happens.
const (
	UploadPrefix   = "agm_ut_"
	DownloadPrefix = "agm_dt_"
)

// Kind is which of the two ticket types a token is.
type Kind string

const (
	KindUpload   Kind = "upload"
	KindDownload Kind = "download"
)

// ErrTicketRefused is the single refusal.
//
// **Every refusal is the same message whatever the reason** (spec section
// 10.3): a wrong audience, a bad signature, an expired ticket and a token
// that belongs to nobody are indistinguishable, so a status code teaches an
// attacker nothing about which guesses were once valid.
var ErrTicketRefused = errors.New("the ticket is not valid for this request")

// Signer mints and verifies ticket values. The key is derived from
// AGENT_GM_DATA_KEY under `agent-gm/ticket/v1`, distinct from the session,
// attachment and cursor keys, so a ticket signature can never be produced by
// a key minted for something else (spec section 4.5).
type Signer struct {
	key []byte
}

// NewSigner derives the ticket key.
func NewSigner(dk store.DataKey) (*Signer, error) {
	key, err := dk.Derive(store.InfoTicket)
	if err != nil {
		return nil, err
	}
	return &Signer{key: key}, nil
}

// Audience is the one thing a ticket is for: `upload:<upload_id>` or
// `download:<attachment_id>` (spec section 10.3). It is part of the
// redemption rather than a check after it, which is what makes "a token
// presented at the wrong URL is refused **and not spent**" true by
// construction: the wrong audience never reaches the row that would be
// spent.
func Audience(kind Kind, targetID string) string {
	return string(kind) + ":" + targetID
}

// Mint produces a fresh ticket value for one audience.
//
// The value is `<prefix><nonce>.<mac>`, where the MAC covers the audience as
// well as the nonce. Only its SHA-256 hash is stored (section 10.3, section
// 12.1): the value leaves the process once and is never recoverable from the
// database, a log or an audit payload.
func (s *Signer) Mint(kind Kind, targetID string) (value string, hash string, err error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("minting a %s ticket: %w", kind, err)
	}
	enc := base64.RawURLEncoding.EncodeToString(nonce)
	mac := s.mac(kind, targetID, enc)
	value = prefixFor(kind) + enc + "." + mac
	return value, HashToken(value), nil
}

// Verify checks a presented value against one audience and returns the hash
// to look the row up by.
//
// The signature is checked with hmac.Equal -- crypto/subtle underneath -- over
// fixed-length MACs, never with `==` on strings (spec section 12.1). A
// presented value of the wrong shape is refused with exactly the same error
// as one with a bad MAC.
func (s *Signer) Verify(value string, kind Kind, targetID string) (hash string, err error) {
	prefix := prefixFor(kind)
	if !strings.HasPrefix(value, prefix) {
		return "", ErrTicketRefused
	}
	body := strings.TrimPrefix(value, prefix)
	dot := strings.LastIndex(body, ".")
	if dot <= 0 || dot == len(body)-1 {
		return "", ErrTicketRefused
	}
	nonce, presented := body[:dot], body[dot+1:]
	want := s.mac(kind, targetID, nonce)
	if !hmac.Equal([]byte(presented), []byte(want)) {
		return "", ErrTicketRefused
	}
	return HashToken(value), nil
}

// KindOf reports which kind a presented value claims to be, so a redemption
// endpoint can refuse a download ticket presented at an upload URL without
// having to try both.
func KindOf(value string) (Kind, bool) {
	switch {
	case strings.HasPrefix(value, UploadPrefix):
		return KindUpload, true
	case strings.HasPrefix(value, DownloadPrefix):
		return KindDownload, true
	default:
		return "", false
	}
}

// HashToken is the at-rest form of every ticket value: SHA-256, hex. Hashing
// before storage and before comparison keeps the compared lengths constant,
// so a length difference cannot leak (spec section 12.1).
func HashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func (s *Signer) mac(kind Kind, targetID, nonce string) string {
	m := hmac.New(sha256.New, s.key)
	// The audience is inside the MAC, so a value minted for one upload
	// cannot be replayed at another's URL even by an attacker who holds it.
	// Lengths are written in so that ("a", "bc") and ("ab", "c") cannot
	// produce the same input.
	writeField(m, Audience(kind, targetID))
	writeField(m, nonce)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func writeField(w interface{ Write([]byte) (int, error) }, s string) {
	// Errors from hash.Hash.Write are documented never to occur.
	_, _ = fmt.Fprintf(w, "%d:", len(s))
	_, _ = w.Write([]byte(s))
}

func prefixFor(k Kind) string {
	if k == KindUpload {
		return UploadPrefix
	}
	return DownloadPrefix
}

// CurlFor renders the copy-and-paste line the two ticket responses carry
// (spec sections 10.1, 10.2).
//
// **The token appears in the body and never in a URL** (section 10.3), so it
// cannot be captured from a proxy log or a browser history, and it is
// accepted only in the Authorization header -- there is no `?t=` form. That
// is why this builder takes the URL and the token separately and puts the
// token in a header: a single formatted string with the token interpolated
// into the URL is exactly the mistake it exists to prevent.
func CurlDownload(url, token, filename string) string {
	return fmt.Sprintf("curl --fail -H 'Authorization: Bearer %s' -o '%s' '%s'",
		token, shellSafe(filename), url)
}

// CurlUpload renders the upload line.
func CurlUpload(url, token, mimeType string) string {
	return fmt.Sprintf("curl --fail -X PUT -H 'Authorization: Bearer %s' -H 'Content-Type: %s' "+
		"--data-binary @FILE '%s'", token, shellSafe(mimeType), url)
}

// shellSafe strips the one character that would end the single-quoted string
// these lines put a value inside. A filename arriving from Google is not
// trusted to be shell-safe.
func shellSafe(s string) string {
	return strings.ReplaceAll(s, "'", "")
}
