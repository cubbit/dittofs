package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// GoFiles returns the path of each non-test .go file in dir, whatever its
// build tags.
func GoFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var paths []string

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}

	return paths, nil
}

// load parses dir's non-test .go files, whatever their build tags, so a file
// behind a tag cannot hide a declaration, plus the planted files, and
// type-checks them as the package path, reading each import through imp.
func load(imp types.Importer, path, dir string, planted []string) (*types.Package, []*ast.File, error) {
	paths, err := GoFiles(dir)
	if err != nil {
		return nil, nil, err
	}

	paths = append(paths, planted...)

	fset := token.NewFileSet()

	files := make([]*ast.File, 0, len(paths))
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, err
		}

		files = append(files, f)
	}

	conf := types.Config{Importer: imp}

	pkg, err := conf.Check(path, fset, files, nil)
	if err != nil {
		return nil, nil, err
	}

	return pkg, files, nil
}

// Exports type-checks the package path made of dir's non-test .go files,
// whatever their build tags, plus the planted files, reading its imports
// through imp, and prints each exported declaration on one line: a function
// or method with its signature and receiver, a constant with its value, a
// struct with its exported and embedded fields only, since an opaque struct
// may gain unexported fields.
func Exports(imp types.Importer, path, dir string, planted ...string) ([]string, error) {
	pkg, _, err := load(imp, path, dir, planted)
	if err != nil {
		return nil, err
	}

	qualify := func(p *types.Package) string {
		if p == pkg {
			return ""
		}

		return p.Name()
	}

	var got []string

	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}

		if c, ok := obj.(*types.Const); ok {
			got = append(got, types.ObjectString(c, qualify)+" = "+c.Val().ExactString())

			continue
		}

		tn, ok := obj.(*types.TypeName)
		if !ok {
			got = append(got, types.ObjectString(obj, qualify))

			continue
		}

		// An alias prints with its =, and a generic type with its parameters,
		// so neither can stand in for the defined type the pin names.
		head := "type " + name
		if named, ok := tn.Type().(*types.Named); ok && named.TypeParams().Len() > 0 {
			var params []string
			for tp := range named.TypeParams().TypeParams() {
				params = append(params, tp.Obj().Name()+" "+types.TypeString(tp.Constraint(), qualify))
			}

			head += "[" + strings.Join(params, ", ") + "]"
		}

		if tn.IsAlias() {
			head += " ="
		}

		got = append(got, head+" "+typeString(tn.Type().Underlying(), qualify))
		// An interface prints every method in typeString; any other type
		// prints the whole method set, promoted methods of an embedded type
		// included, since a method promoted that way is public too.
		if _, isIface := tn.Type().Underlying().(*types.Interface); !isIface {
			for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
				if sel.Obj().Exported() {
					got = append(got, types.ObjectString(sel.Obj(), qualify))
				}
			}
		}
	}

	return got, nil
}

// typeString prints t as types.TypeString does, but a struct with its
// exported and embedded fields only and no tags, nested anonymous structs included.
func typeString(t types.Type, qualify types.Qualifier) string {
	st, ok := t.(*types.Struct)
	if !ok {
		return types.TypeString(t, qualify)
	}

	var fields []string

	for f := range st.Fields() {
		if f.Exported() || f.Embedded() {
			fields = append(fields, f.Name()+" "+typeString(f.Type(), qualify))
		}
	}

	return "struct{" + strings.Join(fields, "; ") + "}"
}

// Violations compares got with want, whatever their order: each line of got
// not in want is an extra export, each line of want not in got a missing one.
func Violations(got, want []string) []string {
	var bad []string

	for _, g := range got {
		if !slices.Contains(want, g) {
			bad = append(bad, "extra export: "+g)
		}
	}

	for _, w := range want {
		if !slices.Contains(got, w) {
			bad = append(bad, "missing export: "+w)
		}
	}

	slices.Sort(bad)

	return bad
}

// WithHidden copies dir's non-test .go files and hidden, a planted file
// behind a build tag, into out, an empty directory, so the file reaches a
// check through the directory scan, as a real file would. It returns out.
func WithHidden(out, dir, hidden string) (string, error) {
	paths, err := GoFiles(dir)
	if err != nil {
		return "", err
	}

	for _, p := range append(paths, hidden) {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}

		if err := os.WriteFile(filepath.Join(out, filepath.Base(p)), b, 0o600); err != nil {
			return "", err
		}
	}

	return out, nil
}
