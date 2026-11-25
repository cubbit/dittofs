package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
)

// Impurities parses dir's non-test .go files, whatever their build tags, plus
// the planted ones, and reports each package-level var not named in vars and
// each function that is not a method, init included: a contract holds no
// state and no behaviour beyond methods over its own values (RFC 0 §1.2),
// which neither an import walk nor an export pin can see.
func Impurities(dir string, vars []string, planted ...string) ([]string, error) {
	paths, err := GoFiles(dir)
	if err != nil {
		return nil, err
	}

	paths = append(paths, planted...)

	var bad []string

	for _, p := range paths {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					bad = append(bad, "func "+d.Name.Name+" is not a method")
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}

				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if !slices.Contains(vars, n.Name) {
							bad = append(bad, "var "+n.Name+" is state")
						}
					}
				}
			}
		}
	}

	slices.Sort(bad)

	return bad, nil
}
