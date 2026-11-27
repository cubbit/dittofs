package engine

import (
	"errors"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/cubbit/dittofs/"

// allowed is every import a package of the engine may have besides the
// standard library and the engine's own packages. Each entry names the
// packages under this directory it applies to; "*" is every one. The
// filesystem's package is not on it: the engine takes only its errors, from
// pkg/vfs/vfserr. Open state, and the node's content wiring that builds,
// starts and stops it with the engine, are the fixture's and the unit
// tests', which run them for real.
var allowed = map[string][]string{
	module + "pkg/journal":          {"*"},
	module + "pkg/health":           {"*"},
	module + "pkg/metadata/entity":  {"*"},
	module + "pkg/metadata/kvstore": {"*"},
	// The syncer's contract alone, for the store a share names: never its
	// implementation.
	module + "pkg/syncer": {"*"},
	// The work scheduler, a library every component instantiates, whose
	// figures Stats reports.
	module + "pkg/workqueue":         {"*"},
	module + "pkg/openstate":         {"enginetest", unitTests},
	module + "internal/node/content": {"enginetest", unitTests},
	module + "pkg/vfs/vfserr":        {"*"},
	// The test fixture composes the engine over the real journal, its seam and
	// profiler, the real store on an in-memory kv.KV, and a test clock. The
	// unit tests' start failure tests, and the fixture's rows of the self-fence,
	// put the fault-injecting kv.KV under that store.
	module + "pkg/journal/journaltest":          {"enginetest", internalTests},
	module + "pkg/journal/journaltest/kit":      {"enginetest"},
	module + "internal/prof":                    {"enginetest", unitTests},
	module + "pkg/metadata/kv":                  {"enginetest", unitTests},
	module + "pkg/metadata/kv/badgerkv":         {"enginetest", unitTests},
	module + "pkg/metadata/kv/keys":             {"enginetest", unitTests},
	module + "pkg/metadata/kv/kvtest":           {"enginetest", unitTests},
	module + "pkg/metadata/kvstore/kvstoretest": {"enginetest", unitTests},
	module + "pkg/sim":                          {"enginetest", unitTests, internalTests},
	// The operation sequence harness that the engine's fuzz target runs under.
	module + "test/fuzz": {unitTests},
	// The engine reads a full device's free space on Windows through the
	// system package the journal's platform layer already uses, rather than
	// wrapping the Win32 call by hand.
	"golang.org/x/sys/windows": {"."},
}

// unitTests names the engine's external test package, whose unit
// composition runs the engine over the fake journal, the real store on an
// in-memory kv.KV, a test clock and the profiler, and whose start failure
// tests put the fault-injecting kv.KV under that store. The engine's own
// files import none of them.
const unitTests = "._test"

// internalTests names the engine's test files in its own package, which use
// the test clock and a real journal fixture to check the version-limit probe.
const internalTests = "._internal"

// TestImportRule walks every package under this directory, test packages
// included, and fails on any import the engine may not have: anything but
// the standard library, the engine's own packages and the allowlist.
// Test files are checked like any other, and so are files built only for
// another platform.
func TestImportRule(t *testing.T) {
	checked := 0
	err := filepath.WalkDir(".", func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		rel := filepath.ToSlash(dir)
		imports, internal, xtest := packageImports(t, dir)
		for _, imp := range imports {
			if why := forbidden(rel, imp); why != "" {
				t.Errorf("%s: forbidden import %q: %s", rel, imp, why)
			}
		}
		for _, imp := range internal {
			// A package's tests may import what the package may, and the
			// test clock besides.
			if why := forbidden(rel+"_internal", imp); why != "" && forbidden(rel, imp) != "" {
				t.Errorf("%s (its tests): forbidden import %q: %s", rel, imp, why)
			}
		}
		for _, imp := range xtest {
			if why := forbidden(rel+"_test", imp); why != "" {
				t.Errorf("%s_test: forbidden import %q: %s", rel, imp, why)
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("no package checked")
	}
}

// TestImportRuleRefusesFilesystemAndOpenState pins the rule's edges: the
// filesystem's packages, the journal's test kit, open state, the node's
// content wiring and the syncer's implementation are refused, while the
// metadata packages the engine needs, the syncer's contract and the
// filesystem's errors are allowed.
func TestImportRuleRefusesFilesystemAndOpenState(t *testing.T) {
	for _, c := range []struct {
		path    string
		allowed bool
	}{
		{module + "pkg/vfs", false},
		{module + "pkg/vfs/vfstest", false},
		{module + "internal/vfs", false},
		{module + "pkg/journal/journaltest", false},
		{module + "pkg/journal", true},
		{module + "pkg/engine/enginetest", true},
		{module + "pkg/metadata/entity", true},
		{module + "pkg/metadata/kvstore", true},
		{module + "pkg/openstate", false},
		{module + "internal/node/content", false},
		{module + "pkg/vfs/vfserr", true},
		{module + "pkg/syncer", true},
		{module + "pkg/syncer/pool", false},
		{module + "pkg/workqueue", true},
		{"context", true},
	} {
		if got := forbidden(".", c.path) == ""; got != c.allowed {
			t.Errorf("importing %s: allowed %v, want %v", c.path, got, c.allowed)
		}
	}
}

// The syncer's contract, which the engine may import, imports nothing but
// the standard library and the remote tier's contract, which imports nothing
// but the standard library, so the allowance never reaches the syncer's
// implementation, or any other, through it.
func TestSyncerContractReachesNoImplementation(t *testing.T) {
	for _, c := range []struct{ dir, pkg, allowed string }{
		{filepath.Join("..", "syncer"), "pkg/syncer", module + "pkg/remote"},
		{filepath.Join("..", "remote"), "pkg/remote", ""},
	} {
		imports, _, _ := packageImports(t, c.dir)
		for _, imp := range imports {
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") && imp != c.allowed {
				t.Errorf("%s imports %q, which reaches past the contracts", c.pkg, imp)
			}
		}
	}
}

// forbidden returns why the package at rel, a slash-separated path relative
// to this directory, may not import path, or "" when it may.
func forbidden(rel, path string) string {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") || within(path, module+"pkg/engine") {
		return ""
	}
	for _, pkg := range allowed[path] {
		if pkg == "*" || pkg == rel {
			return ""
		}
	}
	return "not on the engine's allowlist"
}

// within reports whether path is root or a package below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// packageImports returns every import of the package in dir: its files built
// for this platform and its files built only for another platform, which
// go/build would otherwise leave unlisted; apart, those of its test files in
// the package; and apart, those of its external test package.
func packageImports(t *testing.T, dir string) (imports, internal, xtest []string) {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		var noGo *build.NoGoError
		if !errors.As(err, &noGo) {
			t.Fatalf("build.ImportDir %s: %v", dir, err)
		}
	}
	imports = append([]string{}, pkg.Imports...)
	fset := token.NewFileSet()
	for _, name := range pkg.IgnoredGoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Path.Value, name, err)
			}
			imports = append(imports, p)
		}
	}
	return imports, pkg.TestImports, pkg.XTestImports
}

// TestImportRuleKeepsTheCompositionInTests pins that the unit composition's
// store, clock and profiler are allowed in the engine's external test
// package and refused in the engine itself.
func TestImportRuleKeepsTheCompositionInTests(t *testing.T) {
	if why := forbidden(internalTests, module+"pkg/sim"); why != "" {
		t.Errorf("the engine's own tests may not import the test clock: %s", why)
	}
	for _, path := range []string{module + "pkg/metadata/kv/badgerkv", module + "pkg/metadata/kvstore/kvstoretest"} {
		if forbidden(internalTests, path) == "" {
			t.Errorf("the engine's own tests may import %s", path)
		}
	}
	for _, path := range []string{module + "pkg/sim", module + "pkg/metadata/kv/badgerkv", module + "pkg/metadata/kvstore/kvstoretest"} {
		if forbidden(".", path) == "" {
			t.Errorf("the engine may import %s", path)
		}
		if why := forbidden(unitTests, path); why != "" {
			t.Errorf("the unit composition may not import %s: %s", path, why)
		}
	}
}
