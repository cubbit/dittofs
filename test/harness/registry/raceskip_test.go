package registry

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// raceSkip is a test that gates a check on the race detector, so only a run
// without -race enforces it.
type raceSkip struct {
	Pkg   string          // ./ path of its package
	Name  string          // the test function, or "" for a helper: the whole package must run
	Tags  constraint.Expr // its file's build constraint, or nil
	Short bool            // the gate also skips under -short
}

// goTestCell is what one go test command of the registry runs without -race.
type goTestCell struct {
	Unit      string
	Pkgs      []string // ./ paths, a trailing /... matching the subtree
	Run, Skip *regexp.Regexp
	Tags      []string
	Short     bool
}

// Every test that skips under the race detector runs in some cell without it:
// unit runs every package with -race, so a test that skips there and that no
// other cell names is enforced nowhere.
func TestEveryRaceSkipRunsWithoutRace(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	r, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	skips, err := findRaceSkips(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) == 0 {
		t.Fatal("no test skips under the race detector: the scan finds nothing, so it checks nothing")
	}
	var units []*Suite
	for _, s := range r.Suites {
		units = append(units, r.Units(s)...)
	}
	for _, s := range unenforced(skips, goTestCells(units)) {
		if s.Name == "" {
			t.Errorf("%s: a helper gates a check on the race detector and no pr or merge cell runs the whole package without -race", s.Pkg)
		} else {
			t.Errorf("%s %s gates a check on the race detector and no pr or merge cell runs it without -race", s.Pkg, s.Name)
		}
	}
}

// findRaceSkips parses every _test.go file under root, outside testdata and
// dot directories, for the checks gated on the race detector.
func findRaceSkips(root string) ([]raceSkip, error) {
	var out []raceSkip
	helperAt := map[string]int{} // a package's one helper entry, by index
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "vendor" || n == "graphify-out") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := "./" + filepath.ToSlash(rel)
		for _, g := range raceGated(f) {
			if i, ok := helperAt[pkg]; ok && g.Name == "" {
				out[i].Short = out[i].Short || g.Short
				continue
			} else if g.Name == "" {
				helperAt[pkg] = len(out)
			}
			g.Pkg, g.Tags = pkg, buildConstraint(f)
			out = append(out, g)
		}
		return nil
	})
	return out, err
}

// raceGated returns the race gates of f: one per Test function holding one
// anywhere in its body, closures and loops included, and one with no name if
// any other declaration holds one. A helper's gate is not traced to the tests
// that call it; the whole package must run without -race instead.
func raceGated(f *ast.File) []raceSkip {
	var out []raceSkip
	var helper *raceSkip
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		isTest := ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test")
		found, short := false, false
		ast.Inspect(decl, func(n ast.Node) bool {
			if is, ok := n.(*ast.IfStmt); ok {
				if g, sh := raceGate(is); g {
					found, short = true, short || sh
				}
			}
			return true
		})
		switch {
		case !found:
		case isTest:
			out = append(out, raceSkip{Name: fn.Name.Name, Short: short})
		case helper == nil:
			helper = &raceSkip{Short: short}
		default:
			helper.Short = helper.Short || short
		}
	}
	if helper != nil {
		out = append(out, *helper)
	}
	return out
}

// raceGate reports whether is gates a check on the race detector: its
// condition holds the flag, and either the flag is negated (the body is the
// check) or the body leaves by a Skip, continue or return (what follows is),
// or an else holds the check. short reports a testing.Short() beside it.
func raceGate(is *ast.IfStmt) (gate, short bool) {
	has, negated := false, false
	var walk func(e ast.Expr, neg bool)
	walk = func(e ast.Expr, neg bool) {
		switch e := e.(type) {
		case *ast.ParenExpr:
			walk(e.X, neg)
		case *ast.UnaryExpr:
			walk(e.X, neg != (e.Op == token.NOT))
		case *ast.BinaryExpr:
			walk(e.X, neg)
			walk(e.Y, neg)
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Short" {
				short = true
			}
		case *ast.Ident, *ast.SelectorExpr:
			if isRaceFlag(e) {
				has, negated = true, negated || neg
			}
		}
	}
	walk(is.Cond, false)
	if !has {
		return false, false
	}
	return negated || is.Else != nil || leaves(is.Body), short
}

func isRaceFlag(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "raceEnabled"
	case *ast.SelectorExpr:
		return e.Sel.Name == "RaceEnabled"
	}
	return false
}

// leaves reports whether b calls a Skip, or continues or returns, outside a
// closure of its own.
func leaves(b *ast.BlockStmt) bool {
	found := false
	ast.Inspect(b, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			found = true
		case *ast.BranchStmt:
			found = found || n.Tok == token.CONTINUE
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Skip") {
				found = true
			}
		}
		return !found
	})
	return found
}

