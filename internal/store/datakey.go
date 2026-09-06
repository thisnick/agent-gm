package store

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DataKey is AGENT_GM_DATA_KEY: a 256-bit key supplied as 64 hex characters or
// standard base64 (spec section 4.5).
//
// One data key covers every account, and it is not rotatable in place. The key
// and the data directory move together, always.
type DataKey [32]byte

// Info strings for the four purposes the key is derived for. They are
// distinct so that a session envelope key can never be used to sign a ticket.
const (
	InfoSession       = "agent-gm/session/v1"
	InfoAttachmentKey = "agent-gm/attachment-key/v1"
	InfoTicket        = "agent-gm/ticket/v1"
	InfoCursor        = "agent-gm/cursor/v1"
)

// ErrBadDataKey is returned when AGENT_GM_DATA_KEY is not a 256-bit value.
var ErrBadDataKey = errors.New("AGENT_GM_DATA_KEY must be a 256-bit key, as 64 hex characters or standard base64")

// ParseDataKey accepts the two documented encodings.
func ParseDataKey(s string) (DataKey, error) {
	var k DataKey
	s = strings.TrimSpace(s)
	if s == "" {
		return k, ErrBadDataKey
	}
	if len(s) == 64 {
		raw, err := hex.DecodeString(s)
		if err == nil {
			copy(k[:], raw)
			return k, nil
		}
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return k, ErrBadDataKey
	}
	copy(k[:], raw)
	return k, nil
}

// Derive produces a 32-byte subkey for one purpose by HKDF-SHA256.
func (k DataKey) Derive(info string) ([]byte, error) {
	out, err := hkdf.Key(sha256.New, k[:], nil, info, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving %s: %w", info, err)
	}
	return out, nil
}
