package workqueue

import (
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
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
// golang.org/x/tools, which this rule refuses.
func TestImportRule(t *testing.T) {
	for _, bad := range violations(t, ".") {
		t.Error(bad)
	}
}

// violations walks the package in dir and returns one line per import it may
// not have.
func violations(t *testing.T, dir string) []string {
	t.Helper()

	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("build.ImportDir %s: %v", dir, err)
	}

	// IgnoredGoFiles are the files built only for another platform; parse
	// them so the rule holds on every platform, not only this one.
	refs := map[string][]string{
		"import":      pkg.Imports,
		"test import": slices.Concat(pkg.TestImports, pkg.XTestImports),
	}

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
		t.Fatalf("%s: no production import read: the walk checked nothing", dir)
	}

	var bad []string

	for kind, paths := range refs {
		for _, path := range paths {
			if reason := forbidden(kind == "test import", path); reason != "" {
				bad = append(bad, fmt.Sprintf("forbidden %s %q: %s", kind, path, reason))
			}
		}
	}

	slices.Sort(bad)

	return bad
}

// The walk reads a package's production files, those built only for another
// platform included, and refuses a component's import in either; a test file
// may import pkg/health.
func TestImportRuleWalksPlantedFiles(t *testing.T) {
	health := modulePath + "/pkg/health"

	for _, c := range []struct {
		name, file, body string
		want             []string
	}{
		{"production file", "x.go", `import _ "` + health + `"`,
			[]string{`forbidden import "` + health + `": the scheduler may import only the standard library`}},
		{"other platform", "x_plan9.go", `import _ "` + health + `"`,
			[]string{`forbidden import "` + health + `": the scheduler may import only the standard library`}},
		{"test file", "x_test.go", `import _ "` + health + `"`, nil},
	} {
		dir := t.TempDir()
		write := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\n\n"+body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		write("p.go", `import _ "context"`)
		write(c.file, c.body)

		if got := violations(t, dir); !slices.Equal(got, c.want) {
			t.Errorf("%s: violations %q, want %q", c.name, got, c.want)
		}
	}
}

// forbidden returns why the package may not import path, from a test file when
// test is set, or "" when it may.
func forbidden(test bool, path string) string {
	switch {
	case path == "C":
		return "cgo reaches code no import names"
	case path == "unsafe" && !test:
		return "unsafe and go:linkname reach other packages' symbols with no import of them"
	case isStdlib(path):
		return ""
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

// isStdlib reports whether path is a package of the toolchain's standard
// library: one whose source is under GOROOT, so a module path without a dot is
// not mistaken for one.
func isStdlib(path string) bool {
	fi, err := os.Stat(filepath.Join(build.Default.GOROOT, "src", filepath.FromSlash(path)))

	return err == nil && fi.IsDir()
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
		{false, "internal/abi", false},
		{false, "C", true},
		{false, "unsafe", true},
		{true, "unsafe", false},
		{false, "evil", true},
		{false, modulePath + "/pkg/health", true},
		{false, modulePath + "/pkg/metadata/entity", true},
		{false, modulePath + "/pkg/sim", true},
		{false, "golang.org/x/sys/unix", true},
		{true, modulePath + "/pkg/health", false},
		{true, modulePath + "/pkg/sim", false},
		{true, modulePath + "/pkg/metadata/entity", true},
	} {
		if got := forbidden(c.test, c.path) != ""; got != c.refused {
			t.Errorf("forbidden(test=%v, %q) refused = %v, want %v", c.test, c.path, got, c.refused)
		}
	}
}
