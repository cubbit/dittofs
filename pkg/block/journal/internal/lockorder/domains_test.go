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

// rankIdents maps each domain to the lockorder constant's name an Init (or
// InitLeaf) call wires it with.
var rankIdents = map[string]string{
	"file append":    "FileAppend",
	"capacity":       "Capacity",
	"file index":     "FileIndex",
	"file map":       "FileMap",
	"segment append": "SegmentAppend",
	"storage guard":  "StorageGuard",
}

// initsWired collects, per field name, the lockorder domains an Init or
// InitLeaf call inside f wires on a field of that name. A ranked lock's rank
// is installed only by such a call, so the scan asks this map to prove each
// lock it accepts is wired in the same file.
func initsWired(f *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	argDomain := func(e ast.Expr) (string, bool) {
		sel, isSel := e.(*ast.SelectorExpr)
		if !isSel {
			return "", false
		}
		if x, isIdent := sel.X.(*ast.Ident); !isIdent || x.Name != "lockorder" {
			return "", false
		}
		for d, id := range rankIdents {
			if sel.Sel.Name == id {
				return d, true
			}
		}
		return "", false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}
		fn, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || (fn.Sel.Name != "Init" && fn.Sel.Name != "InitLeaf") {
			return true
		}
		recv, isField := fn.X.(*ast.SelectorExpr)
		if !isField {
			return true
		}
		d, ok := argDomain(call.Args[0])
		if !ok {
			return true
		}
		if out[recv.Sel.Name] == nil {
			out[recv.Sel.Name] = map[string]bool{}
		}
		out[recv.Sel.Name][d] = true
		return true
	})
	return out
}

// initWired returns what is wrong with a ranked lock whose field is named
// name and whose marker text is text, or "" when it is right: at least one of
// the domains its marker names must be wired by an Init call on a field of
// that name in the file, so a lock whose rank is never installed fails here
// rather than carrying a rank-0 marker through the recorder-blind runs.
func initWired(name, text string, wired map[string]map[string]bool) string {
	ranks, wiredAtAll := wired[name]
	if !wiredAtAll {
		return fmt.Sprintf("an Init(lockorder.<rank>) call wiring a field named %s in this file", name)
	}
	for _, d := range domainNames {
		if strings.Contains(text, d) && ranks[d] {
			return ""
		}
	}
	return "a marker naming one of the domains an Init or InitLeaf call in this file wires"
}

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
	wired := initsWired(f)
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
					break
				}
				// A charge the recorder can see exists only once Init
				// installs the rank, so a ranked field must also be one
				// a matching Init in this file wires. A sync lock skips
				// this: it is invisible to the recorder by design, and
				// its marker alone says how it orders.
				if kind == "lockorder" {
					if want := initWired(name, text, wired); want != "" {
						report(fld.Pos(), name, kind, text, want)
					}
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

var bare sync.Mutex
var marked sync.Mutex // decision: marked is a leaf.
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	locks, offenders := scanFile(fset, f)
	if locks != 7 {
		t.Errorf("scanned %d locks, want 7", locks)
	}
	// The declaration pass reports the package-level var first, then the
	// struct walk reports the fields.
	want := []string{"p.go:23:5 bare", "p.go:7:2 a", "p.go:8:2 b"}
	if len(offenders) != len(want) {
		t.Fatalf("offenders = %q, want %d: the unmarked var, and the two locks of the struct whose one doc marker cannot cover both", offenders, len(want))
	}
	for i, w := range want {
		if !strings.Contains(offenders[i], w) {
			t.Errorf("offender %d = %q, want it to name %q", i, offenders[i], w)
		}
	}
}

// ponytail: this checks locks only, by syntax. Lock-free state (atomics) is
// outside the six domains and is not checked. It does not see a lock declared
// as `var mu = sync.Mutex{}` with no type, `new(sync.Mutex)`, `type X
// sync.Mutex`, `type X = lockorder.Mutex`, through an import alias, or in a
// function-local variable. The Init rule matches by field name inside the
// file: it cannot see a lock Init'ed from a package-level var, an Init in a
// different file of the same package, one Init'ed with a nil recorder — the
// call matches statically while wiring nothing — or two same-named fields of
// different ranks in one package, where either rank's Init satisfies both
// markers. The recorded fixture's wiring probes are the net under all of
// those; upgrade to a go/types pass if a lock lands unwired through any of
// the shapes above.
func TestEveryLockNamesItsDomain(t *testing.T) {
	// Every internal package is scanned, so a lock added to a new one fails
	// here rather than passing unseen. The recorder's own package is left
	// out: its mutex is deliberately unranked. The journal package's own
	// production files are scanned too: they sit one level up from every
	// internal package, and only their non-test files at that level are
	// read — the packages below them that are test-support (journaltest and
	// the conformance kit) hold locks exercised only by tests and are not
	// walked.
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
	const parent = "../.."
	scanOne := func(fset *token.FileSet, path string, offenders *[]string) (int, error) {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return 0, fmt.Errorf("parsing %s: %w", path, err)
		}
		n, bad := scanFile(fset, f)
		*offenders = append(*offenders, bad...)
		return n, nil
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
			n, err := scanOne(fset, path, &offenders)
			if err != nil {
				return err
			}
			scanned[dir] += n
			total += n
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	parentFiles, err := filepath.Glob(filepath.Join(parent, "*.go"))
	if err != nil {
		t.Fatalf("reading the journal package's files: %v", err)
	}
	for _, path := range parentFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		n, err := scanOne(fset, path, &offenders)
		if err != nil {
			t.Fatal(err)
		}
		scanned[parent] += n
		total += n
	}
	if total == 0 {
		t.Error("scanned no locks; the scan is not looking at the code")
	}
	// The packages that hold locks today must still be reached, the journal
	// package's own files among them.
	for _, name := range []string{"index", "segment", "capacity", "datapath", "durability"} {
		if scanned[filepath.Join("..", name)] == 0 {
			t.Errorf("%s: scanned no locks; the scan is not looking at the code", name)
		}
	}
	if scanned[parent] == 0 {
		t.Error("the journal package's own files: scanned no locks; the scan is not looking at the code")
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
