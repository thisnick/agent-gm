package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/api"
	"github.com/thisnick/agent-gm/internal/apierr"
	"github.com/thisnick/agent-gm/internal/cli"
)

// docs/api.md and docs/cli.md are reference pages: a reader trusts them the
// way they trust a schema, and a stale line in one of them is worse than a
// missing one, because it names something that will be refused. Spec section
// 7.1's strict parameter rejection is exactly the kind of promise a
// hand-maintained page breaks quietly -- a parameter renamed in
// internal/api/routes.go leaves a doc line that tells an agent author to send
// a field the server now answers `invalid_request` for.
//
// So the pages are checked against the two inventories in both directions.
// The route tables in docs/api.md are delimited with HTML comments and
// parsed; everything else is checked by presence.
//
// Spec sections 7, 7.2, 10, 11.1, 11.2, 11.3.

// docsDir is resolved from this file's own path rather than from the working
// directory, so the test passes wherever `go test` is invoked from.
func docsDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	dir := filepath.Join(filepath.Dir(self), "..", "..", "docs")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("docs directory %s: %v", dir, err)
	}
	return dir
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir(t), name))
	if err != nil {
		t.Fatalf("reading docs/%s: %v", name, err)
	}
	return string(b)
}

// documentedRoute is one row of a route-inventory table in docs/api.md.
type documentedRoute struct {
	method string
	path   string
	params map[string]bool
	line   int
}

var backticked = regexp.MustCompile("`([^`]+)`")

