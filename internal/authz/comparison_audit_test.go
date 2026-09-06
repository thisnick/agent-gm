package authz_test

// Spec section 16 Slice 2 test 23, and spec section 12.1's closing sentence:
// "A unit test enumerates every comparison site and fails on a `==` or a
// `bytes.Equal` against a secret-derived value."
//
// This is done by parsing this package's own source with go/ast, not by
// grepping for a magic comment -- a comment is not the thing being checked,
// and a site that forgot the comment would be exactly the site that also
// forgot the constant-time compare.
//
// The audit is only as good as its notion of "secret-derived", so that notion
// is an explicit, documented list below rather than a heuristic. Adding a new
// name for a secret, a presented credential or a hash of one means adding it
// to secretDerivedNames; that is the deliberate cost of introducing a new
// spelling for a secret, and it is cheaper than the leak.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// secretDerivedNames is the enumerated set of identifiers and struct fields
// whose value is a secret, a caller-presented credential, or a hash of one.
//
// A comparison with `==`, `!=` or bytes.Equal touching any of these is a
// violation of spec section 12.1. Everything here is either a parameter name,
// a local, or a field on a store row that holds a hash.
var secretDerivedNames = map[string]bool{
	// The admin secret, in every spelling it has.
	"secret":           true,
	"adminSecret":      true,
	"AdminSecret":      true,
	"presentedSecret":  true,
	"configured":       true,
	"configuredSecret": true,

	// A presented credential of any kind.
	"presented":             true,
	"presentedValue":        true,
	"presentedToken":        true,
	"presentedAccessToken":  true,
	"presentedRefreshToken": true,

	// Minted token values, which are secrets until they leave the process.
	"tokenValue":   true,
	"accessValue":  true,
	"refreshValue": true,
	"AccessToken":  true,
	"RefreshToken": true,

	// Hashes and digests. Fixed-length, but still secret-derived: comparing
	// two hashes with `==` leaks the position of the first differing byte,
	// which is the whole reason section 12.1 names hashes explicitly.
	"hash":            true,
	"tokenHash":       true,
	"TokenHash":       true,
	"secretHash":      true,
	"adminSecretHash": true,
	"storedHash":      true,
	"storedHex":       true,
	"storedDigest":    true,
	"stored":          true,
	"computed":        true,
	"presentedHash":   true,
	"presentedDigest": true,

	// The secret-generation marker of section 12.1's rotation rule.
	"generation":       true,
	"secretGeneration": true,
	"SecretGeneration": true,
}

// secretDerivedFuncs is the enumerated set of functions whose RESULT is
// secret-derived. A comparison against a call to one of these is a violation
// even when the result is never named.
var secretDerivedFuncs = map[string]bool{
	"hashSecret":       true,
	"secretGeneration": true,
	"tokenDigest":      true,
	"tokenHash":        true,
	"hexDigest":        true,
	"digest":           true,
	"newTokenValue":    true,
	// crypto/sha256's one-shot helpers, in case a site ever hashes inline.
	"Sum256": true,
	"Sum224": true,
}

// The one documented carve-out: `len(x)` is NOT secret-derived, even when x
// is. Spec section 12.1 requires the compared LENGTHS to be constant, which
// makes a length an ordinary integer with no secret in it; comparing one is
// how a decoded hash is validated before it is compared. Every other call is
// secret-derived only if its function is in secretDerivedFuncs, so nothing
// escapes silently.
var transparentConversions = map[string]bool{"string": true}

type finding struct {
	Pos  string
	Kind string
	Text string
}

func (f finding) String() string { return fmt.Sprintf("%s: %s: %s", f.Pos, f.Kind, f.Text) }

// auditComparisons parses every non-test .go file in dir and returns every
// comparison site that touches a secret-derived value with `==`, `!=` or
// bytes.Equal.
//
// Test files are excluded on purpose: a test comparing two values a mint
// returned to it is not a credential check, and including them would make the
// audit fail on its own assertions.
func auditComparisons(dir string) ([]finding, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []finding
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.EQL && x.Op != token.NEQ {
					return true
				}
				if isSecretDerived(x.X) || isSecretDerived(x.Y) {
					out = append(out, finding{
						Pos:  fset.Position(x.Pos()).String(),
						Kind: "== or != against a secret-derived value",
						Text: exprText(x.X) + " " + x.Op.String() + " " + exprText(x.Y),
					})
				}
			case *ast.CallExpr:
				if !isBytesEqual(x.Fun) {
					return true
				}
				for _, a := range x.Args {
					if isSecretDerived(a) {
						out = append(out, finding{
							Pos:  fset.Position(x.Pos()).String(),
							Kind: "bytes.Equal against a secret-derived value",
							Text: "bytes.Equal(" + exprText(a) + ", ...)",
						})
						break
					}
				}
			}
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	return out, nil
}

func isBytesEqual(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Equal" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "bytes"
}

