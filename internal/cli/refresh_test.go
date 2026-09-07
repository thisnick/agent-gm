package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/cli"
)

// A stored profile refreshes itself (spec section 11.5).
//
// This is a production bug's regression suite. `agm` refreshed exactly one
// credential -- the AGENT_GM_REFRESH_TOKEN_FILE automation pair -- so a
// profile written by `agm auth login` was presented until the server refused
// it, with a working refresh token sitting beside it in the same file.
// `admin.access_token_ttl` defaults to 15 minutes, so an admin session was
// dead a quarter of an hour after it was minted and the next command read as
// a server fault.
//
// The clock is injected throughout: "within 60 seconds of expiry" is not a
// window the wall clock can be asked about, and no test here sleeps.

// fixedClock is a clock that does not move, so a window is a fact rather than
// a race.
func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// profileFile writes a credentials file holding one active profile.
func profileFile(t *testing.T, dir, server string, p cli.Profile) string {
	t.Helper()
	path := filepath.Join(dir, "credentials.json")
	name := hostOf(t, server)
	p.Server = server
	writeCredentials(t, path, cli.Credentials{
		Version:       1,
		ActiveProfile: name,
		Profiles:      map[string]cli.Profile{name: p},
	})
	return path
}

// The proactive trigger: a token past its stated expiry is never presented.
// It is exchanged first, and the rotation is stored.
//
// Plant: restore the `cred.Source == SourceRefreshFile` guard in
// internal/cli/run.go's connect so a profile never refreshes, and this fails
// with exit 3.
func TestAnExpiredProfileIsRefreshedBeforeTheRequestAndNeverPresented(t *testing.T) {
	s := newStub(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	credentials := profileFile(t, t.TempDir(), s.URL, cli.Profile{
		AccessToken:  "agm_at_stale",
		RefreshToken: "agm_rt_stale",
		ExpiresAt:    now.Add(-time.Minute).Format(time.RFC3339),
	})

	got := runCLIAt(t, s, noProfileEnv(credentials), fixedClock(now), "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Fatalf("the profile was refreshed %d times, want 1", n)
	}
	for _, r := range s.seen() {
		if r.Authorization == "Bearer agm_at_stale" {
			t.Errorf("%s %s presented a token this process already knew was expired",
				r.Method, r.Path)
		}
		if r.Path == "/v1/health" && r.Authorization != "Bearer agm_at_rotated" {
			t.Errorf("the command presented %q, not the freshly exchanged token",
				r.Authorization)
		}
	}

	stored := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]
	if stored.AccessToken != "agm_at_rotated" {
		t.Errorf("the rotated access token was not stored: %q", stored.AccessToken)
	}
	if stored.RefreshToken != "agm_rt_rotated" {
		t.Errorf("the rotated refresh token was not stored: %q", stored.RefreshToken)
	}
	if stored.ExpiresAt != "2099-01-01T00:00:00Z" {
		t.Errorf("the new expiry was not stored: %q", stored.ExpiresAt)
	}
	info, err := os.Stat(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the credentials file is mode %v after a rotation, want 0600",
			info.Mode().Perm())
	}
}

// The window, in both directions: a token 30 seconds from expiry is
// refreshed; one ten minutes from expiry is used as it is.
//
// Plant: compare with `now.After(expiry)` rather than
// `now.Add(refreshSkew).Before(expiry)` -- refresh only once PAST expiry --
// and the first half of this fails, because a token that dies mid-flight is
// exactly what the window exists to prevent.
func TestATokenInsideTheRefreshWindowIsExchangedAndOneOutsideIsNot(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	inside := newStub(t)
	credentials := profileFile(t, t.TempDir(), inside.URL, cli.Profile{
		AccessToken:  "agm_at_stale",
		RefreshToken: "agm_rt_stale",
		ExpiresAt:    now.Add(30 * time.Second).Format(time.RFC3339),
	})
	if got := runCLIAt(t, inside, noProfileEnv(credentials), fixedClock(now),
		"", "health"); got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if n := inside.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Errorf("a token 30s from expiry was refreshed %d times, want 1 "+
			"(the window is 60s)", n)
	}

	outside := newStub(t)
	credentials = profileFile(t, t.TempDir(), outside.URL, cli.Profile{
		AccessToken:  "agm_at_live",
		RefreshToken: "agm_rt_live",
		ExpiresAt:    now.Add(10 * time.Minute).Format(time.RFC3339),
	})
	if got := runCLIAt(t, outside, noProfileEnv(credentials), fixedClock(now),
		"", "health"); got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if n := outside.countOf("POST", "/v1/auth/refresh"); n != 0 {
		t.Errorf("a token ten minutes from expiry was refreshed %d times; refreshing on "+
			"every invocation spends a rotation each time", n)
	}
	if seen := outside.seen(); len(seen) != 1 || seen[0].Authorization != "Bearer agm_at_live" {
		t.Errorf("the live token was not the one presented: %+v", outside.seen())
	}
}

