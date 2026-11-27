package carver

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// table is every package of the carver and what it may reach (RFC 2 §1.1,
// RFC 0 §1.2): the root, the contract, its own settings value and the entity
// package; the settings value nothing; the chunker the settings value alone;
// carve and the fold the root, with the settings value it brings, and the
// entity package with the libraries it imports, BLAKE3 among them. carve also
// reaches internal/blake3, whose guts backing is among the entity package's
// libraries. Never another component's implementation, never v1's carver.
// Besides the package it tests, a test may reach the architecture tests'
// support and carvetest, the carver's test components, which no production row
// admits; carvetest reaches the standard library alone, and its tests BLAKE3,
// internal/blake3 and internal/prof besides. A row is an upper bound: a
// package that reaches less passes.
var table = []archtest.Row{
	{Pkg: "pkg/carver", Reach: []string{"pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: carverTests},
	{Pkg: "pkg/carver/internal/params", Tests: carverTests},
	{Pkg: "pkg/carver/carve", Reach: []string{"internal/blake3", "pkg/carver", "pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: carverTests},
	{Pkg: "pkg/carver/internal/chunker", Reach: []string{"pkg/carver/internal/params"}, Tests: chunkerTests, TestLibs: archtest.EntityLibs()},
	{Pkg: "pkg/carver/assembler", Reach: []string{"pkg/carver", "pkg/carver/internal/params", "pkg/metadata/entity"}, Libs: archtest.EntityLibs(), Tests: carverTests},
	{Pkg: "pkg/carver/internal/carvetest", Tests: []string{"internal/prof", "internal/blake3"}, TestLibs: blake3Libs},
}

// carverTests is what every carver package's tests may reach besides.
var carverTests = append(slices.Clone(archtest.Support), "pkg/carver/internal/carvetest")

// chunkerTests is what the chunker's tests may reach besides: the carver's
// tests' reach, and carve with what it reaches, so a test cuts under the
// settings the carver's constructor builds.
var chunkerTests = append(slices.Clone(carverTests), "pkg/carver", "pkg/carver/carve", "pkg/metadata/entity")

// blake3Libs is lukechampine.com/blake3 with what it imports, guts among
// them, which internal/blake3 is backed by: the entity package's libraries but
// its UUID one.
var blake3Libs = slices.DeleteFunc(archtest.EntityLibs(), func(lib string) bool { return lib == "github.com/google/uuid" })

// Every package under pkg/carver, its tests included, reaches only what its
// row allows, on every platform the walk lists.
func TestImportRule(t *testing.T) {
	graphs, err := archtest.Walk(".", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range graphs {
		for _, want := range []string{"pkg/carver", "pkg/carver [test]", "pkg/carver/internal/params", "pkg/carver/carve", "pkg/carver/internal/chunker", "pkg/carver/internal/chunker [test]", "pkg/carver/assembler", "pkg/carver/internal/carvetest", "pkg/carver/internal/carvetest [test]"} {
			if !slices.Contains(g.Roots, archtest.Module+want) {
				t.Fatalf("the walk listed %q, not %s", g.Roots, want)
			}
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

// A file no target builds is named, since go list leaves it out of every
// graph: one behind a tag the walk never sets, one built only without cgo, and
// one built for another platform by its file name. One built for a walked
// platform, with cgo, or without the race detector is not.
func TestImportRuleNamesUnwalkedFiles(t *testing.T) {
	got, err := archtest.Unwalked("testdata/walk/pkg/carver")
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
		{"carve importing the settings package", m("pkg/carver/carve"), map[string][]string{
			m("pkg/carver/carve"): {m("pkg/carver"), m("internal/blake3"), m("pkg/settings")},
		}, []string{"pkg/carver/carve reaches pkg/settings, which row pkg/carver/carve does not allow"}},
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
		{"carve importing the test components", m("pkg/carver/carve"), map[string][]string{
			m("pkg/carver/carve"): {m("pkg/carver"), m("pkg/carver/internal/carvetest")},
		}, []string{"pkg/carver/carve reaches pkg/carver/internal/carvetest, which row pkg/carver/carve does not allow"}},
		{"the test components importing BLAKE3", m("pkg/carver/internal/carvetest"), map[string][]string{
			m("pkg/carver/internal/carvetest"): {"lukechampine.com/blake3"},
		}, []string{"pkg/carver/internal/carvetest imports lukechampine.com/blake3, which row pkg/carver/internal/carvetest does not allow"}},
		{"the test components importing the root", m("pkg/carver/internal/carvetest"), map[string][]string{
			m("pkg/carver/internal/carvetest"): {m("pkg/carver")},
		}, []string{"pkg/carver/internal/carvetest reaches pkg/carver, which row pkg/carver/internal/carvetest does not allow"}},
		{"the test components' tests importing the root", m("pkg/carver/internal/carvetest [test]"), map[string][]string{
			m("pkg/carver/internal/carvetest [test]"): {m("pkg/carver")},
		}, []string{"pkg/carver/internal/carvetest [test] reaches pkg/carver, which row pkg/carver/internal/carvetest does not allow"}},
		{"the test components' tests importing BLAKE3", m("pkg/carver/internal/carvetest [test]"), map[string][]string{
			m("pkg/carver/internal/carvetest [test]"): {"lukechampine.com/blake3", m("internal/prof"), m("internal/blake3")},
			m("internal/blake3"):                      {"lukechampine.com/blake3/guts"},
		}, nil},
		{"the test components importing the shared hasher", m("pkg/carver/internal/carvetest"), map[string][]string{
			m("pkg/carver/internal/carvetest"): {m("internal/blake3")},
		}, []string{"pkg/carver/internal/carvetest reaches internal/blake3, which row pkg/carver/internal/carvetest does not allow"}},
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