// buildConstraint returns the //go:build line above f's package clause.
func buildConstraint(f *ast.File) constraint.Expr {
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			if constraint.IsGoBuild(c.Text) {
				if e, err := constraint.Parse(c.Text); err == nil {
					return e
				}
			}
		}
	}
	return nil
}

// goTestCells reads each unit's go test commands that run without -race in
// the pr or merge tier, so a gate is enforced on the change that adds it, not
// first by a nightly run. A sh: command is not read, so a test it alone runs
// counts as unenforced.
func goTestCells(units []*Suite) []goTestCell {
	var out []goTestCell
	for _, u := range units {
		if u.Tier != "pr" && u.Tier != "merge" {
			continue
		}
		for _, c := range u.Cmd {
			w := strings.Fields(c.Line)
			if len(w) < 2 || w[0] != "go" || w[1] != "test" {
				continue
			}
			cell, race := goTestCell{Unit: u.Name}, false
			for i := 2; i < len(w); i++ {
				flag, val, hasVal := strings.Cut(w[i], "=")
				next := func() string {
					if hasVal {
						return val
					}
					if i+1 < len(w) {
						i++
						return w[i]
					}
					return ""
				}
				switch {
				case flag == "-race":
					race = !hasVal || val == "true"
				case flag == "-run":
					cell.Run = regexp.MustCompile(topLevel(next()))
				case flag == "-skip":
					cell.Skip = regexp.MustCompile(topLevel(next()))
				case flag == "-short":
					cell.Short = !hasVal || val == "true"
				case w[i] == "{args}":
					cell.Short = slices.ContainsFunc(slices.Collect(maps.Values(u.Args)), func(a string) bool { return strings.Contains(a, "-short") })
				case flag == "-tags":
					cell.Tags = strings.Split(next(), ",")
				case w[i] == "{packages}" && u.Packages != nil:
					cell.Pkgs = append(cell.Pkgs, u.Packages.Patterns...)
				case strings.HasPrefix(w[i], "./"):
					cell.Pkgs = append(cell.Pkgs, w[i])
				}
			}
			if !race {
				out = append(out, cell)
			}
		}
	}
	return out
}

// topLevel keeps the part of a -run or -skip pattern that go test matches
// against the top-level test name: the text before the first unbracketed /.
func topLevel(p string) string {
	depth := 0
	for i, r := range p {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '/':
			if depth == 0 {
				return p[:i]
			}
		}
	}
	return p
}

// unenforced returns the race gates no cell runs: none whose packages hold the
// test's, whose -run matches its name and -skip doesn't (or, for a helper's,
// that names no -run or -skip), that doesn't pass -short to a gate that skips
// under it, and whose tags, with the runner's own, meet its file's build
// constraint.
func unenforced(skips []raceSkip, cells []goTestCell) []raceSkip {
	var out []raceSkip
	for _, s := range skips {
		if !slices.ContainsFunc(cells, func(c goTestCell) bool { return runs(c, s) }) {
			out = append(out, s)
		}
	}
	return out
}

func runs(c goTestCell, s raceSkip) bool {
	if !slices.ContainsFunc(c.Pkgs, func(p string) bool { return pkgMatches(p, s.Pkg) }) {
		return false
	}
	if s.Name == "" && (c.Run != nil || c.Skip != nil) || s.Short && c.Short {
		return false
	}
	if c.Run != nil && !c.Run.MatchString(s.Name) || c.Skip != nil && c.Skip.MatchString(s.Name) {
		return false
	}
	return s.Tags == nil || s.Tags.Eval(func(tag string) bool {
		return slices.Contains(c.Tags, tag) || runnerTags[tag] || strings.HasPrefix(tag, "go1.")
	})
}

// runnerTags are the build tags the CI runner, linux on amd64 with cgo, sets
// without a -tags flag.
var runnerTags = map[string]bool{"linux": true, "unix": true, "amd64": true, "cgo": true, "gc": true}

func pkgMatches(pattern, pkg string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if base, ok := strings.CutSuffix(pattern, "/..."); ok {
		return pkg == base || strings.HasPrefix(pkg, base+"/") || base == "."
	}
	return pattern == pkg
}

