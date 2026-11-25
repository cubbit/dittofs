package kvstore

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// componentPackages are the implementations the store may not import: the
// settings component hands the store what it needs as values at Open.
var componentPackages = []string{
	"github.com/cubbit/dittofs/pkg/settings",
}

// componentImports lists each .go file under dir, tests and subpackages
// included and testdata left out, plus the planted files, that imports one
// of componentPackages, as file: path.
func componentImports(dir string, planted ...string) ([]string, error) {
	paths := slices.Clone(planted)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			return filepath.SkipDir
		case !d.IsDir() && strings.HasSuffix(p, ".go"):
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
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

			if slices.ContainsFunc(componentPackages, func(c string) bool {
				return path == c || strings.HasPrefix(path, c+"/")
			}) {
				bad = append(bad, filepath.Base(p)+": "+path)
			}
		}
	}

	return bad, nil
}

// The store imports no settings package; a planted file that does is caught.
func TestStoreImportsNoSettings(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name    string
		planted []string
		want    []string
	}{
		{"the package", nil, nil},
		{
			"a file importing the settings table",
			[]string{filepath.Join("testdata", "imports", "settings.go")},
			[]string{"settings.go: github.com/cubbit/dittofs/pkg/settings"},
		},
	} {
		got, err := componentImports(".", tc.planted...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: settings imports %q, want %q", tc.name, got, tc.want)
		}
	}
}
