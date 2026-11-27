package archtest

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// Purity is what a package may declare at package level beyond types,
// constants and methods.
type Purity struct {
	Vars       []string // the variables it may hold, by name: its error values
	Funcs      bool     // whether it may declare functions other than methods
	Interfaces bool     // whether it may declare interface types
}

// Impurities type-checks the package path made of dir's non-test .go files,
// whatever their build tags, plus the planted ones, reading its imports
// through imp, and reports each package-level variable rule does not name,
// each function that is not a method unless rule allows them, init always,
// since it runs on import, and each interface type, an alias or a type defined over one
// included, unless rule allows them. These are the state and behaviour an
// import walk and an export pin cannot see. A symbol files for different
// platforms both declare is checked in each file, not refused as redeclared.
func Impurities(imp types.Importer, path, dir string, rule Purity, planted ...string) ([]string, error) {
	pkg, files, err := load(imp, path, dir, planted, true)
	if err != nil {
		return nil, err
	}

	var bad []string

	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && (!rule.Funcs || d.Name.Name == "init") {
					bad = append(bad, "func "+d.Name.Name+" is not a method")
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}

				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if !slices.Contains(rule.Vars, n.Name) {
							bad = append(bad, "var "+n.Name+" is state")
						}
					}
				}
			}
		}
	}

	if !rule.Interfaces {
		for _, name := range pkg.Scope().Names() {
			if tn, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok && types.IsInterface(tn.Type()) {
				bad = append(bad, "type "+name+" is an interface")
			}
		}
	}

	slices.Sort(bad)

	return bad, nil
}

// ErrorTypes type-checks the package as Impurities does and names each
// package-level type that is an error: one whose method set, or its pointer's,
// has Error() string, a method promoted from an embedded field, one on a
// generic type and an alias's included. Such a type is an error of the
// package's own, which a constant or a return site can carry with no variable
// declared.
func ErrorTypes(imp types.Importer, path, dir string, planted ...string) ([]string, error) {
	pkg, _, err := load(imp, path, dir, planted, true)
	if err != nil {
		return nil, err
	}

	var bad []string

	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}

		obj, _, _ := types.LookupFieldOrMethod(tn.Type(), true, pkg, "Error")
		if fn, ok := obj.(*types.Func); ok && isErrorMethod(fn.Type().(*types.Signature)) {
			bad = append(bad, "type "+name+" is an error")
		}
	}

	return bad, nil
}

// isErrorMethod reports whether sig is the error interface's Error() string.
func isErrorMethod(sig *types.Signature) bool {
	return sig.Params().Len() == 0 && sig.Results().Len() == 1 &&
		types.Identical(sig.Results().At(0).Type(), types.Typ[types.String])
}
