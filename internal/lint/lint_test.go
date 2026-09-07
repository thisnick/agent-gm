package lint_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/lint"
	"github.com/thisnick/agent-gm/internal/mcp"
)

// docsDir is `docs/` relative to this package.
func docsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "docs")
}

// TestNameLint is the lint itself (spec section 13.5), and is what
// `devbox run lint-names` runs.
//
// It reads the catalogue the server actually serves and every markdown page
// under `docs/`. A failure names the surface, the word and the text it was
// found in, because a lint whose failure is "docs are wrong" is a lint
// somebody deletes.
func TestNameLint(t *testing.T) {
	findings, err := lint.Run(docsDir(t))
	if err != nil {
		t.Fatalf("running the name lint: %v", err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
	if len(findings) > 0 {
		t.Fatalf("%d banned name(s) reached a live contract", len(findings))
	}
}

// TestNameLintCatchesEveryBannedWordInAServedDescription proves the first
// direction of section 16 Slice 3 test 24: every banned word IS caught when it
// appears in a served tool description.
//
// It is a table over lint.BannedWords itself rather than a list somebody typed
// out, so a word added to the ban list without a matcher fails here rather
// than silently passing for ever.
func TestNameLintCatchesEveryBannedWordInAServedDescription(t *testing.T) {
	for _, word := range lint.BannedWords {
		t.Run(word, func(t *testing.T) {
			text := "Send a message into the " + word + " you chose."
			findings := lint.CheckText("the description of tool send_message", text)
			if len(findings) == 0 {
				t.Fatalf("%q reached a served description and the lint did not catch it", word)
			}
			if findings[0].Word != word {
				t.Fatalf("caught %q, wanted %q", findings[0].Word, word)
			}
		})
	}
	for _, prefix := range lint.BannedPrefixes {
		t.Run(prefix, func(t *testing.T) {
			findings := lint.CheckText("the description of tool get_message", "pass "+prefix+"id here")
			if len(findings) == 0 {
				t.Fatalf("the %q prefix reached a served description and the lint did not catch it", prefix)
			}
		})
	}
}

// TestNameLintCatchesEveryBannedWordInADocsPage is the same direction for the
// other half of the lint's input: a `docs/` page's own prose.
func TestNameLintCatchesEveryBannedWordInADocsPage(t *testing.T) {
	for _, word := range lint.BannedWords {
		t.Run(word, func(t *testing.T) {
			page := "# A page\n\nAgent GM keeps one " + word + " per account.\n"
			findings := lint.CheckMarkdownText("docs/invented.md", page)
			if len(findings) == 0 {
				t.Fatalf("%q reached a docs page and the lint did not catch it", word)
			}
			if !strings.HasPrefix(findings[0].Where, "docs/invented.md:") {
				t.Fatalf("the finding does not name the file and line: %q", findings[0].Where)
			}
		})
	}
}

// TestNameLintExemptions is the other direction of test 24: the exempt cases
// still pass.
//
// Each is exempt for a stated reason, and the reason is what the test asserts,
// because an exemption nobody can explain is an exemption that will be widened
// by the next person who trips over it.
func TestNameLintExemptions(t *testing.T) {
	t.Run("a fenced block quoting upstream", func(t *testing.T) {
		page := "# Compatibility\n\nAgent GM reads the same field upstream does:\n\n" +
			"```go\n// connector/handlematrix.go\ntype Portal struct { RoomID string; EventID string }\n```\n\n" +
			"That structure never reaches a surface Agent GM serves.\n"
		if findings := lint.CheckMarkdownText("docs/compatibility.md", page); len(findings) > 0 {
			t.Fatalf("a fenced block quoting upstream was flagged: %v", findings)
		}
	})

	t.Run("a block quote", func(t *testing.T) {
		page := "# History\n\nAgent MX's own note said:\n\n> one portal per room, and a redaction event\n\nAgent GM does none of that.\n"
		if findings := lint.CheckMarkdownText("docs/history.md", page); len(findings) > 0 {
			t.Fatalf("a block quote was flagged: %v", findings)
		}
	})

	t.Run("plans/AGENT_GM_SPEC.md is exempt in full, including section 18.3", func(t *testing.T) {
		// The exemption is structural: `lint.Run` reads `docs/` and nothing
		// else. What has to be asserted is that the exemption MATTERS -- that
		// the spec really does carry the banned words, so the lint would fail
		// on it if the scope ever widened -- and that no finding from the
		// whole lint names a file outside `docs/`.
		spec := filepath.Join("..", "..", "plans")
		specFindings, err := lint.CheckMarkdown(spec)
		if err != nil {
			t.Fatalf("scanning plans: %v", err)
		}
		if len(specFindings) == 0 {
			t.Fatal("plans/ carries none of the banned words, so its exemption is untested")
		}
		var sawSection183 bool
		for _, f := range specFindings {
			if f.Word == "provider" || f.Word == "portal" || f.Word == "room" {
				sawSection183 = true
			}
		}
		if !sawSection183 {
			t.Fatalf("the spec was expected to carry the Agent MX vocabulary of section 18.3; found %v", specFindings)
		}
		all, err := lint.Run(docsDir(t))
		if err != nil {
			t.Fatalf("running the lint: %v", err)
		}
		for _, f := range all {
			if strings.Contains(f.Where, "plans") {
				t.Fatalf("the lint read %s, which section 13.5 exempts in full", f.Where)
			}
		}
	})

	t.Run("send_mode is not a bare mode argument", func(t *testing.T) {
		arg := mcp.Arg{Name: "send_mode", Description: "irrelevant", Schema: &mcp.Schema{Type: "string"}}
		if findings := lint.CheckText("argument x.send_mode (name)", arg.Name); len(findings) > 0 {
			t.Fatalf("send_mode was flagged as a banned word: %v", findings)
		}
		if bannedArgument(arg.Name) {
			t.Fatal("send_mode matched the argument deny list, which is matched WHOLE and never as a substring")
		}
	})

	t.Run("search's mode is a search vocabulary", func(t *testing.T) {
		tool, ok := mcp.ToolByName("search_messages")
		if !ok {
			t.Fatal("search_messages is not in the catalogue")
		}
		var found bool
		for _, a := range tool.Args {
			if a.Name == "mode" {
				found = true
			}
		}
		if !found {
			t.Fatal("search_messages has no mode argument, so this exemption is untested")
		}
		for _, f := range lint.CheckCatalogue() {
			if strings.Contains(f.Where, "search_messages.mode") {
				t.Fatalf("search's mode was flagged: %v", f)
			}
		}
	})
}

// TestABareModeArgumentIsCaught is the other side of the `mode` rule: an
// argument called `mode` whose values are NOT the search vocabulary is a
// finding. Without this, "the banned mode is an argument name" would be a rule
// that never fires.
func TestABareModeArgumentIsCaught(t *testing.T) {
	// The lint reads the real catalogue, so this asserts on the rule's own
	// predicate through a catalogue that carries a planted argument.
	planted := mcp.Arg{
		Name:        "mode",
		Description: "how to delete",
		Schema: &mcp.Schema{Type: "string", Enum: []any{"for_me", "for_everyone"},
			OneOf: []*mcp.Schema{{Const: "for_me"}, {Const: "for_everyone"}}},
	}
	if !lint.IsBareModeArgument(planted) {
		t.Fatal("a `mode` argument choosing between destructive behaviours was not recognised as one")
	}
	real, ok := mcp.ToolByName("search_messages")
	if !ok {
		t.Fatal("search_messages is not in the catalogue")
	}
	for _, a := range real.Args {
		if a.Name == "mode" && lint.IsBareModeArgument(a) {
			t.Fatal("search's words|exact mode was treated as a bare mode argument")
		}
	}
}

func bannedArgument(name string) bool {
	for _, b := range lint.BannedArgumentNames {
		if b == name {
			return true
		}
	}
	return false
}
