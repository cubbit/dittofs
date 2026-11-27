package archtest

import (
	"bytes"
	"cmp"
	"fmt"
	"go/build"
	"io/fs"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Module is the module's import path prefix; rows and violations name the
// module's packages relative to it.
const Module = "github.com/cubbit/dittofs/"

// Row binds the packages Pkg names: a module-relative path, or path/... for it
// and every package beneath it. The row that names a package most closely is
// its row.
type Row struct {
	Pkg   string
	Reach []string // the module's packages they may reach, transitively
	But   []string // packages refused although Reach names them
	Libs  []string // libraries outside the standard library they may reach, transitively
	Tests []string // the module's packages their tests may reach besides, when the walk lists tests
	// TestLibs are the libraries their tests may reach besides, when the walk
	// lists tests, so a test helper library never becomes production's.
	TestLibs []string
}

// testMark ends the name of a test variant in a Graph: the package compiled
// with its _test.go files, path [test], or its external test package,
// path_test [test].
const testMark = " [test]"

// Graph is one platform's import graph as go list reports it.
type Graph struct {
	Roots   []string            // the packages walked, by import path
	Imports map[string][]string // each listed package's direct imports, by import path
}

// targets are the platforms the walk lists each package for, with cgo on, so a
// file built for one of them only, or only with cgo, is checked too.
//
// decision: the walk lists each GOOS/GOARCH pair in targets, with cgo on and
// no build tags. A file no target builds, by its name's platform suffix or by
// its build constraint, is not walked, and Unwalked names it, so a test fails
// on it; widen this when a walked package needs one.
var targets = [][2]string{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

// Walk lists every package under dir and everything it imports with go list
// -deps, once per target, and returns each target's graph. Without tests go
// list reads production files only, so a test's imports are never walked,
// which is what RFC 0 §1.2 binds; with tests each package's test variant and
// external test package are walked too, as roots of their own.
func Walk(dir string, tests bool) ([]Graph, error) {
	var graphs []Graph

	for _, target := range targets {
		goos, goarch := target[0], target[1]

		args := []string{"list", "-tags=", "-deps", "-f", `{{.ImportPath}}|{{.DepOnly}}|{{join .Imports ","}}`}
		if tests {
			args = append(args, "-test")
		}

		cmd := exec.Command("go", append(args, "./...")...)
		cmd.Dir = dir
		// A planted tree is a module of its own, outside any workspace.
		cmd.Env = append(cmd.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=1")

		var stderr bytes.Buffer

		cmd.Stderr = &stderr

		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s for %s/%s: %w: %s", dir, goos, goarch, err, stderr.String())
		}

		g, err := parse(out)
		if err != nil {
			return nil, fmt.Errorf("go list in %s: %w", dir, err)
		}

		graphs = append(graphs, g)
	}

	return graphs, nil
}

// parse reads one go list run's lines, ImportPath|DepOnly|Imports, into a
// graph. A package the patterns name is a root, unless it is recompiled for
// another package's test binary: its own variants are roots already.
func parse(out []byte) (Graph, error) {
	g := Graph{Imports: map[string][]string{}}

	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) != 3 {
			return Graph{}, fmt.Errorf("unexpected line %q", line)
		}

		name := variant(fields[0])

		// The test binary's main package imports only the variants and the
		// standard library's test runner; a package that merely ends in
		// .test is walked.
		if strings.HasSuffix(name, ".test") && strings.Contains(fields[2], "testing/internal/testdeps") {
			continue
		}

		if fields[1] == "false" && (!strings.Contains(name, " [") || strings.HasSuffix(name, testMark)) {
			g.Roots = append(g.Roots, name)
		}

		// A test variant's name holds a space, so imports are joined by commas.
		for imp := range strings.SplitSeq(fields[2], ",") {
			if imp != "" {
				g.Imports[name] = append(g.Imports[name], variant(imp))
			}
		}
	}

	return g, nil
}

// variant names path [path.test], a package compiled with its _test.go files,
// path [test], and path_test [path.test], its external test package,
// path_test [test]. Any other path [x.test] is path recompiled for x's test
// binary and keeps its name, so it is never merged with path's own variant.
func variant(name string) string {
	path, forTest, ok := strings.Cut(name, " [")
	if ok && forTest == strings.TrimSuffix(path, "_test")+".test]" {
		return path + testMark
	}

	return name
}

// pkgOf returns the module-relative package a graph node is a build of.
func pkgOf(name string) string {
	path, _, _ := strings.Cut(strings.TrimPrefix(name, Module), " [")

	return path
}

// matches reports whether path, module-relative or a library's, is pattern or,
// for a pattern ending in /..., the package it names or one beneath it.
func matches(pattern, path string) bool {
	if root, ok := strings.CutSuffix(pattern, "/..."); ok {
		return path == root || strings.HasPrefix(path, root+"/")
	}

	return path == pattern
}

// matchesAny reports whether path matches one of patterns.
func matchesAny(patterns []string, path string) bool {
	return slices.ContainsFunc(patterns, func(pat string) bool { return matches(pat, path) })
}

// rowOf returns the row that names pkg most closely.
func rowOf(table []Row, pkg string) (Row, bool) {
	var best Row

	found := false

	for _, r := range table {
		if matches(r.Pkg, pkg) && (!found || len(strings.TrimSuffix(r.Pkg, "/...")) > len(strings.TrimSuffix(best.Pkg, "/..."))) {
			best, found = r, true
		}
	}

	return best, found
}