// parseRouteInventory reads every row between the
// `<!-- route-inventory:begin -->` and `<!-- route-inventory:end -->` markers.
// A row is `| `METHOD` | `/path` | `scope` | parameters | notes |`, and the
// parameters cell names each parameter in backticks.
func parseRouteInventory(t *testing.T, doc string) map[string]documentedRoute {
	t.Helper()
	out := map[string]documentedRoute{}
	inBlock := false
	for i, line := range strings.Split(doc, "\n") {
		switch {
		case strings.Contains(line, "route-inventory:begin"):
			inBlock = true
			continue
		case strings.Contains(line, "route-inventory:end"):
			inBlock = false
			continue
		}
		if !inBlock || !strings.HasPrefix(strings.TrimSpace(line), "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 4 {
			t.Errorf("docs/api.md:%d: route row has %d cells, want at least 4: %s",
				i+1, len(cells), strings.TrimSpace(line))
			continue
		}
		method := strings.Trim(strings.TrimSpace(cells[0]), "`")
		path := strings.Trim(strings.TrimSpace(cells[1]), "`")
		params := map[string]bool{}
		for _, m := range backticked.FindAllStringSubmatch(cells[3], -1) {
			params[m[1]] = true
		}
		key := method + " " + path
		if _, dup := out[key]; dup {
			t.Errorf("docs/api.md:%d: %s is documented twice", i+1, key)
		}
		out[key] = documentedRoute{method: method, path: path, params: params, line: i + 1}
	}
	if len(out) == 0 {
		t.Fatal("docs/api.md: found no route-inventory rows; are the " +
			"<!-- route-inventory:begin --> markers still there?")
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAPIDocDocumentsEveryRoute asserts the route inventory and docs/api.md
// agree in both directions: every route is documented, and no documented row
// names a route the inventory does not have.
func TestAPIDocDocumentsEveryRoute(t *testing.T) {
	documented := parseRouteInventory(t, readDoc(t, "api.md"))

	inventory := map[string]api.Route{}
	for _, r := range api.Routes {
		key := r.Method + " " + r.Path
		inventory[key] = r
		if _, ok := documented[key]; !ok {
			t.Errorf("docs/api.md does not document %s (route %q). Add a row to a "+
				"route-inventory table.", key, r.Name)
		}
	}
	for key, d := range documented {
		if _, ok := inventory[key]; !ok {
			t.Errorf("docs/api.md:%d documents %s, which internal/api.Routes does "+
				"not have. Stale row?", d.line, key)
		}
	}
}

// TestAPIDocDocumentsEveryParameter asserts that the parameters cell of each
// documented route is exactly the route's query parameters plus its body
// fields. Both directions matter: a missing one is a reader sending a filter
// they will never get, and an extra one is a reader sending a field that
// spec section 7.1 refuses by name.
//
// Path parameters are not checked here: they are visible in the path itself
// and are never accepted as query parameters.
func TestAPIDocDocumentsEveryParameter(t *testing.T) {
	documented := parseRouteInventory(t, readDoc(t, "api.md"))

	for _, r := range api.Routes {
		key := r.Method + " " + r.Path
		d, ok := documented[key]
		if !ok {
			continue // reported by TestAPIDocDocumentsEveryRoute
		}
		want := map[string]bool{}
		for _, q := range r.Query {
			want[q] = true
		}
		for _, b := range r.Body {
			want[b] = true
		}
		for p := range want {
			if !d.params[p] {
				t.Errorf("docs/api.md:%d: %s accepts %q, which the page does not name",
					d.line, key, p)
			}
		}
		for p := range d.params {
			if !want[p] {
				t.Errorf("docs/api.md:%d: %s is documented as accepting %q, which the "+
					"route does not accept. It would be refused with invalid_request.",
					d.line, key, p)
			}
		}
		if len(want) == 0 && len(d.params) != 0 {
			t.Errorf("docs/api.md:%d: %s takes no query or body parameters; the page "+
				"names %v", d.line, key, sortedKeys(d.params))
		}
	}
}

// prosePath finds a `METHOD /path` mention anywhere in the page, including
// one broken across a line, so a stale route named in a sentence fails too
// and not only a stale table row.
var prosePath = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE)[\s` + "`" + `]+(/[A-Za-z0-9/_{}.-]+)`)

// braces normalises `{conversation_id}` to `{}` so prose that writes `{id}`
// is compared on its shape rather than on the parameter's spelling.
var braces = regexp.MustCompile(`\{[^}]*\}`)

// proseAllowed are paths that are legitimately named with a method but are
// not `/v1` routes: the RFC surfaces of spec sections 8 and 9, which keep
// their own behaviour and arrive in Slice 3.
var proseAllowed = map[string]bool{
	"/mcp":             true,
	"/oauth/token":     true,
	"/oauth/revoke":    true,
	"/oauth/authorize": true,
}

// TestAPIDocNamesNoUnknownRouteInProse catches a route that was renamed or
// removed and left behind in a sentence.
func TestAPIDocNamesNoUnknownRouteInProse(t *testing.T) {
	doc := readDoc(t, "api.md")

	known := map[string]bool{}
	for _, r := range api.Routes {
		known[r.Method+" "+braces.ReplaceAllString(r.Path, "{}")] = true
	}
	for _, m := range prosePath.FindAllStringSubmatch(doc, -1) {
		path := strings.TrimRight(m[2], ".,;:`")
		if proseAllowed[path] {
			continue
		}
		key := m[1] + " " + braces.ReplaceAllString(path, "{}")
		if !known[key] {
			t.Errorf("docs/api.md names %s %s, which internal/api.Routes does not have",
				m[1], path)
		}
	}
}

// TestAPIDocDocumentsEveryErrorCode asserts every code of spec section 7.2,
// as implemented in internal/apierr, appears on the page.
func TestAPIDocDocumentsEveryErrorCode(t *testing.T) {
	doc := readDoc(t, "api.md")
	for _, c := range apierr.Codes() {
		if !strings.Contains(doc, "`"+string(c)+"`") {
			t.Errorf("docs/api.md does not document the error code %q", c)
		}
	}
}

// normaliseFlags turns the CLI inventory's prose descriptions into flag
// names. `Supplies` describes how a parameter is supplied in a sentence, so a
// mention can arrive as `--paste/--paste-file` or as `--file's`; the page
// documents the flags, not the sentence.
func normaliseFlags(mentioned []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range mentioned {
		for _, part := range strings.Split(f, "/") {
			part = strings.TrimSuffix(part, "'s")
			part = strings.Trim(part, "`'\",.;:")
			if len(part) > 2 && strings.HasPrefix(part, "--") && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestCLIDocDocumentsEveryCommand asserts every command of the CLI inventory,
// and every flag those commands name, appears in docs/cli.md.
func TestCLIDocDocumentsEveryCommand(t *testing.T) {
	doc := readDoc(t, "cli.md")

	for _, c := range cli.Commands {
		if !strings.Contains(doc, c.String()) {
			t.Errorf("docs/cli.md does not document the command %q", c.String())
		}
		for _, f := range normaliseFlags(c.FlagsMentioned()) {
			if !strings.Contains(doc, f) {
				t.Errorf("docs/cli.md does not mention %s, which %s supplies a route "+
					"parameter with", f, c.String())
			}
		}
	}
	for _, f := range cli.GlobalFlags() {
		if !strings.Contains(doc, f) {
			t.Errorf("docs/cli.md does not document the global flag %s (spec 11.1)", f)
		}
	}
}

// TestCLIDocDocumentsEveryExitCode asserts every exit code of spec section
// 11.2 appears as a row of a table in docs/cli.md, so the page a script
// author reads cannot omit one.
func TestCLIDocDocumentsEveryExitCode(t *testing.T) {
	doc := readDoc(t, "cli.md")

	want := map[int]bool{
		apierr.ExitOK:              true,
		apierr.ExitOperationFailed: true,
		apierr.ExitLocalConfig:     true,
	}
	for _, code := range apierr.ExitCodes() {
		want[code] = true
	}
	codes := make([]int, 0, len(want))
	for c := range want {
		codes = append(codes, c)
	}
	sort.Ints(codes)

	for _, c := range codes {
		row := fmt.Sprintf("| `%d` |", c)
		if !strings.Contains(doc, row) {
			t.Errorf("docs/cli.md has no exit-code table row %q for exit %d "+
				"(spec 11.2)", row, c)
		}
	}
	if strings.Contains(doc, "| `1` |") {
		t.Error("docs/cli.md documents exit 1, which spec 11.2 leaves deliberately " +
			"unassigned")
	}
}
