package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// nodeExports returns the violations of the node's surface in the package
// whose sources are in dir: each exported package-level declaration other
// than New, Node and Config, each exported method of Node other than Start
// and Stop, and each of those five missing. It reads every .go file but the
// tests, whatever its build tags, so a file behind a tag cannot hide one.
func nodeExports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{"New": false, "Node": false, "Config": false, "Node.Start": false, "Node.Stop": false}
	var bad []string
	note := func(name string) {
		if _, ok := want[name]; ok {
			want[name] = true
		} else {
			bad = append(bad, "extra export: "+name)
		}
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					note(d.Name.Name)
					continue
				}
				recv := d.Recv.List[0].Type
				if s, ok := recv.(*ast.StarExpr); ok {
					recv = s.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					note(id.Name + "." + d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							note(s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								note(n.Name)
							}
						}
					}
				}
			}
		}
	}
	for name, seen := range want {
		if !seen {
			bad = append(bad, "missing export: "+name)
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// The node exports New, Node and Config, and Node's Start and Stop, and
// nothing else; the planted tree, with one export too many, is caught.
func TestNodeExports(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{".", nil},
		{filepath.Join("testdata", "arch", "exports"), []string{"extra export: Extra"}},
	} {
		got, err := nodeExports(tc.dir)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.dir, got, tc.want)
		}
	}
}
