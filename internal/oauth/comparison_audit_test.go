package oauth

// Spec section 12.1: "A unit test enumerates every comparison site and fails
// on a `==` or a `bytes.Equal` against a secret-derived value."
//
// `internal/authz` has had this audit since Slice 2. This package needed one
// too and did not have it: compare.go's own comment promised it, a reviewer
// planted `equalConstantTime(presented, expected) -> presented == expected`,
// and the plant survived the whole suite. A comment claiming a test is worth
// less than no claim at all, because it stops the next reader looking.
//
// The audit parses this package's own source with go/ast rather than grepping
// for a magic comment: a comment is not the thing being checked, and a site
// that forgot the comment would be exactly the site that also forgot the
// constant-time compare.
//
// It is deliberately a copy of the idea rather than a shared helper. The
// `secretDerivedNames` list is the whole content of the audit and it is
// different here -- this package compares cookie handles, form tokens, PKCE
// verifiers and hidden form echoes, none of which `internal/authz` has ever
// heard of. A shared list would be a list that is right for neither.

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

// secretDerivedNames is the enumerated set of identifiers whose value is a
// secret, a caller-presented credential, a hash of one, or a value whose
// comparison an attacker can time.
//
// The last category is why the hidden ECHOES of section 9.4 are here. They are
// not secrets; they are compared one against another in a loop, and a loop
// that short-circuits on the first difference is a loop that can be measured.
var secretDerivedNames = map[string]bool{
	// Presented credentials, in every spelling this package has.
	"presented":         true,
	"presentedVerifier": true,
	"presentedMAC":      true,
	"presentedToken":    true,
	"presentedDigest":   true,
	"expected":          true,
	"expectedMAC":       true,

	// The two browser secrets.
	"handle":     true,
	"formToken":  true,
	"form_token": true,

	// PKCE.
	"verifier":        true,
	"challenge":       true,
	"storedChallenge": true,
	"codeChallenge":   true,

	// Enrollment and authorization codes, and their canonical forms.
	"code":      true,
	"codeHash":  true,
	"canonical": true,
	"value":     true,

	// Hashes and digests, fixed-length but still secret-derived: comparing
	// two hashes with `==` leaks the position of the first differing byte.
	"hash":         true,
	"storedHex":    true,
	"storedDigest": true,
	"stored":       true,
	"computed":     true,
	"d":            true,
}

// secretDerivedFuncs is the enumerated set of functions whose RESULT is
// secret-derived. A comparison against a call to one of these is a violation
// even when the result is never named.
var secretDerivedFuncs = map[string]bool{
	"sum":                   true,
	"hexOf":                 true,
	"handleHash":            true,
	"formTokenHash":         true,
	"formTokenFor":          true,
	"authorizationCodeHash": true,
	"EnrollmentCodeHash":    true,
	"newSecretValue":        true,
	"NewEnrollmentCode":     true,
	"Sum256":                true,
}

type finding struct {
	Pos  string
	Text string
}

func (f finding) String() string { return f.Pos + ": " + f.Text }

// TestNoSecretIsComparedWithEquals is the audit.
//
// Plant P2: rewrite equalConstantTime's body to `return presented == expected`
// and this fails naming compare.go and the parameter. Planted 2026-09-07 by
// the reviewer; killed here.
func TestNoSecretIsComparedWithEquals(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	var findings []finding
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			// Test files are excluded on purpose: a test comparing two
			// values a mint returned to it is not a credential check, and
			// including them would make the audit fail on its own
			// assertions.
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BinaryExpr:
				if node.Op != token.EQL && node.Op != token.NEQ {
					return true
				}
				// A comparison against the empty string literal is a
				// PRESENCE check, not a credential check: it reveals only
				// whether the caller sent the field, which the request
				// already reveals. Any other literal would be a hardcoded
				// secret and is still a violation.
				if isEmptyString(node.X) || isEmptyString(node.Y) {
					return true
				}
				if lhs, ok := secretDerived(node.X); ok {
					findings = append(findings, finding{
						Pos:  fset.Position(node.Pos()).String(),
						Text: fmt.Sprintf("%s is compared with %s", lhs, node.Op),
					})
				}
				if rhs, ok := secretDerived(node.Y); ok {
					findings = append(findings, finding{
						Pos:  fset.Position(node.Pos()).String(),
						Text: fmt.Sprintf("%s is compared with %s", rhs, node.Op),
					})
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Equal" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "bytes" {
					return true
				}
				findings = append(findings, finding{
					Pos:  fset.Position(node.Pos()).String(),
					Text: "bytes.Equal is not constant-time; use hmac.Equal",
				})
			}
			return true
		})
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].Pos < findings[j].Pos })
	for _, f := range findings {
		t.Errorf("spec section 12.1: %s", f)
	}
	if len(findings) > 0 {
		t.Log("Every comparison of a caller-supplied value against a stored one goes " +
			"through compare.go, which uses hmac.Equal over fixed-length digests.")
	}
}

// isEmptyString reports whether an expression is the literal "".
func isEmptyString(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// secretDerived reports whether an expression is one of the enumerated
// secret-derived names or a call to one of the enumerated functions.
//
// `len(x)` is the one documented carve-out and is NOT secret-derived even
// when x is: section 12.1 requires the compared LENGTHS to be constant, which
// makes a length an ordinary integer with no secret in it, and comparing one
// is how a decoded hash is validated before it is compared.
func secretDerived(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		if secretDerivedNames[e.Name] {
			return e.Name, true
		}
	case *ast.SelectorExpr:
		if secretDerivedNames[e.Sel.Name] {
			return e.Sel.Name, true
		}
	case *ast.CallExpr:
		switch fn := e.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "len" || fn.Name == "string" {
				return "", false
			}
			if secretDerivedFuncs[fn.Name] {
				return fn.Name + "()", true
			}
		case *ast.SelectorExpr:
			if secretDerivedFuncs[fn.Sel.Name] {
				return fn.Sel.Name + "()", true
			}
		}
	case *ast.IndexExpr:
		return secretDerived(e.X)
	}
	return "", false
}

// TestTheAuditSeesAPlant is the audit's own meta-test: it proves the audit
// would catch the plant that provoked it, by running the same walk over a
// synthetic file rather than by trusting that it would.
//
// Without this, an audit that silently matched nothing -- a typo in a name, a
// walk that skipped a node kind -- would pass for ever and prove nothing.
func TestTheAuditSeesAPlant(t *testing.T) {
	const planted = `package oauth

func equalConstantTime(presented, expected string) bool {
	return presented == expected
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "planted.go")
	if err := os.WriteFile(path, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || be.Op != token.EQL {
			return true
		}
		if _, ok := secretDerived(be.X); ok {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the audit does not recognise `presented == expected` as a violation, so " +
			"it would pass over the plant it exists to catch")
	}
}
