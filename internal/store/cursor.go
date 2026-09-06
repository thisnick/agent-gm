package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Cursors are opaque, HMAC-signed with the data key under
// `agent-gm/cursor/v1`, and bound to the endpoint and the filter set as
// written (spec section 7.4).
//
// The binding is to the query *as written*, not to a normalised form: a
// cursor issued without `folder` is not valid when replayed with
// `folder=active`, although the two select the same rows. A client that walks
// a listing sends the same query string on every page anyway, so the strict
// form costs a correct caller nothing and catches an incorrect one on its
// first page.
//
// A cursor encodes `(sent_at_ms, id)` -- never an offset and never a rowid --
// so it is stable across the equal timestamps Google produces in a burst
// (spec section 5.4).

// Cursor is one position in an ordered listing.
type Cursor struct {
	SentAtMS int64
	ID       string
}

// ErrCursorInvalid means the cursor did not verify: it was truncated,
// re-encoded, or its signature does not match. The API layer turns it into
// invalid_request.
var ErrCursorInvalid = errors.New("cursor is not valid")

// ErrCursorFilterMismatch means the cursor verified but was issued for a
// different endpoint or a different filter set. It is distinguishable from
// ErrCursorInvalid on purpose: one is tampering, the other is a client that
// changed its query mid-walk, and the operator wants to tell them apart even
// though both are invalid_request to the caller.
var ErrCursorFilterMismatch = errors.New("cursor was issued for a different query")

const cursorVersion = "agmc1"

// FilterFingerprint hashes a filter set exactly as the caller wrote it.
//
// The input is the raw query parameters with the pagination controls
// (`cursor`, `limit`) removed; nothing is defaulted, lowercased or dropped
// first. A key that is absent fingerprints differently from the same key
// present with any value, which is precisely the "issued without folder,
// replayed with folder=active" case spec section 7.4 names.
func FilterFingerprint(params map[string][]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		// Length prefixes, so no pair of key/value strings can be
		// concatenated into a different pair with the same bytes.
		writeLenPrefixed(h, k)
		vs := params[k]
		_, _ = fmt.Fprintf(h, "%d\n", len(vs))
		for _, v := range vs {
			writeLenPrefixed(h, v)
		}
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func writeLenPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = fmt.Fprintf(h, "%d:", len(s))
	_, _ = h.Write([]byte(s))
	_, _ = h.Write([]byte{'\n'})
}

// EncodeCursor mints an opaque cursor for one position in one endpoint's
// listing under one filter fingerprint.
func EncodeCursor(key DataKey, endpoint, fingerprint string, c Cursor) (string, error) {
	mac, err := cursorKey(key)
	if err != nil {
		return "", err
	}
	payload := cursorPayload(endpoint, fingerprint, c)
	sig := signCursor(mac, payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// DecodeCursor verifies a cursor and returns its position.
//
// The signature is checked before the binding, so a tampered cursor is never
// reported as a filter mismatch and a filter mismatch is never mistaken for
// tampering.
func DecodeCursor(key DataKey, endpoint, fingerprint, token string) (Cursor, error) {
	var c Cursor
	mac, err := cursorKey(key)
	if err != nil {
		return c, err
	}
	rawPayload, rawSig, ok := strings.Cut(token, ".")
	if !ok {
		return c, ErrCursorInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(rawPayload)
	if err != nil {
		return c, ErrCursorInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(rawSig)
	if err != nil {
		return c, ErrCursorInvalid
	}
	if !hmac.Equal(sig, signCursor(mac, string(payload))) {
		return c, ErrCursorInvalid
	}

	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 5 || parts[0] != cursorVersion {
		return c, ErrCursorInvalid
	}
	sentAt, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return c, ErrCursorInvalid
	}
	// The signature covered the endpoint and the fingerprint, so a mismatch
	// here is an honest cursor replayed against a different query rather than
	// a forged one.
	if parts[1] != endpoint || parts[2] != fingerprint {
		return c, ErrCursorFilterMismatch
	}
	return Cursor{SentAtMS: sentAt, ID: parts[4]}, nil
}

func cursorPayload(endpoint, fingerprint string, c Cursor) string {
	return strings.Join([]string{
		cursorVersion, endpoint, fingerprint,
		strconv.FormatInt(c.SentAtMS, 10), c.ID,
	}, "\x00")
}

func signCursor(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(payload))
	return m.Sum(nil)
}

// cursorKey derives the signing key through the one HKDF helper in
// datakey.go. There is deliberately no second derivation: the info string
// agent-gm/cursor/v1 is what keeps a cursor key from ever being a ticket key.
func cursorKey(key DataKey) ([]byte, error) { return key.Derive(InfoCursor) }
