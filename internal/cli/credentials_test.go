package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/cli"
)

// Spec section 11.5's write-back safety rule, in both directions. It is the
// part of the credentials story worth knowing before an automated run, and
// the ordering is the whole of it:
//
//   - the destination is proved writable BEFORE the token is spent, so an
//     unwritable directory is exit 9 while the token is still good;
//   - a token the server has already refused is exit 3, and is never retried.
//
// Rotation makes a refresh token single-use: a run that exchanges one and
// then cannot store the replacement has LOST it. That is the failure the
// ordering exists to prevent, so the first test asserts not only the exit
// code but that the exchange never happened and the stored token is
// unchanged.
//
// Plant: move the beginWrite call in internal/cli/run.go's
// exchangeRefreshToken to AFTER the POST /v1/auth/refresh, and
// TestAnUnwritableDestinationIsNineAndSpendsNothing fails on the request
// count. Planted 2026-09-06.
func TestAnUnwritableDestinationIsNineAndSpendsNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write into a 0500 directory")
	}
	s := newStub(t)

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "refresh-token")
	if err := os.WriteFile(tokenFile, []byte("agm_rt_original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The directory the rename would happen in is not writable, so the
	// temporary file cannot be created.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_REFRESH_TOKEN_FILE": tokenFile,
		"AGENT_GM_CLIENT_ID":          "client_01k4z2p8w1",
		"AGENT_GM_ACCESS_TOKEN":       "",
	}, "", "health")

	if got.code != 9 {
		t.Fatalf("an unwritable credentials destination exited %d; spec 11.5 says 9, while the "+
			"token is still good\nstderr: %s", got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 0 {
		t.Errorf("the token was spent %d times before the destination was proved writable; "+
			"spec 11.5 requires the temporary file to be created FIRST", n)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "agm_rt_original" {
		t.Errorf("the stored refresh token changed: %q", string(raw))
	}
}

// The other direction: a token the server has already refused is exit 3, and
// is never retried.
func TestARefusedRefreshTokenIsThreeAndIsNeverRetried(t *testing.T) {
	s := newStub(t)
	s.refreshRejects = true

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "refresh-token")
	if err := os.WriteFile(tokenFile, []byte("agm_rt_refused\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_REFRESH_TOKEN_FILE": tokenFile,
		"AGENT_GM_CLIENT_ID":          "client_01k4z2p8w1",
		"AGENT_GM_ACCESS_TOKEN":       "",
	}, "", "health")

	if got.code != 3 {
		t.Fatalf("a refused refresh token exited %d; spec 11.5 says 3, log in again\nstderr: %s",
			got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Errorf("the refused token was presented %d times; spec 11.5 says it is never "+
			"retried", n)
	}
	raw, _ := os.ReadFile(tokenFile)
	if strings.TrimSpace(string(raw)) != "agm_rt_refused" {
		t.Errorf("a refused exchange rewrote the token file: %q", string(raw))
	}
}

// The happy path rotates: the file holds the replacement afterwards, at 0600.
func TestASuccessfulExchangeStoresTheRotatedToken(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "refresh-token")
	if err := os.WriteFile(tokenFile, []byte("agm_rt_original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_REFRESH_TOKEN_FILE": tokenFile,
		"AGENT_GM_CLIENT_ID":          "client_01k4z2p8w1",
		"AGENT_GM_ACCESS_TOKEN":       "",
	}, "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "agm_rt_rotated" {
		t.Errorf("the rotated token was not stored: %q", string(raw))
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the refresh token file is mode %v, want 0600", info.Mode().Perm())
	}
	// The access token the exchange produced is what the command used.
	for _, r := range s.seen() {
		if r.Path == "/v1/health" && r.Authorization != "Bearer agm_at_rotated" {
			t.Errorf("the command used %q rather than the freshly exchanged access token",
				r.Authorization)
		}
	}
}

// `agm auth login` writes the profile at 0600 inside a 0700 directory, and
// proves the destination writable before it mints a session.
func TestAuthLoginStoresTheProfileAt0600(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	credentials := filepath.Join(dir, "state", "credentials.json")

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_CREDENTIALS_FILE": credentials,
		"AGENT_GM_ACCESS_TOKEN":     "",
	}, "a-fictional-admin-secret-at-least-43-characters-long\n",
		"auth", "login", "--admin", "--secret-stdin", "--server", s.URL)
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}

	info, err := os.Stat(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the credentials file is mode %v, want 0600 (spec 11.5)", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(credentials))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("the credentials directory is mode %v, want 0700 (spec 11.5)",
			dirInfo.Mode().Perm())
	}

	var stored cli.Credentials
	raw, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	name := hostOf(t, s.URL)
	profile, ok := stored.Profiles[name]
	if !ok {
		t.Fatalf("no profile named %q was written: %s", name, raw)
	}
	if profile.Server != s.URL {
		t.Errorf("the profile is bound to %q, not to the server it logged in to", profile.Server)
	}
	if profile.AccessToken != "agm_at_stub" {
		t.Errorf("the access token was not stored")
	}
	// The token is stored, not printed: a transcript is not where a bearer
	// token belongs (spec section 12.1).
	if strings.Contains(got.stdout, "agm_at_stub") || strings.Contains(got.stderr, "agm_at_stub") {
		t.Errorf("the access token was printed:\nstdout: %s\nstderr: %s", got.stdout, got.stderr)
	}
	// And the secret never appeared in the output either.
	if strings.Contains(got.stdout+got.stderr, "a-fictional-admin-secret") {
		t.Error("the admin secret appeared in the output")
	}
}

