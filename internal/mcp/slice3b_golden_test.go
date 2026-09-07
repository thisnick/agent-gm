package mcp_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/thisnick/agent-gm/internal/mcp"
)

// The served catalogue, pinned byte for byte (review finding B-2).
//
// Everything about the catalogue was asserted structurally -- every argument
// HAS a description, every schema IS closed, no description contains a banned
// word -- and nothing asserted what any of it SAYS. The reviewer shortened
// `send_message.text`'s description from
//
//	"The message body. Optional only when `upload_ids` is present, in which
//	 case it is the caption."
//
// to "The message body." and the whole suite stayed green. That sentence is
// the only place a model is told `text` may be omitted when sending an
// attachment; losing it costs the model a fact and costs us nothing we would
// notice.
//
// So the catalogue is a golden file. It is the surface a model reads, and
// §8.2's schema rules are only worth having if the words are the contract too
// -- a description is not decoration, it is the whole of what the model knows
// about an argument before it uses it. The golden covers tool names,
// descriptions, annotations, every argument name and description, every enum
// and its per-value descriptions, and every output schema, because those are
// exactly the things §8.2 fixes and none of them had a byte-level assertion.
//
// Regenerate deliberately: `go test ./internal/mcp/ -run Golden -update`, then
// READ THE DIFF. A regeneration that is not read is a golden that asserts
// nothing.

var updateGolden = flag.Bool("update", false, "rewrite the served-catalogue golden file")

const goldenPath = "testdata/served-catalogue.json"

// TestTheServedCatalogueMatchesItsGolden renders the catalogue exactly as
// `tools/list` serialises it and compares it to the checked-in file.
func TestTheServedCatalogueMatchesItsGolden(t *testing.T) {
	got := renderCatalogue(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("writing the golden: %v", err)
		}
		t.Log("golden rewritten; read the diff before committing it")
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v\nIf this is a new tool, regenerate with -update and read the diff.",
			goldenPath, err)
	}
	if string(got) == string(want) {
		return
	}

	// Name the first tool that differs, so a failure says WHAT changed rather
	// than that something did. A diff of two megabytes of JSON is a failure
	// nobody reads.
	t.Errorf("the served catalogue has drifted from %s.\n%s\n\n"+
		"If the change is intended, regenerate with\n"+
		"    go test ./internal/mcp/ -run Golden -update\n"+
		"and read the diff: every one of these strings is something a model is "+
		"told, and section 8.2 makes the words as much of the contract as the types.",
		goldenPath, firstDifference(t, want, got))
}

// renderCatalogue serialises every tool the way the wire carries it.
func renderCatalogue(t *testing.T) []byte {
	t.Helper()
	type entry struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Scope        string          `json:"scope"`
		Annotations  mcp.Annotations `json:"annotations"`
		InputSchema  any             `json:"inputSchema"`
		OutputSchema any             `json:"outputSchema"`
	}
	entries := make([]entry, 0, len(mcp.Tools))
	for _, tool := range mcp.Tools {
		entries = append(entries, entry{
			Name:         tool.Name,
			Description:  tool.Description,
			Scope:        string(tool.Scope),
			Annotations:  tool.Annotations,
			InputSchema:  tool.InputSchema(),
			OutputSchema: tool.OutputSchema(),
		})
	}
	// Sorted, so that reordering the catalogue source is not a diff. Order is
	// not part of the contract; `tools/list` is a set.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	body, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("rendering the catalogue: %v", err)
	}
	return append(body, '\n')
}

// firstDifference reports the first tool whose rendering differs, with the
// line that differs inside it.
func firstDifference(t *testing.T, want, got []byte) string {
	t.Helper()
	var oldEntries, newEntries []map[string]any
	if json.Unmarshal(want, &oldEntries) != nil || json.Unmarshal(got, &newEntries) != nil {
		return "the golden file is not valid JSON; regenerate it"
	}
	byName := func(entries []map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, e := range entries {
			name, _ := e["name"].(string)
			out[name] = e
		}
		return out
	}
	oldByName, newByName := byName(oldEntries), byName(newEntries)

	var names []string
	for name := range oldByName {
		names = append(names, name)
	}
	for name := range newByName {
		if _, ok := oldByName[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		before, hadBefore := oldByName[name]
		after, hasAfter := newByName[name]
		switch {
		case !hadBefore:
			return "tool " + name + " is new"
		case !hasAfter:
			return "tool " + name + " is gone"
		}
		b, _ := json.MarshalIndent(before, "", "  ")
		a, _ := json.MarshalIndent(after, "", "  ")
		if string(b) != string(a) {
			return "tool " + name + " changed:\n  golden:\n" + indent(string(b)) +
				"\n  served:\n" + indent(string(a))
		}
	}
	return "the tools are identical but the file is not; the rendering changed"
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "    " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
