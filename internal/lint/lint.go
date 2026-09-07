// Package lint is the name lint of spec section 13.5.
//
// It reads **the catalogue the server actually serves** -- tool names, tool
// descriptions, argument names and descriptions, enum values, and the
// `initialize` instructions block -- plus every markdown page under `docs/`,
// and fails on a Matrix vocabulary asserted as a live contract.
//
// The point is not tidiness. Agent GM is a rewrite of an Agent MX design
// (spec sections 1.3, 18.3), and the failure it guards against is a word from
// the old system reaching a cold agent as though it were part of this one's
// contract. A model that reads `event_id` in a tool description will send an
// `event_id`, and the refusal it gets back will be a mystery to whoever reads
// the transcript.
//
// Three scoping rules, all load-bearing, because a lint that fails its own
// spec is a lint nobody will keep:
//
//   - **The served catalogue is checked for every banned word.** Those are the
//     things a cold agent reads.
//   - **Markdown is checked under `docs/` only**, and only outside fenced code
//     blocks and block quotes. `plans/AGENT_GM_SPEC.md` is exempt in full: it
//     has to say what Agent MX called things, name upstream files such as
//     `connector/handlematrix.go`, and use ordinary English like "exit-code
//     matrix" and "log redaction".
//   - **The banned `mode` is an argument name**, matched whole against a deny
//     list, never as a substring. `send_mode` never reaches a surface (section
//     4.6), and `search`'s `mode` is `words|exact` -- a search vocabulary
//     rather than a switch between destructive behaviours, which is the thing
//     Agent MX retired.
package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// BannedWords is the Matrix vocabulary, as words. Each is matched
// case-insensitively on a word boundary, so `bedroom` is not `room` and
// `Provider` is `provider`.
var BannedWords = []string{
	"room",
	"portal",
	"event_id",
	"provider",
	"redact",
	"generation",
	"outbox",
	"tombstone",
}

// BannedPrefixes are prefixes no identifier may carry.
var BannedPrefixes = []string{"room_"}

// BannedArgumentNames is the deny list of section 13.5's third rule. It is
// matched **whole**: an argument called `send_mode` or `delivery_state` is not
// on it, because it is not `mode`.
var BannedArgumentNames = []string{"mode"}

// SearchModeValues is the one vocabulary that redeems an argument named
// `mode`.
//
// Section 13.5 exempts `search`'s `mode` by name, and the reason it gives is
// what the values are: `words|exact` is a search vocabulary, not a switch
// between destructive behaviours. So the exemption is written as that reason
// rather than as the tool's name -- a second tool that grew a `mode` meaning
// something else would be caught, and this one goes on passing only for as
// long as its values stay what they are.
var SearchModeValues = []string{"exact", "words"}

// Finding is one violation.
type Finding struct {
	// Where names the thing that carries it: a served surface, or a file and
	// line.
	Where string
	// Word is the banned word, prefix or argument name found.
	Word string
	// Context is the text it was found in, trimmed.
	Context string
	// Reason says why it is banned here, for a reader who has not read
	// section 13.5.
	Reason string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %q -- %s\n    %s", f.Where, f.Word, f.Reason, f.Context)
}

// wordPattern matches a banned word on a word boundary. `event_id` contains an
// underscore, which Go's \b does not treat as a boundary character the way a
// reader expects, so the boundaries are spelt out.
func wordPattern(word string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(word) + `($|[^A-Za-z0-9_])`)
}

var wordPatterns = func() map[string]*regexp.Regexp {
	out := map[string]*regexp.Regexp{}
	for _, w := range BannedWords {
		out[w] = wordPattern(w)
	}
	return out
}()

// CheckText scans one piece of served or documented text for every banned word
// and prefix.
func CheckText(where, text string) []Finding {
	var out []Finding
	for _, w := range BannedWords {
		if loc := wordPatterns[w].FindStringIndex(text); loc != nil {
			out = append(out, Finding{
				Where:   where,
				Word:    w,
				Context: excerpt(text, loc[0]),
				Reason:  "a Matrix word asserted as a live Agent GM contract (spec section 13.5)",
			})
		}
	}
	for _, p := range BannedPrefixes {
		if i := strings.Index(strings.ToLower(text), p); i >= 0 {
			out = append(out, Finding{
				Where:   where,
				Word:    p,
				Context: excerpt(text, i),
				Reason:  "a Matrix identifier prefix (spec section 13.5)",
			})
		}
	}
	return out
}

func excerpt(text string, at int) string {
	start := max(at-40, 0)
	end := min(at+60, len(text))
	return strings.TrimSpace(strings.ReplaceAll(text[start:end], "\n", " "))
}