// The on-refusal trigger, which is also the path a profile from an older
// build takes: it records no expiry, the server refuses the token, and the
// CLI refreshes ONCE and sends the same request again.
//
// Plant: make the retry a loop rather than a single attempt and
// TestARefusedProfileRefreshIsThreeAndIsNotRetried fails on the refresh
// count; drop the retry entirely and this one fails with exit 3.
func TestAnInvalidTokenRefreshesOnceAndRetriesTheSameRequest(t *testing.T) {
	s := newStub(t)
	s.reject("agm_at_revoked")
	credentials := profileFile(t, t.TempDir(), s.URL, cli.Profile{
		AccessToken:  "agm_at_revoked",
		RefreshToken: "agm_rt_live",
		// No expires_at at all: this is a profile an older build wrote.
	})

	got := runCLIAt(t, s, noProfileEnv(credentials), nil, "", "health")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Fatalf("the profile was refreshed %d times, want exactly 1", n)
	}
	if n := s.countOf("GET", "/v1/health"); n != 2 {
		t.Errorf("the request was sent %d times; want 2 -- the refusal and the retry", n)
	}
	last := s.seen()[len(s.seen())-1]
	if last.Authorization != "Bearer agm_at_rotated" {
		t.Errorf("the retry presented %q", last.Authorization)
	}
	if stored := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]; //nolint:staticcheck
	stored.AccessToken != "agm_at_rotated" {
		t.Errorf("the rotation was not written back: %q", stored.AccessToken)
	}
}

