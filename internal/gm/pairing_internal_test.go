package gm

import (
	"strings"
	"testing"
)

// F-4, plant R-M9: the empty-Mobile.SourceID refusal.
//
// acct_ is UUIDv5(ns, "account", address), so AccountID("") is a perfectly
// valid UUID: without this guard two different degenerate pairings would
// derive the SAME acct_ ID and adopt each other's conversations and messages
// (spec sections 3.2, 4.1). The guard is unexported and reachable only
// through StartGooglePairing, which needs a real libgm.Client -- which is why
// it is tested here, in the same package, rather than through the supervisor.
func TestPlausibleAddress(t *testing.T) {
	refused := []string{
		"",                  // Google returned nothing
		"noatsign",          // not an address at all
		"@example.com",      // no local part
		"alex@",             // no domain
		"@",                 // neither
		"alex example@x.co", // a space
		"alex@example.com\n",
		"alex\t@example.com",
		"alex@exa\rmple.com",
	}
	for _, s := range refused {
		if plausibleAddress(s) {
			t.Errorf("plausibleAddress(%q) = true; the pairing must be refused", s)
		}
		// The guard the adapter actually calls, so a plant that neuters it
		// dies here rather than surviving in an untested call site.
		if _, err := AccountAddressFromPairing(s); err == nil {
			t.Errorf("AccountAddressFromPairing(%q) succeeded; the pairing must be refused", s)
		}
	}

	accepted := []string{
		"alex@example.com",
		"a@b.co",
		"alex.smith+tag@example.co.uk",
		"ALEX@EXAMPLE.COM", // the caller lowercases; the shape is still plausible
	}
	for _, s := range accepted {
		if !plausibleAddress(s) {
			t.Errorf("plausibleAddress(%q) = false; this is a Google account address", s)
		}
		got, err := AccountAddressFromPairing(s)
		if err != nil {
			t.Errorf("AccountAddressFromPairing(%q): %v", s, err)
			continue
		}
		// It lowercases, so the same account written two ways is one account.
		if got != strings.ToLower(s) {
			t.Errorf("AccountAddressFromPairing(%q) = %q, want it lowercased", s, got)
		}
	}
}

// The refusal produces pairing_no_account, and no account is created from it.
func TestNoAccountAddressClassifies(t *testing.T) {
	e := Classify(ErrNoAccountAddress)
	if e.Code != CodePairingNoAccount {
		t.Errorf("code = %s, want %s", e.Code, CodePairingNoAccount)
	}
	if e.HTTPStatus != 409 {
		t.Errorf("status = %d, want 409", e.HTTPStatus)
	}
}
