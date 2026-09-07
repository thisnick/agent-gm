package cli_test

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/cli"
)

// Profiles, spec section 11.5 as the owner's 2026-09-06 decision settles it.
//
// The whole point is that nothing is implicit. There is no profile called
// "default", no compiled-in hostname, and no server the CLI will talk to
// because it guessed. A profile is created by `agm auth login`, named for the
// server's host, and recorded as the active one; every later command uses the
// active profile and needs no `--server`.
//
// Every server in this file is a stub on 127.0.0.1 or the fixture origin
// https://gm.example.test. No deployment hostname appears anywhere.

const fixtureServer = "https://gm.example.test"

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("%q is not a URL with a host: %v", raw, err)
	}
	return u.Host
}

func writeCredentials(t *testing.T, path string, c cli.Credentials) {
	t.Helper()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCredentials(t *testing.T, path string) cli.Credentials {
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

// noProfileEnv is runCLI's environment with every implicit credential and
// server removed, so what a test proves is what the credentials file says and
// nothing else.
func noProfileEnv(credentials string) map[string]string {
	return map[string]string{
		"AGENT_GM_ACCESS_TOKEN":     "",
		"AGENT_GM_URL":              "",
		"AGENT_GM_CREDENTIALS_FILE": credentials,
	}
}

// 1. `agm auth login --server <url>` stores a profile named for the host and
// makes it active.
//
// Plant: in internal/cli/credentials.go's ResolveCredential, drop the
// `creating` branch's ProfileNameForServer call and use LegacyDefaultProfile
// instead, and this test fails on the profile name.
func TestAuthLoginNamesTheProfileForItsHostAndActivatesIt(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")

	got := runCLI(t, s, noProfileEnv(credentials),
		"a-fictional-admin-secret-at-least-43-characters-long\n",
		"auth", "login", "--admin", "--secret-stdin", "--server", s.URL)
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}

	stored := readCredentials(t, credentials)
	want := hostOf(t, s.URL)
	if _, ok := stored.Profiles[want]; !ok {
		t.Fatalf("no profile named %q was written; the file holds %v", want, stored.Profiles)
	}
	if _, legacy := stored.Profiles[cli.LegacyDefaultProfile]; legacy {
		t.Error("a profile named \"default\" was written; a profile is named for its server")
	}
	if stored.ActiveProfile != want {
		t.Errorf("active_profile is %q, want %q: a login that does not activate its own "+
			"profile leaves the next command with nothing to use", stored.ActiveProfile, want)
	}
	if stored.Profiles[want].Server != s.URL {
		t.Errorf("the profile is bound to %q, not to the server it logged in to",
			stored.Profiles[want].Server)
	}
}

// 2. A later command with no flags at all uses the active profile: its
// server AND its token.
//
// Plant: make ResolveCredential ignore Credentials.ActiveProfile (fall back to
// the empty name), and this test fails with exit 9 and the no-server message.
func TestACommandWithNoFlagsUsesTheActiveProfile(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	host := hostOf(t, s.URL)
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: host,
		Profiles: map[string]cli.Profile{
			host:                  {Server: s.URL, AccessToken: "agm_at_active"},
			"gm.example.test":     {Server: fixtureServer, AccessToken: "agm_at_other"},
			"two.gm.example.test": {Server: "https://two.gm.example.test", AccessToken: "agm_at_two"},
		},
	})

	got := runCLI(t, s, noProfileEnv(credentials), "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	seen := s.seen()
	if len(seen) == 0 {
		t.Fatal("no request was sent")
	}
	if seen[0].Authorization != "Bearer agm_at_active" {
		t.Errorf("the command presented %q; the active profile's token is agm_at_active",
			seen[0].Authorization)
	}
}

