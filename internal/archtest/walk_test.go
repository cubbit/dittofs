package archtest

import (
	"slices"
	"strings"
	"testing"
)

// TestEntityLibs: EntityLibs is exactly the libraries the entity package
// reaches, transitively, through the module's packages and other libraries, on
// every platform the walk lists.
func TestEntityLibs(t *testing.T) {
	graphs, err := Walk("../../pkg/metadata/entity", false)
	if err != nil {
		t.Fatal(err)
	}

	var got []string

	for _, g := range graphs {
		seen := map[string]bool{}

		for queue := []string{Module + "pkg/metadata/entity"}; len(queue) > 0; queue = queue[1:] {
			for _, imp := range g.Imports[queue[0]] {
				if seen[imp] {
					continue
				}

				seen[imp] = true
				queue = append(queue, imp)

				if isLibrary(imp) {
					got = append(got, imp)
				}
			}
		}
	}

	slices.Sort(got)

	if got = slices.Compact(got); !slices.Equal(got, EntityLibs()) {
		t.Errorf("entity's libraries: got %q, want EntityLibs %q", got, EntityLibs())
	}
}

// parse keeps a package's own test variants as roots, by the [test] name, and
// keeps a package recompiled for another package's test binary apart from its
// own variant and out of the roots: its imports are that binary's, not its own.
func TestParseKeepsRecompiledVariantsApart(t *testing.T) {
	out := []byte(`m/x|false|m/z
m/y|false|m/x
m/x [m/x.test]|false|m/z,m/w
m/y [m/x.test]|false|m/x [m/x.test]
m/x_test [m/x.test]|false|m/x [m/x.test],m/y [m/x.test]
m/x.test|false|m/x [m/x.test],m/x_test [m/x.test],testing/internal/testdeps
`)

	g, err := parse(out)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"m/x", "m/y", "m/x [test]", "m/x_test [test]"}; !slices.Equal(g.Roots, want) {
		t.Errorf("roots: got %q, want %q", g.Roots, want)
	}

	if got, want := g.Imports["m/y [test]"], []string(nil); !slices.Equal(got, want) {
		t.Errorf("imports of m/y [test]: got %q, want %q, since y has no test of its own", got, want)
	}

	if got, want := g.Imports["m/y [m/x.test]"], []string{"m/x [test]"}; !slices.Equal(got, want) {
		t.Errorf("imports of m/y [m/x.test]: got %q, want %q", got, want)
	}
}

// Support is this package and exactly what it imports from the module, on
// every platform the walk lists.
func TestSupport(t *testing.T) {
	graphs, err := Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}

	got := []string{"internal/archtest"}

	for _, g := range graphs {
		for dep := range g.Imports {
			if rel, ours := strings.CutPrefix(dep, Module); ours && rel != "internal/archtest" {
				got = append(got, rel)
			}
		}
	}

	slices.Sort(got)

	if got = slices.Compact(got); !slices.Equal(got, Support) {
		t.Errorf("archtest's module packages: got %q, want Support %q", got, Support)
	}
}

// A row's TestLibs let its tests reach a library its production code may not.
func TestCheckTestLibs(t *testing.T) {
	table := []Row{{Pkg: "p", TestLibs: []string{"example.com/req"}}}

	for _, tc := range []struct {
		root string
		want []string
	}{
		{Module + "p [test]", nil},
		{Module + "p", []string{"p imports example.com/req, which row p does not allow"}},
	} {
		g := Graph{Roots: []string{tc.root}, Imports: map[string][]string{tc.root: {"example.com/req"}}}
		if got := Check(table, g); !slices.Equal(got, tc.want) {
			t.Errorf("%s importing example.com/req: violations %q, want %q", tc.root, got, tc.want)
		}
	}
}

// Unwalked skips what go list ignores, a directory whose name starts with _
// or ., but not the directory it is given, even when that is ".". A tagged
// test file is reported only for a walk with tests, the one that reads it.
func TestUnwalkedSkipsIgnoredDirectories(t *testing.T) {
	t.Chdir("testdata/unwalked")

	for tests, want := range map[bool][]string{
		false: {"tagged.go is built for no platform the walk lists"},
		true:  {"tagged.go is built for no platform the walk lists", "tagged_test.go is built for no platform the walk lists"},
	} {
		got, err := Unwalked(".", tests)
		if err != nil {
			t.Fatalf("Unwalked over testdata/unwalked, tests %v: %v", tests, err)
		}

		if !slices.Equal(got, want) {
			t.Errorf("unwalked files, tests %v: got %q, want %q", tests, got, want)
		}
	}
}
