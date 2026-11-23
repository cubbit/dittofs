package settings

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// storePackages are the store's packages: pkg/settings reads the store only
// through what its caller hands it.
var storePackages = []string{
	"github.com/cubbit/dittofs/pkg/metadata/kvstore",
	"github.com/cubbit/dittofs/pkg/metadata/kv",
}

// storeImports lists each store package dir's non-test .go files, plus the
// planted files, import, as file: path.
func storeImports(dir string, planted ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	paths := slices.Clone(planted)

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}

	var bad []string

	for _, p := range paths {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, err
			}

			if slices.ContainsFunc(storePackages, func(store string) bool {
				return path == store || strings.HasPrefix(path, store+"/")
			}) {
				bad = append(bad, filepath.Base(p)+": "+path)
			}
		}
	}

	return bad, nil
}

// pkg/settings imports no store package; a planted file that does is caught.
func TestSettingsImportsNoStore(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name    string
		planted []string
		want    []string
	}{
		{"the package", nil, nil},
		{
			"a file importing the store",
			[]string{filepath.Join("testdata", "imports", "store.go")},
			[]string{"store.go: github.com/cubbit/dittofs/pkg/metadata/kvstore"},
		},
	} {
		got, err := storeImports(".", tc.planted...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: store imports %q, want %q", tc.name, got, tc.want)
		}
	}
}
