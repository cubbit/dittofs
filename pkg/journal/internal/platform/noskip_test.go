package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// TestNoSkip fails on a skip in any test of this package or of the storage
// contract: a call of Skip, Skipf or SkipNow on any receiver, and any use of
// testing.Short. A test skipped under -short passes while it protects
// nothing. It reads every test file whatever its build tags, so the tests of
// every platform are checked on any platform, and finds the testing package
// by import path, so a renamed import is caught; a dot import, whose names it
// could not tell apart, is refused. A skip inside a helper of another
// package, such as the profiler's Watch, is not seen. The two rows the
// decision marker on skipByName names are exempt, and they alone.
func TestNoSkip(t *testing.T) {
	prof.Watch(t, prof.Options{})

	skips := map[string]bool{
		"Skip":    true,
		"Skipf":   true,
		"SkipNow": true,
	}

	// decision: two rows over the host's own machine may skip by name —
	// TestCheckRealExtOptions, whose directory and kernel the runner has, not
	// the test, and TestCheckDirOSRealVolume, whose volume's write-cache
	// answer no test arranges. Their subject being the machine's property, a
	// run without it must say what the machine answered instead of passing
	// silently; each skip names the answer, so it is a visible report, never
	// counted as a pass. Every other skip stays an error. Withdraw the
	// exemption when a test can arrange its own ext filesystem or volume.
	skipByName := map[string]bool{
		"check_linux_test.go":      true,
		"checkdir_windows_test.go": true,
	}

	fset := token.NewFileSet()
	checked := 0

	for _, dir := range []string{".", filepath.Join("..", "storage")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}

			path := filepath.Join(dir, e.Name())
			maySkip := skipByName[e.Name()]

			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}

			checked++

			names := map[string]bool{}

			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}

				if p != "testing" {
					continue
				}

				local := "testing"

				if imp.Name != nil {
					local = imp.Name.Name
				}

				if local == "." {
					t.Errorf(
						"%s: \"testing\" imported as ., whose Short this check cannot see",
						fset.Position(imp.Pos()),
					)
				}

				names[local] = true
			}

			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				if x, ok := sel.X.(*ast.Ident); ok && names[x.Name] && sel.Sel.Name == "Short" {
					t.Errorf(
						"%s: testing.Short: no test here may depend on -short",
						fset.Position(sel.Pos()),
					)
				}

				if skips[sel.Sel.Name] && !maySkip {
					t.Errorf(
						"%s: .%s: no test here may skip",
						fset.Position(sel.Pos()),
						sel.Sel.Name,
					)
				}

				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no test file was read")
	}
}
