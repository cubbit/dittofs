package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
)

// Errors lists every refusal core declares, once each: a sentinel added to
// core but not to Errors would reach a table no totality test checks it
// against. ErrParked is a handler's return, never a refusal a table maps.
func TestErrorsListsEverySentinelOnce(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sentinels int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if strings.HasPrefix(n.Name, "Err") && n.Name != "ErrParked" {
							sentinels++
						}
					}
				}
			}
		}
	}
	errs := core.Errors()
	if len(errs) != sentinels {
		t.Errorf("Errors lists %d errors; core declares %d refusals", len(errs), sentinels)
	}
	for i, e := range errs {
		if e == nil || e == core.ErrParked || slices.Contains(errs[:i], e) {
			t.Errorf("Errors()[%d] = %v: nil, ErrParked or listed twice", i, e)
		}
	}
}
