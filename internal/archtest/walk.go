package archtest

import (
	"bytes"
	"fmt"
	"os/exec"
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
	Libs  []string // libraries outside the standard library they may reach
	Tests []string // the module's packages their tests may reach besides, when the walk lists tests
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
// decision: the walk lists Linux, macOS and Windows on amd64 and arm64, with
// cgo on and no build tags. A file behind a custom tag, or built only for
// another platform, is not walked; widen this when a walked package gains one.
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

		args := []string{"list", "-tags=", "-deps", "-f", `{{.ImportPath}}|{{.DepOnly}}|{{join .Imports " "}}`}
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

		g := Graph{Imports: map[string][]string{}}

		for line := range strings.Lines(string(out)) {
			fields := strings.Split(strings.TrimSpace(line), "|")
			if len(fields) != 3 {
				return nil, fmt.Errorf("go list in %s: unexpected line %q", dir, line)
			}

			name := variant(fields[0])

			// The test binary's main package imports only the variants and the
			// standard library's test runner.
			if strings.HasSuffix(name, ".test") {
				continue
			}

			if fields[1] == "false" {
				g.Roots = append(g.Roots, name)
			}

			for _, imp := range strings.Fields(fields[2]) {
				g.Imports[name] = append(g.Imports[name], variant(imp))
			}
		}

		graphs = append(graphs, g)
	}

	return graphs, nil
}

// variant names a package go list prints as path [pkg.test] path [test].
func variant(name string) string {
	if path, _, ok := strings.Cut(name, " ["); ok {
		return path + testMark
	}

	return name
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
// let it reach. It follows every import inside the module, transitively, and
// checks each library a module package on the way imports, but not what a
// library imports, since no library imports one of the module's packages. A
// violation reached through other packages names the chain. A test variant is
// held to its package's row, and may reach besides the package itself and what
// the row's Tests name.
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
			switch {
			case strings.HasPrefix(imp, Module):
				if _, seen := via[imp]; seen {
					continue
				}

				via[imp] = n
				queue = append(queue, imp)

				if dep := strings.TrimSuffix(strings.TrimPrefix(imp, Module), testMark); !allowed(dep) {
					report("reaches", dep, chain(n))
				}
			case isLibrary(imp):
				if _, seen := via[imp]; seen {
					continue
				}

				// A library is checked, never followed.
				via[imp] = n

				if !matchesAny(r.Libs, imp) {
					report("imports", imp, chain(n))
				}
			}
		}
	}

	return bad
}
