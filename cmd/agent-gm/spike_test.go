package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// The spike is exercised as a real process: these tests build the binary once
// and run it, so the exit codes of spec section 11.2 are produced by a real
// invocation rather than asserted about a function.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agent-gm-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "agent-gm")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			binPath = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the spike: %v\n%s", buildErr, binPath)
	}
	return binPath
}

type result struct {
	code   int
	stdout string
	stderr string
}

// run invokes the spike with a fresh data directory and the fake backend,
// which needs BOTH AGENT_GM_BACKEND=fake and AGENT_GM_ALLOW_FAKE=1.
func run(t *testing.T, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(os.Environ(),
		"AGENT_GM_DATA_KEY="+strings.Repeat("44", 32),
		"AGENT_GM_LOG_LEVEL=error",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running the spike: %v", err)
		}
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func fakeEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"AGENT_GM_DATA_DIR":   t.TempDir(),
		"AGENT_GM_BACKEND":    "fake",
		"AGENT_GM_ALLOW_FAKE": "1",
		"XDG_STATE_HOME":      t.TempDir(),
	}
}

// A fake backend without AGENT_GM_ALLOW_FAKE=1 refuses to start, so a
// production deployment cannot be talked into serving an empty in-memory
// phone (spec sections 13.1, 15.1).
func TestFakeBackendRefusesWithoutTheSecondFlag(t *testing.T) {
	env := fakeEnv(t)
	delete(env, "AGENT_GM_ALLOW_FAKE")
	got := run(t, env, "", "spike", "diag")
	if got.code != 9 {
		t.Errorf("exit = %d, want 9 (local configuration)", got.code)
	}
	if !strings.Contains(got.stderr, "AGENT_GM_ALLOW_FAKE") {
		t.Errorf("stderr does not name the missing flag: %s", got.stderr)
	}
	// With both, it starts.
	if got := run(t, fakeEnv(t), "", "spike", "diag"); got.code != 0 {
		t.Errorf("with both flags exit = %d: %s", got.code, got.stderr)
	}
}