// CheckCatalogue reads the catalogue the server actually serves.
//
// It reads mcp.Tools and mcp.Instructions rather than a copy, so a description
// edited into a violation fails here without anybody having to remember to
// update a fixture -- which is exactly the reviewer's planted-mutation case of
// section 13.5: putting a wrong sentence back into a served tool description
// must be caught by a lint test.
func CheckCatalogue() []Finding {
	var out []Finding
	out = append(out, CheckText("the instructions block (spec section 8.3)", mcp.Instructions)...)
	for _, t := range mcp.Tools {
		out = append(out, CheckText("tool name "+t.Name, t.Name)...)
		out = append(out, CheckText("the description of tool "+t.Name, t.Description)...)
		for _, a := range t.Args {
			where := fmt.Sprintf("argument %s.%s", t.Name, a.Name)
			out = append(out, CheckText(where+" (name)", a.Name)...)
			out = append(out, CheckText(where, a.Description)...)
			out = append(out, checkArgumentName(t, a)...)
			for _, v := range enumValues(a) {
				out = append(out, CheckText(where+" enum value", v)...)
			}
			for _, d := range enumDescriptions(a) {
				out = append(out, CheckText(where+" enum value description", d)...)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].Word < out[j].Word
	})
	return out
}

// checkArgumentName applies the third rule: the banned `mode` is an ARGUMENT
// NAME, matched whole, never as a substring.
func checkArgumentName(t mcp.Tool, a mcp.Arg) []Finding {
	for _, banned := range BannedArgumentNames {
		if a.Name != banned {
			continue
		}
		if banned == "mode" && !IsBareModeArgument(a) {
			// `search`'s `mode` is `words|exact`: a search vocabulary rather
			// than a switch between destructive behaviours, which is the
			// thing Agent MX retired (spec section 13.5).
			continue
		}
		return []Finding{{
			Where:   fmt.Sprintf("argument %s.%s (name)", t.Name, a.Name),
			Word:    banned,
			Context: a.Description,
			Reason: "a bare `mode` argument. Agent MX's `mode` chose between destructive " +
				"behaviours; Agent GM has no such switch (spec sections 4.6, 13.5)",
		}}
	}
	return nil
}

// IsBareModeArgument reports whether an argument is the `mode` section 13.5
// bans: one named `mode` whose values are anything other than the search
// vocabulary.
//
// It is exported because it is the rule, and a rule the meta-test can only
// reach through a whole catalogue is a rule the meta-test can only test in one
// direction.
func IsBareModeArgument(a mcp.Arg) bool {
	return a.Name == "mode" && !isSearchVocabulary(a)
}

// isSearchVocabulary reports whether an argument's accepted values are exactly
// the search vocabulary.
func isSearchVocabulary(a mcp.Arg) bool {
	values := enumValues(a)
	sort.Strings(values)
	if len(values) != len(SearchModeValues) {
		return false
	}
	for i, v := range values {
		if v != SearchModeValues[i] {
			return false
		}
	}
	return true
}

// enumValues is an argument's accepted string values, with the `null` that
// makes an optional filter omissible left out -- it is not a name.
func enumValues(a mcp.Arg) []string {
	if a.Schema == nil {
		return nil
	}
	var out []string
	for _, v := range a.Schema.Enum {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func enumDescriptions(a mcp.Arg) []string {
	if a.Schema == nil {
		return nil
	}
	var out []string
	for _, one := range a.Schema.OneOf {
		if one.Description != "" {
			out = append(out, one.Description)
		}
	}
	return out
}

// CheckMarkdown scans every markdown page under dir, outside fenced code
// blocks and block quotes.
//
// A fenced block is exempt because a page has to be able to quote an upstream
// file, a Matrix payload or a log line in order to explain it; a block quote is
// exempt for the same reason. What is checked is the page's own prose, which
// is where a word becomes a claim about Agent GM rather than a quotation.
func CheckMarkdown(dir string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, checkMarkdownBody(path, string(body))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out, nil
}

// checkMarkdownBody is the per-file scan, exported to the tests through
// CheckMarkdownText so the meta-test can build a page in memory rather than on
// disk.
func checkMarkdownBody(path, body string) []Finding {
	var out []Finding
	fenced := false
	var fence string
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !fenced && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			fenced, fence = true, trimmed[:3]
			continue
		}
		if fenced {
			if strings.HasPrefix(trimmed, fence) {
				fenced = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			// A block quote: quoted text, not this page's own claim.
			continue
		}
		for _, f := range CheckText(fmt.Sprintf("%s:%d", path, i+1), line) {
			f.Context = strings.TrimSpace(line)
			out = append(out, f)
		}
	}
	return out
}

// CheckMarkdownText scans one page's text, for a test that would rather not
// write a file.
func CheckMarkdownText(name, body string) []Finding { return checkMarkdownBody(name, body) }

// Run is the whole lint: the served catalogue plus `docs/`.
func Run(docsDir string) ([]Finding, error) {
	out := CheckCatalogue()
	pages, err := CheckMarkdown(docsDir)
	if err != nil {
		return nil, err
	}
	return append(out, pages...), nil
}