// 3. A login to a second server creates a second profile and leaves the
// first exactly as it was.
func TestASecondLoginAddsASecondProfileWithoutDisturbingTheFirst(t *testing.T) {
	first, second := newStub(t), newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	secret := "a-fictional-admin-secret-at-least-43-characters-long\n"

	if got := runCLI(t, first, noProfileEnv(credentials), secret,
		"auth", "login", "--admin", "--secret-stdin", "--server", first.URL); got.code != 0 {
		t.Fatalf("the first login exited %d\nstderr: %s", got.code, got.stderr)
	}
	before := readCredentials(t, credentials)

	if got := runCLI(t, second, noProfileEnv(credentials), secret,
		"auth", "login", "--admin", "--secret-stdin", "--server", second.URL); got.code != 0 {
		t.Fatalf("the second login exited %d\nstderr: %s", got.code, got.stderr)
	}
	after := readCredentials(t, credentials)

	firstHost, secondHost := hostOf(t, first.URL), hostOf(t, second.URL)
	if len(after.Profiles) != 2 {
		t.Fatalf("there are %d profiles, want 2: %v", len(after.Profiles), after.Profiles)
	}
	if !reflect.DeepEqual(after.Profiles[firstHost], before.Profiles[firstHost]) {
		t.Errorf("the first profile changed: %+v became %+v",
			before.Profiles[firstHost], after.Profiles[firstHost])
	}
	if after.Profiles[secondHost].Server != second.URL {
		t.Errorf("the second profile is bound to %q", after.Profiles[secondHost].Server)
	}
	if after.ActiveProfile != secondHost {
		t.Errorf("active_profile is %q; the profile just logged in to is the active one",
			after.ActiveProfile)
	}
}

// 4a. `agm profiles use` switches, and `agm profiles list` marks the active
// one.
func TestProfilesUseSwitchesAndListMarksTheActiveOne(t *testing.T) {
	a, b := newStub(t), newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	hostA, hostB := hostOf(t, a.URL), hostOf(t, b.URL)
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: hostA,
		Profiles: map[string]cli.Profile{
			hostA: {Server: a.URL, AccessToken: "agm_at_a"},
			hostB: {Server: b.URL, AccessToken: "agm_at_b"},
		},
	})

	list := runCLI(t, a, noProfileEnv(credentials), "", "profiles", "list")
	if list.code != 0 {
		t.Fatalf("`agm profiles list` exited %d\nstderr: %s", list.code, list.stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(list.stdout), "\n") {
		switch {
		case strings.Contains(line, hostA) && !strings.HasPrefix(line, "*"):
			t.Errorf("the active profile is not marked: %q", line)
		case strings.Contains(line, hostB) && strings.HasPrefix(line, "*"):
			t.Errorf("an inactive profile is marked active: %q", line)
		}
	}

	use := runCLI(t, b, noProfileEnv(credentials), "", "profiles", "use", hostB)
	if use.code != 0 {
		t.Fatalf("`agm profiles use` exited %d\nstderr: %s", use.code, use.stderr)
	}
	if got := readCredentials(t, credentials).ActiveProfile; got != hostB {
		t.Fatalf("active_profile is %q after `profiles use %s`", got, hostB)
	}

	// And the switch is what the next command acts on.
	if got := runCLI(t, b, noProfileEnv(credentials), "", "health"); got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if len(b.seen()) == 0 {
		t.Error("after `profiles use`, the command did not talk to that profile's server")
	}
	if n := len(a.seen()); n != 0 {
		t.Errorf("the command sent %d requests to the profile that is no longer active", n)
	}

	// An unknown name is refused rather than recorded.
	bad := runCLI(t, b, noProfileEnv(credentials), "", "profiles", "use", "not-a-profile")
	if bad.code != 2 {
		t.Errorf("`agm profiles use` on an unknown name exited %d, want 2", bad.code)
	}
	if got := readCredentials(t, credentials).ActiveProfile; got != hostB {
		t.Errorf("a refused `profiles use` still changed active_profile to %q", got)
	}
}

// 4b. `agm profiles remove` removes, and never leaves `active_profile`
// naming a profile that is gone.
func TestProfilesRemoveNeverLeavesADanglingActiveProfile(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	host := hostOf(t, s.URL)
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: host,
		Profiles: map[string]cli.Profile{
			host:                {Server: s.URL, AccessToken: "agm_at_stub"},
			"gm.example.test":   {Server: fixtureServer, AccessToken: "agm_at_one"},
			"two.gm.example.te": {Server: "https://two.gm.example.te", AccessToken: "agm_at_two"},
		},
	})

	got := runCLI(t, s, noProfileEnv(credentials), "", "profiles", "remove", host)
	if got.code != 0 {
		t.Fatalf("`agm profiles remove` exited %d\nstderr: %s", got.code, got.stderr)
	}
	after := readCredentials(t, credentials)
	if _, still := after.Profiles[host]; still {
		t.Error("the profile is still stored")
	}
	if after.ActiveProfile != "" {
		t.Errorf("active_profile is %q, which is not a stored profile: %v",
			after.ActiveProfile, after.Profiles)
	}
	// With no active profile, the next bare command says so; it does not
	// reach a server.
	next := runCLI(t, s, noProfileEnv(credentials), "", "health")
	if next.code != 9 {
		t.Errorf("a command with no active profile exited %d, want 9\nstderr: %s",
			next.code, next.stderr)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent with no active profile", n)
	}

	// Removing the second-to-last leaves the last one active rather than
	// nothing at all.
	if got := runCLI(t, s, noProfileEnv(credentials), "",
		"profiles", "remove", "two.gm.example.te"); got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if got := readCredentials(t, credentials).ActiveProfile; got != "gm.example.test" {
		t.Errorf("with one profile left, active_profile is %q", got)
	}

	if bad := runCLI(t, s, noProfileEnv(credentials), "",
		"profiles", "remove", "not-a-profile"); bad.code != 2 {
		t.Errorf("removing an unknown profile exited %d, want 2", bad.code)
	}
}

