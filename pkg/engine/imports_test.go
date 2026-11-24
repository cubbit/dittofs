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
// pkg/vfs/vfserr. Open state is enginewire's, which
// builds, starts and stops it with the engine, and its test's, the
// fixture's and the unit tests', which run it for real.
var allowed = map[string][]string{
	module + "pkg/journal":          {"*"},
	module + "pkg/health":           {"*"},
	module + "pkg/metadata/entity":  {"*"},
	module + "pkg/metadata/kvstore": {"*"},
	module + "pkg/openstate":        {"enginetest", "enginewire", unitTests, wireTests},
	module + "pkg/vfs/vfserr":       {"*"},
	// The test fixture composes the engine over the real journal, its seam and
	// profiler, the real store on an in-memory kv.KV, and a test clock. The
	// unit tests' start failure tests, and the fixture's rows of the self-fence,
	// put the fault-injecting kv.KV under that store. enginewire's test runs
	// its calls over the same real store, journal and clock, under the
	// profiler.
	module + "pkg/journal/journaltest":          {"enginetest", internalTests, wireTests},
	module + "pkg/journal/journaltest/kit":      {"enginetest", wireTests},
	module + "pkg/journal/journaltest/raw":      {wireTests},
	module + "internal/prof":                    {"enginetest", unitTests, wireTests},
	module + "pkg/metadata/kv":                  {"enginetest", unitTests, wireTests},
	module + "pkg/metadata/kv/badgerkv":         {"enginetest", unitTests, wireTests},
	module + "pkg/metadata/kv/keys":             {"enginetest", unitTests},
	module + "pkg/metadata/kv/kvtest":           {"enginetest", unitTests},
	module + "pkg/metadata/kvstore/kvstoretest": {"enginetest", unitTests, wireTests},
	module + "pkg/sim":                          {"enginetest", unitTests, internalTests, wireTests},
	// enginewire adapts the host's tracer to each journal's own Tracer
	// option, so its spans and log lines name the journal by one identity;
	// the node hands the composition the only tracer the engine sees. Its
	// test drives the same adapter over the real SDK's in-memory recorder.
	"go.opentelemetry.io/otel/trace":               {"enginewire", wireTests},
	"go.opentelemetry.io/otel/attribute":           {"enginewire"},
	"go.opentelemetry.io/otel/codes":               {"enginewire"},
	"go.opentelemetry.io/otel/sdk/trace":           {wireTests},
	"go.opentelemetry.io/otel/sdk/trace/tracetest": {wireTests},
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

// wireTests names enginewire's external test package.
const wireTests = "enginewire_test"

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
// filesystem's packages, the journal's test kit and open state are refused,
// while the metadata packages the engine needs and the filesystem's errors
// are allowed.
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
		{module + "pkg/vfs/vfserr", true},
		{"context", true},
	} {
		if got := forbidden(".", c.path) == ""; got != c.allowed {
			t.Errorf("importing %s: allowed %v, want %v", c.path, got, c.allowed)
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
