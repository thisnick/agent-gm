package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/cli"
)

// CLI profiles against a REAL server (spec section 11.5, as the owner's
// 2026-09-06 decision settles it).
//
// internal/cli's profiles_test.go proves the same rules against a stub. This
// file exists because the claim that follows from them -- "log in once, then
// every command needs no --server" -- is a claim about a credential a real
// server minted and a real server then accepts. A stub cannot refuse a token
// it never issued.
//
// Nothing here names a deployment hostname: every server is a listener on
// 127.0.0.1 that this test started.

const profilesTestSecret = oauthTestAdminSecret + "\n"

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("%q is not a URL with a host: %v", raw, err)
	}
	return u.Host
}

func readStoredCredentials(t *testing.T, path string) cli.Credentials {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	var c cli.Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return c
}

// runAgm drives the real CLI with NO ambient environment at all: no
// AGENT_GM_URL, no AGENT_GM_ACCESS_TOKEN, nothing. Whatever the command
// manages to do, it does from the credentials file.
func runAgm(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runAgmAt(t, nil, args...)
}

// runAgmAt is runAgm with the clock injected, for the proactive refresh of
// spec section 11.5. The server's clock is real; only the CLI's view of "is
// this token about to expire" moves, which is exactly the decision under test.
func runAgmAt(t *testing.T, now func() time.Time, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(cli.Env{
		Args:   args,
		Stdin:  strings.NewReader(profilesTestSecret),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
		Now:    now,
	})
	return code, stdout.String(), stderr.String()
}

