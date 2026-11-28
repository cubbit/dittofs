package carver

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// table is every package of the carver and what it may reach (RFC 2 §1.1,
// RFC 0 §1.2): the root, the contract, its own settings value and the entity
// package; the settings value nothing; carve and the fold the root, with the
// settings value it brings, and the entity package with the libraries it
// imports, BLAKE3 among them. Never another component's implementation, never
// v1's carver. A test may reach besides the package it tests and the
// architecture tests' support. A row is an upper bound: a
// package that reaches less passes.
var table = []archtest.Row{
	{Pkg: "pkg/carver", Reach: []string{"pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: archtest.Support},
	{Pkg: "pkg/carver/internal/params", Tests: archtest.Support},
	{Pkg: "pkg/carver/carve", Reach: []string{"pkg/carver", "pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: archtest.Support},
	{Pkg: "pkg/carver/assembler", Reach: []string{"pkg/carver", "pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: archtest.Support},
}

// Every package under pkg/carver, its tests included, reaches only what its
// row allows, on every platform the walk lists.
func TestImportRule(t *testing.T) {
	graphs, err := archtest.Walk(".", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range graphs {
		for _, want := range []string{"pkg/carver", "pkg/carver [test]", "pkg/carver/internal/params", "pkg/carver/carve", "pkg/carver/assembler"} {
			if !slices.Contains(g.Roots, archtest.Module+want) {
				t.Fatalf("the walk listed %q, not %s", g.Roots, want)
			}
		}
	}

	for _, v := range archtest.Check(table, graphs...) {
		t.Error(v)
	}

	unwalked, err := archtest.Unwalked(".", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range unwalked {
		t.Error(v)
	}
}

// A file no target builds is named, since go list leaves it out of every
// graph: one behind a tag the walk never sets, one built only without cgo, and
// one built for another platform by its file name. One built for a walked
// platform, with cgo, or without the race detector is not.
func TestImportRuleNamesUnwalkedFiles(t *testing.T) {
	got, err := archtest.Unwalked("testdata/walk/pkg/carver", true)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"testdata/walk/pkg/carver/carve/cut_nocgo.go is built for no platform the walk lists",
		"testdata/walk/pkg/carver/carve/tagged.go is built for no platform the walk lists",
		"testdata/walk/pkg/carver/carve/x_plan9.go is built for no platform the walk lists",
	}
	if !slices.Equal(got, want) {
		t.Errorf("unwalked files %q, want %q", got, want)
	}
}

// The walk over a planted module reports, with the chain, a store and a
// library the root reaches through the entity package, and carve through the
// root; a store an internal test file imports, and v1's carver an external
// test imports, which a walk of production files only misses; and a package
// with no row.
func TestImportRuleWalksTestsTransitively(t *testing.T) {
	graphs, err := archtest.Walk("testdata/walk/pkg/carver", true)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"pkg/carver reaches example.com/lib through pkg/metadata/entity, which row pkg/carver does not allow",
		"pkg/carver reaches pkg/metadata/kvstore through pkg/metadata/entity, which row pkg/carver does not allow",
		"pkg/carver/carve [test] reaches example.com/lib through pkg/carver -> pkg/metadata/entity, which row pkg/carver/carve does not allow",
		"pkg/carver/carve [test] reaches pkg/metadata/kvstore, which row pkg/carver/carve does not allow",
		"pkg/carver/carve reaches example.com/lib through pkg/carver -> pkg/metadata/entity, which row pkg/carver/carve does not allow",
		"pkg/carver/carve reaches pkg/metadata/kvstore through pkg/carver -> pkg/metadata/entity, which row pkg/carver/carve does not allow",
		"pkg/carver/carve_test [test] reaches example.com/lib through pkg/carver/carve [test] -> pkg/carver -> pkg/metadata/entity, which row pkg/carver/carve does not allow",
		"pkg/carver/carve_test [test] reaches pkg/block/carver, which row pkg/carver/carve does not allow",
		"pkg/carver/carve_test [test] reaches pkg/metadata/kvstore through pkg/carver/carve [test], which row pkg/carver/carve does not allow",
		"pkg/carver/section has no row",
	}
	if got := archtest.Check(table, graphs...); !slices.Equal(got, want) {
		t.Errorf("violations over the planted module %q, want %q", got, want)
	}
}

// m names a package of the module by its module-relative path.
func m(rel string) string { return archtest.Module + rel }

// The rule over planted graphs, each import refused with its reason: an
// allowlist that grew a wildcard lets one through, and a walk of direct
// imports misses the store the entity package brings.
func TestImportRuleRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    string
		imports map[string][]string
		want    []string
	}{
		{"the root as it is", m("pkg/carver"), map[string][]string{
			m("pkg/carver"):                 {m("pkg/carver/internal/params"), m("pkg/metadata/entity"), "errors", "time"},
			m("pkg/metadata/entity"):        {"github.com/google/uuid", "lukechampine.com/blake3", "bytes"},
			m("pkg/carver/internal/params"): nil,
		}, nil},
		{"the root importing the journal", m("pkg/carver"), map[string][]string{
			m("pkg/carver"): {m("pkg/journal")},
		}, []string{"pkg/carver reaches pkg/journal, which row pkg/carver does not allow"}},
		{"carve importing the chunker's settings section", m("pkg/carver/carve"), map[string][]string{
			m("pkg/carver/carve"): {m("pkg/carver"), m("pkg/carver/section")},
		}, []string{"pkg/carver/carve reaches pkg/carver/section, which row pkg/carver/carve does not allow"}},
		{"a test importing a store", m("pkg/carver/carve [test]"), map[string][]string{
			m("pkg/carver/carve [test]"): {m("pkg/carver"), m("internal/archtest"), m("pkg/metadata/kvstore")},
		}, []string{"pkg/carver/carve [test] reaches pkg/metadata/kvstore, which row pkg/carver/carve does not allow"}},
		{"production importing the architecture tests' support", m("pkg/carver/carve"), map[string][]string{
			m("pkg/carver/carve"): {m("internal/archtest")},
		}, []string{"pkg/carver/carve reaches internal/archtest, which row pkg/carver/carve does not allow"}},
		{"the fold importing v1's carver", m("pkg/carver/assembler"), map[string][]string{
			m("pkg/carver/assembler"): {m("pkg/carver"), m("pkg/block/carver")},
		}, []string{"pkg/carver/assembler reaches pkg/block/carver, which row pkg/carver/assembler does not allow"}},
		{"the root reaching a store through the entity package", m("pkg/carver"), map[string][]string{
			m("pkg/carver"):          {m("pkg/metadata/entity")},
			m("pkg/metadata/entity"): {m("pkg/metadata/kvstore")},
		}, []string{"pkg/carver reaches pkg/metadata/kvstore through pkg/metadata/entity, which row pkg/carver does not allow"}},
		{"the root reaching a library through the entity package", m("pkg/carver"), map[string][]string{
			m("pkg/carver"):          {m("pkg/metadata/entity")},
			m("pkg/metadata/entity"): {"github.com/klauspost/compress/zstd"},
		}, []string{"pkg/carver reaches github.com/klauspost/compress/zstd through pkg/metadata/entity, which row pkg/carver does not allow"}},
		{"the root reaching a library through BLAKE3", m("pkg/carver"), map[string][]string{
			m("pkg/carver"):           {m("pkg/metadata/entity")},
			m("pkg/metadata/entity"):  {"lukechampine.com/blake3"},
			"lukechampine.com/blake3": {"github.com/klauspost/compress/zstd"},
		}, []string{"pkg/carver reaches github.com/klauspost/compress/zstd through pkg/metadata/entity -> lukechampine.com/blake3, which row pkg/carver does not allow"}},
		{"the settings value importing the entity package", m("pkg/carver/internal/params"), map[string][]string{
			m("pkg/carver/internal/params"): {m("pkg/metadata/entity")},
		}, []string{"pkg/carver/internal/params reaches pkg/metadata/entity, which row pkg/carver/internal/params does not allow"}},
		{"the fold importing a library", m("pkg/carver/assembler"), map[string][]string{
			m("pkg/carver/assembler"): {m("pkg/carver"), "github.com/klauspost/compress/zstd"},
		}, []string{"pkg/carver/assembler imports github.com/klauspost/compress/zstd, which row pkg/carver/assembler does not allow"}},
		{"the settings value importing BLAKE3", m("pkg/carver/internal/params"), map[string][]string{
			m("pkg/carver/internal/params"): {"lukechampine.com/blake3"},
		}, []string{"pkg/carver/internal/params imports lukechampine.com/blake3, which row pkg/carver/internal/params does not allow"}},
	} {
		g := archtest.Graph{Roots: []string{tc.root}, Imports: tc.imports}
		if got := archtest.Check(table, g); !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}
