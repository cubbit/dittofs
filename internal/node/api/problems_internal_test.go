package api

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// covered are the packages whose every declared error, or code, the problem
// table names: the directory, the name problems.go imports it as, and the
// prefix of what it declares. Identity's package joins this list, and its
// errors the table, once it declares them.
var covered = []struct{ dir, alias, prefix string }{
	{"../../../pkg/node/apiv1", "apiv1", "Code"},
	{"../../../pkg/metadata/entity", "entity", "Err"},
	{"../../../pkg/vfs/vfserr", "vfserr", "Err"},
	{"../../../pkg/metadata/errors", "merrs", "Err"},
	{"../../../pkg/metadata/kv", "kv", "Err"},
}

// Every error and code the covered packages declare, read from their source,
// has a row in problems.go, and a component's row answers the error's own
// name as its code: a new error with no row, or a row answering another name,
// fails here.
func TestProblemTableNamesEveryDeclaredError(t *testing.T) {
	named, codes := tableRows(t)
	for _, c := range covered {
		for _, name := range declared(t, c.dir, c.prefix) {
			sel := c.alias + "." + name
			if !named[sel] {
				t.Errorf("%s has no row in the problem table", sel)
				continue
			}
			if code, ok := codes[sel]; ok && code != snake(strings.TrimPrefix(name, "Err")) {
				t.Errorf("%s answers code %q, want %q", sel, code, snake(strings.TrimPrefix(name, "Err")))
			}
		}
	}
}

// Every value vfs.Errors lists, the typed errors of every kind among them,
// and every metadata store code, answers a named row; an error nothing names
// answers 500, reported unnamed so the caller counts it.
func TestProblemTableIsTotal(t *testing.T) {
	errs := vfs.Errors()
	for c := merrs.ErrorCode(1); !strings.HasPrefix(c.String(), "Unknown"); c++ {
		errs = append(errs, &merrs.StoreError{Code: c, Message: "m"})
	}
	if len(errs) < 40 {
		t.Fatalf("collected %d errors, want vfs's and the store's codes", len(errs))
	}
	for _, err := range errs {
		if p, ok := problemOf(fmt.Errorf("wrapped: %w", err)); !ok || p.Code == "" {
			t.Errorf("%v: answered %d %q, named %v; want a row", err, p.Status, p.Code, ok)
		}
	}
	if p, ok := problemOf(errors.New("no row names this")); ok || p.Status != http.StatusInternalServerError {
		t.Errorf("an unnamed error: answered %d, named %v; want 500 and unnamed", p.Status, ok)
	}
}

// A component's error reaches the body by its name, wrapped or typed: a
// share being removed answers 409 stale, a store code its name, and a typed
// vfs error the sentinel it is.
func TestProblemCodeIsTheComponentsName(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("put grant: %w", entity.ErrStale), http.StatusConflict, "stale"},
		{entity.ErrGenerationMismatch, http.StatusPreconditionFailed, "generation_mismatch"},
		{merrs.NewInvalidArgumentError("a netgroup needs a name"), http.StatusBadRequest, "invalid_argument"},
		{&vfs.AccessError{Cause: vfs.AccessReadOnly}, http.StatusForbidden, "access"},
		{&vfs.Delay{Cause: vfs.DelayFrozen}, http.StatusConflict, "delay"},
		{&fieldError{field: "spec.name", detail: "unknown field"}, http.StatusBadRequest, "invalid"},
		{&refusal{code: "password_change_required"}, http.StatusForbidden, "password_change_required"},
	} {
		p, _ := problemOf(tc.err)
		if p.Status != tc.status || string(p.Code) != tc.code {
			t.Errorf("%v: answered %d %q, want %d %q", tc.err, p.Status, p.Code, tc.status, tc.code)
		}
	}
	if p, _ := problemOf(&fieldError{field: "spec.name", detail: "unknown field"}); p.Field != "spec.name" {
		t.Errorf("a refused field: problem's field %q, want spec.name", p.Field)
	}
}

// declared returns the exported names starting with prefix that the
// package's non-test files declare as a variable or a constant.
func declared(t *testing.T, dir, prefix string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR && g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					if n.IsExported() && strings.HasPrefix(n.Name, prefix) {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s declares nothing starting %s", dir, prefix)
	}
	slices.Sort(names)
	return names
}

// tableRows reads problems.go and returns each pkg.Name its rows and keys
// name, and, for a component's row, the code it answers.
func tableRows(t *testing.T) (named map[string]bool, codes map[string]string) {
	t.Helper()
	src, err := os.ReadFile("problems.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "problems.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	named, codes = map[string]bool{}, map[string]string{}
	selector := func(e ast.Expr) string {
		if c, ok := e.(*ast.CallExpr); ok && len(c.Args) == 1 {
			e = c.Args[0]
		}
		if s, ok := e.(*ast.SelectorExpr); ok {
			if x, ok := s.X.(*ast.Ident); ok {
				return x.Name + "." + s.Sel.Name
			}
		}
		return ""
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if s := selector(n.Key); s != "" {
				named[s] = true
			}
		case *ast.CompositeLit:
			if len(n.Elts) != 2 {
				return true
			}
			s := selector(n.Elts[0])
			a, ok := n.Elts[1].(*ast.CompositeLit)
			if s == "" || !ok || len(a.Elts) != 3 {
				return true
			}
			named[s] = true
			if lit, ok := a.Elts[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				codes[s], _ = strconv.Unquote(lit.Value)
			}
		}
		return true
	})
	return named, codes
}

// snake is a Go name in snake case, an acronym kept as one word: NotFound is
// not_found, IOError io_error.
func snake(name string) string {
	r := []rune(name)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && unicode.IsUpper(c) && (unicode.IsLower(r[i-1]) || i+1 < len(r) && unicode.IsLower(r[i+1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}
