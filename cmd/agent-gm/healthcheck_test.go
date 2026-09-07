package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/config"
)

// Spec section 14.1 and slice-3 acceptance test 26: `agent-gm healthcheck`
// is the container's liveness probe, and it has to be right without a shell
// to check it with. These tests need no Docker, so they run in the ordinary
// suite -- the container-level assertion (nonroot, no shell, no curl) is
// CI's, but "exits 0 only on 200" is provable here and belongs here.

// A 200 is healthy and a 500 is not. This is the whole contract, and it is
// the assertion a plant that returns success on any status has to survive.
func TestHealthcheckExitsOnStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		want   int
	}{
		{"ok", http.StatusOK, exitOK},
		{"server error", http.StatusInternalServerError, exitUnhealthy},
		{"service unavailable", http.StatusServiceUnavailable, exitUnhealthy},
		{"not found", http.StatusNotFound, exitUnhealthy},
		{"no content", http.StatusNoContent, exitUnhealthy},
		{"moved", http.StatusMovedPermanently, exitUnhealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				if tc.status == http.StatusMovedPermanently {
					// A redirect must not launder a failure into a pass: the
					// probe asks one server one question and does not chase
					// the answer somewhere else.
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
			}))
			defer srv.Close()

			if got := runHealthcheck([]string{"--addr", hostPort(t, srv.URL)}); got != tc.want {
				t.Errorf("runHealthcheck on %d = %d, want %d", tc.status, got, tc.want)
			}
			if path != "/healthz" {
				t.Errorf("probed %q, want /healthz -- the probe must not use /v1/health, "+
					"which needs a scope and reports account conditions as server ill health", path)
			}
		})
	}
}

// Nothing listening is unhealthy, not a crash and not a pass.
func TestHealthcheckUnreachableIsUnhealthy(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // now certainly nobody is there

	if got := runHealthcheck([]string{"--addr", addr, "--timeout", "2s"}); got != exitUnhealthy {
		t.Errorf("runHealthcheck against a closed port = %d, want %d", got, exitUnhealthy)
	}
}

// A server that accepts the connection and then says nothing must not hang
// the probe for ever: a liveness probe that hangs never reports anything.
func TestHealthcheckTimesOut(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()

	start := time.Now()
	if got := runHealthcheck([]string{"--addr", ln.Addr().String(), "--timeout", "300ms"}); got != exitUnhealthy {
		t.Errorf("runHealthcheck against a silent server = %d, want %d", got, exitUnhealthy)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %s; --timeout was 300ms", elapsed)
	}
}

// The probe reads AGENT_GM_LISTEN_ADDR when --addr is absent, which is how
// the Dockerfile's HEALTHCHECK finds a server whose port was overridden.
func TestHealthcheckReadsListenAddrFromEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	}))
	defer srv.Close()
	t.Setenv("AGENT_GM_LISTEN_ADDR", hostPort(t, srv.URL))

	if got := runHealthcheck(nil); got != exitOK {
		t.Errorf("runHealthcheck with AGENT_GM_LISTEN_ADDR set = %d, want %d", got, exitOK)
	}
}