// A profile is bound to an exact server, and since the owner's 2026-09-06
// decision that binding is enforced BEFORE the request rather than by
// omitting the token: `--server` naming a server this machine holds no
// profile for is refused as `invalid_request` (exit 2), naming
// `agm auth login --server`.
//
// The old behaviour -- send the request anyway, with no Authorization header
// -- is worse in the one case that matters: it reaches an origin the owner
// never logged in to, and reports whatever that origin says.
func TestAProfileIsNotForwardedToAnotherServer(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.json")
	write := cli.Credentials{Version: 1, ActiveProfile: "elsewhere.example.test",
		Profiles: map[string]cli.Profile{
			"elsewhere.example.test": {
				Server: "https://elsewhere.example.test", AccessToken: "agm_at_elsewhere"},
		}}
	raw, err := json.Marshal(write)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_CREDENTIALS_FILE": credentials,
		"AGENT_GM_ACCESS_TOKEN":     "",
		"AGENT_GM_URL":              "",
	}, "", "health", "--server", s.URL)
	if got.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", got.code, got.stderr)
	}
	for _, r := range s.seen() {
		if strings.Contains(r.Authorization, "agm_at_elsewhere") {
			t.Fatalf("the profile's token for another origin was sent to %s", s.URL)
		}
	}
	if n := len(s.seen()); n != 0 {
		t.Fatalf("%d requests reached a server no profile is stored for", n)
	}
}

// The precedence table of section 11.5: AGENT_GM_ACCESS_TOKEN wins over the
// stored profile, and is used as given.
func TestTheEnvironmentAccessTokenWinsOverTheProfile(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials.json")
	raw, err := json.Marshal(cli.Credentials{Version: 1, ActiveProfile: hostOf(t, s.URL),
		Profiles: map[string]cli.Profile{
			hostOf(t, s.URL): {Server: s.URL, AccessToken: "agm_at_profile"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_CREDENTIALS_FILE": credentials,
		"AGENT_GM_ACCESS_TOKEN":     "agm_at_environment",
	}, "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if s.seen()[0].Authorization != "Bearer agm_at_environment" {
		t.Errorf("the profile's token was used over AGENT_GM_ACCESS_TOKEN: %q",
			s.seen()[0].Authorization)
	}
}

// No server anywhere is a local configuration failure: exit 9, decided before
// anything is sent. TestNoServerAnywhereFailsWithoutReachingABuiltInHost in
// profiles_test.go is the other half: there is no compiled-in hostname to
// reach instead.
func TestNoServerConfiguredIsNine(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, map[string]string{
		"AGENT_GM_URL":              "",
		"AGENT_GM_ACCESS_TOKEN":     "",
		"AGENT_GM_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "credentials.json"),
	}, "", "health")
	if got.code != 9 {
		t.Fatalf("no configured server exited %d, want 9\nstderr: %s", got.code, got.stderr)
	}
}

// A session that was not narrowed says so, once, on stderr. A machine that
// only reads does not need `messages:write`, and the moment to narrow is the
// mint: a refresh can never widen one.
func TestAdminLoginSuggestsNarrowingWhenItGrantsAllFourScopes(t *testing.T) {
	s := newStub(t)
	dir := t.TempDir()

	got := runCLI(t, s, map[string]string{
		"AGENT_GM_CREDENTIALS_FILE": filepath.Join(dir, "credentials.json"),
		"AGENT_GM_ACCESS_TOKEN":     "",
	}, "a-fictional-admin-secret-at-least-43-characters-long\n",
		"auth", "login", "--admin", "--secret-stdin", "--server", s.URL)
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "Narrow with --scopes") {
		t.Errorf("a full-scope admin session printed no hint:\nstderr: %s", got.stderr)
	}
	// The hint is a hint: it goes to the transcript, not to stdout, and it
	// does not change the exit code.
	if strings.Contains(got.stdout, "Narrow with --scopes") {
		t.Errorf("the hint was written to stdout, which is the data channel:\n%s", got.stdout)
	}
}
