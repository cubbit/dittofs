package registry

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// raceSkip is a test that skips itself under the race detector, so only a run
// without -race enforces it.
type raceSkip struct {
	Pkg  string          // ./ path of its package
	Name string          // the test function
	Tags constraint.Expr // its file's build constraint, or nil
}

// goTestCell is what one go test command of the registry runs without -race.
type goTestCell struct {
	Unit      string
	Pkgs      []string // ./ paths, a trailing /... matching the subtree
	Run, Skip *regexp.Regexp
	Tags      []string
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
		t.Errorf("%s %s skips under -race and no cell runs it without -race", s.Pkg, s.Name)
	}
}

// findRaceSkips parses every _test.go file under root, outside testdata and
// dot directories, for the tests that open with a race skip.
func findRaceSkips(root string) ([]raceSkip, error) {
	var out []raceSkip
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
		for _, name := range raceSkipping(f) {
			out = append(out, raceSkip{Pkg: "./" + filepath.ToSlash(rel), Name: name, Tags: buildConstraint(f)})
		}
		return nil
	})
	return out, err
}

// raceSkipping returns the Test functions of f holding, at their top level,
// `if raceEnabled {` or `if <pkg>.RaceEnabled {` whose body calls a Skip.
func raceSkipping(f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		for _, st := range fn.Body.List {
			if is, ok := st.(*ast.IfStmt); ok && isRaceFlag(is.Cond) && callsSkip(is.Body) {
				out = append(out, fn.Name.Name)
				break
			}
		}
	}
	return out
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

func callsSkip(b *ast.BlockStmt) bool {
	found := false
	ast.Inspect(b, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Skip") {
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

// goTestCells reads each unit's go test commands that run without -race and
// outside the manual tier, which never runs on its own. A sh: command is not
// read, so a test it alone runs counts as unenforced.
func goTestCells(units []*Suite) []goTestCell {
	var out []goTestCell
	for _, u := range units {
		if u.Tier == "manual" {
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

// unenforced returns the race skips no cell runs: none whose packages hold the
// test's, whose -run matches its name and -skip doesn't, and whose tags meet
// its file's build constraint.
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
	if c.Run != nil && !c.Run.MatchString(s.Name) || c.Skip != nil && c.Skip.MatchString(s.Name) {
		return false
	}
	return s.Tags == nil || s.Tags.Eval(func(tag string) bool { return slices.Contains(c.Tags, tag) })
}

func pkgMatches(pattern, pkg string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if base, ok := strings.CutSuffix(pattern, "/..."); ok {
		return pkg == base || strings.HasPrefix(pkg, base+"/") || base == "."
	}
	return pattern == pkg
}

// The scan finds a test by either race flag, with its file's constraint, and
// leaves out a test whose race branch doesn't skip or isn't at its top level.
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
	}
	want := []string{"./a TestLocalFlag", "./a TestProfFlag", "./b TestTagged"}
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
