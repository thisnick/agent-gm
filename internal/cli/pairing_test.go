package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/cli"
	"github.com/thisnick/agent-gm/internal/gm"
)

// Spec section 11.4: when there is no Chrome, `agm pair` does NOT fail with a
// missing-binary error. It explains what it needs and offers the two ways
// forward, in the stated order, best first.
func TestNoChromeMessageOffersBothWaysInOrder(t *testing.T) {
	msg := cli.NoChromeMessage("https://gm.agent-wx.app")

	if strings.Contains(strings.ToLower(msg), "no such file") ||
		strings.Contains(strings.ToLower(msg), "executable file not found") {
		t.Error("the message must not read as a missing-binary error")
	}
	for _, want := range []string{
		"could not find Chrome or Chromium",
		"seven session cookies",
		"httpOnly",
		"agm pair --server https://gm.agent-wx.app",
		"agm pair --paste",
		"agm pair --paste-file",
		"AGENT_GM_CHROME",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q", want)
		}
	}
	server := strings.Index(msg, "agm pair --server")
	paste := strings.Index(msg, "agm pair --paste")
	if server < 0 || paste < 0 || server > paste {
		t.Error("the --server option must come before --paste: best first")
	}
	if !strings.Contains(msg, "1.") || !strings.Contains(msg, "2.") {
		t.Error("the two ways forward must be numbered")
	}
}

func TestFindChromeHonoursTheEnvironmentFirst(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "chrome")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := cli.FindChrome(binary)
	if err != nil {
		t.Fatalf("FindChrome: %v", err)
	}
	if got != binary {
		t.Errorf("FindChrome returned %s, want %s", got, binary)
	}

	// A path that is not an executable is named, not silently ignored.
	missing := filepath.Join(dir, "nope")
	_, err = cli.FindChrome(missing)
	if !errors.Is(err, cli.ErrNoChrome) {
		t.Fatalf("got %v, want ErrNoChrome", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// The kept Chrome profile is keyed by account. A single shared profile would
// hold whichever account signed in last (spec section 11.4).
func TestProfileDirIsKeyedByAccount(t *testing.T) {
	state := "/tmp/state"
	a := cli.ProfileDir(state, "acct_a")
	b := cli.ProfileDir(state, "acct_b")
	if a == b {
		t.Fatal("two accounts share one Chrome profile")
	}
	if filepath.Base(a) != "acct_a" {
		t.Errorf("the profile directory is %s, want it named for the account", a)
	}
	// Adding a NEW account uses a fresh directory, so it always starts signed
	// out and the owner is never offered the wrong account by accident.
	n1 := cli.ProfileDir(state, "")
	n2 := cli.ProfileDir(state, "")
	if n1 == n2 {
		t.Error("two new-account pairings share a profile directory")
	}
	if !strings.HasPrefix(filepath.Base(n1), "new-") {
		t.Errorf("a new-account profile is %s", n1)
	}
}

func TestForgetBrowser(t *testing.T) {
	state := t.TempDir()
	for _, id := range []string{"acct_a", "acct_b"} {
		if err := os.MkdirAll(cli.ProfileDir(state, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// With an account it removes one.
	if err := cli.ForgetBrowser(state, "acct_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cli.ProfileDir(state, "acct_a")); !os.IsNotExist(err) {
		t.Error("acct_a's profile survived")
	}
	if _, err := os.Stat(cli.ProfileDir(state, "acct_b")); err != nil {
		t.Error("acct_b's profile was removed too")
	}
	// Without one it removes them all.
	if err := cli.ForgetBrowser(state, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "chrome-profile")); !os.IsNotExist(err) {
		t.Error("the profile root survived")
	}
	// The effect sentence is the one the prompt and the route both use.
	if !strings.Contains(cli.ForgetBrowserEffect, "sign in to Google again") {
		t.Errorf("the effect sentence is %q", cli.ForgetBrowserEffect)
	}
}

// --- the paste fallback ------------------------------------------------------

const (
	// Nonfunctional placeholders. A real Google cookie never enters this
	// repository.
	fixtureValue = "FIXTURE-NOT-A-REAL-COOKIE"
)

func fullCookieHeader() string {
	var parts []string
	for _, n := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
		parts = append(parts, n+"="+fixtureValue+"-"+n)
	}
	return strings.Join(parts, "; ")
}

func TestParsePasteAcceptsJSON(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i, n := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + n + `":"` + fixtureValue + `"`)
	}
	b.WriteString("}")

	got, err := cli.ParsePaste(b.String())
	if err != nil {
		t.Fatalf("ParsePaste: %v", err)
	}
	if len(got) != 7 {
		t.Errorf("kept %d cookies, want exactly the seven", len(got))
	}
	for _, n := range gm.GaiaRequiredCookies {
		if got[n] == "" {
			t.Errorf("%s is missing", n)
		}
	}
}

// A cURL command copied from browser devtools, which is upstream's own
// instruction text (connector/login.go:227).
func TestParsePasteAcceptsACurlCommand(t *testing.T) {
	cmd := `curl 'https://messages.google.com/web/config' \
  -H 'accept: */*' \
  -H 'cookie: ` + fullCookieHeader() + `' \
  -H 'user-agent: Mozilla/5.0' \
  --compressed`
	got, err := cli.ParsePaste(cmd)
	if err != nil {
		t.Fatalf("ParsePaste: %v", err)
	}
	for _, n := range gm.GaiaRequiredCookies {
		if got[n] != fixtureValue+"-"+n {
			t.Errorf("%s = %q", n, got[n])
		}
	}
	// Nothing but the seven is kept: a devtools copy carries far more than
	// Agent GM needs, and none of the rest is stored.
	if _, ok := got["accept"]; ok {
		t.Error("a header leaked into the cookie set")
	}
	if len(got) != 7 {
		t.Errorf("kept %d cookies, want 7", len(got))
	}
}

// A paste scoped to .google.com alone silently omits OSID, which is the most
// common way this fails. The CLI names the missing cookies and the domain
// each comes from.
func TestParsePasteNamesTheMissingCookiesAndTheirDomains(t *testing.T) {
	var parts []string
	for _, n := range gm.GaiaRequiredCookies {
		if n == "OSID" {
			continue // host-scoped to messages.google.com
		}
		parts = append(parts, n+"="+fixtureValue)
	}
	cmd := `curl 'https://www.google.com/' -H 'cookie: ` + strings.Join(parts, "; ") + `'`

	_, err := cli.ParsePaste(cmd)
	var missing *cli.MissingCookiesError
	if !errors.As(err, &missing) {
		t.Fatalf("got %v, want a MissingCookiesError", err)
	}
	if len(missing.Missing) != 1 || missing.Missing[0] != "OSID" {
		t.Fatalf("missing = %v, want [OSID]", missing.Missing)
	}
	if !strings.Contains(err.Error(), "messages.google.com") {
		t.Errorf("the error does not name OSID's domain: %v", err)
	}
	if !strings.Contains(err.Error(), "OSID") {
		t.Errorf("the error does not name the cookie: %v", err)
	}
}

func TestParsePasteRejectsRubbish(t *testing.T) {
	for _, input := range []string{"", "   ", "hello", "{not json"} {
		if _, err := cli.ParsePaste(input); err == nil {
			t.Errorf("ParsePaste(%q) succeeded", input)
		}
	}
}

// The paste is never echoed: the error carries the cookie NAMES, never a
// value.
func TestParsePasteNeverEchoesAValue(t *testing.T) {
	cmd := `curl 'https://www.google.com/' -H 'cookie: SID=` + fixtureValue + `'`
	_, err := cli.ParsePaste(cmd)
	if err == nil {
		t.Fatal("expected the paste to be refused")
	}
	if strings.Contains(err.Error(), fixtureValue) {
		t.Errorf("the error echoes a cookie value: %v", err)
	}
}

// The capture reads exactly the seven and knows which domain each comes from.
func TestTheSevenCookiesAndTheirDomains(t *testing.T) {
	if len(gm.GaiaRequiredCookies) != 6 {
		t.Errorf("the required set has %d cookies, want 6", len(gm.GaiaRequiredCookies))
	}
	if len(gm.GaiaOptionalCookies) != 1 {
		t.Errorf("the optional set has %d cookies, want 1", len(gm.GaiaOptionalCookies))
	}
	if gm.GaiaCookieDomains["OSID"] != "messages.google.com" {
		t.Error("OSID must be host-scoped to messages.google.com")
	}
	for _, n := range []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSIDTS"} {
		if gm.GaiaCookieDomains[n] != ".google.com" {
			t.Errorf("%s is scoped to %q, want .google.com", n, gm.GaiaCookieDomains[n])
		}
	}
	// Five of the seven are httpOnly, which is why a JS-only capture cannot
	// work and the read goes through the browser's own cookie store.
	if len(gm.GaiaHTTPOnlyCookies) != 5 {
		t.Errorf("the httpOnly set has %d cookies, want 5", len(gm.GaiaHTTPOnlyCookies))
	}
	for _, n := range gm.GaiaHTTPOnlyCookies {
		if _, ok := gm.GaiaCookieDomains[n]; !ok {
			t.Errorf("%s is not one of the seven", n)
		}
	}
	for _, n := range []string{"APISID", "SAPISID"} {
		for _, h := range gm.GaiaHTTPOnlyCookies {
			if h == n {
				t.Errorf("%s is readable from JavaScript and must not be listed httpOnly", n)
			}
		}
	}
	// Upstream's own capture URL.
	if gm.GaiaCaptureURL != "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config" {
		t.Errorf("the capture URL is %q", gm.GaiaCaptureURL)
	}
}

