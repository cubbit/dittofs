package core_test

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
)

var update = flag.Bool("update", false, "rewrite testdata/surface from the source")

// frozen is every package whose exported surface RFC 20 freezes: core's §4.7,
// the contract, the settings, the budgets and the two constructors. Each is
// compared, declaration by declaration, with its golden file, which is
// changed only with the RFC.
var frozen = map[string]string{
	"core":      ".",
	"adapter":   "../../../pkg/adapter",
	"config":    "../../../pkg/adapter/config",
	"resources": "../../../pkg/resources",
	"nfs":       "../../../pkg/adapter/nfs",
	"smb":       "../../../pkg/adapter/smb",
}

// surface prints, sorted, each exported declaration of dir's non-test files without
// its comments or bodies: types with their exported fields and every
// interface method, functions and methods with their signatures, constants
// and variables with their values.
func surface(t *testing.T, dir string) []byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var decls []string
	fset := token.NewFileSet()
	emit := func(node any) {
		var b strings.Builder
		cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
		if err := cfg.Fprint(&b, fset, node); err != nil {
			t.Fatal(err)
		}
		decls = append(decls, b.String()+"\n\n")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() && (d.Recv == nil || receiverExported(d.Recv)) {
					d.Body = nil
					emit(d)
				}
			case *ast.GenDecl:
				if d.Tok == token.IMPORT {
					continue
				}
				var keep []ast.Spec
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							dropUnexportedFields(s.Type)
							keep = append(keep, s)
						}
					case *ast.ValueSpec:
						if exportedName(s.Names) {
							keep = append(keep, s)
						}
					}
				}
				if len(keep) > 0 {
					d.Specs = keep
					emit(d)
				}
			}
		}
	}
	// Sorted, so the golden does not depend on the order of files or of
	// declarations within one.
	slices.Sort(decls)
	return []byte(strings.Join(decls, ""))
}

func receiverExported(recv *ast.FieldList) bool {
	typ := recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	return typ.(*ast.Ident).IsExported()
}

func exportedName(names []*ast.Ident) bool {
	for _, n := range names {
		if n.IsExported() {
			return true
		}
	}
	return false
}

// dropUnexportedFields removes a struct's unexported fields, in nested
// unnamed structs too; an embedded field is kept as its type name says.
func dropUnexportedFields(expr ast.Expr) {
	st, ok := expr.(*ast.StructType)
	if !ok {
		return
	}
	var keep []*ast.Field
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 || exportedName(f.Names) {
			dropUnexportedFields(f.Type)
			keep = append(keep, f)
		}
	}
	st.Fields.List = keep
}

// Each frozen package's exported declarations are exactly its golden file:
// a changed signature, field or constant fails here, as does an export the
// RFC does not name. Change one with the RFC, then rerun with -update.
func TestSurfaceIsFrozen(t *testing.T) {
	for name, dir := range frozen {
		got := surface(t, dir)
		golden := filepath.Join("testdata", "surface", name+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s's exported surface differs from %s; change it with RFC 20 first, then rerun with -update.\n%s", name, golden, diff(want, got))
		}
	}
}

// diff is a unified diff of want against got, line by line.
func diff(want, got []byte) string {
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(want)),
		B:        difflib.SplitLines(string(got)),
		FromFile: "want",
		ToFile:   "got",
		Context:  2,
	})
	if err != nil {
		return err.Error()
	}
	return d
}
