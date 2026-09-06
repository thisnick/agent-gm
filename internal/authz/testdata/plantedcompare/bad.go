// Package plantedcompare is a deliberately WRONG fixture. It exists only so
// that the comparison-site audit of spec section 16 Slice 2 test 23 can be
// pointed at code that violates section 12.1 and shown to catch it.
//
// It lives under testdata/, which the go tool ignores, so nothing here is ever
// compiled into Agent GM. Do not copy anything from this file.
package plantedcompare

import "bytes"

// A string comparison against a presented secret. Not constant-time; the
// comparison short-circuits on the first differing byte and the lengths differ.
func checkSecret(presentedSecret, configured string) bool {
	return presentedSecret == configured
}

// The same mistake spelled with a hash. Constant length, still not
// constant-time.
func checkTokenHash(tokenHash, storedHash string) bool {
	if tokenHash != storedHash {
		return false
	}
	return true
}

// bytes.Equal over a secret-derived value.
func checkDigest(computed, stored []byte) bool {
	return bytes.Equal(computed, stored)
}

// A comparison hidden behind a string conversion.
func checkConverted(presentedDigest [32]byte, storedHex string) bool {
	return string(presentedDigest[:]) == storedHex
}

// Not a violation: comparing a length is not comparing a value, and section
// 12.1 requires lengths to be constant precisely so that this is safe.
func lengthOK(presentedSecret string) bool {
	return len(presentedSecret) == 0
}
