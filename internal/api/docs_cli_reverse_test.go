package api_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/cli"
)

// R-8, the reviewer's one surviving plant. The CLI doc test checked ONE
// direction -- every command in the inventory appears somewhere in
// docs/cli.md, by substring -- and nothing checked the reverse. So the
// reviewer appended a section documenting `agm conversations purge`, a
// command that does not exist, claiming it "permanently erases a thread from
// every device", and the suite stayed green.
//
// That is worse than an ordinary doc bug. Google's delete is delete-for-me
// (D14) and §7.7 fixes the exact sentence every surface uses, precisely so a
// human or a model cannot read a broader claim off one surface than another.
// A page that invents a destructive command with a broader scope claim
// defeats the whole arrangement, and the page is what an owner reads before
// typing something irreversible.
//
// So: every command heading and synopsis line in docs/cli.md must name a
// command the compiled inventory has, and every effect sentence the page
// quotes must be one of the three constants.
//
// Plant: append a section for a command that does not exist and
// TestCLIDocNamesNoCommandThatDoesNotExist fails naming it. Planted
// 2026-09-07.
func TestCLIDocNamesNoCommandThatDoesNotExist(t *testing.T) {
	doc := readDoc(t, "cli.md")

	known := map[string]bool{}
	for _, c := range cli.Commands {
		known[c.Name] = true
		// A group's verbs are written several ways in the inventory --
		// "conversations archive|unarchive|pin|unpin|mark-unread" is one
		// entry -- so each alternative counts as known.
		if i := strings.LastIndex(c.Name, " "); i >= 0 {
			group, verbs := c.Name[:i], c.Name[i+1:]
			for _, v := range strings.Split(verbs, "|") {
				known[group+" "+v] = true
			}
		}
	}
	// The local commands section 11.3 lists that drive no route, so the
	// inventory does not carry them.
	for _, local := range []string{"completion", "version", "pair"} {
		known[local] = true
	}

	// Two shapes name a command on the page: a heading, and a synopsis line
	// in a fenced block. Both are parsed, because the plant used a heading
	// and a rename would use a synopsis.
	for _, found := range cliCommandsNamedIn(doc) {
		if !known[found.name] {
			t.Errorf("docs/cli.md documents `agm %s` (%s), which the CLI inventory does "+
				"not have. A page that invents a command -- especially a destructive "+
				"one -- is read before somebody types it", found.name, found.where)
		}
	}
}

// Every `effect` sentence the page quotes must be one of the three compiled
// constants, byte for byte. The sentence is the same words in the route's
// response, the MCP tool description and the `agm` prompt (§7.7); a fourth
// wording on the page is a fourth claim.
func TestCLIDocQuotesOnlyTheRealEffectSentences(t *testing.T) {
	doc := readDoc(t, "cli.md") + "\n" + readDoc(t, "api.md")
	real := []string{
		apierr.EffectMessageDelete,
		apierr.EffectConversationDelete,
		apierr.EffectAccountRemove,
	}
	// Any sentence claiming a delete reaches beyond the owner's own copy is
	// the specific lie D14 exists to prevent.
	for _, overreach := range []string{
		"from every device", "for everyone", "on the recipient's phone",
		"permanently erases a thread", "deletes it for both",
	} {
		if strings.Contains(strings.ToLower(doc), overreach) {
			t.Errorf("the docs claim a delete %q; Google's delete is delete-for-me "+
				"and every surface says the same words (D14, 7.7)", overreach)
		}
	}
	for _, want := range real {
		if !strings.Contains(doc, want) {
			t.Errorf("the docs never quote the effect sentence %q", want)
		}
	}
}

type namedCommand struct{ name, where string }

var (
	cliHeading  = regexp.MustCompile(`(?m)^#{2,4}\s+` + "`?" + `agm ([a-z][a-z0-9 -]*?)` + "`?" + `\s*$`)
	// Up to three segments, because section 11.3 has `admin settings set`
	// and `admin audit list`. Matching only two reported them as commands
	// that do not exist, which is the right failure for the wrong reason.
	cliSynopsis = regexp.MustCompile(`(?m)^agm ([a-z][a-z0-9-]*(?: [a-z][a-z0-9-|]*){0,2})`)
)

// cliCommandsNamedIn extracts every command docs/cli.md names, from headings
// and from synopsis lines in fenced blocks.
func cliCommandsNamedIn(doc string) []namedCommand {
	seen := map[string]string{}
	for _, m := range cliHeading.FindAllStringSubmatch(doc, -1) {
		seen[strings.TrimSpace(m[1])] = "heading"
	}
	for _, m := range cliSynopsis.FindAllStringSubmatch(doc, -1) {
		name := strings.TrimSpace(m[1])
		if _, dup := seen[name]; !dup {
			seen[name] = "synopsis"
		}
	}
	out := make([]namedCommand, 0, len(seen))
	for name, where := range seen {
		out = append(out, namedCommand{name: name, where: where})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
