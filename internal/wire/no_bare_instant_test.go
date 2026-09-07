package wire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// W-1's general form. A struct field that is a time.Time and carries a json
// tag is marshalled by Go's default encoder, which renders RFC3339Nano and
// therefore STRIPS trailing zeros -- so its width depends on its value, and
// section 4.4's millisecond precision holds nine times in ten.
//
// This has now happened twice: accounts.StateChange.At (V-1) and the three
// health blocks (W-1), both in the package whose objects are served without
// passing through internal/api's DTO layer. Twice is a pattern, and the fix
// for a pattern is a rule the compiler or a test can hold rather than a
// habit. Every instant on the wire is a string rendered by this package.
//
// Plant: type any such field back to time.Time and this fails naming the file
// and the field. Planted 2026-09-07.
func TestNoJSONFieldIsABareTime(t *testing.T) {
	root := repoRoot(t)
	checked := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Dot-directories are not this repository's source: .git holds
			// objects, and .claude holds review worktrees -- whole second
			// copies of the tree, whose findings would be this tree's
			// findings reported twice and at the wrong paths.
			if name := info.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				if f.Tag == nil || !strings.Contains(f.Tag.Value, "json:") {
					continue
				}
				checked++
				if !isTime(f.Type) {
					continue
				}
				name := "«embedded»"
				if len(f.Names) > 0 {
					name = f.Names[0].Name
				}
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: field %s is a time.Time with a json tag. Go marshals "+
					"one as RFC3339Nano, which strips trailing zeros, so its width "+
					"depends on its value and section 4.4's millisecond precision "+
					"holds for nine values in ten. Render it with wire.Instant / "+
					"wire.InstantPtr and type the field *string, so this cannot come "+
					"back by accident", rel, fset.Position(f.Pos()).Line, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if checked < 100 {
		t.Fatalf("only %d json-tagged fields were examined; the walk has stopped "+
			"finding this repository's structs", checked)
	}
}

// isTime reports whether an expression is a time.Time in any wrapper a struct
// field can put one in: a pointer, a slice or array, or a map key or value.
//
// The unwrapping is the whole point. A guard that recognised only the bare
// form would have passed W-1 -- every field there was a *time.Time -- while
// looking like protection, which is worse than no guard at all. The same
// reasoning covers the containers: nothing in this tree serves a
// []time.Time today, and a guard exists for the case nobody thought of.
func isTime(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return isTime(t.X)
	case *ast.ArrayType:
		return isTime(t.Elt)
	case *ast.MapType:
		return isTime(t.Key) || isTime(t.Value)
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "time" && t.Sel.Name == "Time"
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Join(filepath.Dir(self), "..", "..")
}
