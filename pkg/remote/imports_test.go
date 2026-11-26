package remote

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/cubbit/dittofs/"

// row binds the packages pkg names: a module-relative path, or path/... for it
// and every package beneath it. The row that names a package most closely is
// its row.
type row struct {
	pkg   string
	reach []string // the module's packages they may reach, transitively
	but   []string // packages refused although reach names them
	libs  []string // libraries outside the standard library they may reach, transitively
}

// aws is the AWS SDK for Go v2 with its runtime.
var aws = []string{"github.com/aws/aws-sdk-go-v2/...", "github.com/aws/smithy-go/..."}

// table is every package of the remote tier, landed or not, and what its
// production code may reach (RFC 0 §1.2, RFC 4 §1.2): the contract nothing but
// the standard library; the codec the contract and the transforms' and the
// metadata entities' contracts, never a transform, the carver, the engine or a
// store; a store the contract and its own packages, never the codec. A row is
// an upper bound: a package that reaches less passes.
var table = []row{
	{pkg: "pkg/remote"},
	{pkg: "pkg/remote/codec", reach: []string{"pkg/remote", "pkg/transform", "pkg/transform/keys", "pkg/metadata/entity/..."}},
	{pkg: "pkg/remote/memory/...", reach: []string{"pkg/remote", "pkg/sim", "pkg/remote/memory/..."}},
	{pkg: "pkg/remote/remotetest", reach: []string{"pkg/remote", "internal/prof"}},
	{pkg: "pkg/remote/s3/...", reach: []string{"pkg/remote", "pkg/remote/s3/..."}, but: []string{"pkg/remote/s3/s3fault/..."}, libs: aws},
	{pkg: "pkg/remote/s3/s3fault/...", reach: []string{"pkg/remote/s3/s3fault/..."}, libs: aws},
}

// listed is one package as go list reports it, production files only.
type listed struct {
	path string
	deps []string // every package it imports, transitively
}

// targets are the platforms the walk lists each package for, with cgo on, so a
// file built for one of them only, or only with cgo, is checked too.
//
// decision: the walk lists each GOOS/GOARCH pair in targets, with cgo on and
// no build tags. A file behind a custom tag, or built only for
// another platform, is not walked; widen this when a package under pkg/remote
// gains one.
var targets = [][2]string{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

// walk lists every package under dir with go list, once per target, and
// merges what each target reaches. .Deps is the transitive import set, and
// without -test go list reads production files only, so a test's imports are
// never walked (RFC 0 §1.2).
func walk(dir string) ([]listed, error) {
	merged := map[string]*listed{}

	for _, target := range targets {
		goos, goarch := target[0], target[1]

		cmd := exec.Command("go", "list", "-tags=", "-f", `{{.ImportPath}}|{{join .Deps " "}}`, "./...")
		cmd.Dir = dir
		// The planted tree is a module of its own, outside any workspace.
		cmd.Env = append(cmd.Environ(), "GOWORK=off", "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=1")

		var stderr bytes.Buffer

		cmd.Stderr = &stderr

		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s for %s/%s: %w: %s", dir, goos, goarch, err, stderr.String())
		}

		for line := range strings.Lines(string(out)) {
			fields := strings.Split(strings.TrimSpace(line), "|")
			if len(fields) != 2 {
				return nil, fmt.Errorf("go list in %s: unexpected line %q", dir, line)
			}

			p := merged[fields[0]]
			if p == nil {
				p = &listed{path: fields[0]}
				merged[fields[0]] = p
			}

			p.deps = append(p.deps, strings.Fields(fields[1])...)
		}
	}

	var pkgs []listed

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
func rowOf(table []row, pkg string) (row, bool) {
	var best row

	found := false

	for _, r := range table {
		if matches(r.pkg, pkg) && (!found || len(strings.TrimSuffix(r.pkg, "/...")) > len(strings.TrimSuffix(best.pkg, "/..."))) {
			best, found = r, true
		}
	}

	return best, found
}

// check returns each violation of table by pkgs: a package with no row, a
// module package its row does not let it reach, and a library outside the
// standard library its row does not let it import. Libraries are checked where
// they are imported only, since no library imports one of the module's
// packages.
func check(pkgs []listed, table []row) []string {
	var bad []string

	for _, p := range pkgs {
		rel := strings.TrimPrefix(p.path, module)

		r, ok := rowOf(table, rel)
		if !ok {
			bad = append(bad, rel+" has no row")

			continue
		}

		for _, dep := range p.deps {
			d, ours := strings.CutPrefix(dep, module)
			if !ours {
				continue
			}

			allowed := slices.ContainsFunc(r.reach, func(pat string) bool { return matches(pat, d) }) &&
				!slices.ContainsFunc(r.but, func(pat string) bool { return matches(pat, d) })
			if !allowed {
				bad = append(bad, fmt.Sprintf("%s reaches %s, which row %s does not allow", rel, d, r.pkg))
			}
		}

		for _, imp := range p.deps {
			first, _, _ := strings.Cut(imp, "/")
			if !strings.Contains(first, ".") || strings.HasPrefix(imp, module) {
				continue
			}

			if !slices.ContainsFunc(r.libs, func(pat string) bool { return matches(pat, imp) }) {
				bad = append(bad, fmt.Sprintf("%s reaches %s, which row %s does not allow", rel, imp, r.pkg))
			}
		}
	}

	slices.Sort(bad)

	return slices.Compact(bad)
}

// Every package under pkg/remote reaches, through its production code, only
// what its row allows.
func TestImportWalk(t *testing.T) {
	pkgs, err := walk(".")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(pkgs, func(p listed) bool { return p.path == module+"pkg/remote" }) {
		t.Fatalf("the walk listed %d packages, none of them pkg/remote", len(pkgs))
	}

	for _, v := range check(pkgs, table) {
		t.Error(v)
	}
}

// The walk over a planted module, whose codec reaches the engine through an
// intermediate package, and the carver, GC and the journal from files built
// only for Windows, only for arm64 and only with cgo, and whose codec test
// imports the syncer, reports exactly the production reaches: a walk of direct
// imports only misses the engine, a walk of one platform or without cgo misses
// the others, and a walk that binds tests reports the syncer too.
func TestImportWalkFollowsProductionImportsTransitively(t *testing.T) {
	pkgs, err := walk("testdata/walk/pkg/remote")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"pkg/remote/codec reaches pkg/carver, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/engine, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/gc, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/journal, which row pkg/remote/codec does not allow",
	}
	if got := check(pkgs, table); !slices.Equal(got, want) {
		t.Errorf("violations over the planted module %q, want %q", got, want)
	}
}