// isSecretDerived walks one operand. It is deliberately structural: an
// expression is secret-derived if it names, selects, indexes, slices or
// converts something in the enumerated sets.
func isSecretDerived(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return secretDerivedNames[x.Name]
	case *ast.SelectorExpr:
		// row.TokenHash, s.adminSecretHash: the field name settles it, and so
		// does a secret-derived receiver.
		return secretDerivedNames[x.Sel.Name] || isSecretDerived(x.X)
	case *ast.IndexExpr:
		return isSecretDerived(x.X)
	case *ast.SliceExpr:
		return isSecretDerived(x.X)
	case *ast.ParenExpr:
		return isSecretDerived(x.X)
	case *ast.StarExpr:
		return isSecretDerived(x.X)
	case *ast.UnaryExpr:
		return isSecretDerived(x.X)
	case *ast.CallExpr:
		// A conversion is transparent: string(d[:]) is as secret as d.
		if id, ok := x.Fun.(*ast.Ident); ok && transparentConversions[id.Name] {
			for _, a := range x.Args {
				if isSecretDerived(a) {
					return true
				}
			}
			return false
		}
		if _, ok := x.Fun.(*ast.ArrayType); ok { // []byte(x)
			for _, a := range x.Args {
				if isSecretDerived(a) {
					return true
				}
			}
			return false
		}
		switch fn := x.Fun.(type) {
		case *ast.Ident:
			return secretDerivedFuncs[fn.Name]
		case *ast.SelectorExpr:
			return secretDerivedFuncs[fn.Sel.Name]
		}
		return false
	}
	return false
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprText(x.Fun) + "(...)"
	case *ast.BasicLit:
		return x.Value
	case *ast.SliceExpr:
		return exprText(x.X) + "[:]"
	case *ast.IndexExpr:
		return exprText(x.X) + "[...]"
	case *ast.ParenExpr:
		return "(" + exprText(x.X) + ")"
	case *ast.UnaryExpr:
		return x.Op.String() + exprText(x.X)
	case *ast.StarExpr:
		return "*" + exprText(x.X)
	}
	return "?"
}

// TestSlice2Test23ComparisonSitesAreConstantTime is spec section 16 Slice 2
// test 23: no comparison site in internal/authz compares a secret-derived
// value with `==`, `!=` or bytes.Equal.
func TestSlice2Test23ComparisonSitesAreConstantTime(t *testing.T) {
	findings, err := auditComparisons(".")
	if err != nil {
		t.Fatalf("auditing internal/authz: %v", err)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			t.Errorf("secret comparison is not constant-time (spec section 12.1): %s", f)
		}
		t.Fatalf("%d comparison site(s) must use crypto/subtle.ConstantTimeCompare or hmac.Equal over fixed-length hashes", len(findings))
	}
}

// TestSlice2Test23AuditDetectsPlantedComparison is the negative half: the
// audit itself is exercised against a fixture that DOES violate section 12.1,
// so a green run of the test above means the detector works rather than
// meaning the detector is asleep.
func TestSlice2Test23AuditDetectsPlantedComparison(t *testing.T) {
	findings, err := auditComparisons(filepath.Join("testdata", "plantedcompare"))
	if err != nil {
		t.Fatalf("auditing the planted fixture: %v", err)
	}
	// Four plants: a string ==, a hash !=, a bytes.Equal, and one hidden
	// behind a string conversion.
	if len(findings) != 4 {
		for _, f := range findings {
			t.Logf("found: %s", f)
		}
		t.Fatalf("the audit found %d planted violations, want 4 -- the detector is not catching what it must", len(findings))
	}
	var kinds int
	for _, f := range findings {
		if strings.HasPrefix(f.Kind, "bytes.Equal") {
			kinds++
		}
	}
	if kinds != 1 {
		t.Fatalf("want exactly one bytes.Equal finding, got %d", kinds)
	}
	// And the documented carve-out must NOT fire: a length check is not a
	// value comparison.
	for _, f := range findings {
		if strings.Contains(f.Text, "len(") {
			t.Fatalf("the audit flagged a length check, which section 12.1 explicitly permits: %s", f)
		}
	}
}

// TestComparisonAuditNamesAreDocumented keeps the enumerated sets honest: an
// empty or accidentally-cleared set would make the audit above pass over any
// code at all.
func TestComparisonAuditNamesAreDocumented(t *testing.T) {
	if len(secretDerivedNames) < 20 {
		t.Fatalf("secretDerivedNames has shrunk to %d entries; a new spelling for a secret can now escape the audit", len(secretDerivedNames))
	}
	if len(secretDerivedFuncs) < 5 {
		t.Fatalf("secretDerivedFuncs has shrunk to %d entries", len(secretDerivedFuncs))
	}
	for _, must := range []string{"presentedSecret", "presentedRefreshToken", "presentedAccessToken", "TokenHash", "adminSecretHash"} {
		if !secretDerivedNames[must] {
			t.Errorf("%q must be in secretDerivedNames: it names a secret-derived value in this package", must)
		}
	}
}
