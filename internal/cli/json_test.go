package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/cli"
)

// invocationFor gives every command of the inventory a real invocation to be
// run with. It is keyed by Command.Name, and TestEveryCommandHasAnInvocation
// fails if a command is added without one -- which is what keeps section 16
// Slice 2 test 26 a table over cli.Commands rather than a sample.
func invocationFor(t *testing.T, name string) (args []string, stdin string) {
	t.Helper()
	paste := pasteFixture(t)
	dest := filepath.Join(t.TempDir(), "IMG_0421.jpg")

	table := map[string][]string{
		"pair":                   {"pair", "--paste-file", paste},
		"pair --refresh-cookies": {"pair", "--refresh-cookies", "--account", "acct_01k4z0aa", "--paste-file", paste},

		"accounts list":     {"accounts", "list"},
		"accounts show":     {"accounts", "show", "acct_01k4z0aa"},
		"accounts label":    {"accounts", "label", "acct_01k4z0aa", "personal"},
		"accounts sign-out": {"accounts", "sign-out", "acct_01k4z0aa", "--yes"},
		"accounts remove":   {"accounts", "remove", "acct_01k4z0aa", "--yes"},
		"session":           {"session", "--account", "acct_01k4z0aa"},
		"reconnect":         {"reconnect", "--account", "acct_01k4z0aa"},
		"health":            {"health"},

		"conversations list":  {"conversations", "list", "--limit", "2"},
		"conversations show":  {"conversations", "show", "conv_01k4z2p8vq"},
		"conversations start": {"conversations", "start", "+12025550123", "--account", "acct_01k4z0aa"},
		"conversations archive|unarchive|pin|unpin|mark-unread": {"conversations", "archive", "conv_01k4z2p8vq"},
		"conversations mark-read":                               {"conversations", "mark-read", "conv_01k4z2p8vq", "--message", "msg_01k4z2p8vt"},
		"conversations delete":                                  {"conversations", "delete", "conv_01k4z2p8vq", "--yes"},
		"conversations typing":                                  {"conversations", "typing", "conv_01k4z2p8vq"},

		"messages list":            {"messages", "list", "conv_01k4z2p8vq"},
		"messages show":            {"messages", "show", "msg_01k4z2p8vt"},
		"messages context":         {"messages", "context", "msg_01k4z2p8vt", "--before", "5", "--after", "5"},
		"messages search":          {"messages", "search", "photo", "--mode", "words"},
		"messages send":            {"messages", "send", "conv_01k4z2p8vq", "--text", "on my way"},
		"messages delete":          {"messages", "delete", "msg_01k4z2p8vt", "--yes"},
		"messages add-reaction":    {"messages", "add-reaction", "msg_01k4z2p8vt", "\U0001F44D"},
		"messages remove-reaction": {"messages", "remove-reaction", "msg_01k4z2p8vt", "\U0001F44D"},

		"attachments list":     {"attachments", "list", "msg_01k4z2p8vt"},
		"attachments show":     {"attachments", "show", "att_9f3c"},
		"attachments download": {"attachments", "download", "att_9f3c", "--out", dest},
		"contacts list":        {"contacts", "list", "--top"},

		"operations list": {"operations", "list", "--terminal"},
		"operations show": {"operations", "show", "op_01k4z2p8vv"},
		"operations wait": {"operations", "wait", "op_01k4z2p8vv"},

		"auth login":  {"auth", "login", "--admin", "--secret-stdin"},
		"auth logout": {"auth", "logout", "--yes"},
		"auth whoami": {"auth", "whoami"},

		"admin settings list": {"admin", "settings", "list"},
		"admin settings get":  {"admin", "settings", "get", "operations.wait_timeout"},
		"admin settings set":  {"admin", "settings", "set", "operations.wait_timeout=60s"},
		"admin audit list":    {"admin", "audit", "list", "--kind-prefix", "auth."},
		"admin backfill":      {"admin", "backfill", "--account", "acct_01k4z0aa"},
		"admin backup":        {"admin", "backup"},
		"admin diagnostics":   {"admin", "diagnostics", "--account", "acct_01k4z0aa"},

		"admin enrollment-codes create": {"admin", "enrollment-codes", "create", "claude.ai"},
		"admin enrollment-codes list":   {"admin", "enrollment-codes", "list"},
		"admin enrollment-codes show":   {"admin", "enrollment-codes", "show", "enroll_01k4z9"},
		"admin enrollment-codes revoke": {"admin", "enrollment-codes", "revoke", "enroll_01k4z9", "--yes"},

		"admin authorization-requests list":    {"admin", "authorization-requests", "list", "--status", "pending"},
		"admin authorization-requests show":    {"admin", "authorization-requests", "show", "authreq_01k4za"},
		"admin authorization-requests approve": {"admin", "authorization-requests", "approve", "authreq_01k4za"},
		"admin authorization-requests deny":    {"admin", "authorization-requests", "deny", "authreq_01k4za", "--reason", "not mine"},

		"admin authorizations list":   {"admin", "authorizations", "list"},
		"admin authorizations show":   {"admin", "authorizations", "show", "auth_01k4zb"},
		"admin authorizations revoke": {"admin", "authorizations", "revoke", "auth_01k4zb", "--yes"},

		"admin clients list":   {"admin", "clients", "list"},
		"admin clients show":   {"admin", "clients", "show", "client_01k4zc"},
		"admin clients revoke": {"admin", "clients", "revoke", "client_01k4zc", "--yes"},
	}

	args, ok := table[name]
	if !ok {
		t.Fatalf("no invocation is declared for the command %q; add one to invocationFor "+
			"so section 16 Slice 2 test 26 covers it", name)
	}
	if name == "auth login" {
		stdin = "a-fictional-admin-secret-at-least-43-characters-long\n"
	}
	return args, stdin
}

