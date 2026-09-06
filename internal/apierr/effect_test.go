package apierr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectSentencesAreTheSpecsWords reads plans/AGENT_GM_SPEC.md and
// asserts each effect sentence appears in it verbatim. It compares against
// the spec itself rather than against a second copy in the test, because the
// whole point of these constants is that there is exactly one wording; a test
// holding its own copy would just be a fourth place to drift.
//
// The spec is compared with Markdown blockquote markers stripped and
// whitespace collapsed, so a sentence the spec wraps across lines -- the
// account-removal one is a wrapped blockquote -- still matches.
func TestEffectSentencesAreTheSpecsWords(t *testing.T) {
	spec := collapseSpaces(stripBlockquotes(readSpec(t)))

	for _, sentence := range Effects() {
		if !strings.Contains(spec, collapseSpaces(sentence)) {
			t.Errorf("this sentence is not in plans/AGENT_GM_SPEC.md:\n%q", sentence)
		}
	}
}

// TestEffectSentencesSayWhatIsNotAffected pins the clause that makes each
// sentence safe to show an agent: "delete" on a messaging surface is
// ambiguous, and a model that believes it can unsend a message will act on
// that belief.
func TestEffectSentencesSayWhatIsNotAffected(t *testing.T) {
	cases := []struct {
		name     string
		sentence string
		must     []string
	}{
		{
			"message delete", EffectMessageDelete,
			[]string{"this message", "your Google Messages account only", "the recipient keeps it"},
		},
		{
			"conversation delete", EffectConversationDelete,
			[]string{"this conversation", "your Google Messages account only", "the other people in it keep it"},
		},
		{
			"account removal", EffectAccountRemove,
			[]string{"permanently deletes", "conversations, messages, attachments and operations", "untouched"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.must {
				if !strings.Contains(tc.sentence, want) {
					t.Errorf("the sentence does not say %q:\n%q", want, tc.sentence)
				}
			}
			if strings.HasSuffix(tc.sentence, ".") {
				t.Errorf("the sentence carries its own full stop, so a surface that adds one gets two:\n%q", tc.sentence)
			}
		})
	}
}

// TestEffectSentencesAreDistinct proves the three are three, so a route
// cannot print the conversation sentence for a message delete and pass a
// byte-for-byte comparison.
func TestEffectSentencesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Effects() {
		if seen[s] {
			t.Fatalf("two effect sentences are identical: %q", s)
		}
		seen[s] = true
	}
	if len(Effects()) != 3 {
		t.Fatalf("Effects() has %d entries, want 3", len(Effects()))
	}
}

// readSpec finds plans/AGENT_GM_SPEC.md by walking up from the package
// directory to the module root.
func readSpec(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "plans", "AGENT_GM_SPEC.md")
		if body, err := os.ReadFile(candidate); err == nil {
			return string(body)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("plans/AGENT_GM_SPEC.md not found above the package directory")
		}
		dir = parent
	}
}

// collapseSpaces reduces every run of whitespace to one space, so a sentence
// the spec wraps across lines compares equal to the constant.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stripBlockquotes removes the leading Markdown "> " of each line, so a
// sentence the spec quotes across several lines reads as one sentence.
func stripBlockquotes(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimLeft(line, " \t"), ">")
	}
	return strings.Join(lines, "\n")
}