// A missing data key is a local configuration failure, exit 9, and says how
// to make one.
func TestMissingDataKeyIsExitNine(t *testing.T) {
	cmd := exec.Command(binary(t), "spike", "diag")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"AGENT_GM_DATA_DIR=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	code := 0
	var exitErr *exec.ExitError
	if err != nil && asExitError(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	if code != 9 {
		t.Errorf("exit = %d, want 9\n%s", code, out)
	}
	if !strings.Contains(string(out), "AGENT_GM_DATA_KEY") {
		t.Errorf("the message does not name the variable: %s", out)
	}
}

// Slice 1 acceptance test 13's non-live half: diag prints the compiled
// ConfigVersion and the pin, with or without an account. The LIVE values are
// the point of the live gate; this asserts the shape.
func TestDiagPrintsTheCompiledConfigVersion(t *testing.T) {
	got := run(t, fakeEnv(t), "", "spike", "diag")
	if got.code != 0 {
		t.Fatalf("exit = %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		"config_version_compiled:  " + gm.CompiledConfigVersion().String(),
		"upstream_commit:          " + gm.PinnedUpstreamCommit,
		"none connected",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("diag does not print %q:\n%s", want, got.stdout)
		}
	}
}

// The no-Chrome path: `spike pair` explains what it needs and exits 9. It
// does not report a missing binary, and the exit code is not 2 -- nothing
// about the command was malformed.
//
// This is the non-live half of Slice 1 live-gate test 8. The live gate hides
// Chrome from PATH on a real machine; here AGENT_GM_CHROME names a path that
// is not an executable, which reaches the same branch.
func TestPairWithNoChromePrintsTheTwoOptionsAndExitsNine(t *testing.T) {
	env := fakeEnv(t)
	env["AGENT_GM_CHROME"] = filepath.Join(t.TempDir(), "no-chrome-here")
	got := run(t, env, "", "spike", "pair")
	if got.code != 9 {
		t.Fatalf("exit = %d, want 9\nstdout: %s\nstderr: %s", got.code, got.stdout, got.stderr)
	}
	out := got.stderr
	server := strings.Index(out, "agm pair --server")
	paste := strings.Index(out, "agm pair --paste")
	if server < 0 || paste < 0 {
		t.Fatalf("the two ways forward are missing:\n%s", out)
	}
	if server > paste {
		t.Error("--server must come before --paste: best first")
	}
	if strings.Contains(out, "no such file or directory") {
		t.Errorf("it reported a missing binary:\n%s", out)
	}
}

// `spike pair --paste` is the documented fallback for a machine with no
// Chrome. End to end against the fake: it pairs, persists the session, and
// the account is connected.
func TestPairPasteListAndSendAgainstTheFake(t *testing.T) {
	env := fakeEnv(t)

	// Nonfunctional placeholders. A real Google cookie never enters this
	// repository.
	cookies := map[string]string{}
	for _, n := range append(append([]string{}, gm.GaiaRequiredCookies...), gm.GaiaOptionalCookies...) {
		cookies[n] = "FIXTURE-" + n
	}
	blob, err := json.Marshal(cookies)
	if err != nil {
		t.Fatal(err)
	}

	paired := run(t, env, string(blob), "spike", "pair", "--paste")
	if paired.code != 0 {
		t.Fatalf("pair exit = %d\nstdout: %s\nstderr: %s", paired.code, paired.stdout, paired.stderr)
	}
	if !strings.Contains(paired.stdout, "Paired.") {
		t.Errorf("pair did not report success:\n%s", paired.stdout)
	}
	if !strings.Contains(paired.stdout, "acct_") {
		t.Errorf("pair did not print an acct_ ID:\n%s", paired.stdout)
	}
	// The emoji is printed for the owner to tap.
	if !strings.Contains(paired.stdout, "Tap this one:") {
		t.Errorf("pair did not show the emoji prompt:\n%s", paired.stdout)
	}
	// Both phone settings are stated once, after pairing.
	for _, want := range []string{"default SMS app", "Group messaging"} {
		if !strings.Contains(paired.stdout, want) {
			t.Errorf("the after-pairing notes do not mention %q:\n%s", want, paired.stdout)
		}
	}
	// The session file is on disk at mode 0600 in a 0700 directory.
	sessions := filepath.Join(env["AGENT_GM_DATA_DIR"], "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("sessions/ holds %d files, want 1", len(entries))
	}
	fi, err := os.Stat(filepath.Join(sessions, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the session file is mode %o, want 600", fi.Mode().Perm())
	}
	// Nothing else wrote the cookies anywhere: no file under the data
	// directory or the state directory contains one.
	assertNoCookieOnDisk(t, env["AGENT_GM_DATA_DIR"], "FIXTURE-SID")
	assertNoCookieOnDisk(t, env["XDG_STATE_HOME"], "FIXTURE-SID")

	// list finds the fixture conversation, with a conv_ ID.
	listed := run(t, env, "", "spike", "list")
	if listed.code != 0 {
		t.Fatalf("list exit = %d: %s", listed.code, listed.stderr)
	}
	if !strings.Contains(listed.stdout, "Fixture direct") {
		t.Fatalf("list did not show the conversation:\n%s", listed.stdout)
	}
	var convID string
	for _, line := range strings.Split(listed.stdout, "\n") {
		if strings.HasPrefix(line, "conv_") {
			convID = strings.Fields(line)[0]
			break
		}
	}
	if convID == "" {
		t.Fatalf("list printed no conv_ ID:\n%s", listed.stdout)
	}

	// send takes a conv_ ID and reports the status the phone gave.
	sent := run(t, env, "", "spike", "send", convID, "agent-gm slice 1 test")
	if sent.code != 0 {
		t.Fatalf("send exit = %d\nstdout: %s\nstderr: %s", sent.code, sent.stdout, sent.stderr)
	}
	if !strings.Contains(sent.stdout, "status: SUCCESS") {
		t.Errorf("send did not report SUCCESS:\n%s", sent.stdout)
	}
	if !strings.Contains(sent.stdout, "tmp_id: ") {
		t.Errorf("send did not print the tmp ID:\n%s", sent.stdout)
	}

	// A raw Google ID, or any ID with the wrong prefix, is a usage error
	// naming the expected prefix -- never not_found (spec section 4.1).
	bad := run(t, env, "", "spike", "send", "goog-conv-1", "hello")
	if bad.code != 2 {
		t.Errorf("a wrong-prefix ID gave exit %d, want 2\n%s", bad.code, bad.stderr)
	}
	if !strings.Contains(bad.stderr, "conv_") {
		t.Errorf("the error does not name the expected prefix: %s", bad.stderr)
	}

	// A well-formed conv_ ID that does not exist is exit 5.
	absent := run(t, env, "", "spike", "send",
		"conv_00000000-0000-5000-8000-000000000000", "hello")
	if absent.code != 5 {
		t.Errorf("an absent conversation gave exit %d, want 5\n%s", absent.code, absent.stderr)
	}

	// diag now reports the account, with both ConfigVersions.
	diag := run(t, env, "", "spike", "diag")
	if diag.code != 0 {
		t.Fatalf("diag exit = %d: %s", diag.code, diag.stderr)
	}
	for _, want := range []string{"config_version_live:", "config_version_stale:", "is_default_sms_app:"} {
		if !strings.Contains(diag.stdout, want) {
			t.Errorf("diag does not print %q:\n%s", want, diag.stdout)
		}
	}
}

// assertNoCookieOnDisk walks a directory and fails if any file contains the
// sentinel. Nothing writes the cookies to a file of Agent GM's own: the
// session envelope is encrypted, and nothing else touches them.
func assertNoCookieOnDisk(t *testing.T, root, sentinel string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not a leak
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		if strings.Contains(string(data), sentinel) {
			t.Errorf("%s contains a cookie value", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUsageErrors(t *testing.T) {
	if got := run(t, fakeEnv(t), "", "nonsense"); got.code != 2 {
		t.Errorf("an unknown command gave exit %d, want 2", got.code)
	}
	if got := run(t, fakeEnv(t), "", "spike", "nonsense"); got.code != 2 {
		t.Errorf("an unknown subcommand gave exit %d, want 2", got.code)
	}
	if got := run(t, fakeEnv(t), "", "spike"); got.code != 2 {
		t.Errorf("spike with no subcommand gave exit %d, want 2", got.code)
	}
	if got := run(t, fakeEnv(t), "", "version"); got.code != 0 {
		t.Errorf("version gave exit %d, want 0", got.code)
	} else if !strings.Contains(got.stdout, gm.PinnedUpstreamCommit) {
		t.Errorf("version does not name the pin: %s", got.stdout)
	}
	// --refresh-cookies names an account.
	env := fakeEnv(t)
	if got := run(t, env, "{}", "spike", "pair", "--paste", "--refresh-cookies"); got.code != 2 {
		t.Errorf("--refresh-cookies with no --account gave exit %d, want 2", got.code)
	}
	// A duration with a unit the CLI does not take is exit 2 naming the flag.
	if got := run(t, env, "", "spike", "watch", "--for", "5x"); got.code != 2 {
		t.Errorf("a bad duration gave exit %d, want 2", got.code)
	}
}

// A paste missing OSID names the cookie and its domain, and never echoes a
// value.
func TestPastePathNamesMissingCookies(t *testing.T) {
	cookies := map[string]string{}
	for _, n := range gm.GaiaRequiredCookies {
		if n == "OSID" {
			continue
		}
		cookies[n] = "FIXTURE-VALUE"
	}
	blob, err := json.Marshal(cookies)
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, fakeEnv(t), string(blob), "spike", "pair", "--paste")
	if got.code != 2 {
		t.Errorf("exit = %d, want 2", got.code)
	}
	if !strings.Contains(got.stderr, "OSID") || !strings.Contains(got.stderr, "messages.google.com") {
		t.Errorf("the error does not name OSID and its domain: %s", got.stderr)
	}
	if strings.Contains(got.stderr, "FIXTURE-VALUE") {
		t.Errorf("the error echoed a cookie value: %s", got.stderr)
	}
}
