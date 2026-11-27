package blake3

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// table is the package's one row: a library outside the component set, it
// reaches no package of the module and no library but its backing, guts, with
// what guts imports. Its tests may reach the architecture tests' support,
// internal/prof among it, and the library's own API, which they compare
// against.
var table = []archtest.Row{{
	Pkg:      "internal/blake3",
	Libs:     backing,
	Tests:    archtest.Support,
	TestLibs: []string{"lukechampine.com/blake3", "lukechampine.com/blake3/bao"},
}}

// backing is lukechampine.com/blake3/guts and what it imports, transitively.
var backing = []string{"lukechampine.com/blake3/guts", "github.com/klauspost/cpuid/v2", "golang.org/x/sys/unix"}

// The package, its tests included, reaches only what its row allows, on every
// platform the walk lists.
func TestImportRule(t *testing.T) {
	graphs, err := archtest.Walk(".", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range graphs {
		if !slices.Contains(g.Roots, archtest.Module+"internal/blake3") {
			t.Fatalf("the walk listed %q, not internal/blake3", g.Roots)
		}
	}

	for _, v := range archtest.Check(table, graphs...) {
		t.Error(v)
	}

	unwalked, err := archtest.Unwalked(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range unwalked {
		t.Error(v)
	}
}

// The row refuses a component, reached directly or through a library, and
// the library's own API in production, which starts goroutines.
func TestImportRuleRefuses(t *testing.T) {
	self := archtest.Module + "internal/blake3"

	for _, tc := range []struct {
		name    string
		root    string
		imports map[string][]string
		want    []string
	}{
		{"the package as it is", self, map[string][]string{
			self:                           {"lukechampine.com/blake3/guts", "encoding/binary"},
			"lukechampine.com/blake3/guts": {"github.com/klauspost/cpuid/v2"},
		}, nil},
		{"importing the carver", self, map[string][]string{
			self: {archtest.Module + "pkg/carver"},
		}, []string{"internal/blake3 reaches pkg/carver, which row internal/blake3 does not allow"}},
		{"importing the library's own API", self, map[string][]string{
			self: {"lukechampine.com/blake3"},
		}, []string{"internal/blake3 imports lukechampine.com/blake3, which row internal/blake3 does not allow"}},
		{"its tests importing the library's own API", self + " [test]", map[string][]string{
			self + " [test]":          {"lukechampine.com/blake3", archtest.Module + "internal/prof"},
			"lukechampine.com/blake3": {"lukechampine.com/blake3/bao", "lukechampine.com/blake3/guts"},
		}, nil},
	} {
		g := archtest.Graph{Roots: []string{tc.root}, Imports: tc.imports}
		if got := archtest.Check(table, g); !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}
