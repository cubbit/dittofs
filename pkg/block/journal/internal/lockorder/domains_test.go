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

// decision: the scan reads the field's own comments first. It falls back to
// the enclosing type's doc comment only when the field has none carrying a
// decision: and the type declares exactly one lock, so a marker written for
// one field never covers another.
func markerText(own, enclosing []*ast.CommentGroup, locksInEnclosing int) string {
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
	if locksInEnclosing != 1 {
		return join(own)
	}
	return join(own) + join(enclosing)
}

var (
	leafPhrase = regexp.MustCompile(`\bis a leaf\b`)
	// unrankedPhrase is how a plain lock that is not a leaf says so: held
	// across other locks or I/O, it sits outside the six domains, which the
	// recorder cannot see, so the marker records the order it relies on.
	unrankedPhrase = regexp.MustCompile(`\bis unranked\b`)
	negation       = regexp.MustCompile(`\b(not|no|never)\b|n't\b`)
	clauseEnd      = regexp.MustCompile(`[.;:!?]`)
)

// hasPhrase reports whether the marker says, un-negated, the phrase re
// matches (as in "mu is a leaf mutex"). A negation earlier in the same
// clause, as in "is not a leaf" or "never is a leaf", cancels the phrase.
func hasPhrase(text string, re *regexp.Regexp) bool {
	text = strings.Join(strings.Fields(text), " ")
	for _, loc := range re.FindAllStringIndex(text, -1) {
		before := text[:loc[0]]
		if i := clauseEnd.FindAllStringIndex(before, -1); len(i) > 0 {
			before = before[i[len(i)-1][1]:]
		}
		if !negation.MatchString(before) {
			return true
		}
	}
	return false
}

// classify returns what is wrong with a lock of the given kind ("sync" or
// "lockorder") whose marker text is text, or "" when it is right. The marker
// is the text decision: plus either one of the six domain names, for a
// lockorder lock, or the phrase "is a leaf" or "is unranked", for a sync lock.
func classify(kind, text string) string {
	if !strings.Contains(text, "decision:") {
		return "a decision: marker naming a domain or leaf"
	}
	leaf := hasPhrase(text, leafPhrase)
	unranked := hasPhrase(text, unrankedPhrase)
	named := false
	for _, d := range domainNames {
		named = named || strings.Contains(text, d)
	}
	switch {
	case kind == "sync" && (leaf || unranked):
		return ""
	case kind == "sync" && named:
		return "a lockorder lock (a ranked domain needs the ranked type) or the phrase \"is a leaf\" or \"is unranked\""
	case kind == "sync":
		return "the phrase \"is a leaf\" or \"is unranked\""
	case leaf:
		return "a domain, not leaf"
	case !named:
		return "one of the six domain names"
	}
	return ""
}

