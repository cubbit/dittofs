package vfs

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// emitters is every file allowed to read a call's protocol label: the
// access event's emitter. The label is for observability alone.
var emitters = map[string]bool{"internal/vfs/authz/authz.go": true}

// guarded is every package directory, relative to the module root, whose
// rules must read a capability the caller states, never a protocol kind;
// each is walked with its subpackages.
var guarded = []string{"internal/vfs", "pkg/vfs", "pkg/openstate"}

// No rule of the filesystem, open state or pkg/vfs compares against a
// protocol kind: every non-test file is parsed, and each ==, != or case
// naming one fails the test with its file and line. The kinds are read from
// their declaration, so a new one is covered without editing this test.
// Only a kind written in place at the comparison is caught: one copied to a
// local first, passed to slices.Contains, used as a map key or compared by
// < or > slips past.
func TestNoRuleReadsAProtocol(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information to find the module root by")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..")
	kinds := protocolKinds(t, filepath.Join(root, "pkg", "metadata", "entity", "openstate.go"))
	if len(kinds) == 0 {
		t.Fatal("no protocol kind found in their declaration")
	}
	var sites []string
	for _, dir := range guarded {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "testdata":
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			found, err := protocolComparisons(rel, src, kinds)
			sites = append(sites, found...)
			if err == nil && !emitters[rel] {
				found, err = labelReads(rel, src)
				sites = append(sites, found...)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sites {
		t.Errorf("%s; act on a capability the caller states, and leave the label to the emitters (RFC 17 §3.0)", s)
	}
}

// Nothing in the module builds a Caps by switching on a protocol kind: an
// adapter states its one profile, so every non-test file under internal, pkg
// and cmd is parsed, and an if or switch over a kind whose branch names a
// capability or a profile fails the test with its file and line. The names
// are read from package caps, so a new one is covered.
func TestNoCapsIsBuiltFromAProtocol(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information to find the module root by")
	}
	root := filepath.Join(filepath.Dir(here), "..", "..")
	kinds := protocolKinds(t, filepath.Join(root, "pkg", "metadata", "entity", "openstate.go"))
	names := constNames(t, filepath.Join(root, "pkg", "vfs", "caps", "caps.go"))
	if len(kinds) == 0 || len(names) == 0 {
		t.Fatalf("read %d protocol kinds and %d capability names, want some of each", len(kinds), len(names))
	}
	var sites []string
	for _, dir := range []string{"internal", "pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor"):
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found, err := capsFromProtocol(filepath.ToSlash(rel), src, kinds, names)
			sites = append(sites, found...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sites {
		t.Errorf("%s; state the adapter's one profile instead", s)
	}
}

// The builder finder reports an if or a case over a kind whose branch names
// a capability, and not one whose branch names none, nor a capability set
// with no protocol in sight.
func TestTheCapsBuilderFinder(t *testing.T) {
	kinds := map[string]bool{"ProtocolSMB": true}
	names := map[string]bool{"SMBCaps": true, "HostCall": true}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"an if", "if p == entity.ProtocolSMB {\nc = vfs.SMBCaps\n}", []string{"x.go:4 builds a Caps from ProtocolSMB"}},
		{"an else", "if p != entity.ProtocolSMB {\n} else {\nc |= vfs.HostCall\n}", []string{"x.go:4 builds a Caps from ProtocolSMB"}},
		{"a case", "switch p {\ncase entity.ProtocolSMB:\nc = vfs.SMBCaps\n}", []string{"x.go:5 builds a Caps from ProtocolSMB"}},
		{"a branch naming no capability", "if p == entity.ProtocolSMB {\nn++\n}", nil},
		{"a profile stated outright", "c := vfs.SMBCaps", nil},
	} {
		src := "package x\n\nfunc f() {\n" + tc.body + "\n}\n"
		got, err := capsFromProtocol("x.go", []byte(src), kinds, names)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: found %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The label finder reports a call of ProtocolFrom, qualified or not, and
// not a call of WithProtocol, which sets the label.
func TestTheLabelFinder(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"a qualified read", "p, _ := vfs.ProtocolFrom(ctx)", []string{"x.go:4 reads the protocol label"}},
		{"an unqualified read", "_, ok := ProtocolFrom(ctx)", []string{"x.go:4 reads the protocol label"}},
		{"a label set", "ctx = vfs.WithProtocol(ctx, p)", nil},
	} {
		src := "package x\n\nfunc f() {\n" + tc.body + "\n}\n"
		got, err := labelReads("x.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: found %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The finder reports a comparison and a case naming a kind, and not a record
// built with one, nor a comparison of something else.
func TestTheProtocolFinder(t *testing.T) {
	kinds := map[string]bool{"ProtocolSMB": true, "ProtocolNLM": true}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"a comparison", "if c.Protocol == entity.ProtocolSMB {}", []string{"x.go:4 compares against ProtocolSMB"}},
		{"an inequality", "_ = entity.ProtocolNLM != p", []string{"x.go:4 compares against ProtocolNLM"}},
		{"a case", "switch p {\ncase entity.ProtocolNLM:\n}", []string{"x.go:5 compares against ProtocolNLM"}},
		{"a record built with a kind", "_ = entity.ClientRecord{Protocol: entity.ProtocolNLM}", nil},
		{"an assignment", "c.Protocol = entity.ProtocolSMB", nil},
		{"another comparison", "if c.Recovery == entity.RecoverReconnect {}", nil},
	} {
		src := "package x\n\nfunc f() {\n" + tc.body + "\n}\n"
		got, err := protocolComparisons("x.go", []byte(src), kinds)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: found %q, want %q", tc.name, got, tc.want)
		}
	}
}