// One attempt, and only one. A server that refuses the rotated token too --
// the authorization itself is gone, not merely the token -- must produce one
// refresh and one retry, and then stop.
//
// Plant: retry twice rather than once in internal/cli/client.go's Do and this
// fails on both counts.
func TestAServerThatRefusesEvenTheRotatedTokenRefreshesOnlyOnce(t *testing.T) {
	s := newStub(t)
	s.reject("agm_at_revoked")
	s.reject("agm_at_rotated")
	credentials := profileFile(t, t.TempDir(), s.URL, cli.Profile{
		AccessToken:  "agm_at_revoked",
		RefreshToken: "agm_rt_live",
	})

	got := runCLIAt(t, s, noProfileEnv(credentials), nil, "", "health")
	if got.code != 3 {
		t.Fatalf("exited %d, want 3\nstderr: %s", got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Errorf("the profile was refreshed %d times, want exactly 1", n)
	}
	if n := s.countOf("GET", "/v1/health"); n != 2 {
		t.Errorf("the request was sent %d times, want 2 -- the refusal and one retry", n)
	}
}

// A profile with no refresh token keeps the old behaviour: it fails saying
// what to type, and it does not loop.
func TestAProfileWithNoRefreshTokenSaysLogInAgain(t *testing.T) {
	s := newStub(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	credentials := profileFile(t, t.TempDir(), s.URL, cli.Profile{
		AccessToken: "agm_at_stale",
		ExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
	})

	got := runCLIAt(t, s, noProfileEnv(credentials), fixedClock(now), "", "health")
	if got.code != 3 {
		t.Fatalf("exited %d, want 3\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "agm auth login") {
		t.Errorf("the message does not say what to do:\n%s", got.stderr)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent with a token already known to be expired", n)
	}
}

// A refresh token the server refuses is exit 3, once. Rotation makes it
// single-use, so a second attempt cannot succeed and only obscures what
// happened.
func TestARefusedProfileRefreshIsThreeAndIsNotRetried(t *testing.T) {
	s := newStub(t)
	s.refreshRejects = true
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	credentials := profileFile(t, t.TempDir(), s.URL, cli.Profile{
		AccessToken:  "agm_at_stale",
		RefreshToken: "agm_rt_revoked",
		ExpiresAt:    now.Add(-time.Hour).Format(time.RFC3339),
	})

	got := runCLIAt(t, s, noProfileEnv(credentials), fixedClock(now), "", "health")
	if got.code != 3 {
		t.Fatalf("exited %d, want 3\nstderr: %s", got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 1 {
		t.Errorf("the refused refresh token was presented %d times; it is never retried", n)
	}
	if !strings.Contains(got.stderr, "agm auth login") {
		t.Errorf("the message does not say what to do:\n%s", got.stderr)
	}
	// Nothing was overwritten: the profile is still what it was.
	if stored := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]; //nolint:staticcheck
	stored.RefreshToken != "agm_rt_revoked" {
		t.Errorf("a refused refresh rewrote the profile: %+v", stored)
	}
}

// Write-back safety, unchanged by any of this: a credentials destination
// that cannot be written is exit 9 with the refresh token still GOOD, and the
// exchange never happened.
//
// The fault injected is a `credentials.lock` that is a directory, so the
// advisory lock cannot be opened. A 0500 directory is NOT the fault to use
// here: Store.Begin asserts 0700 on the credentials directory (spec section
// 11.5 states that mode), and the owner of a directory can always chmod it
// back, so the store would repair the very thing the test was trying to
// break.
//
// Plant: move the store.Begin() call in refreshProfile to after the POST and
// this fails on the refresh count -- the token would have been spent with
// nowhere to put the replacement, which rotation makes unrecoverable.
func TestAnUnwritableDestinationNeverSpendsAProfileRefreshToken(t *testing.T) {
	s := newStub(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentials := profileFile(t, dir, s.URL, cli.Profile{
		AccessToken:  "agm_at_stale",
		RefreshToken: "agm_rt_good",
		ExpiresAt:    now.Add(-time.Hour).Format(time.RFC3339),
	})
	if err := os.Mkdir(filepath.Join(dir, cli.CredentialsLockName), 0o700); err != nil {
		t.Fatal(err)
	}

	got := runCLIAt(t, s, noProfileEnv(credentials), fixedClock(now), "", "health")
	if got.code != 9 {
		t.Fatalf("exited %d, want 9 while the token is still good\nstderr: %s",
			got.code, got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 0 {
		t.Errorf("the refresh token was spent %d times before the destination was proved "+
			"writable; rotation makes it single-use", n)
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("%d requests were sent at all", n)
	}
	if stored := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]; //nolint:staticcheck
	stored.RefreshToken != "agm_rt_good" {
		t.Errorf("the stored refresh token changed: %+v", stored)
	}
}

// `--profile` selecting a profile for one invocation must not make it the
// active one, and a rotation must not do it by the back door either.
func TestARotationDoesNotChangeWhichProfileIsActive(t *testing.T) {
	active, other := newStub(t), newStub(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	activeHost, otherHost := hostOf(t, active.URL), hostOf(t, other.URL)
	writeCredentials(t, credentials, cli.Credentials{
		Version:       1,
		ActiveProfile: activeHost,
		Profiles: map[string]cli.Profile{
			activeHost: {Server: active.URL, AccessToken: "agm_at_active"},
			otherHost: {Server: other.URL, AccessToken: "agm_at_stale",
				RefreshToken: "agm_rt_stale",
				ExpiresAt:    now.Add(-time.Hour).Format(time.RFC3339)},
		},
	})

	got := runCLIAt(t, other, noProfileEnv(credentials), fixedClock(now), "",
		"health", "--profile", otherHost)
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	after := readCredentials(t, credentials)
	if after.ActiveProfile != activeHost {
		t.Errorf("a refresh on the --profile profile made it active: %q", after.ActiveProfile)
	}
	if after.Profiles[otherHost].AccessToken != "agm_at_rotated" {
		t.Errorf("the rotation was not stored: %+v", after.Profiles[otherHost])
	}
}

// The credentials lock has a deadline (review finding C-2).
//
// The lock is held across the network exchange on purpose -- the rotation and
// the write-back are one operation, and a rotated refresh token is single-use,
// so a second invocation must not interleave. But the wait for it used to be
// an unbounded `LOCK_EX`, which turns "another agm is busy" into a hang with
// no output: the operator learns nothing and has nothing to act on. It also
// made a reviewer's plant fail by ten-minute deadlock rather than by
// assertion, which is the same shape as the concurrency-budget test one layer
// up.
//
// Bounded, it is exit 9 naming the lock, with nothing spent.
func TestAHeldCredentialsLockFailsWithinItsDeadlineRatherThanHanging(t *testing.T) {
	if !cli.LockSupported {
		// Windows has no advisory lock, so there is nothing to wait for and
		// nothing to bound. Said out loud rather than passed silently: this
		// is also the platform where section 11.5's rule has no
		// implementation at all, which is a Slice 4 problem.
		t.Skip("no advisory file lock on this platform")
	}

	s := newStub(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	credentials := profileFile(t, dir, s.URL, cli.Profile{
		AccessToken:  "agm_at_stale",
		RefreshToken: "agm_rt_good",
		ExpiresAt:    now.Add(-time.Hour).Format(time.RFC3339),
	})

	// Another process holds the lock and never lets go.
	held, err := os.OpenFile(filepath.Join(dir, cli.CredentialsLockName),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := cli.LockForTest(held); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}

	done := make(chan struct{})
	var got result
	go func() {
		defer close(done)
		got = runCLIAt(t, s, noProfileEnv(credentials), fixedClock(now), "", "health")
	}()

	// Generously longer than LockWait, and far shorter than a package
	// timeout: the assertion is that this RETURNS, not how fast.
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("`agm` was still waiting for the credentials lock after 20s with a 200ms " +
			"deadline configured; an unbounded wait is a hang with no output")
	}

	if got.code != 9 {
		t.Errorf("a held lock exited %d, want 9\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, cli.CredentialsLockName) {
		t.Errorf("the failure does not name the lock, so an operator cannot act on it:\n%s",
			got.stderr)
	}
	if n := s.countOf("POST", "/v1/auth/refresh"); n != 0 {
		t.Errorf("the refresh token was spent %d times while the lock was unavailable", n)
	}
	if stored := readCredentials(t, credentials).Profiles[hostOf(t, s.URL)]; stored.RefreshToken != "agm_rt_good" {
		t.Errorf("the stored refresh token changed: %+v", stored)
	}
}