// scanFile checks every lock declared in f, returning how many it saw and one
// message per offender.
func scanFile(fset *token.FileSet, f *ast.File) (locks int, offenders []string) {
	report := func(pos token.Pos, name, kind, text, want string) {
		offenders = append(offenders, fmt.Sprintf("%s %s: got a %s lock with marker %q, want %s", fset.Position(pos), name, kind, strings.TrimSpace(text), want))
	}
	unsupported := func(pos token.Pos, name string) {
		offenders = append(offenders, fmt.Sprintf("%s %s: lock type in a declaration form the scan does not understand", fset.Position(pos), name))
	}
	type enclosing struct {
		docs  []*ast.CommentGroup
		locks int // locks the struct declares
	}
	encl := map[*ast.StructType]*enclosing{}
	count := func(st *ast.StructType) int {
		n := 0
		for _, fld := range st.Fields.List {
			if k, _ := lockKind(fld.Type); k != "" {
				n++
			}
		}
		return n
	}
	for _, decl := range f.Decls {
		gd, isGen := decl.(*ast.GenDecl)
		if !isGen {
			continue
		}
		for _, spec := range gd.Specs {
			switch sp := spec.(type) {
			case *ast.TypeSpec:
				if st, isStruct := sp.Type.(*ast.StructType); isStruct {
					encl[st] = &enclosing{[]*ast.CommentGroup{sp.Doc, gd.Doc}, count(st)}
				}
			case *ast.ValueSpec: // package-level var
				for _, n := range nodes(sp.Type, sp.Values) {
					ast.Inspect(n, func(n ast.Node) bool {
						if st, isStruct := n.(*ast.StructType); isStruct {
							encl[st] = &enclosing{[]*ast.CommentGroup{sp.Doc, sp.Comment, gd.Doc}, count(st)}
						}
						return true
					})
				}
				if gd.Tok != token.VAR || sp.Type == nil {
					continue
				}
				kind, ok := lockKind(sp.Type)
				switch {
				case !ok:
					unsupported(sp.Pos(), sp.Names[0].Name)
				case kind != "":
					locks++
					one := 0
					if len(gd.Specs) == 1 {
						one = 1
					}
					text := markerText([]*ast.CommentGroup{sp.Doc, sp.Comment}, []*ast.CommentGroup{gd.Doc}, one)
					if want := classify(kind, text); want != "" {
						report(sp.Pos(), sp.Names[0].Name, kind, text, want)
					}
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		st, isStruct := n.(*ast.StructType)
		if !isStruct {
			return true
		}
		e := encl[st]
		if e == nil {
			e = &enclosing{locks: count(st)}
		}
		for _, fld := range st.Fields.List {
			kind, ok := lockKind(fld.Type)
			name := "<embedded>"
			if len(fld.Names) > 0 {
				name = fld.Names[0].Name
			}
			switch {
			case !ok:
				unsupported(fld.Pos(), name)
			case kind != "":
				locks++
				text := markerText([]*ast.CommentGroup{fld.Doc, fld.Comment}, e.docs, e.locks)
				if want := classify(kind, text); want != "" {
					report(fld.Pos(), name, kind, text, want)
				}
			}
		}
		return true
	})
	return locks, offenders
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name, kind, text, want string
	}{
		{"negated leaf on sync", "sync", "decision: mu is the capacity domain. It is not a leaf.", "a lockorder lock (a ranked domain needs the ranked type) or the phrase \"is a leaf\" or \"is unranked\""},
		{"never a leaf on sync", "sync", "decision: mu never is a leaf", "the phrase \"is a leaf\" or \"is unranked\""},
		{"leaf on sync", "sync", "decision: mu is a leaf mutex, held for a map access.", ""},
		{"leaf then negation after on sync", "sync", "decision: mu is a leaf, never held across I/O.", ""},
		{"leaf split over lines", "sync", "decision: mu is a\nleaf lock.", ""},
		{"domain on lockorder", "lockorder", "decision: mu is the capacity domain.", ""},
		{"unranked on sync", "sync", "decision: mu is unranked, held across I/O.", ""},
		{"negated unranked on sync", "sync", "decision: mu is not unranked.", "the phrase \"is a leaf\" or \"is unranked\""},
		{"leaf on lockorder", "lockorder", "decision: mu is a leaf.", "a domain, not leaf"},
		{"neither on lockorder", "lockorder", "decision: mu guards things.", "one of the six domain names"},
		{"no decision", "sync", "mu is a leaf.", "a decision: marker naming a domain or leaf"},
		{"decision with neither", "sync", "decision: mu guards things.", "the phrase \"is a leaf\" or \"is unranked\""},
	} {
		if got := classify(c.kind, c.text); got != c.want {
			t.Errorf("%s: classify(%q, %q) = %q, want %q", c.name, c.kind, c.text, got, c.want)
		}
	}
}

func TestScanFileMarkerRules(t *testing.T) {
	const src = `package p

import "sync"

// decision: the lock is a leaf.
type two struct {
	a sync.Mutex
	b sync.Mutex
}

// decision: the lock is a leaf.
type one struct {
	a sync.Mutex
}

type each struct {
	// decision: a is a leaf.
	a sync.Mutex
	// decision: b is a leaf.
	b sync.Mutex
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	locks, offenders := scanFile(fset, f)
	if locks != 5 {
		t.Errorf("scanned %d locks, want 5", locks)
	}
	if len(offenders) != 2 || !strings.Contains(offenders[0], "p.go:7:2") || !strings.Contains(offenders[1], "p.go:8:2") {
		t.Errorf("offenders = %q, want exactly the two locks of the struct whose one doc marker cannot cover both (p.go:7:2 and p.go:8:2)", offenders)
	}
}

// ponytail: this checks locks only, by syntax. Lock-free state (atomics) is
// outside the six domains and is not checked. It does not see a lock declared
// as `var mu = sync.Mutex{}` with no type, `new(sync.Mutex)`, `type X
// sync.Mutex`, `type X = lockorder.Mutex`, through an import alias, or in a
// function-local variable. Upgrade to a go/types check if a domain starts
// being named on atomics or any of those forms appears.
func TestEveryLockNamesItsDomain(t *testing.T) {
	// Every internal package is scanned, so a lock added to a new one fails
	// here rather than passing unseen. The recorder's own package is left
	// out: its mutex is deliberately unranked.
	pkgs, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("reading the internal packages: %v", err)
	}
	var dirs []string
	for _, p := range pkgs {
		if !p.IsDir() || p.Name() == "lockorder" || p.Name() == "testdata" || p.Name() == "vendor" {
			continue
		}
		dirs = append(dirs, filepath.Join("..", p.Name()))
	}
	fset := token.NewFileSet()
	var offenders []string
	scanned := map[string]int{}
	total := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			n, bad := scanFile(fset, f)
			scanned[dir] += n
			total += n
			offenders = append(offenders, bad...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if total == 0 {
		t.Error("scanned no locks; the scan is not looking at the code")
	}
	// The packages that hold locks today must still be reached.
	for _, name := range []string{"index", "segment", "capacity", "datapath", "durability"} {
		if scanned[filepath.Join("..", name)] == 0 {
			t.Errorf("%s: scanned no locks; the scan is not looking at the code", name)
		}
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
