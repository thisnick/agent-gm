package mcp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// docPath is `docs/mcp.md` relative to this package.
func docPath() string { return filepath.Join("..", "..", "docs", "mcp.md") }

func readDoc(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(docPath())
	if err != nil {
		t.Fatalf("reading docs/mcp.md: %v", err)
	}
	return string(body)
}

// TestSlice3Test20InstructionsMatchTheDocs is acceptance test 20.
//
// The `initialize` instructions block is byte-identical to the "First five
// minutes" section of `docs/mcp.md` modulo markdown quoting.
//
// It matters because the two have different readers and the same content: a
// client that surfaces instructions shows the block to its model, and a human
// reads the page. Two copies that could drift would drift, and the one that
// went stale would be the one nobody reads -- which is the model's.
func TestSlice3Test20InstructionsMatchTheDocs(t *testing.T) {
	section := firstFiveMinutes(t, readDoc(t))
	served := strings.TrimRight(mcp.Instructions, "\n")

	if section == served {
		return
	}
	// Name the first differing line rather than dumping four kilobytes: a
	// failure a reader cannot act on is a failure somebody deletes the test
	// over.
	docLines := strings.Split(section, "\n")
	servedLines := strings.Split(served, "\n")
	for i := range max(len(docLines), len(servedLines)) {
		var d, s string
		if i < len(docLines) {
			d = docLines[i]
		}
		if i < len(servedLines) {
			s = servedLines[i]
		}
		if d != s {
			t.Fatalf("docs/mcp.md and the served instructions block differ at line %d of the section:\n"+
				"  docs/mcp.md: %q\n  served:      %q", i+1, d, s)
		}
	}
	t.Fatalf("the two differ in length: docs has %d lines, served has %d", len(docLines), len(servedLines))
}

// firstFiveMinutes extracts the section by PARSING the page, so the test
// reads what a reader reads rather than a fixture beside it.
//
// "Modulo markdown quoting" is the one transformation allowed: a line quoted
// as a block quote has its `> ` removed. The page does not quote it today, so
// this is a tolerance rather than a rewrite.
func firstFiveMinutes(t *testing.T, page string) string {
	t.Helper()
	const heading = "## First five minutes"
	lines := strings.Split(page, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/mcp.md has no %q heading", heading)
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	body := lines[start:end]
	out := make([]string, 0, len(body))
	for _, line := range body {
		switch {
		case strings.HasPrefix(line, "> "):
			out = append(out, line[2:])
		case line == ">":
			out = append(out, "")
		default:
			out = append(out, line)
		}
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// TestSlice3DocsCoverTheServedCatalogue keeps `docs/mcp.md` honest about the
// two things it duplicates from the code: the tool names and the annotations.
//
// Section 13.5's planted-mutation discipline is the reason. A reviewer who
// edits a served annotation must see a test fail; if the page were only prose,
// the mutation would survive and the page would quietly become wrong.
func TestSlice3DocsCoverTheServedCatalogue(t *testing.T) {
	page := readDoc(t)

	for _, tool := range mcp.Tools {
		if !strings.Contains(page, "`"+tool.Name+"`") {
			t.Errorf("docs/mcp.md never names the tool %s", tool.Name)
		}
		row := annotationRow(page, tool.Name)
		if row == "" {
			t.Errorf("docs/mcp.md has no annotations row for %s", tool.Name)
			continue
		}
		want := []bool{tool.ReadOnlyHint, tool.DestructiveHint, tool.IdempotentHint, tool.OpenWorldHint}
		got := boolsOf(row)
		if len(got) != 4 {
			t.Errorf("the annotations row for %s does not carry four hints: %q", tool.Name, row)
			continue
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("docs/mcp.md's annotations row for %s is %q, and the served value of hint %d is %v",
					tool.Name, row, i+1, w)
			}
		}
	}

	// The fresh-key sentence appears on the page, so a reviewer who mutates
	// the served constant sees this fail as well as the catalogue test.
	if !strings.Contains(normaliseSpaces(page), normaliseSpaces(mcp.FreshKeySentence)) {
		t.Error("docs/mcp.md does not carry the fresh-key sentence of section 8.2")
	}

	// Every named exclusion is on the page with its reason, because an
	// exclusion a reader cannot find is indistinguishable from an omission.
	for _, e := range mcp.Exclusions {
		if !strings.Contains(normaliseSpaces(page), normaliseSpaces(e.Reason)) {
			t.Errorf("docs/mcp.md does not carry the reason for excluding %s", e.Route)
		}
	}
}

// annotationRow finds the table row for a tool in the annotations table.
func annotationRow(page, tool string) string {
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| `"+tool+"` | ") && strings.Count(line, "|") == 6 {
			return line
		}
	}
	return ""
}

func boolsOf(row string) []bool {
	var out []bool
	for _, cell := range strings.Split(row, "|") {
		switch strings.TrimSpace(cell) {
		case "true":
			out = append(out, true)
		case "false":
			out = append(out, false)
		}
	}
	return out
}

// normaliseSpaces collapses whitespace and drops block-quote markers, so a
// sentence the page wraps across three quoted lines still compares equal to
// the one constant it came from.
func normaliseSpaces(s string) string {
	var words []string
	for _, w := range strings.Fields(s) {
		if w == ">" {
			continue
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}
