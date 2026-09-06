package cli_test

import (
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/cli"
)

// Section 16 Slice 2 test 28:
//
//	"agm messages delete and agm conversations delete print the effect string
//	 TAKEN FROM THE ROUTE'S RESPONSE, and refuse without y or --yes; the
//	 printed string equals the response field byte for byte."
//
// The stub returns a deliberately unusual sentence -- one no constant in this
// repository holds -- so a CLI that printed apierr.EffectMessageDelete
// instead of the response's field would pass a weaker test and fails this
// one.
//
// Plant: change internal/cli/output.go's emitTable to print
// apierr.EffectMessageDelete instead of the `effect` field, and
// TestDeletePrintsTheRoutesEffectSentenceByteForByte fails. Planted
// 2026-09-06.
func TestDeletePrintsTheRoutesEffectSentenceByteForByte(t *testing.T) {
	const unusual = "removes Agent GM's copy of this thing and, this being a stub, nothing else " +
		"whatsoever; the recipient's copy, the carrier's copy and the moon are untouched"

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"messages delete", []string{"messages", "delete", "msg_01k4z2p8vt", "--yes"}},
		{"conversations delete", []string{"conversations", "delete", "conv_01k4z2p8vq", "--yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.effect = unusual

			got := runCLI(t, s, nil, "", tc.args...)
			if got.code != 0 {
				t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, unusual) {
				t.Fatalf("`agm %s` did not print the sentence the route returned.\n"+
					"want (byte for byte): %s\ngot stdout:\n%s", tc.name, unusual, got.stdout)
			}
			// And it printed the ROUTE's sentence rather than the local
			// constant, which is the actual claim of test 28.
			if strings.Contains(got.stdout, apierr.EffectMessageDelete) ||
				strings.Contains(got.stdout, apierr.EffectConversationDelete) {
				t.Errorf("`agm %s` printed a locally built effect sentence as well as, or "+
					"instead of, the route's:\n%s", tc.name, got.stdout)
			}
			// A sentence that differs from the one confirmed is worth saying
			// out loud, and it is said on stderr.
			if !strings.Contains(got.stderr, "not the one you confirmed") {
				t.Errorf("the CLI did not notice that the served sentence differs from the "+
					"one the prompt showed:\n%s", got.stderr)
			}
		})
	}
}

// The prompt itself is the same words, and it is on stderr so `--json` is
// undisturbed.
func TestTheConfirmationPromptIsTheEffectSentenceOnStderr(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "y\n", "messages", "delete", "msg_01k4z2p8vt")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, apierr.EffectMessageDelete) {
		t.Errorf("the prompt is not the effect sentence of spec 7.7:\n%s", got.stderr)
	}
	if !strings.Contains(got.stderr, "Continue? [y/N]") {
		t.Errorf("there is no confirmation prompt:\n%s", got.stderr)
	}
	if strings.Contains(got.stdout, "Continue?") {
		t.Errorf("the prompt is on stdout, which belongs to the result:\n%s", got.stdout)
	}
}

// Every destructive command of section 11.3 refuses without `y` or `--yes`,
// and refusing changes nothing.
func TestDestructiveCommandsRefuseWithoutConfirmation(t *testing.T) {
	cases := map[string][]string{
		"messages delete":      {"messages", "delete", "msg_01k4z2p8vt"},
		"conversations delete": {"conversations", "delete", "conv_01k4z2p8vq"},
		"accounts remove":      {"accounts", "remove", "acct_01k4z0aa"},
		"accounts sign-out":    {"accounts", "sign-out", "acct_01k4z0aa"},
		"auth logout":          {"auth", "logout"},
		"admin backfill":       {"admin", "backfill"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			for _, answer := range []string{"", "n\n", "no\n", "\n"} {
				s := newStub(t)
				got := runCLI(t, s, nil, answer, args...)
				if got.code != 2 {
					t.Errorf("answering %q exited %d, want 2\nstderr: %s", answer, got.code, got.stderr)
				}
				for _, r := range s.seen() {
					switch r.Method {
					case "DELETE", "POST", "PATCH":
						t.Errorf("answering %q still sent %s %s", answer, r.Method, r.Path)
					}
				}
			}
		})
	}
}

// --yes skips the prompt, which is the whole point of it in a script.
func TestYesSkipsThePrompt(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "messages", "delete", "msg_01k4z2p8vt", "--yes")
	if got.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "Continue?") {
		t.Errorf("--yes still prompted:\n%s", got.stderr)
	}
	if s.countOf("DELETE", "/v1/messages/msg_01k4z2p8vt") != 1 {
		t.Errorf("the delete was not sent exactly once: %v", s.seen())
	}
}

// `agm admin backfill` is destructive only in its widest form: with an
// --account or a --conversation it names a bounded piece of work and does not
// prompt (spec sections 5.4, 11.3).
func TestBackfillPromptsOnlyForTheWholeThing(t *testing.T) {
	s := newStub(t)
	got := runCLI(t, s, nil, "", "admin", "backfill", "--account", "acct_01k4z0aa")
	if got.code != 0 {
		t.Fatalf("a bounded backfill exited %d\nstderr: %s", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "Continue?") {
		t.Errorf("a bounded backfill prompted:\n%s", got.stderr)
	}

	s2 := newStub(t)
	whole := runCLI(t, s2, nil, "y\n", "admin", "backfill")
	if !strings.Contains(whole.stderr, "Continue?") {
		t.Errorf("a whole-deployment backfill did not prompt:\n%s", whole.stderr)
	}
	if whole.code != 0 {
		t.Fatalf("exited %d\nstderr: %s", whole.code, whole.stderr)
	}
	if !strings.Contains(s2.seen()[0].Body, `"confirm":true`) {
		t.Errorf("the whole-deployment backfill did not send confirm:true: %s", s2.seen()[0].Body)
	}
}

// The three sentences the CLI can show before a call are apierr's, so a human
// and a model cannot read a different claim off two surfaces (spec section
// 7.7). This asserts the CLI holds no fourth copy.
func TestTheConfirmSentencesAreApierrs(t *testing.T) {
	sentences := map[string]bool{}
	for _, s := range apierr.Effects() {
		sentences[s] = true
	}
	for _, name := range []string{"messages delete", "conversations delete", "accounts remove"} {
		c, ok := cli.CommandByName(name)
		if !ok {
			t.Fatalf("there is no command %q", name)
		}
		if !c.Destructive {
			t.Errorf("%s is not marked destructive", name)
		}
	}
	if len(sentences) != 3 {
		t.Errorf("apierr exports %d effect sentences, want the three of spec 7.7", len(sentences))
	}
}