// The scan finds a gate by either race flag, anywhere in a test, negated or
// beside testing.Short(), with its file's constraint, and a helper's gate for
// its whole package; it leaves out a race branch or case that gates no check.
func TestFindRaceSkips(t *testing.T) {
	got, err := findRaceSkips(filepath.Join("testdata", "raceskip"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Pkg+" "+s.Name)
		if s.Name == "TestTagged" && (s.Tags == nil || s.Tags.String() != "slow") {
			t.Errorf("TestTagged: constraint %v, want slow", s.Tags)
		}
		if s.Short != (s.Name == "TestShortOrRace") {
			t.Errorf("%s: short %t, want %t", s.Name, s.Short, !s.Short)
		}
	}
	want := []string{"./a TestLocalFlag", "./a TestProfFlag", "./a TestNestedSkip", "./a TestShortOrRace", "./a TestNegatedAssertion", "./a TestContinueInLoop", "./b TestTagged", "./c "}
	if !slices.Equal(names, want) {
		t.Errorf("found %q, want %q", names, want)
	}
}

// A race skip is enforced only by a cell without -race that reaches its
// package, names it, doesn't skip it and builds its file.
func TestUnenforced(t *testing.T) {
	slow, err := constraint.Parse("//go:build slow")
	if err != nil {
		t.Fatal(err)
	}
	skip := raceSkip{Pkg: "./pkg/x", Name: "TestAllocs"}
	tagged := raceSkip{Pkg: "./pkg/x", Name: "TestAllocs", Tags: slow}
	linux, err := constraint.Parse("//go:build linux && amd64 && go1.21")
	if err != nil {
		t.Fatal(err)
	}
	onLinux := raceSkip{Pkg: "./pkg/x", Name: "TestAllocs", Tags: linux}
	short := raceSkip{Pkg: "./pkg/x", Name: "TestAllocs", Short: true}
	helper := raceSkip{Pkg: "./pkg/x"}
	cases := []struct {
		name string
		line string
		pkgs *Packages
		tier string
		s    raceSkip
		want bool // enforced
	}{
		{"the package by path", "go test ./pkg/x/", nil, "pr", skip, true},
		{"a subtree pattern", "go test ./pkg/...", nil, "pr", skip, true},
		{"the whole module", "go test ./...", nil, "pr", skip, true},
		{"{packages} from patterns", "go test {packages}", &Packages{Patterns: []string{"./pkg/..."}}, "pr", skip, true},
		{"with -race", "go test -race ./pkg/x/", nil, "pr", skip, false},
		{"with -race=true", "go test -race=true ./pkg/x/", nil, "pr", skip, false},
		{"another package", "go test ./pkg/xy/", nil, "pr", skip, false},
		{"a -run naming it", "go test -run=^(TestOther|TestAllocs)$ ./pkg/x/", nil, "pr", skip, true},
		{"a -run naming it, spaced", "go test -run ^TestAllocs$ ./pkg/x/", nil, "pr", skip, true},
		{"a -run with a subtest part", "go test -run=^TestAllocs$/case ./pkg/x/", nil, "pr", skip, true},
		{"a -run naming another", "go test -run=^TestAllocsMore$ ./pkg/x/", nil, "pr", skip, false},
		{"a -skip naming it", "go test -skip=^TestAllocs$ ./pkg/x/", nil, "pr", skip, false},
		{"the manual tier", "go test ./pkg/x/", nil, "manual", skip, false},
		{"the nightly tier", "go test ./pkg/x/", nil, "nightly", skip, false},
		{"the merge tier", "go test ./pkg/x/", nil, "merge", skip, true},
		{"the runner's platform tags", "go test ./pkg/x/", nil, "pr", onLinux, true},
		{"-short on a gate beside testing.Short()", "go test -short ./pkg/x/", nil, "pr", short, false},
		{"-short on a plain gate", "go test -short ./pkg/x/", nil, "pr", skip, true},
		{"no -short on a gate beside testing.Short()", "go test ./pkg/x/", nil, "pr", short, true},
		{"a helper's, by the whole package", "go test ./pkg/x/", nil, "pr", helper, true},
		{"a helper's, by a -run", "go test -run=. ./pkg/x/", nil, "pr", helper, false},
		{"a helper's, by a -skip", "go test -skip=^TestOther$ ./pkg/x/", nil, "pr", helper, false},
		{"its tag missing", "go test ./pkg/x/", nil, "pr", tagged, false},
		{"its tag given", "go test -tags=a,slow ./pkg/x/", nil, "pr", tagged, true},
	}
	for _, c := range cases {
		u := &Suite{Name: "s", Tier: c.tier, Packages: c.pkgs, Cmd: Commands{{Line: c.line}}}
		got := len(unenforced([]raceSkip{c.s}, goTestCells([]*Suite{u}))) == 0
		if got != c.want {
			t.Errorf("%s: %q enforced %t, want %t", c.name, c.line, got, c.want)
		}
	}
}