// One login, then every later command needs no --server: it uses the active
// profile, and the token that profile holds is one the real server accepts.
func TestSlice3bLoginActivatesItsProfileAndLaterCommandsNeedNoServer(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	host := mustHost(t, h.http.URL)

	code, _, stderr := runAgm(t, "auth", "login", "--admin", "--secret-stdin",
		"--server", h.http.URL, "--credentials-file", credentials)
	if code != 0 {
		t.Fatalf("`agm auth login --admin` exited %d\nstderr: %s", code, stderr)
	}

	stored := readStoredCredentials(t, credentials)
	if _, ok := stored.Profiles[host]; !ok {
		t.Fatalf("no profile named %q: %v", host, stored.Profiles)
	}
	if stored.ActiveProfile != host {
		t.Fatalf("active_profile is %q, want %q", stored.ActiveProfile, host)
	}

	// No --server, no --profile, no environment. This is the whole point of
	// the decision.
	code, stdout, stderr := runAgm(t, "auth", "whoami",
		"--credentials-file", credentials, "--json")
	if code != 0 {
		t.Fatalf("`agm auth whoami` with no flags exited %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "admin") {
		t.Errorf("whoami did not report the admin session it logged in as:\n%s", stdout)
	}

	// `agm profiles list` marks it, and drives no route: it answers with the
	// server stopped.
	h.stop()
	code, stdout, stderr = runAgm(t, "profiles", "list", "--credentials-file", credentials)
	if code != 0 {
		t.Fatalf("`agm profiles list` exited %d\nstderr: %s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "*") ||
		!strings.Contains(stdout, host) {
		t.Errorf("the active profile is not marked in:\n%s", stdout)
	}
}

// A login to a second server adds a second profile, leaves the first alone,
// and `agm profiles use` switches between two servers that are both really
// running.
func TestSlice3bTwoServersAreTwoProfilesAndUseSwitchesBetweenThem(t *testing.T) {
	first := newOAuthHarnessOnItsOwnURL(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	firstHost := mustHost(t, first.http.URL)

	if code, _, stderr := runAgm(t, "auth", "login", "--admin", "--secret-stdin",
		"--server", first.http.URL, "--credentials-file", credentials); code != 0 {
		t.Fatalf("the first login exited %d\nstderr: %s", code, stderr)
	}
	before := readStoredCredentials(t, credentials)

	second := newOAuthHarnessOnItsOwnURL(t)
	secondHost := mustHost(t, second.http.URL)
	if code, _, stderr := runAgm(t, "auth", "login", "--admin", "--secret-stdin",
		"--server", second.http.URL, "--credentials-file", credentials); code != 0 {
		t.Fatalf("the second login exited %d\nstderr: %s", code, stderr)
	}

	after := readStoredCredentials(t, credentials)
	if len(after.Profiles) != 2 {
		t.Fatalf("there are %d profiles, want 2: %v", len(after.Profiles), after.Profiles)
	}
	if after.Profiles[firstHost].AccessToken != before.Profiles[firstHost].AccessToken {
		t.Error("the second login overwrote the first server's token")
	}
	if after.ActiveProfile != secondHost {
		t.Fatalf("active_profile is %q after logging in to the second server", after.ActiveProfile)
	}

	// The active profile's token is accepted by ITS server, and that is the
	// server a bare command reaches.
	if code, _, stderr := runAgm(t, "auth", "whoami",
		"--credentials-file", credentials); code != 0 {
		t.Fatalf("whoami on the second server exited %d\nstderr: %s", code, stderr)
	}

	// Switch back, and the first server's own token is what is presented.
	if code, _, stderr := runAgm(t, "profiles", "use", firstHost,
		"--credentials-file", credentials); code != 0 {
		t.Fatalf("`agm profiles use` exited %d\nstderr: %s", code, stderr)
	}
	if code, _, stderr := runAgm(t, "auth", "whoami",
		"--credentials-file", credentials); code != 0 {
		t.Fatalf("whoami after switching exited %d\nstderr: %s", code, stderr)
	}

	// A --server naming the OTHER running server is still refused unless a
	// profile holds it -- and here one does, so it is honoured, with that
	// server's own token.
	if code, _, stderr := runAgm(t, "auth", "whoami", "--server", second.http.URL,
		"--credentials-file", credentials); code != 0 {
		t.Fatalf("--server naming a stored profile's server exited %d\nstderr: %s", code, stderr)
	}
	// ... whereas one no profile holds is refused before anything is sent.
	code, _, stderr := runAgm(t, "auth", "whoami", "--server", "https://gm.example.test",
		"--credentials-file", credentials)
	if code != 2 {
		t.Fatalf("--server naming an unknown server exited %d, want 2\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "agm auth login --server") {
		t.Errorf("the refusal does not name `agm auth login --server`:\n%s", stderr)
	}
}

// A stored profile refreshes itself against the REAL server (spec section
// 11.5).
//
// `admin.access_token_ttl` defaults to 15 minutes and nothing refreshed a
// profile, so an admin session was dead a quarter of an hour after it was
// minted and the next command read as a server fault. The clock is injected
// rather than waited out: the CLI is told it is standing at the token's
// expiry, and the exchange it then performs is a real one.
func TestSlice3bAnExpiringProfileRefreshesItselfAgainstTheServer(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	host := mustHost(t, h.http.URL)

	if code, _, stderr := runAgm(t, "auth", "login", "--admin", "--secret-stdin",
		"--server", h.http.URL, "--credentials-file", credentials); code != 0 {
		t.Fatalf("`agm auth login --admin` exited %d\nstderr: %s", code, stderr)
	}
	before := readStoredCredentials(t, credentials).Profiles[host]
	if before.ExpiresAt == "" {
		t.Fatal("the profile records no expires_at, so nothing can refresh proactively")
	}
	if before.RefreshToken == "" {
		t.Fatal("the profile records no refresh token")
	}
	expiry, err := time.Parse(time.RFC3339, before.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", before.ExpiresAt, err)
	}

	// Standing 30 seconds before the expiry: inside the 60-second window.
	code, _, stderr := runAgmAt(t, func() time.Time { return expiry.Add(-30 * time.Second) },
		"auth", "whoami", "--credentials-file", credentials)
	if code != 0 {
		t.Fatalf("`agm auth whoami` exited %d\nstderr: %s", code, stderr)
	}

	after := readStoredCredentials(t, credentials).Profiles[host]
	if after.AccessToken == before.AccessToken {
		t.Error("the access token was not rotated; nothing refreshed")
	}
	if after.RefreshToken == before.RefreshToken {
		t.Error("the refresh token was not rotated")
	}
	// The new expiry is recorded. It is not asserted to DIFFER from the old
	// one: the TTL is fixed and the timestamp is truncated to the second, so
	// a login and a refresh in the same second legitimately produce the same
	// string. What matters is that the field is still there and still ahead
	// of the old one, so the next command does not refresh again for nothing.
	newExpiry, err := time.Parse(time.RFC3339, after.ExpiresAt)
	if err != nil {
		t.Fatalf("the refreshed profile's expires_at %q is not RFC3339: %v", after.ExpiresAt, err)
	}
	if newExpiry.Before(expiry) {
		t.Errorf("the stored expiry went backwards: %s then %s", before.ExpiresAt, after.ExpiresAt)
	}

	// And the token it exchanged really works, while the one it replaced is
	// single-use and gone: presenting the OLD refresh token is refused.
	if whoami := h.get("/v1/auth/whoami", bearer(after.AccessToken)); whoami.StatusCode != 200 {
		t.Fatalf("the refreshed access token answered %d at whoami", whoami.StatusCode)
	}
	reuse := h.postJSON("/v1/auth/refresh", map[string]any{"refresh_token": before.RefreshToken})
	if reuse.StatusCode == 200 {
		t.Error("the refresh token that was already spent was accepted a second time")
	}
	_ = readBody(t, reuse)
}

// A revoked authorization is exit 3 with the login guidance: the refusal is
// not a merely expired access token any more, it is a refresh that was
// attempted and refused.
func TestSlice3bARevokedAuthorizationIsExitThreeWithLoginGuidance(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	host := mustHost(t, h.http.URL)

	if code, _, stderr := runAgm(t, "auth", "login", "--admin", "--secret-stdin",
		"--server", h.http.URL, "--credentials-file", credentials); code != 0 {
		t.Fatalf("`agm auth login --admin` exited %d\nstderr: %s", code, stderr)
	}
	profile := readStoredCredentials(t, credentials).Profiles[host]
	if profile.AuthorizationID == "" {
		t.Fatal("the profile records no authorization_id")
	}

	// A SECOND admin session does the revoking, so the CLI's own session is
	// the one that dies.
	admin := h.adminToken()
	req, err := http.NewRequest(http.MethodDelete,
		h.http.URL+"/v1/admin/authorizations/"+profile.AuthorizationID, nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer(admin)(req)
	revoke := h.do(req)
	if revoke.StatusCode != 200 {
		t.Fatalf("revoking answered %d: %s", revoke.StatusCode, readBody(t, revoke))
	}
	_ = readBody(t, revoke)

	code, _, stderr := runAgm(t, "auth", "whoami", "--credentials-file", credentials)
	if code != 3 {
		t.Fatalf("a revoked authorization exited %d, want 3\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "agm auth login") {
		t.Errorf("the message does not say what to do:\n%s", stderr)
	}
	// The refresh was attempted and refused; the profile is left as it was
	// rather than half-rewritten.
	if after := readStoredCredentials(t, credentials).Profiles[host]; after.RefreshToken !=
		profile.RefreshToken {
		t.Error("a refused refresh rewrote the profile")
	}
}
