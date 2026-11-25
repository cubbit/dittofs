package archtest

import (
	"slices"
	"strings"
	"testing"
)

// TestEntityLibs: EntityLibs is exactly what the entity package imports from outside the
// standard library and the module, on every platform the walk lists.
func TestEntityLibs(t *testing.T) {
	graphs, err := Walk("../../pkg/metadata/entity", false)
	if err != nil {
		t.Fatal(err)
	}

	var got []string

	for _, g := range graphs {
		for _, imp := range g.Imports[Module+"pkg/metadata/entity"] {
			if isLibrary(imp) {
				got = append(got, imp)
			}
		}
	}

	slices.Sort(got)

	if got = slices.Compact(got); !slices.Equal(got, EntityLibs()) {
		t.Errorf("entity's libraries: got %q, want EntityLibs %q", got, EntityLibs())
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