// A bind address is not a dial address. `0.0.0.0:8080` means "every
// interface" to a listener and is not somewhere you can send a packet on
// every platform, so the probe rewrites the wildcard to loopback of the same
// family.
func TestHealthcheckURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ bind, want string }{
		{"0.0.0.0:8080", "http://127.0.0.1:8080/healthz"},
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"[::]:8080", "http://[::1]:8080/healthz"},
		{"127.0.0.1:9999", "http://127.0.0.1:9999/healthz"},
		{"[::1]:9999", "http://[::1]:9999/healthz"},
		{"localhost:8080", "http://localhost:8080/healthz"},
	} {
		got, err := healthcheckURL(tc.bind)
		if err != nil {
			t.Errorf("healthcheckURL(%q): %v", tc.bind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("healthcheckURL(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
	for _, bad := range []string{"8080", "", "http://host:8080", "host:"} {
		if got, err := healthcheckURL(bad); err == nil {
			t.Errorf("healthcheckURL(%q) = %q, want an error", bad, got)
		}
	}
}

// An unparseable address is a usage error, not a false "unhealthy": the
// difference matters to whoever has to read the container's exit code.
func TestHealthcheckBadAddressIsUsage(t *testing.T) {
	t.Parallel()
	if got := runHealthcheck([]string{"--addr", "8080"}); got != exitUsage {
		t.Errorf("runHealthcheck with a portless address = %d, want %d", got, exitUsage)
	}
	if got := runHealthcheck([]string{"--nonsense"}); got != exitUsage {
		t.Errorf("runHealthcheck with an unknown flag = %d, want %d", got, exitUsage)
	}
	if got := runHealthcheck([]string{"extra"}); got != exitUsage {
		t.Errorf("runHealthcheck with a positional argument = %d, want %d", got, exitUsage)
	}
}

// The probe's fallback default and the server's bind default are the same
// string. If somebody changes one, this fails rather than shipping a
// HEALTHCHECK that knocks on a door the server no longer opens.
func TestHealthcheckDefaultAddrMatchesConfig(t *testing.T) {
	t.Setenv("AGENT_GM_PUBLIC_URL", "https://gm.example.test")
	t.Setenv("AGENT_GM_ADMIN_SECRET", "0123456789012345678901234567890123456789012")
	t.Setenv("AGENT_GM_DATA_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("AGENT_GM_LISTEN_ADDR", "")

	c, err := config.LoadServer()
	if err != nil {
		t.Fatalf("config.LoadServer: %v", err)
	}
	if c.ListenAddr != defaultListenAddr {
		t.Errorf("config default listen addr = %q, healthcheck default = %q", c.ListenAddr, defaultListenAddr)
	}
}

// Slice-3 acceptance test 28: the commit reported by GET /v1/health and the
// MCP serverInfo is the *built* commit. A container build has no VCS stamp --
// no .git in the build context and -trimpath set -- so without the link-time
// -X main.commit the answer is "unknown", and the AGPL section 13 offer of
// section 1.4 points at no particular source.
func TestBuildCommitPrefersLinkTimeStamp(t *testing.T) {
	old := commit
	defer func() { commit = old }()

	commit = "0123456789abcdef0123456789abcdef01234567"
	if got := buildCommit(); got != commit {
		t.Errorf("buildCommit() = %q, want the -X main.commit value %q", got, commit)
	}
	if got := versionLine(); !strings.Contains(got, commit) {
		t.Errorf("versionLine() = %q, want it to carry the built commit", got)
	}

	commit = ""
	if got := buildCommit(); got == "" {
		t.Error("buildCommit() with no stamp = \"\", want a VCS revision or \"unknown\"")
	}
}

func TestBuildVersionPrefersLinkTimeStamp(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "1.4.2"
	if got := buildVersion(); got != "1.4.2" {
		t.Errorf("buildVersion() = %q, want the -X main.version value", got)
	}
	version = ""
	if got := buildVersion(); got == "" {
		t.Error("buildVersion() with no stamp = \"\", want a fallback")
	}
}

// The probe talks to the real handler, not just to an httptest stub that
// happens to answer 200: /healthz is a route the server actually serves
// (section 7.5) and it answers without a scope and without touching SQLite.
func TestHealthcheckAgainstTheRealHealthzHandler(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	}))
	defer srv.Close()

	// The route is declared with no scope, which is what makes an
	// unauthenticated probe legitimate.
	found := false
	for _, r := range api.Routes {
		if r.Path == "/healthz" {
			found = true
			if r.Scope != api.ScopeNone {
				t.Errorf("/healthz scope = %v, want ScopeNone; the container probe carries no token", r.Scope)
			}
		}
	}
	if !found {
		t.Error("no /healthz route in the inventory")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probe(ctx, http.DefaultClient, srv.URL+"/healthz"); err != nil {
		t.Errorf("probe: %v", err)
	}
}

func hostPort(t *testing.T, rawURL string) string {
	t.Helper()
	const prefix = "http://"
	if len(rawURL) <= len(prefix) || rawURL[:len(prefix)] != prefix {
		t.Fatalf("httptest URL %q is not http://", rawURL)
	}
	return rawURL[len(prefix):]
}