// 5. `--profile` and AGENT_GM_PROFILE select a profile for one invocation,
// without changing which one is active.
func TestProfileFlagAndEnvironmentSelectOneInvocation(t *testing.T) {
	active, other := newStub(t), newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	activeHost, otherHost := hostOf(t, active.URL), hostOf(t, other.URL)
	fixture := cli.Credentials{
		Version:       1,
		ActiveProfile: activeHost,
		Profiles: map[string]cli.Profile{
			activeHost: {Server: active.URL, AccessToken: "agm_at_active"},
			otherHost:  {Server: other.URL, AccessToken: "agm_at_other"},
		},
	}
	writeCredentials(t, credentials, fixture)

	if got := runCLI(t, other, noProfileEnv(credentials), "",
		"health", "--profile", otherHost); got.code != 0 {
		t.Fatalf("--profile exited %d\nstderr: %s", got.code, got.stderr)
	}
	if seen := other.seen(); len(seen) != 1 || seen[0].Authorization != "Bearer agm_at_other" {
		t.Errorf("--profile did not select that profile's server and token: %+v", other.seen())
	}

	env := noProfileEnv(credentials)
	env["AGENT_GM_PROFILE"] = otherHost
	if got := runCLI(t, other, env, "", "health"); got.code != 0 {
		t.Fatalf("AGENT_GM_PROFILE exited %d\nstderr: %s", got.code, got.stderr)
	}
	if n := len(other.seen()); n != 2 {
		t.Errorf("AGENT_GM_PROFILE selected %d requests to the named profile, want 2", n)
	}
	if n := len(active.seen()); n != 0 {
		t.Errorf("%d requests went to the active profile's server anyway", n)
	}

	// Neither changed what is active: selection is for one invocation.
	if got := readCredentials(t, credentials).ActiveProfile; got != activeHost {
		t.Errorf("active_profile became %q; --profile selects, it does not switch", got)
	}

	// A --profile naming nothing stored is refused.
	bad := runCLI(t, other, noProfileEnv(credentials), "", "health", "--profile", "not-a-profile")
	if bad.code != 2 {
		t.Errorf("--profile on an unknown name exited %d, want 2\nstderr: %s", bad.code, bad.stderr)
	}
}

// 6. `--server` naming a server no profile holds is refused on a command that
// is not `agm auth login`, and the message names what to type.
//
// Plant: delete the final refusal in ResolveCredential (return `c, nil`
// instead), and this test fails on the exit code.
func TestAnUnknownServerIsRefusedAndNamesAuthLogin(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: "gm.example.test",
		Profiles: map[string]cli.Profile{
			"gm.example.test": {Server: fixtureServer, AccessToken: "agm_at_elsewhere"},
		},
	})

	got := runCLI(t, s, noProfileEnv(credentials), "", "health", "--server", s.URL)
	if got.code != 2 {
		t.Fatalf("--server naming an unknown server exited %d, want 2 (invalid_request)"+
			"\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "agm auth login --server") {
		t.Errorf("the message does not name `agm auth login --server`:\n%s", got.stderr)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent to a server this machine holds no profile for", n)
	}
	for _, r := range s.seen() {
		if strings.Contains(r.Authorization, "agm_at_elsewhere") {
			t.Fatal("another origin's token was forwarded to the server named by --server")
		}
	}
}

// `agm auth login` is the exception, because it is where a profile comes
// from: it may name a server this machine has never seen.
func TestAuthLoginMayNameAServerWithNoProfile(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: "gm.example.test",
		Profiles: map[string]cli.Profile{
			"gm.example.test": {Server: fixtureServer, AccessToken: "agm_at_elsewhere"},
		},
	})

	got := runCLI(t, s, noProfileEnv(credentials),
		"a-fictional-admin-secret-at-least-43-characters-long\n",
		"auth", "login", "--admin", "--secret-stdin", "--server", s.URL)
	if got.code != 0 {
		t.Fatalf("`agm auth login --server` on a new server exited %d\nstderr: %s",
			got.code, got.stderr)
	}
	if _, ok := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]; !ok {
		t.Error("the login did not create a profile for the server it was given")
	}
}

