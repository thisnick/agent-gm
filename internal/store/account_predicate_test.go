package store_test

// Spec section 13.2, last bullet: every query carries its account.
//
// This test enumerates the store's own query strings out of its source and
// fails on one that filters conversations, messages, participants, contacts,
// operations or backfill_state without either an `account_id` predicate or an
// explicit all-accounts marker.
//
// The marker is a real thing in the source -- the SQL comment
// `-- all-accounts:` -- and not an allowlist of function names, so it is
// greppable, it travels with the query it excuses, and it forces whoever adds
// an unscoped query to write down why in the place a reviewer will read.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The tables spec section 13.2 names.
var accountScopedTables = []string{
	"conversations", "messages", "participants", "contacts", "operations", "backfill_state",
}

const allAccountsMarker = "-- all-accounts:"

// tableRef matches a table in a position that reads or writes rows of it. The
// negative lookahead cannot be expressed in Go's regexp, so `messages_fts`
// and `media_cache_entries` are excluded by requiring a non-word character or
// end of string after the name.
var tableRef = regexp.MustCompile(
	`(?is)\b(?:from|join|update|into)\s+(` + strings.Join(accountScopedTables, "|") + `)(\W|$)`)

func TestEveryQueryCarriesItsAccountOrSaysWhyNot(t *testing.T) {
	// The files are listed and parsed one at a time rather than with
	// go/parser.ParseDir, which is deprecated -- it does not consider build
	// tags when associating files with packages -- and would fail the lint.
	fset := token.NewFileSet()
	dir, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading internal/store: %v", err)
	}
	files := map[string]*ast.File{}
	for _, entry := range dir {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// migrations.go is schema, not the query layer: its statements are
		// DDL plus one-time index backfills that run before any listener
		// binds and are process-wide by definition, and a migration is never
		// edited after it ships (spec section 4.3), so an account predicate
		// could not be added to a shipped one anyway. It carries the
		// all-accounts marker regardless, so the exclusion is belt and
		// braces rather than the only thing keeping it quiet.
		if name == "migrations.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no non-test source file was parsed, so this test would pass vacuously")
	}

	var literals, marked, scoped int
	for name, file := range files {
		{
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					// A raw string with an escape Go cannot unquote cannot be
					// SQL this package writes; take the literal bytes.
					text = lit.Value
				}
				m := tableRef.FindStringSubmatch(text)
				if m == nil {
					return true
				}
				literals++
				pos := fset.Position(lit.Pos())
				hasAccount := strings.Contains(text, "account_id")
				hasMarker := strings.Contains(text, allAccountsMarker)
				switch {
				case hasMarker:
					marked++
				case hasAccount:
					scoped++
				default:
					t.Errorf("%s:%d: a query over %q carries neither an account_id "+
						"predicate nor an %q marker:\n%s",
						name, pos.Line, m[1], allAccountsMarker, strings.TrimSpace(text))
				}
				return true
			})
		}
	}

	if literals == 0 {
		t.Fatal("found no queries at all, so this test proves nothing")
	}
	if marked == 0 {
		t.Fatalf("found no %q markers; either the marker was renamed or every "+
			"all-accounts query lost its explanation", allAccountsMarker)
	}
	if scoped == 0 {
		t.Fatal("found no account_id-scoped queries, which cannot be right")
	}
	t.Logf("checked %d queries: %d scoped by account_id, %d explicitly all-accounts",
		literals, scoped, marked)
}

// The marker has to be greppable from a shell, not merely present in an AST.
func TestTheAllAccountsMarkerIsAPlainSQLComment(t *testing.T) {
	if !strings.HasPrefix(allAccountsMarker, "--") {
		t.Fatalf("the marker %q is not a SQL comment, so it would change the "+
			"meaning of the query it appears in", allAccountsMarker)
	}
}
