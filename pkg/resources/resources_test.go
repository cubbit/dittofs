package resources

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The classes are RFC 24's four, ascending: small, directory, transfer, large.
func TestClassSizes(t *testing.T) {
	want := [...]int{4 << 10, 64 << 10, 1 << 20, 8 << 20}
	if classes != want {
		t.Fatalf("classes = %v, want %v", classes, want)
	}
}

// pkg/resources is a leaf any component can take: it imports only the
// standard library and the metrics registry.
func TestImportsOnlyStdlibAndMetrics(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			stdlib := !strings.Contains(strings.Split(p, "/")[0], ".")
			if !stdlib && p != "github.com/cubbit/dittofs/pkg/metrics" {
				t.Errorf("%s imports %s; pkg/resources imports only the standard library and pkg/metrics", f, p)
			}
		}
	}
}