// The checker, one case per branch.
func TestImportCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkg  listed
		want []string
	}{
		{"an allowed reach", listed{
			path: module + "pkg/remote/codec",
			deps: []string{module + "pkg/remote", module + "pkg/transform", module + "pkg/metadata/entity/internal", "context"},
		}, nil},
		{"a library reached through an allowed package", listed{
			path: module + "pkg/remote/codec",
			deps: []string{module + "pkg/transform", "github.com/klauspost/compress/zstd"},
		}, []string{"pkg/remote/codec reaches github.com/klauspost/compress/zstd, which row pkg/remote/codec does not allow"}},
		{"a reach the row refuses", listed{
			path: module + "pkg/remote/codec",
			deps: []string{module + "pkg/remote", module + "pkg/transform/chain"},
		}, []string{"pkg/remote/codec reaches pkg/transform/chain, which row pkg/remote/codec does not allow"}},
		{"a package with no row", listed{path: module + "pkg/remote/gcs"}, []string{"pkg/remote/gcs has no row"}},
		{"a library the contract imports", listed{
			path: module + "pkg/remote",
			deps: []string{"github.com/aws/aws-sdk-go-v2/aws", "io"},
		}, []string{"pkg/remote reaches github.com/aws/aws-sdk-go-v2/aws, which row pkg/remote does not allow"}},
		{"a library a store imports", listed{
			path: module + "pkg/remote/s3",
			deps: []string{module + "pkg/remote", module + "pkg/remote/s3/internal/sign", "github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/smithy-go"},
		}, nil},
		{"a store reaching its fault transport", listed{
			path: module + "pkg/remote/s3",
			deps: []string{module + "pkg/remote", module + "pkg/remote/s3/s3fault"},
		}, []string{"pkg/remote/s3 reaches pkg/remote/s3/s3fault, which row pkg/remote/s3/... does not allow"}},
		{"the fault transport by its own row", listed{
			path: module + "pkg/remote/s3/s3fault",
			deps: []string{module + "pkg/remote"},
		}, []string{"pkg/remote/s3/s3fault reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
		{"a package beneath the fault transport by its row", listed{
			path: module + "pkg/remote/s3/s3fault/wire",
			deps: []string{module + "pkg/remote/s3/s3fault", module + "pkg/remote"},
		}, []string{"pkg/remote/s3/s3fault/wire reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
	} {
		if got := check([]listed{tc.pkg}, table); !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}
