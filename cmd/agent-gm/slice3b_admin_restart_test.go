package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/cli"
)

// An admin session must survive a restart of the server (§4: durable state
// lives in SQLite; §9.6: admin sessions have TTLs, and a TTL is a promise
// about time rather than about a process).
//
// Found in production: after the container restarted, the host CLI's admin
// session -- minted about two hours earlier, well inside its TTL -- was
// refused with "the bearer token was rejected", while an OAuth access token
// minted at the same time kept working. The two are stored in the same tables,
// so a difference between them is a bug in whatever treats them differently.
func TestSlice3bAnAdminSessionSurvivesARestart(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()

	if resp := h.get("/v1/auth/whoami", bearer(admin)); resp.StatusCode != http.StatusOK {
		t.Fatalf("the freshly minted admin session answered %d before any restart", resp.StatusCode)
	}
	before := dataOf(t, h.get("/v1/auth/whoami", bearer(admin)))

	// A real restart: the server is closed and a new one is built over the
	// SAME data directory, with the same configuration, exactly as a
	// container restart does it.
	restarted := h.restart(t)

	resp := restarted.get("/v1/auth/whoami", bearer(admin))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an admin session minted before the restart answered %d after it: %s\n"+
			"Section 4 puts durable state in SQLite and section 9.6 gives admin sessions a "+
			"TTL; a session that dies because the process did is neither.",
			resp.StatusCode, readBody(t, resp))
	}
	after := dataOf(t, resp)
	if after["authorization_id"] != before["authorization_id"] {
		t.Errorf("the same token resolves to authorization %v after the restart and %v before",
			after["authorization_id"], before["authorization_id"])
	}

	// And it is still an admin session, not something narrowed by the restart.
	if resp := restarted.get("/v1/admin/authorizations", bearer(admin)); resp.StatusCode != http.StatusOK {
		t.Errorf("after the restart the admin session answered %d on an admin route",
			resp.StatusCode)
	}
}

// And the mechanism that really killed the production session, isolated: a
// restart under a DIFFERENT admin secret.
//
// This is section 12.1 working as specified -- "changing the admin secret
// revokes every previous admin bootstrap authorization on next start" -- and
// it reproduces the production symptom exactly, including the part that made
// it look like a bug: **the OAuth token survives**. Only admin bootstrap
// authorizations are revoked, because only they are derived from the secret.
// So a deployment whose admin secret is not stable across restarts loses its
// operator CLI on every restart and keeps its connectors, which is precisely
// what was observed.
func TestSlice3bARestartUnderANewAdminSecretRevokesOnlyTheAdminSession(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	f := h.fullFlow("http://127.0.0.1:53211/callback")
	tokens := tokenValues(t, h.exchange(f, nil))
	access, _ := tokens["access_token"].(string)

	restarted := h.restartWithSecret(t, "a-different-admin-secret-0123456789abcdefghijklmnopqrstuv")

	if resp := restarted.get("/v1/auth/whoami", bearer(admin)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after a restart under a new admin secret the old admin session answered %d, "+
			"want 401 -- section 12.1 revokes it on next start", resp.StatusCode)
	}
	// The OAuth grant is untouched, which is the asymmetry that makes this
	// look like a bug in production when the real cause is an unstable secret.
	if resp := restarted.get("/v1/auth/whoami", bearer(access)); resp.StatusCode != http.StatusOK {
		t.Errorf("a restart under a new admin secret also killed an OAuth access token (%d); "+
			"section 12.1 revokes admin bootstrap authorizations only", resp.StatusCode)
	}
}

// And the message an operator actually sees, because the hour lost to this
// was lost to the message rather than to the behaviour.
//
// `agm` cannot tell from a 401 which credential it was carrying, so it names
// both fixes and the one cause that does not look like a cause: the admin
// secret changing. A message that said only "log in again with `agm auth
// login`" sends an operator holding an admin profile to the wrong command and
// tells them nothing about why a connector's token still works.
func TestSlice3bTheCLIExplainsARefusedAdminSession(t *testing.T) {
	h := newOAuthHarness(t)
	admin := h.adminToken()
	restarted := h.restartWithSecret(t, "a-different-admin-secret-0123456789abcdefghijklmnopqrstuv")

	var stdout, stderr bytes.Buffer
	env := map[string]string{"AGENT_GM_ACCESS_TOKEN": admin}
	code := cli.Run(cli.Env{
		Args: []string{
			"admin", "authorizations", "list",
			"--server", restarted.http.URL,
			"--credentials-file", filepath.Join(t.TempDir(), "credentials.json"),
		},
		Stdout: &stdout,
		Stderr: &stderr,
		Getenv: func(k string) string { return env[k] },
	})
	if code == 0 {
		t.Fatalf("the revoked admin session was accepted: %s", stdout.String())
	}
	got := stderr.String()
	for _, want := range []string{
		"agm auth login --admin",
		"AGENT_GM_ADMIN_SECRET",
		"a restart alone does not end",
		"auth.admin_authorization_revoked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not mention %q; an operator reading it has to guess.\n%s",
				want, got)
		}
	}
}
