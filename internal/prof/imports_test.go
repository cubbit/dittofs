package prof

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The profiler imports the standard library only, test files included: the
// journal, the engine and the adapters all read it from their tests, so a
// dependency taken here would reach each of them.
func TestImportsStandardLibraryOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go file found here: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !standard(path) {
				t.Errorf("%s imports %q: the profiler may import the standard library only", name, path)
			}
		}
	}
}

// A path outside the standard library is reported, so the check above cannot
// pass by matching nothing.
func TestStandardRefusesAModulePath(t *testing.T) {
	for path, want := range map[string]bool{
		"runtime/pprof":                     true,
		"testing":                           true,
		"github.com/cubbit/dittofs/pkg/sim": false,
		"golang.org/x/sys/unix":             false,
	} {
		if got := standard(path); got != want {
			t.Errorf("standard(%q) = %v, want %v", path, got, want)
		}
	}
}

// standard reports whether path is a standard library package: its first
// element has no dot.
func standard(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