// 7. Migration: a credentials file carrying a profile literally named
// "default" is renamed to its server's host and activated, on read, without
// losing a field.
//
// Plant: remove the LegacyDefaultProfile branch from
// internal/cli/credentials.go's migrate, and this test fails on the profile
// name.
func TestADefaultProfileIsMigratedToItsHostAndActivated(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	writeCredentials(t, credentials, cli.Credentials{
		Version: 1,
		Profiles: map[string]cli.Profile{
			cli.LegacyDefaultProfile: {
				Server:          s.URL,
				AccessToken:     "agm_at_legacy",
				RefreshToken:    "agm_rt_legacy",
				Scopes:          []string{"messages:read", "messages:write"},
				AuthorizationID: "authz_01k4z2p8w1",
				Issuer:          s.URL,
				Resource:        s.URL + "/mcp",
				ClientID:        "client_01k4z2p8w1",
			},
		},
	})

	// A bare command works, which is the migration doing its job: the old
	// file had no active_profile at all.
	got := runCLI(t, s, noProfileEnv(credentials), "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if seen := s.seen(); len(seen) == 0 || seen[0].Authorization != "Bearer agm_at_legacy" {
		t.Fatalf("the migrated profile's token was not used: %+v", s.seen())
	}

	// `agm profiles use` writes the file back, which is where the rename
	// lands on disk. The rename itself is on read and is idempotent.
	host := hostOf(t, s.URL)
	if use := runCLI(t, s, noProfileEnv(credentials), "",
		"profiles", "use", host); use.code != 0 {
		t.Fatalf("`agm profiles use %s` exited %d\nstderr: %s", host, use.code, use.stderr)
	}
	after := readCredentials(t, credentials)
	if _, legacy := after.Profiles[cli.LegacyDefaultProfile]; legacy {
		t.Error("the \"default\" profile is still on disk after a write-back")
	}
	migrated, ok := after.Profiles[host]
	if !ok {
		t.Fatalf("no profile named %q: %v", host, after.Profiles)
	}
	if after.ActiveProfile != host {
		t.Errorf("active_profile is %q, want %q", after.ActiveProfile, host)
	}
	// Every other field survived.
	want := cli.Profile{
		Server: s.URL, AccessToken: "agm_at_legacy", RefreshToken: "agm_rt_legacy",
		Scopes:          []string{"messages:read", "messages:write"},
		AuthorizationID: "authz_01k4z2p8w1", Issuer: s.URL, Resource: s.URL + "/mcp",
		ClientID: "client_01k4z2p8w1",
	}
	if migrated.Server != want.Server || migrated.AccessToken != want.AccessToken ||
		migrated.RefreshToken != want.RefreshToken || migrated.Issuer != want.Issuer ||
		migrated.Resource != want.Resource || migrated.ClientID != want.ClientID ||
		migrated.AuthorizationID != want.AuthorizationID ||
		strings.Join(migrated.Scopes, " ") != strings.Join(want.Scopes, " ") {
		t.Errorf("the migration lost a field:\n got %+v\nwant %+v", migrated, want)
	}
	if after.Version != 1 {
		t.Errorf("the file's version became %d; an added field is not a new format",
			after.Version)
	}
}

// 8. No server configured anywhere fails, and fails LOCALLY. There is no
// compiled-in hostname to fall back on, so nothing is dialled and the message
// says what to type.
//
// Plant: restore a hostname fallback anywhere on the server-resolution path
// (for instance in NoChromeMessage or in ResolveCredential), and the
// no-hardcoded-host assertion below fails.
func TestNoServerAnywhereFailsWithoutReachingABuiltInHost(t *testing.T) {
	s := newStub(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")

	got := runCLI(t, s, noProfileEnv(credentials), "", "health")
	if got.code != 9 {
		t.Fatalf("no configured server exited %d, want 9\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "agm auth login --server") {
		t.Errorf("the message does not say what to do:\n%s", got.stderr)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent with no server configured", n)
	}
	// Nothing on any output stream names a host the operator did not give.
	for _, out := range []string{got.stdout, got.stderr, cli.NoChromeMessage("")} {
		if strings.Contains(out, "://gm.") && !strings.Contains(out, "gm.example.test") {
			t.Errorf("a built-in hostname appears in the CLI's own output:\n%s", out)
		}
	}
}