func TestEveryCommandHasAnInvocation(t *testing.T) {
	for _, c := range cli.Commands {
		invocationFor(t, c.Name)
	}
}

// Section 16 Slice 2 test 26:
//
//	"--json puts exactly one JSON value on stdout and everything else on
//	 stderr, for every command, asserted by parsing stdout as JSON."
//
// It is a table over cli.Commands -- every command, not a sample -- with
// stdout and stderr captured separately. `exactly one` is asserted by
// decoding one value and then requiring the decoder to be at EOF, so a
// second value, a stray line or a log line all fail.
//
// Plant: write any diagnostic with fmt.Fprintln(o.stdout, ...) in
// internal/cli/output.go's Warnf and this fails for every command that
// carries a warning; write one in Infof and TestJSONPutsExactlyOneValueOnStdout
// fails for `agm attachments download`. Planted 2026-09-06.
func TestJSONPutsExactlyOneValueOnStdout(t *testing.T) {
	for _, c := range cli.Commands {
		t.Run(c.Name, func(t *testing.T) {
			args, stdin := invocationFor(t, c.Name)
			s := newStub(t)
			// Something to say on stderr, on every command: a warning in the
			// envelope is the server's channel for it (spec section 7.1),
			// and --verbose is the CLI's.
			got := runCLI(t, s, nil, stdin, append(args, "--json", "--verbose")...)

			if got.code != 0 {
				t.Fatalf("`agm %s --json` exited %d\nstdout: %s\nstderr: %s",
					c.Name, got.code, got.stdout, got.stderr)
			}
			assertExactlyOneJSONValue(t, c.Name, got.stdout)
			if got.stderr == "" {
				t.Errorf("--verbose said nothing on stderr, so this run does not prove the "+
					"two streams are separated for `agm %s`", c.Name)
			}
		})
	}
}

// The same promise for --output json, which --json is a synonym for.
func TestOutputJSONIsTheSameAsJSON(t *testing.T) {
	s := newStub(t)
	withFlag := runCLI(t, s, nil, "", "health", "--json")
	withOutput := runCLI(t, s, nil, "", "health", "--output", "json")
	if withFlag.stdout != withOutput.stdout {
		t.Errorf("--json and --output json disagree:\n%q\n%q", withFlag.stdout, withOutput.stdout)
	}
}

// The JSON value is the server envelope plus the CLI metadata of section
// 11.3: the effective profile and the effective server.
func TestTheJSONValueCarriesTheEnvelopeAndTheCLIMetadata(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "health", "--json")

	var value map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &value); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.stdout)
	}
	for _, key := range []string{"data", "next_cursor", "warnings", "request_id", "profile", "server"} {
		if _, ok := value[key]; !ok {
			t.Errorf("the emitted value has no %q; spec 11.3 says the envelope plus the "+
				"effective profile and server", key)
		}
	}
	if value["server"] != s.URL {
		t.Errorf("server is %v, want %s", value["server"], s.URL)
	}
}

// Human output prints the operation ID first for every mutation (spec
// section 11.3), so a transcript always contains the thing to pass to
// `agm operations wait`.
func TestHumanOutputPrintsTheOperationIDFirst(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq", "--text", "on my way")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	first := strings.SplitN(strings.TrimSpace(got.stdout), "\n", 2)[0]
	if strings.TrimSpace(first) != "op_01k4z2p8vv" {
		t.Errorf("the first line of human output is %q, not the operation ID", first)
	}
}

// --quiet silences progress but not the result.
func TestQuietKeepsTheResult(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "health", "--json", "--quiet")
	assertExactlyOneJSONValue(t, "health --quiet", got.stdout)
}

// --output jsonl streams one object per line for a listing.
func TestJSONLStreamsOneObjectPerLine(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "conversations", "list", "--output", "jsonl")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	for i, line := range strings.Split(strings.TrimSpace(got.stdout), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d of jsonl output is not one JSON object: %v\n%s", i+1, err, line)
		}
	}
}

func assertExactlyOneJSONValue(t *testing.T, name, stdout string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(stdout)))
	var value any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("stdout of `agm %s --json` is not one JSON value: %v\nstdout: %q",
			name, err, stdout)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout of `agm %s --json` carries more than one value (%v); spec 11.3 says "+
			"exactly one, and everything else on stderr\nstdout: %q", name, extra, stdout)
	}
}
