package api_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// S-1. The spec is a reference document read as a table, and a markdown table
// row with more cells than its header does not render as a long row: the
// extra cells are DROPPED. A sentence inserted into the wrong cell therefore
// deletes whatever came after it, silently, in the one place a reader looks
// for the contract.
//
// That is not hypothetical. The D34 note was inserted into §7.7's
// `POST /v1/conversations` row with a leading pipe, which gave a 4-column
// table a 5-cell row and made the whole Answer column of that route -- the
// 200 shape, the name-only-for-2+-recipients rule, and the zero-recipients
// refusal -- vanish from the rendered page while the source still contained
// every word of it. docs/api.md's route tables were already parsed by
// TestAPIDocDocumentsEveryRoute, which is why the same mistake in docs/
// would have been caught; the spec had no such check.
//
// This walks EVERY table in the spec rather than §7.7's, because the defect
// is a property of markdown, not of that section.
//
// Plant: add or remove one cell in any spec table row and this fails naming
// the line, the header's width and the row's. Planted 2026-09-07.
func TestSpecTablesHaveOneCellPerColumn(t *testing.T) {
	doc := readSpec(t)

	lines := strings.Split(doc, "\n")
	checked := 0
	for i := 0; i < len(lines); i++ {
		// A table is a header row, a delimiter row of dashes, then rows.
		if !isTableRow(lines[i]) || i+1 >= len(lines) || !isDelimiterRow(lines[i+1]) {
			continue
		}
		want := len(splitCells(lines[i]))
		if want != len(splitCells(lines[i+1])) {
			t.Errorf("plans/AGENT_GM_SPEC.md:%d: the delimiter row has %d columns "+
				"but its header has %d", i+2, len(splitCells(lines[i+1])), want)
		}
		for j := i + 2; j < len(lines) && isTableRow(lines[j]); j++ {
			checked++
			if got := len(splitCells(lines[j])); got != want {
				t.Errorf("plans/AGENT_GM_SPEC.md:%d: this row has %d cells but the "+
					"table has %d columns, so %d cell(s) are DROPPED when it renders "+
					"-- the text is in the file and not on the page:\n%s",
					j+1, got, want, got-want, strings.TrimSpace(lines[j]))
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d table rows were checked; the table scanner has stopped "+
			"finding the spec's tables", checked)
	}
}

func readSpec(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	path := filepath.Join(filepath.Dir(self), "..", "..", "plans", "AGENT_GM_SPEC.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	return string(b)
}

func isTableRow(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "|") && strings.HasSuffix(s, "|") && len(s) > 1
}

func isDelimiterRow(line string) bool {
	if !isTableRow(line) {
		return false
	}
	for _, cell := range splitCells(line) {
		c := strings.TrimSpace(cell)
		if c == "" {
			return false
		}
		if strings.Trim(c, "-:") != "" {
			return false
		}
	}
	return true
}

// splitCells splits a table row on its unescaped pipes, which is what a
// renderer does. A pipe inside backticks still splits -- GFM has no such
// exemption, and believing it does is one way to write a row that loses a
// cell.
func splitCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "|"), "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			cur.WriteByte(s[i])
			cur.WriteByte(s[i+1])
			i++
		case s[i] == '|':
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(cells, cur.String())
}
