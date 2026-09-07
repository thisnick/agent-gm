package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/cli"
)

// `agm auth login` on the OAuth path, driven end to end against the real
// server (spec section 11.5, and the `agm` half of section 16 Slice 3).
//
// This test exists because `agm` performing the SAME flow as any other MCP
// client is a claim, and a claim about a flow is only worth what an execution
// of it is worth. It runs the real `cli.Run` -- the same entry point the
// binary calls -- against the real `buildServer` handler, and plays the part
// of the browser: it loads the authorization screen the CLI printed, submits
// the enrollment code, approves the request through the admin route, and
// completes. Nothing about the credential path is stubbed.

var authorizeURLLine = regexp.MustCompile(`(https?://\S+/oauth/authorize\?\S+)`)

func TestSlice3AgmAuthLoginRunsTheWholeOAuthFlow(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	admin := h.adminToken()
	_, enrollValue := h.enrollmentCode(admin, map[string]any{
		"label":  "agm",
		"scopes": []string{"messages:read", "messages:write"},
	})

	credentials := filepath.Join(t.TempDir(), "credentials.json")

	// A locked buffer, because the CLI writes from its own goroutine while
	// this test reads to find the URL it printed.
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	var wg sync.WaitGroup
	var exit int
	wg.Add(1)
	go func() {
		defer wg.Done()
		exit = cli.Run(cli.Env{
			// `--server` is the httptest listener, because that is what the
			// CLI dials. The ISSUER it verifies is what the metadata says,
			// and the two must agree -- so this test also proves the
			// issuer-binding check does not fire spuriously.
			Args: []string{
				"auth", "login", "--no-browser",
				"--scopes", "messages:read messages:write",
				"--server", h.http.URL,
				"--credentials-file", credentials,
				"--json",
			},
			Stdout: stdout,
			Stderr: stderr,
			Getenv: func(string) string { return "" },
		})
	}()

	// Play the browser. The CLI prints the authorization URL because
	// --no-browser was given; scraping it is exactly what an owner on a
	// headless box does by hand.
	authorizeURL := waitForAuthorizeURL(t, stderr)

	req, err := http.NewRequest(http.MethodGet, authorizeURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	screen := h.do(req)
	if screen.StatusCode != http.StatusOK {
		t.Fatalf("the authorization screen answered %d\n%s", screen.StatusCode, readBody(t, screen))
	}
	h.captureCookie(screen)
	form := hiddenFields(t, readBody(t, screen))
	form.Add("scope_selected", "messages:read")
	form.Add("scope_selected", "messages:write")
	form.Set("enrollment_code", enrollValue)

	submitted := h.postForm("/oauth/authorize", form, h.withCookie)
	if submitted.StatusCode != http.StatusSeeOther {
		t.Fatalf("submitting the form answered %d\n%s",
			submitted.StatusCode, readBody(t, submitted))
	}
	requestID := strings.TrimPrefix(submitted.Header.Get("Location"), "/oauth/requests/")
	_ = readBody(t, submitted)

	approve := h.postJSON("/v1/admin/authorization-requests/"+requestID+"/approve",
		map[string]any{}, bearer(admin))
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approving answered %d\n%s", approve.StatusCode, readBody(t, approve))
	}
	_ = readBody(t, approve)

	complete := h.postForm("/oauth/requests/"+requestID+"/complete",
		url.Values{"form_token": {form.Get("form_token")}}, h.withCookie, h.sameOrigin)
	if complete.StatusCode != http.StatusSeeOther {
		t.Fatalf("completing answered %d\n%s", complete.StatusCode, readBody(t, complete))
	}
	// The callback goes to the CLI's own loopback listener, which is what
	// releases it. Following the redirect is the browser's job, so this test
	// does it.
	callback := complete.Header.Get("Location")
	_ = readBody(t, complete)
	cb, err := http.NewRequest(http.MethodGet, callback, nil)
	if err != nil {
		t.Fatal(err)
	}
	cbResp := h.do(cb)
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("the CLI's callback answered %d: %s", cbResp.StatusCode, readBody(t, cbResp))
	}
	_ = readBody(t, cbResp)

	wg.Wait()
	if exit != 0 {
		t.Fatalf("agm auth login exited %d\nstderr:\n%s\nstdout:\n%s",
			exit, stderr.String(), stdout.String())
	}

	// The profile is stored, bound to the exact issuer and resource, and the
	// tokens are NOT on stdout.
	raw, err := os.ReadFile(credentials) //nolint:gosec // the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		ActiveProfile string `json:"active_profile"`
		Profiles      map[string]struct {
			Server       string   `json:"server"`
			AccessToken  string   `json:"access_token"`
			RefreshToken string   `json:"refresh_token"`
			Scopes       []string `json:"scopes"`
			Issuer       string   `json:"issuer"`
			Resource     string   `json:"resource"`
			ClientID     string   `json:"client_id"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	// The profile is named for the server's HOST and is the active one
	// (the owner's 2026-09-06 decision, spec section 11.5). There is no
	// profile called "default" any more.
	name := mustHost(t, h.http.URL)
	profile, ok := stored.Profiles[name]
	if !ok {
		t.Fatalf("no profile named %q was written: %s", name, raw)
	}
	if stored.ActiveProfile != name {
		t.Errorf("active_profile is %q, want %q", stored.ActiveProfile, name)
	}
	if profile.AccessToken == "" || profile.RefreshToken == "" {
		t.Error("the profile carries no tokens")
	}
	if profile.ClientID == "" {
		t.Error("the profile does not record the client_id its tokens belong to; the " +
			"OAuth refresh grant requires one (section 9.6)")
	}
	if profile.Issuer != h.http.URL {
		t.Errorf("the profile's issuer is %q, want the server's own %q", profile.Issuer, h.http.URL)
	}
	if profile.Resource != h.http.URL+"/mcp" {
		t.Errorf("the profile's resource is %q, want %q", profile.Resource, h.http.URL+"/mcp")
	}
	if strings.Join(profile.Scopes, " ") != "messages:read messages:write" {
		t.Errorf("the granted scopes are %v", profile.Scopes)
	}
	if strings.Contains(stdout.String(), profile.AccessToken) ||
		strings.Contains(stdout.String(), profile.RefreshToken) {
		t.Error("agm auth login printed a token to stdout; stdout is a transcript, and a " +
			"transcript is not where a bearer token belongs (section 12.1)")
	}

	// And the token it obtained actually works.
	whoami := h.get("/v1/auth/whoami", bearer(profile.AccessToken))
	if whoami.StatusCode != http.StatusOK {
		t.Fatalf("the token agm obtained answered %d at whoami", whoami.StatusCode)
	}
	data := dataOf(t, whoami)
	if data["kind"] != "oauth" {
		t.Errorf("the authorization's kind is %v, want oauth", data["kind"])
	}
}

// TestSlice3AgmAuthLoginRefusesAdminScope is section 11.5's other half:
// `admin` is refused at `--scopes` and comes only from
// `agm auth login --admin`.
func TestSlice3AgmAuthLoginRefusesAdminScope(t *testing.T) {
	h := newOAuthHarnessOnItsOwnURL(t)
	var stdout, stderr bytes.Buffer
	exit := cli.Run(cli.Env{
		Args: []string{
			"auth", "login", "--no-browser", "--scopes", "admin",
			"--server", h.http.URL,
			"--credentials-file", filepath.Join(t.TempDir(), "credentials.json"),
		},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(string) string { return "" },
	})
	if exit != 2 {
		t.Fatalf("`--scopes admin` exited %d, want the usage exit 2\n%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "admin is never issued through OAuth") {
		t.Errorf("the refusal does not say why:\n%s", stderr.String())
	}
}

// lockedBuffer is a bytes.Buffer a second goroutine may read while the CLI
// writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForAuthorizeURL(t *testing.T, buf *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := authorizeURLLine.FindStringSubmatch(buf.String()); m != nil {
			return strings.TrimRight(m[1], ".")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agm never printed an authorization URL:\n%s", buf.String())
	return ""
}
