package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	var stdout, stderr bytes.Buffer
	code := cli.Run(cli.Env{
		Args:   args,
		Stdin:  strings.NewReader(profilesTestSecret),
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
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