// isLibrary reports whether path is a package outside the standard library
// and outside the module: its first element holds a dot.
func isLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")

	return strings.Contains(first, ".") && !strings.HasPrefix(path, Module)
}

// Check returns each violation of table by graphs: a package with no row, a
// module package its row does not let it reach, and a library its row does not
// let it reach. It follows every import, transitively, libraries' included, so
// a library another library brings is checked too. A violation reached through
// other packages names the chain. A test variant is held to its package's row,
// and may reach besides the package itself and what the row's Tests and
// TestLibs name.
func Check(table []Row, graphs ...Graph) []string {
	var bad []string

	for _, g := range graphs {
		for _, root := range g.Roots {
			bad = append(bad, checkRoot(table, g, root)...)
		}
	}

	slices.Sort(bad)

	return slices.Compact(bad)
}

// checkRoot walks g breadth first from root, so each violation names the
// shortest chain that reaches it.
func checkRoot(table []Row, g Graph, root string) []string {
	rel := strings.TrimPrefix(root, Module)

	pkg, test := strings.CutSuffix(rel, testMark)
	if test {
		pkg = strings.TrimSuffix(pkg, "_test")
	}

	r, ok := rowOf(table, pkg)
	if !ok {
		return []string{rel + " has no row"}
	}

	allowed := func(dep string) bool {
		if test && (dep == pkg || matchesAny(r.Tests, dep)) {
			return true
		}

		return matchesAny(r.Reach, dep) && !matchesAny(r.But, dep)
	}

	allowedLib := func(lib string) bool {
		return matchesAny(r.Libs, lib) || test && matchesAny(r.TestLibs, lib)
	}

	var bad []string

	report := func(verb, dep string, chain []string) {
		if len(chain) == 0 {
			bad = append(bad, fmt.Sprintf("%s %s %s, which row %s does not allow", rel, verb, dep, r.Pkg))

			return
		}

		bad = append(bad, fmt.Sprintf("%s reaches %s through %s, which row %s does not allow", rel, dep, strings.Join(chain, " -> "), r.Pkg))
	}

	via := map[string]string{root: ""}
	queue := []string{root}

	// chain lists the packages between root and n, module-relative.
	chain := func(n string) []string {
		var c []string
		for ; n != root; n = via[n] {
			c = append(c, strings.TrimPrefix(n, Module))
		}

		slices.Reverse(c)

		return c
	}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]

		for _, imp := range g.Imports[n] {
			if _, seen := via[imp]; seen {
				continue
			}

			switch {
			case strings.HasPrefix(imp, Module):
				via[imp] = n
				queue = append(queue, imp)

				if dep := pkgOf(imp); !allowed(dep) {
					report("reaches", dep, chain(n))
				}
			case isLibrary(imp):
				via[imp] = n
				queue = append(queue, imp)

				if !allowedLib(imp) {
					report("imports", imp, chain(n))
				}
			}
		}
	}

	return bad
}

// EntityLibs returns the libraries the metadata store's entity package
// reaches, transitively, then extra, in a new slice. Every row that lets a
// package reach the entity package lists them, since the walk checks every
// library a package reaches; TestEntityLibs holds the list to the package.
func EntityLibs(extra ...string) []string {
	return append([]string{
		"github.com/google/uuid",
		"github.com/klauspost/cpuid/v2",
		"lukechampine.com/blake3",
		"lukechampine.com/blake3/bao",
		"lukechampine.com/blake3/guts",
	}, extra...)
}

// Support is this package and what it imports from the module: what a walk
// with tests lets every test reach, through a row's Tests, so a test reads the
// tree through this package rather than through a copy of it.
//
// decision: a test may reach this support beyond what its package's
// production code may. It runs go list and type-checks files, so it holds no
// store, fake backend or network; withdraw the allowance if it ever reaches an
// implementation. TestSupport holds the list to the package.
var Support = []string{"internal/archtest", "internal/prof"}

// Unwalked returns each .go file under dir, testdata skipped, that no target
// builds: one whose name carries another platform's suffix, or whose build
// constraint no target satisfies with cgo on and no build tags, such as a
// custom tag or !cgo. go list leaves such a file out of every graph, so a test
// that walks dir calls this too and fails on what it returns.
func Unwalked(dir string) ([]string, error) {
	ctxs := make([]build.Context, 0, len(targets))
	for _, target := range targets {
		c := build.Default
		c.GOOS, c.GOARCH, c.CgoEnabled, c.BuildTags = target[0], target[1], true, nil
		ctxs = append(ctxs, c)
	}

	var bad []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// go list ignores testdata and a directory whose name starts with _
		// or . wholesale; dir itself may be ".".
		if d.IsDir() && path != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}

		// The go command ignores a file whose name starts with _ or . on
		// every platform, so no build reaches it.
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			return nil
		}

		var matchErr error

		built := slices.ContainsFunc(ctxs, func(c build.Context) bool {
			ok, err := c.MatchFile(filepath.Dir(path), name)
			matchErr = cmp.Or(matchErr, err)

			return ok
		})
		if matchErr != nil {
			return matchErr
		}

		if !built {
			bad = append(bad, path+" is built for no platform the walk lists")
		}

		return nil
	})

	return bad, err
}
