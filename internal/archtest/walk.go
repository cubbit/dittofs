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
	Libs  []string // libraries outside the standard library they may import
}

// Listed is one package as go list reports it, production files only.
type Listed struct {
	Path    string
	Deps    []string // every package it imports, transitively
	Imports []string // the packages it imports directly
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

// Walk lists every package under dir with go list, once per target, and
// merges what each target reaches. .Deps is the transitive import set, and
// without -test go list reads production files only, so a test's imports are
// never walked (RFC 0 §1.2).
func Walk(dir string) ([]Listed, error) {
	merged := map[string]*Listed{}

	for _, target := range targets {
		goos, goarch := target[0], target[1]

		cmd := exec.Command("go", "list", "-tags=", "-f", `{{.ImportPath}}|{{join .Deps " "}}|{{join .Imports " "}}`, "./...")
		cmd.Dir = dir
		// A planted tree is a module of its own, outside any workspace.
		cmd.Env = append(cmd.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=1")

		var stderr bytes.Buffer

		cmd.Stderr = &stderr

		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s for %s/%s: %w: %s", dir, goos, goarch, err, stderr.String())
		}

		for line := range strings.Lines(string(out)) {
			fields := strings.Split(strings.TrimSpace(line), "|")
			if len(fields) != 3 {
				return nil, fmt.Errorf("go list in %s: unexpected line %q", dir, line)
			}

			p := merged[fields[0]]
			if p == nil {
				p = &Listed{Path: fields[0]}
				merged[fields[0]] = p
			}

			p.Deps = append(p.Deps, strings.Fields(fields[1])...)
			p.Imports = append(p.Imports, strings.Fields(fields[2])...)
		}
	}

	var pkgs []Listed

	for _, p := range merged {
		pkgs = append(pkgs, *p)
	}

	return pkgs, nil
}

// matches reports whether path, module-relative or a library's, is pattern or,
// for a pattern ending in /..., the package it names or one beneath it.
func matches(pattern, path string) bool {
	if root, ok := strings.CutSuffix(pattern, "/..."); ok {
		return path == root || strings.HasPrefix(path, root+"/")
	}

	return path == pattern
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

// Check returns each violation of table by pkgs: a package with no row, a
// module package its row does not let it reach, and a library outside the
// standard library its row does not let it import. Libraries are checked where
// they are imported only, since no library imports one of the module's
// packages.
func Check(pkgs []Listed, table []Row) []string {
	var bad []string

	for _, p := range pkgs {
		rel := strings.TrimPrefix(p.Path, Module)

		r, ok := rowOf(table, rel)
		if !ok {
			bad = append(bad, rel+" has no row")

			continue
		}

		for _, dep := range p.Deps {
			d, ours := strings.CutPrefix(dep, Module)
			if !ours {
				continue
			}

			allowed := slices.ContainsFunc(r.Reach, func(pat string) bool { return matches(pat, d) }) &&
				!slices.ContainsFunc(r.But, func(pat string) bool { return matches(pat, d) })
			if !allowed {
				bad = append(bad, fmt.Sprintf("%s reaches %s, which row %s does not allow", rel, d, r.Pkg))
			}
		}

		for _, imp := range p.Imports {
			first, _, _ := strings.Cut(imp, "/")
			if !strings.Contains(first, ".") || strings.HasPrefix(imp, Module) {
				continue
			}

			if !slices.ContainsFunc(r.Libs, func(pat string) bool { return matches(pat, imp) }) {
				bad = append(bad, fmt.Sprintf("%s imports %s, which row %s does not allow", rel, imp, r.Pkg))
			}
		}
	}

	slices.Sort(bad)

	return slices.Compact(bad)
}
