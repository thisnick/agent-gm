package oauth

// Every comparison of a caller-supplied value against a stored one is
// constant-time (spec section 12.1), and this file is the only place in the
// package that makes one. comparison_audit_test.go enforces that by parsing
// this package's own source, exactly as `internal/authz` does.
//
// **One hash here is deliberately NOT domain-separated: the enrollment code.**
// Spec section 16 Slice 3 test 12 requires the stored hash to equal SHA-256 of
// the code's canonical form *computed outside the codebase* -- an owner with
// `sha256sum` and the code the server printed can check what is in the
// database themselves. A domain separator would make that impossible to
// verify without running Agent GM's own code, which is the opposite of what
// the test is for. Everything else here is domain-separated, so a digest
// computed for one purpose can never verify a value presented for another.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Domain separators, joined to their inputs with a NUL byte, which cannot
// appear in any of them.
const (
	domainAuthorizationCode = "agent-gm/oauth/authorization-code/v1"
	domainCookieHandle      = "agent-gm/oauth/context-handle/v1"
	domainFormToken         = "agent-gm/oauth/form-token/v1"
	domainContext           = "agent-gm/oauth/context/v1"
)

// digest is a fixed-length SHA-256 hash. Every comparison below is between
// two of these, so the compared lengths are constant and a length difference
// cannot leak.
type digest [sha256.Size]byte

func sum(domain string, parts ...string) digest {
	h := sha256.New()
	h.Write([]byte(domain))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	var d digest
	copy(d[:], h.Sum(nil))
	return d
}

func hexOf(d digest) string { return hex.EncodeToString(d[:]) }

// equalDigest is the constant-time comparison. hmac.Equal wraps
// crypto/subtle.ConstantTimeCompare; both operands are fixed-length by
// construction. The parameters are named for what they hold so the
// comparison-site audit recognises this site by name rather than by luck.
func equalDigest(presentedDigest, storedDigest digest) bool {
	return hmac.Equal(presentedDigest[:], storedDigest[:])
}

// equalHashHex compares a computed digest against a hex digest read back from
// the database, constant-time. A stored value that is not a well-formed
// digest never matches -- it cannot, because a malformed row is not a
// credential -- and the comparison is still burned so the two paths cost the
// same.
func equalHashHex(computed digest, storedHex string) bool {
	raw, err := hex.DecodeString(storedHex)
	if err != nil || len(raw) != sha256.Size {
		var zero digest
		_ = equalDigest(computed, zero)
		return false
	}
	var d digest
	copy(d[:], raw)
	return equalDigest(computed, d)
}

// ---------------------------------------------------------------------------
// Enrollment codes (spec section 9.5).
// ---------------------------------------------------------------------------

// enrollmentAlphabet is Crockford base32 without the four characters a person
// can misread (I, L, O, U). A code is read off a screen and typed into a form,
// so the alphabet is chosen for that and not for entropy density.
const enrollmentAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewEnrollmentCode mints a code value. It is returned to the owner exactly
// once and never stored (spec section 9.5).
//
// The form is four groups of four characters separated by hyphens --
// `A1B2-C3D4-E5F6-G7H8` -- which is 80 bits of entropy, and the hyphens are
// cosmetic: CanonicalEnrollmentCode strips them.
func NewEnrollmentCode() (string, error) {
	const groups, size = 4, 4
	buf := make([]byte, groups*size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range buf {
		if i > 0 && i%size == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(enrollmentAlphabet[int(v)%len(enrollmentAlphabet)])
	}
	return b.String(), nil
}

// CanonicalEnrollmentCode is the form the hash is taken over: upper case,
// with every character outside the alphabet removed.
//
// It is exported because it is part of the contract -- `docs/oauth.md`
// documents it so an owner can reproduce `enrollment_codes.code_hash` with
// `printf '%s' A1B2C3D4E5F6G7H8 | sha256sum` and check for themselves that
// the value is not in the database.
func CanonicalEnrollmentCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if strings.ContainsRune(enrollmentAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EnrollmentCodeHash is the stored form: plain SHA-256 of the canonical form,
// hex. Plain, and documented as plain, for the reason at the top of this file.
func EnrollmentCodeHash(raw string) string {
	h := sha256.Sum256([]byte(CanonicalEnrollmentCode(raw)))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Authorization codes, cookie handles and form tokens.
// ---------------------------------------------------------------------------

// newSecretValue mints 256 bits, base64url without padding. It is used for
// authorization codes and cookie handles, both of which are returned to a
// caller exactly once and stored only as hashes.
func newSecretValue() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func authorizationCodeHash(value string) string {
	return hexOf(sum(domainAuthorizationCode, value))
}

func handleHash(handle string) string { return hexOf(sum(domainCookieHandle, handle)) }

// formTokenFor derives the anti-CSRF form token from the context handle.
//
// It is DERIVED rather than stored so that the waiting page can re-render it
// from the cookie the browser already holds, without the server keeping a
// second secret alive across two requests. Unguessable without the handle,
// which is unguessable without the cookie, which is what the token is for.
func (s *Server) formTokenFor(handle string) string {
	mac := hmac.New(sha256.New, s.cfg.SigningKey)
	mac.Write([]byte(domainFormToken))
	mac.Write([]byte{0})
	mac.Write([]byte(handle))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func formTokenHash(token string) string { return hexOf(sum(domainFormToken, token)) }

// signPayload appends a keyed MAC to a base64url payload, in the
// `<payload>.<mac>` form the context cookie and the hidden context field both
// use.
func (s *Server) signPayload(payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.cfg.SigningKey)
	mac.Write([]byte(domainContext))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyPayload checks the MAC constant-time and returns the payload. A
// value that does not verify returns false and nothing else: the caller
// learns that it was refused, never where it differed.
func (s *Server) verifyPayload(signed string) ([]byte, bool) {
	encoded, presentedMAC, ok := strings.Cut(signed, ".")
	if !ok {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.cfg.SigningKey)
	mac.Write([]byte(domainContext))
	mac.Write([]byte{0})
	mac.Write([]byte(encoded))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(presentedMAC), []byte(expected)) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	return payload, true
}

// equalConstantTime compares two caller-visible strings without leaking where
// they differ. It is used for the hidden echoes of section 9.4, which are not
// secrets but are decided one against another in a loop -- and a loop that
// short-circuits on the first difference is a loop that can be measured.
func equalConstantTime(presented, expected string) bool {
	return hmac.Equal([]byte(presented), []byte(expected))
}

// verifyPKCE checks an RFC 7636 S256 verifier against a stored challenge,
// constant-time.
func verifyPKCE(presentedVerifier, storedChallenge string) bool {
	h := sha256.Sum256([]byte(presentedVerifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	return hmac.Equal([]byte(computed), []byte(storedChallenge))
}
