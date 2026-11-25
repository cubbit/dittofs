package workqueue

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/cubbit/dittofs"

// TestImportRule fails on any import the package may not have. Its production
// files, those built for every platform, import the standard library only, so
// no component, contract or implementation reaches another component through
// the scheduler. Its test files may also import the packages whose types they
// check against the declared interfaces, and the export-data importer.
//
// Deliberately go/build and not go/packages: the latter lives in
// golang.org/x/tools, outside the standard library.
func TestImportRule(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}

	// IgnoredGoFiles are the files built only for another platform; parse
	// them so the rule holds on every platform, not only this one.
	refs := map[string][]string{"test import": append(pkg.TestImports, pkg.XTestImports...)}
	refs["import"] = pkg.Imports

	fset := token.NewFileSet()
	for _, name := range pkg.IgnoredGoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		kind := "import"
		if strings.HasSuffix(name, "_test.go") {
			kind = "test import"
		}

		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Path.Value, name, err)
			}

			refs[kind] = append(refs[kind], path)
		}
	}

	if len(refs["import"]) == 0 {
		t.Fatal("no production import read: the walk checked nothing")
	}

	for kind, paths := range refs {
		for _, path := range paths {
			if reason := forbidden(kind == "test import", path); reason != "" {
				t.Errorf("forbidden %s %q: %s", kind, path, reason)
			}
		}
	}
}

// forbidden returns why the package may not import path, from a test file when
// test is set, or "" when it may.
func forbidden(test bool, path string) string {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return "" // the standard library: its first element carries no dot
	}

	if test {
		switch path {
		case modulePath + "/pkg/workqueue", modulePath + "/pkg/health",
			modulePath + "/pkg/sim", modulePath + "/internal/archtest":
			return ""
		}

		return "the tests may import the standard library, the package, pkg/health, pkg/sim and internal/archtest only"
	}

	return "the scheduler may import only the standard library"
}

// The rule refuses a component's package in production code, the watchdog's
// and the metadata contract's included, and lets the tests reach the types
// they check against.
func TestImportRuleRefuses(t *testing.T) {
	for _, c := range []struct {
		test    bool
		path    string
		refused bool
	}{
		{false, "context", false},
		{false, modulePath + "/pkg/health", true},
		{false, modulePath + "/pkg/metadata/entity", true},
		{false, modulePath + "/pkg/sim", true},
		{false, "golang.org/x/sys/unix", true},
		{true, modulePath + "/pkg/health", false},
		{true, modulePath + "/pkg/sim", false},
		{true, modulePath + "/pkg/metadata/entity", true},
		{true, modulePath + "/pkg/healthz", true},
	} {
		if got := forbidden(c.test, c.path) != ""; got != c.refused {
			t.Errorf("forbidden(test=%v, %q) refused = %v, want %v", c.test, c.path, got, c.refused)
		}
	}
}