// F-7: a header that is not the Cookie header is not a cookie source. Chrome
// sends `x-client-data`, which can carry `=` and `;`, and the old parser fell
// through and mined it.
func TestParsePasteIgnoresNonCookieHeaders(t *testing.T) {
	// Every required cookie arrives in a bogus header; only OSID is in the
	// real Cookie header. Nothing but OSID may be taken.
	var bogus []string
	for _, n := range gm.GaiaRequiredCookies {
		bogus = append(bogus, n+"=SMUGGLED")
	}
	cmd := `curl 'https://messages.google.com/web/config' \
  -H 'x-client-data: ` + strings.Join(bogus, "; ") + `' \
  -H 'cookie: OSID=` + fixtureValue + `'`

	_, err := cli.ParsePaste(cmd)
	var missing *cli.MissingCookiesError
	if !errors.As(err, &missing) {
		t.Fatalf("got %v, want the paste refused for missing cookies", err)
	}
	if len(missing.Missing) != len(gm.GaiaRequiredCookies)-1 {
		t.Errorf("missing = %v; a non-cookie header contributed cookies", missing.Missing)
	}
	for _, n := range missing.Missing {
		if n == "OSID" {
			t.Error("OSID came from the real Cookie header and must be kept")
		}
	}
}

// -b / --cookie carries the cookie string directly, with no `cookie:` prefix.
func TestParsePasteAcceptsTheCookieFlag(t *testing.T) {
	got, err := cli.ParsePaste(`curl 'https://messages.google.com/' -b '` + fullCookieHeader() + `'`)
	if err != nil {
		t.Fatalf("ParsePaste: %v", err)
	}
	if len(got) != 7 {
		t.Errorf("kept %d cookies, want 7", len(got))
	}
}