// protocolKinds is the names of the constants of the const block, in the
// file at path, whose first spec has type Protocol.
func protocolKinds(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST || len(g.Specs) == 0 {
			continue
		}
		first, ok := g.Specs[0].(*ast.ValueSpec)
		if typ, isIdent := first.Type.(*ast.Ident); !ok || !isIdent || typ.Name != "Protocol" {
			continue
		}
		for _, s := range g.Specs {
			for _, n := range s.(*ast.ValueSpec).Names {
				kinds[n.Name] = true
			}
		}
	}
	return kinds
}

// protocolComparisons is each ==, != and case in src, a file named name,
// that names one of kinds, as "name:line compares against kind".
func protocolComparisons(name string, src []byte, kinds map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	kind := func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.Ident:
			if kinds[e.Name] {
				return e.Name
			}
		case *ast.SelectorExpr:
			if kinds[e.Sel.Name] {
				return e.Sel.Name
			}
		}
		return ""
	}
	var out []string
	report := func(at token.Pos, k string) {
		out = append(out, fmt.Sprintf("%s:%d compares against %s", name, fset.Position(at).Line, k))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			for _, e := range []ast.Expr{n.X, n.Y} {
				if k := kind(e); k != "" {
					report(n.Pos(), k)
				}
			}
		case *ast.CaseClause:
			for _, e := range n.List {
				if k := kind(e); k != "" {
					report(e.Pos(), k)
				}
			}
		}
		return true
	})
	return out, nil
}

// constNames is the names of every constant declared in the file at path.
func constNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					names[n.Name] = true
				}
			}
		}
	}
	return names
}

// capsFromProtocol is each if and each case in src, a file named name, that
// tests one of kinds and whose branch names one of names, as "name:line
// builds a Caps from kind".
func capsFromProtocol(name string, src []byte, kinds, names map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	named := func(e ast.Expr, set map[string]bool) string {
		switch e := e.(type) {
		case *ast.Ident:
			if set[e.Name] {
				return e.Name
			}
		case *ast.SelectorExpr:
			if set[e.Sel.Name] {
				return e.Sel.Name
			}
		}
		return ""
	}
	// tests is the kind cond compares against, or "".
	tests := func(cond ast.Node) (k string) {
		ast.Inspect(cond, func(n ast.Node) bool {
			if b, ok := n.(*ast.BinaryExpr); ok && (b.Op == token.EQL || b.Op == token.NEQ) {
				for _, e := range []ast.Expr{b.X, b.Y} {
					if x := named(e, kinds); x != "" && k == "" {
						k = x
					}
				}
			}
			return k == ""
		})
		return k
	}
	builds := func(nodes ...ast.Node) (hit bool) {
		for _, body := range nodes {
			if body == nil || hit {
				continue
			}
			ast.Inspect(body, func(n ast.Node) bool {
				if e, ok := n.(ast.Expr); ok && named(e, names) != "" {
					hit = true
				}
				return !hit
			})
		}
		return hit
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if k := tests(n.Cond); k != "" && builds(n.Body, n.Else) {
				out = append(out, fmt.Sprintf("%s:%d builds a Caps from %s", name, fset.Position(n.Pos()).Line, k))
			}
		case *ast.CaseClause:
			for _, e := range n.List {
				if k := named(e, kinds); k != "" {
					var body []ast.Node
					for _, st := range n.Body {
						body = append(body, st)
					}
					if builds(body...) {
						out = append(out, fmt.Sprintf("%s:%d builds a Caps from %s", name, fset.Position(e.Pos()).Line, k))
					}
					break
				}
			}
		}
		return true
	})
	return out, nil
}

// labelReads is each call of ProtocolFrom in src, a file named name, as
// "name:line reads the protocol label".
func labelReads(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var fn string
		switch e := c.Fun.(type) {
		case *ast.Ident:
			fn = e.Name
		case *ast.SelectorExpr:
			fn = e.Sel.Name
		}
		if fn == "ProtocolFrom" {
			out = append(out, fmt.Sprintf("%s:%d reads the protocol label", name, fset.Position(c.Pos()).Line))
		}
		return true
	})
	return out, nil
}
