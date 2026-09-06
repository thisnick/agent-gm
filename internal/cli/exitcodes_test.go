package cli_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/cli"
)

// Section 16 Slice 2 test 21, the exit-code mapping table of section 11.2 in
// full: every section 7.2 code produced against a server maps to the stated
// exit code.
//
// The table is apierr.ExitCodes() itself, walked exhaustively, so a code
// added to section 7.2 without an exit mapping fails here rather than
// silently exiting 10. The CODE comes from the stub's error envelope and the
// EXPECTATION comes from apierr; the CLI has no second copy of the mapping
// for this test to agree with by accident.
//
// The exit-3 and exit-4 halves the spec attaches to a live admin session --
// an expired access token, a narrowed session calling a route outside its
// scopes -- are the server's to produce; what is under test here is that
// `agm` turns invalid_token into 3 and insufficient_scope into 4 whatever
// produced them.
//
// Plant: map CodePhoneNotResponding to apierr.ExitOperationFailed in
// internal/cli/errors.go and TestExitCodeMatrix fails naming it. Planted
// 2026-09-06.
func TestExitCodeMatrix(t *testing.T) {
	for code, want := range apierr.ExitCodes() {
		t.Run(string(code), func(t *testing.T) {
			s := newStub(t)
			s.fail(code)

			got := runCLI(t, s, nil, "", "health", "--json")
			if got.code != want {
				t.Errorf("the server answered %s and `agm health` exited %d; spec 11.2 says %d\n"+
					"stderr: %s", code, got.code, want, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("a failing command wrote to stdout, which belongs to the result:\n%q",
					got.stdout)
			}
			if !strings.Contains(got.stderr, "agm:") {
				t.Errorf("a failing command said nothing on stderr")
			}
		})
	}
}

// The two rows spec section 11.2 calls out by name, stated as literals rather
// than read from the table, so a wrong edit to the table does not also edit
// what this test expects.
func TestPhoneNotRespondingIsSevenAndNotEight(t *testing.T) {
	s := newStub(t)
	s.fail(apierr.CodePhoneNotResponding)

	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq", "--text", "hello")
	if got.code != 7 {
		t.Fatalf("phone_not_responding exited %d; spec 11.2 says 7, and NOT 8: the operation is "+
			"pending and a script that treats it as failed resends the message\nstderr: %s",
			got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "PENDING") {
		t.Errorf("the message does not say the operation is pending:\n%s", got.stderr)
	}
	if !strings.Contains(got.stderr, "Do not resend") {
		t.Errorf("the message does not say not to resend:\n%s", got.stderr)
	}
	if !strings.Contains(got.stderr, "op_01k4z2p8vv") {
		t.Errorf("the message does not print the operation ID:\n%s", got.stderr)
	}
}

func TestIdempotencyConflictIsTwo(t *testing.T) {
	s := newStub(t)
	s.fail(apierr.CodeIdempotencyConflict)

	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq", "--text", "hello",
		"--idempotency-key", "k1")
	if got.code != 2 {
		t.Fatalf("idempotency_conflict exited %d; spec 11.2 says 2, because reusing a key with a "+
			"different body is a caller mistake\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "never retry it unchanged") {
		t.Errorf("the message does not tell the caller not to retry:\n%s", got.stderr)
	}
}

// A usage error is exit 2 and is decided before anything is sent.
func TestUsageErrorsAreTwoAndSendNothing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		says string
	}{
		{"an unknown flag", []string{"health", "--nope"}, "--nope"},
		{"an unknown command", []string{"conversations", "vanish"}, "not an agm command"},
		{"a missing positional", []string{"messages", "show"}, "<msg-id>"},
		{"a bad output format", []string{"health", "--output", "yaml"}, "--output"},
		{"send with neither text nor file", []string{"messages", "send", "conv_1"}, "--text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			got := runCLI(t, s, nil, "", tc.args...)
			if got.code != 2 {
				t.Errorf("exited %d, want 2\nstderr: %s", got.code, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.says) {
				t.Errorf("stderr does not name %q:\n%s", tc.says, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("wrote to stdout: %q", got.stdout)
			}
		})
	}
}

// A transport failure -- the server not there at all -- is exit 7, the
// "retryable network" half of section 11.2's row.
func TestATransportFailureIsSeven(t *testing.T) {
	s := newStub(t)
	url := s.URL
	s.Close()

	got := runCLI(t, s, map[string]string{"AGENT_GM_URL": url}, "", "health")
	if got.code != 7 {
		t.Fatalf("a refused connection exited %d, want 7\nstderr: %s", got.code, got.stderr)
	}
}

// An operation that reaches `failed` while `agm` waits is exit 8 -- and it is
// the only thing that produces exit 8, which is why no row of section 7.2
// maps to it.
func TestAFailedOperationWhileWaitingIsEight(t *testing.T) {
	s := newStub(t)
	s.operationStatus = "failed"

	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait")
	if got.code != 8 {
		t.Fatalf("a failed operation exited %d, want 8\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "op_01k4z2p8vv") {
		t.Errorf("the failure does not name the operation:\n%s", got.stderr)
	}
}

// A wait that runs out of time is exit 7, prints the operation ID, and says
// it is available to `agm operations wait` (spec section 11.2).
func TestAWaitThatTimesOutIsSevenAndSaysWhereToPickItUp(t *testing.T) {
	s := newStub(t)
	s.operationStatus = "pending"
	s.deliveryState = "pending"

	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait", "--timeout", "0s")
	if got.code != 7 {
		t.Fatalf("a wait that timed out exited %d, want 7\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "op_01k4z2p8vv") {
		t.Errorf("the timeout does not print the operation ID:\n%s", got.stderr)
	}
	if !strings.Contains(got.stderr, "agm operations wait") {
		t.Errorf("the timeout does not say the operation is still waitable:\n%s", got.stderr)
	}
}

// Exit 1 is deliberately unassigned (spec section 11.2), so nothing the CLI
// decides may produce it.
func TestNothingExitsOne(t *testing.T) {
	for _, code := range apierr.ExitCodes() {
		if code == 1 {
			t.Error("apierr maps an error code to exit 1, which spec 11.2 leaves unassigned")
		}
	}
	if cli.ExitCodeFor(nil) != 0 {
		t.Error("no error is not exit 0")
	}
}
