//go:build live

package livegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Section 16 Slice 2 tests 46 and 47: the two pairing gates.
//
// Both need a human -- copying a cURL command out of devtools, or signing in
// to Google again -- so neither can run unattended. What is automated here is
// everything around the human step: the refusal a bad paste must produce, and
// the filesystem assertion D33 turns on.
//
// The coordinator runs these with the owner present. They skip without the
// gate, and the paste ones skip without a paste to feed them.

// Test 46, the half that needs no human at all: a paste missing OSID is
// refused with a message naming OSID and messages.google.com, rather than a
// generic failure.
//
// This is the failure that actually happens. OSID is host-scoped to
// messages.google.com, so a paste copied from a request to accounts.google.com
// -- or any devtools "Copy as cURL" taken before Google Messages for web has
// loaded -- silently omits it, and the pairing then fails much later with
// nothing to act on.
func TestSlice2LivePasteMissingOSIDIsNamed(t *testing.T) {
	requireGate(t)
	bin := agmBinary(t)

	// Six of the seven, deliberately without OSID. These are placeholders,
	// not cookies: nothing here is a credential and nothing is sent.
	paste := `curl 'https://messages.google.com/web/config' ` +
		`-H 'cookie: SID=FIXTURE; HSID=FIXTURE; SSID=FIXTURE; ` +
		`APISID=FIXTURE; SAPISID=FIXTURE; __Secure-1PSIDTS=FIXTURE'`

	_, stderr, code := agmRunStdin(t, paste, "pair", "--paste")
	if code == 0 {
		t.Fatal("a paste missing OSID was accepted")
	}
	if !strings.Contains(stderr, "OSID") {
		t.Errorf("the refusal does not name OSID:\n%s", stderr)
	}
	if !strings.Contains(stderr, "messages.google.com") {
		t.Errorf("the refusal does not name the host OSID is scoped to:\n%s", stderr)
	}
	// Not a generic failure: it must say what to do, not merely that
	// something went wrong.
	for _, generic := range []string{"unexpected error", "internal error"} {
		if strings.Contains(strings.ToLower(stderr), generic) {
			t.Errorf("the refusal is generic:\n%s", stderr)
		}
	}
	_ = bin
}

// Test 46, the half that needs the owner: a real paste reaches `connected`.
//
// It reads the paste from AGENT_GM_LIVE_PASTE_FILE, which the coordinator
// writes by hand from devtools and which is never committed. Without it the
// test skips rather than inventing one.
func TestSlice2LivePasteReachesConnected(t *testing.T) {
	requireGate(t)
	path := os.Getenv("AGENT_GM_LIVE_PASTE_FILE")
	if path == "" {
		t.Skip("no paste: set AGENT_GM_LIVE_PASTE_FILE to a file holding a cURL command " +
			"copied from devtools on messages.google.com")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the paste: %v", err)
	}

	profileRoot := t.TempDir()
	t.Setenv("TMPDIR", profileRoot)
	_, stderr, code := agmRunStdin(t, string(body), "pair", "--paste")
	if code != 0 {
		t.Fatalf("the paste pairing failed with exit %d:\n%s", code, stderr)
	}
	assertNoChromeProfileSurvives(t, profileRoot)
}

// Test 47: `agm pair --refresh-cookies` re-authenticates WITHOUT a re-pair and
// without a new emoji, a capture from a different Google account is
// `pairing_wrong_account` and changes nothing, and -- D33 -- the short-lived
// Chrome profile is gone from the filesystem when the command returns.
//
// The capture itself needs the owner at a browser, so this runs the command
// and asserts what can be asserted from outside: the exit code, that no emoji
// was printed (a refresh is not a pairing), and the profile.
func TestSlice2LiveRefreshCookies(t *testing.T) {
	requireLoggedIn(t)
	account := os.Getenv("AGENT_GM_LIVE_ACCOUNT")
	if account == "" {
		t.Skip("set AGENT_GM_LIVE_ACCOUNT to the acct_ ID to refresh")
	}

	profileRoot := t.TempDir()
	t.Setenv("TMPDIR", profileRoot)

	stdout, stderr, code := agmRun(t, "pair", "--refresh-cookies", "--account", account)
	// D33: the profile is deleted the moment Chrome closes -- on success, on
	// failure, on timeout and on Ctrl-C -- so this assertion holds whatever
	// the exit code was, and is the one that must hold unconditionally.
	assertNoChromeProfileSurvives(t, profileRoot)

	if code != 0 {
		t.Fatalf("the refresh failed with exit %d:\n%s", code, stderr)
	}
	// A refresh re-authenticates an EXISTING pairing: the phone is not asked
	// to confirm anything, so there is no emoji.
	combined := stdout + stderr
	if strings.Contains(combined, "Tap this one") {
		t.Error("the refresh printed an emoji; it re-authenticates rather than re-pairing")
	}
	if !strings.Contains(combined, account) {
		t.Errorf("the refresh does not name the account it refreshed:\n%s", combined)
	}
}

// assertNoChromeProfileSurvives is D33 on the filesystem: the short-lived
// Chrome profile holds a logged-in Google session while Chrome is open and
// must be gone when the command returns, on every path.
func assertNoChromeProfileSurvives(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "agm-chrome-profile-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("a Chrome profile survived the command: %v. It holds a logged-in "+
			"Google session, and D33 says it is deleted the moment Chrome closes", matches)
	}
}
