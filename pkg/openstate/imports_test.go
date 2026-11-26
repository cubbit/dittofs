package openstate_test

import (
	"errors"
	"go/build"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/cubbit/dittofs/"

// contracts is every package of the module outside pkg/openstate that open
// state may reach through any chain of imports of its non-test files: the
// store's value types and errors and the adapters' capability bits, never
// the metadata store or the engine, so the store's views reach the table
// only through the interfaces Deps declares.
var contracts = []string{
	module + "pkg/metadata/entity",
	module + "pkg/metadata/errors",
	module + "pkg/vfs/caps",
}

// TestImportsOnlyContracts walks every package under pkg/openstate, follows
// the imports of their non-test files for every platform through the
// module, and fails on each implementation reached.
func TestImportsOnlyContracts(t *testing.T) {
	for _, bad := range implementationsReached(t, "../..", "pkg/openstate") {
		t.Errorf("pkg/openstate reaches an implementation: %s", bad)
	}
}

// TestImportRuleRefusesImplementations pins the rule's edges.
func TestImportRuleRefusesImplementations(t *testing.T) {
	for _, c := range []struct {
		path    string
		allowed bool
	}{
		{module + "pkg/metadata/kvstore", false},
		{module + "pkg/metadata/kv", false},
		{module + "pkg/engine", false},
		{module + "pkg/vfs", false},
		{module + "pkg/openstate/state", true},
		{module + "pkg/metadata/entity", true},
		{"context", true},
	} {
		if got := allowed(c.path, "pkg/openstate"); got != c.allowed {
			t.Errorf("reaching %s: allowed %v, want %v", c.path, got, c.allowed)
		}
	}
}

// allowed reports whether a package under own may reach path.
func allowed(path, own string) bool {
	first, _, _ := strings.Cut(path, "/")
	rel, inModule := strings.CutPrefix(path, module)
	return !strings.Contains(first, ".") || !inModule || within(rel, own) || slices.Contains(contracts, path)
}

func within(rel, root string) bool { return rel == root || strings.HasPrefix(rel, root+"/") }

// implementationsReached returns each package outside own and contracts that
// own's packages reach, with the chain that reaches it. root is the module's
// directory.
func implementationsReached(t *testing.T, root, own string) []string {
	t.Helper()
	ctx := build.Default
	ctx.UseAllFiles = true // every platform's files, and every build tag's
	via := map[string]string{}
	var queue, bad []string
	err := filepath.WalkDir(filepath.Join(root, own), func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); d.Name() == "testdata" {
			return filepath.SkipDir
		}
		via[module+rel] = ""
		queue = append(queue, module+rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(queue) == 0 {
		t.Fatal("no package checked")
	}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		pkg, err := ctx.ImportDir(filepath.Join(root, strings.TrimPrefix(path, module)), 0)
		if err != nil {
			var noGo *build.NoGoError
			if !errors.As(err, &noGo) {
				t.Fatalf("import %s: %v", path, err)
			}
		}
		for _, imp := range pkg.Imports {
			if _, seen := via[imp]; seen || !strings.HasPrefix(imp, module) {
				continue
			}
			via[imp] = path
			if !allowed(imp, own) {
				chain := imp
				for p := path; p != ""; p = via[p] {
					chain += " <- " + p
				}
				bad = append(bad, chain)
				continue
			}
			queue = append(queue, imp)
		}
	}
	return bad
}
