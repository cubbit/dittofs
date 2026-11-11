package lockorder_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var domainNames = []string{"file append", "capacity", "file index", "file map", "segment append", "storage guard"}

// lockKind classifies a type expression: whether it is (a pointer to, an
// array, slice or map of) a plain or a ranked lock. ok is false when the
// expression mentions a lock type in a shape the scan does not understand.
func lockKind(e ast.Expr) (kind string, ok bool) {
	switch t := e.(type) {
	case *ast.ParenExpr:
		return lockKind(t.X)
	case *ast.StarExpr:
		return lockKind(t.X)
	case *ast.ArrayType:
		return lockKind(t.Elt)
	case *ast.MapType:
		kk, kok := lockKind(t.Key)
		vk, vok := lockKind(t.Value)
		if kk != "" || !kok {
			return "", false
		}
		return vk, vok
	case *ast.SelectorExpr:
		if x, isIdent := t.X.(*ast.Ident); isIdent && (t.Sel.Name == "Mutex" || t.Sel.Name == "RWMutex") {
			switch x.Name {
			case "sync":
				return "sync", true
			case "lockorder":
				return "lockorder", true
			}
		}
		return "", true
	}
	// Any other shape (func, chan, struct, generic, interface...) must not
	// hide a lock type.
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if s, isSel := n.(*ast.SelectorExpr); isSel {
			if k, _ := lockKind(s); k != "" {
				found = true
			}
		}
		return !found
	})
	return "", !found
}

// decision: the scan reads the field's own comments first and falls back to
// the enclosing type's doc comment only when the field has no decision: of
// its own, so a neighbouring field's marker never covers it.
func markerText(own, enclosing []*ast.CommentGroup) string {
	join := func(gs []*ast.CommentGroup) string {
		var b strings.Builder
		for _, g := range gs {
			if g != nil {
				b.WriteString(g.Text())
			}
		}
		return b.String()
	}
	if s := join(own); strings.Contains(s, "decision:") {
		return s
	}
	return join(enclosing)
}

var leafWord = regexp.MustCompile(`\bleaf\b`)

// ponytail: this checks locks only. Lock-free state (atomics) is outside the
// six domains and is not checked. Upgrade to a go/types check if a domain
// starts being named on atomics.
func TestEveryLockNamesItsDomain(t *testing.T) {
	dirs := []string{"../index", "../segment", "../capacity", "../datapath", "../durability"}
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0

	check := func(pos token.Pos, name, kind, text string) {
		scanned++
		bad := func(want string) {
			offenders = append(offenders, fmt.Sprintf("%s %s: got a %s lock with marker %q, want %s", fset.Position(pos), name, kind, strings.TrimSpace(text), want))
		}
		if !strings.Contains(text, "decision:") {
			bad("a decision: marker naming a domain or leaf")
			return
		}
		leaf := leafWord.MatchString(text)
		named := false
		for _, d := range domainNames {
			named = named || strings.Contains(text, d)
		}
		switch {
		case kind == "sync" && !leaf && named:
			bad("a lockorder lock (a ranked domain needs the ranked type) or the word leaf")
		case kind == "sync" && !leaf:
			bad("the word leaf")
		case kind == "lockorder" && !named:
			bad("one of the six domain names")
		case kind == "lockorder" && leaf:
			bad("a domain, not leaf")
		}
	}
	unsupported := func(pos token.Pos, name string) {
		offenders = append(offenders, fmt.Sprintf("%s %s: lock type in a declaration form the scan does not understand", fset.Position(pos), name))
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing %s: %v", e.Name(), err)
			}
			// Enclosing doc comments of every struct type: a named type's, or
			// a var's whose value is an anonymous struct.
			docs := map[*ast.StructType][]*ast.CommentGroup{}
			for _, decl := range f.Decls {
				gd, isGen := decl.(*ast.GenDecl)
				if !isGen {
					continue
				}
				for _, spec := range gd.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						if st, isStruct := sp.Type.(*ast.StructType); isStruct {
							docs[st] = []*ast.CommentGroup{sp.Doc, gd.Doc}
						}
					case *ast.ValueSpec: // package-level var
						varDocs := []*ast.CommentGroup{sp.Doc, sp.Comment, gd.Doc}
						for _, n := range nodes(sp.Type, sp.Values) {
							ast.Inspect(n, func(n ast.Node) bool {
								if st, isStruct := n.(*ast.StructType); isStruct {
									docs[st] = varDocs
								}
								return true
							})
						}
						if gd.Tok != token.VAR || sp.Type == nil {
							continue
						}
						kind, ok := lockKind(sp.Type)
						if !ok {
							unsupported(sp.Pos(), sp.Names[0].Name)
						} else if kind != "" {
							check(sp.Pos(), sp.Names[0].Name, kind, markerText([]*ast.CommentGroup{sp.Doc, sp.Comment}, []*ast.CommentGroup{gd.Doc}))
						}
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				st, isStruct := n.(*ast.StructType)
				if !isStruct {
					return true
				}
				for _, fld := range st.Fields.List {
					kind, ok := lockKind(fld.Type)
					name := "<embedded>"
					if len(fld.Names) > 0 {
						name = fld.Names[0].Name
					}
					if !ok {
						unsupported(fld.Pos(), name)
					} else if kind != "" {
						check(fld.Pos(), name, kind, markerText([]*ast.CommentGroup{fld.Doc, fld.Comment}, docs[st]))
					}
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no locks in index, segment, capacity, datapath, durability; the scan is not looking at the code")
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Error(o)
	}
}

func nodes(t ast.Expr, es []ast.Expr) []ast.Node {
	var out []ast.Node
	if t != nil {
		out = append(out, t)
	}
	for _, e := range es {
		out = append(out, e)
	}
	return out
}
