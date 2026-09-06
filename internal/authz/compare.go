package authz

// Every secret comparison in Agent GM is constant-time (spec section 12.1).
//
// This file is the only place in the package that compares a caller-supplied
// value against a stored one. Everything it compares is a fixed-length
// SHA-256 digest, so the compared lengths are constant and a length difference
// cannot leak; the comparison itself is crypto/subtle.ConstantTimeCompare, via
// hmac.Equal which wraps it. There is no `==` on a string and no bytes.Equal
// anywhere in this package, and comparison_audit_test.go enforces that by
// parsing this package's own source (spec section 16 Slice 2 test 23).
//
// Digests are domain-separated. A digest computed for one purpose can never
// verify a value presented for another, so an admin-secret digest cannot be
// replayed as a token hash and the secret-generation marker stored in
// `authorizations.secret_generation` is not itself a verifier for the secret.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// Digest is a fixed-length SHA-256 hash. Every secret comparison in this
// package is between two of these.
type Digest [sha256.Size]byte

// Domain separators. Each purpose gets its own prefix, joined with a NUL byte
// which cannot appear in any of the inputs.
const (
	domainAdminSecret     = "agent-gm/admin-secret/v1"
	domainSecretGeneraton = "agent-gm/admin-secret-generation/v1"
	domainToken           = "agent-gm/token/v1"
)

func digest(domain string, parts ...string) Digest {
	h := sha256.New()
	h.Write([]byte(domain))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	var d Digest
	copy(d[:], h.Sum(nil))
	return d
}

// hashSecret is the fixed-length digest of an admin secret, presented or
// configured. Both sides of the admin-secret check go through it, so the
// values compared are always 32 bytes regardless of what was typed.
func hashSecret(secret string) Digest { return digest(domainAdminSecret, secret) }

// secretGeneration marks WHICH AGENT_GM_ADMIN_SECRET minted an authorization,
// for the startup revocation of spec section 12.1. It is domain-separated from
// hashSecret so that a leaked generation column is not a verifier.
func secretGeneration(secret string) string {
	d := digest(domainSecretGeneraton, secret)
	return hex.EncodeToString(d[:])
}

// tokenDigest is the stored form of an access or refresh token.
//
// The audience is inside the hash, which is how access tokens are bound to
// <AGENT_GM_PUBLIC_URL>/mcp (spec section 9.6). A token minted under a
// previous origin therefore does not resolve at all under a new one -- it is
// refused as unknown rather than accepted and then audited for audience,
// which is spec section 15.6's "every token minted under the previous origin
// is refused" with no separate check to forget.
//
// The kind is inside the hash too, so an access token can never be presented
// where a refresh token is expected even if its value were somehow known.
func tokenDigest(audience, kind, value string) Digest {
	return digest(domainToken, audience, kind, value)
}

// tokenHash is tokenDigest in the hex form stored in `tokens.token_hash`.
func tokenHash(audience, kind, value string) string {
	d := tokenDigest(audience, kind, value)
	return hex.EncodeToString(d[:])
}

// equalDigest is the constant-time comparison. hmac.Equal wraps
// crypto/subtle.ConstantTimeCompare; both operands are fixed-length by
// construction.
func equalDigest(a, b Digest) bool { return hmac.Equal(a[:], b[:]) }

// equalHashHex compares a computed digest against a hex digest read back from
// the database, constant-time. A stored value that is not a well-formed digest
// never matches -- it cannot, because a malformed row is not a credential.
func equalHashHex(computed Digest, storedHex string) bool {
	stored, err := hex.DecodeString(storedHex)
	if err != nil || len(stored) != sha256.Size {
		// Still burn a comparison against a zero digest so a malformed row
		// and a wrong digest take the same path.
		var zero Digest
		_ = equalDigest(computed, zero)
		return false
	}
	var d Digest
	copy(d[:], stored)
	return equalDigest(computed, d)
}

// newTokenValue mints a fresh 256-bit token value, base64url without padding.
// The value is returned to the caller exactly once and never stored.
func newTokenValue() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}
