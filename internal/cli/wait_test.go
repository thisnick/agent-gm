package cli_test

import (
	"strings"
	"testing"
)

// The duration grammar of spec section 11.1: an integer plus a unit of s, m,
// h or d. There is no 1.5h, no 90 and no 1h30m, and anything else is exit 2
// NAMING THE FLAG.
func TestDurationGrammar(t *testing.T) {
	good := []string{"30s", "15m", "2h", "7d", "0s"}
	for _, v := range good {
		t.Run("accepts "+v, func(t *testing.T) {
			s := newStub(t)
			got := runCLI(t, s, nil, "", "health", "--timeout", v)
			if got.code != 0 {
				t.Errorf("--timeout %s exited %d\nstderr: %s", v, got.code, got.stderr)
			}
		})
	}

	bad := []string{"1.5h", "90", "1h30m", "2w", "", "h", "-5m", "abc"}
	for _, v := range bad {
		t.Run("refuses "+v, func(t *testing.T) {
			s := newStub(t)
			got := runCLI(t, s, nil, "", "health", "--timeout", v)
			if got.code != 2 {
				t.Errorf("--timeout %q exited %d, want 2\nstderr: %s", v, got.code, got.stderr)
			}
			if !strings.Contains(got.stderr, "--timeout") {
				t.Errorf("the error does not name the flag:\n%s", got.stderr)
			}
			if len(s.seen()) != 0 {
				t.Errorf("a bad duration still sent %d requests", len(s.seen()))
			}
		})
	}
}

// `--wait-for` takes one of four words, and anything else is exit 2 naming
// the flag.
func TestWaitForTakesTheFourWords(t *testing.T) {
	for _, v := range []string{"sent", "delivered", "read", "terminal"} {
		t.Run("accepts "+v, func(t *testing.T) {
			s := newStub(t)
			s.deliveryState = "read"
			got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
				"--text", "hello", "--wait-for", v)
			if got.code != 0 {
				t.Errorf("--wait-for %s exited %d\nstderr: %s", v, got.code, got.stderr)
			}
		})
	}
	s := newStub(t)
	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait-for", "acknowledged")
	if got.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "--wait-for") {
		t.Errorf("the error does not name the flag:\n%s", got.stderr)
	}
}

// Spec sections 11.3 and 5.5:
//
//	"agm warns on stderr when --wait-for delivered or read is used on an
//	 sms_mms conversation."
//
// SMS usually stops at `sent`, so an agent waiting for `delivered` can wait
// until its timeout for a state that will never arrive. The warning is on
// stderr and never on stdout, so it does not disturb --json.
//
// Plant: change internal/cli/output.go's Warnf to write to o.stdout and
// TestSMSWarningIsOnStderrAndNotStdout fails. Planted 2026-09-06.
func TestSMSWarningIsOnStderrAndNotStdout(t *testing.T) {
	for _, target := range []string{"delivered", "read"} {
		t.Run(target, func(t *testing.T) {
			s := newStub(t)
			s.conversationType = "sms_mms"
			s.deliveryState = "read"

			got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
				"--text", "on my way", "--wait-for", target, "--json")
			if got.code != 0 {
				t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
			}
			if !strings.Contains(got.stderr, "sms_mms") {
				t.Fatalf("no sms_mms warning on stderr for --wait-for %s:\n%s", target, got.stderr)
			}
			if !strings.Contains(got.stderr, "warning") {
				t.Errorf("the sms_mms notice is not marked as a warning:\n%s", got.stderr)
			}
			if strings.Contains(got.stdout, "sms_mms conversation") {
				t.Errorf("the warning is on stdout, which must stay machine-readable:\n%s",
					got.stdout)
			}
			assertExactlyOneJSONValue(t, "messages send --wait-for "+target, got.stdout)
		})
	}
}

// The warning is for SMS alone: an RCS conversation gets none, and neither
// does a wait for `sent` or `terminal`, which settle on SMS.
func TestNoSMSWarningWhereItWouldBeNoise(t *testing.T) {
	cases := []struct {
		name    string
		convo   string
		waitFor string
	}{
		{"rcs and delivered", "rcs", "delivered"},
		{"sms and sent", "sms_mms", "sent"},
		{"sms and terminal", "sms_mms", "terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.conversationType = tc.convo
			s.deliveryState = "delivered"

			got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
				"--text", "on my way", "--wait-for", tc.waitFor)
			if got.code != 0 {
				t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
			}
			if strings.Contains(got.stderr, "sms_mms conversation") {
				t.Errorf("warned where the wait settles anyway:\n%s", got.stderr)
			}
		})
	}
}

// `delivered` and `read` are MESSAGE states, not operation states, so the
// wait continues after the operation is already terminal (spec section 11.3).
// With the message stuck at `sent` and the operation succeeded, `--wait-for
// delivered` times out -- exit 7 -- where `--wait-for terminal` returns.
func TestDeliveredKeepsWaitingAfterTheOperationIsTerminal(t *testing.T) {
	s := newStub(t)
	s.operationStatus = "succeeded"
	s.deliveryState = "sent"

	delivered := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait-for", "delivered", "--timeout", "0s")
	if delivered.code != 7 {
		t.Errorf("--wait-for delivered on a terminal operation exited %d, want 7: it is a "+
			"MESSAGE state and the wait continues\nstderr: %s", delivered.code, delivered.stderr)
	}

	s2 := newStub(t)
	s2.operationStatus = "succeeded"
	s2.deliveryState = "sent"
	terminal := runCLI(t, s2, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait-for", "terminal")
	if terminal.code != 0 {
		t.Errorf("--wait-for terminal exited %d, want 0: it is the flag that means "+
			"'stop as soon as anything is settled'\nstderr: %s", terminal.code, terminal.stderr)
	}
}

// A wait emits ONE value, whichever way it ends, because the waited result
// replaces the immediate one rather than following it.
func TestAWaitEmitsOneValue(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "messages", "send", "conv_01k4z2p8vq",
		"--text", "hello", "--wait", "--json")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	assertExactlyOneJSONValue(t, "messages send --wait", got.stdout)
}
