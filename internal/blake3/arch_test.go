package blake3

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// The package declares no package-level variable, no interface and no
// function but methods, so it holds no state and stays a library: a planted
// variable and a planted interface are each reported, and the files as they
// are report nothing.
func TestNoStateNoInterface(t *testing.T) {
	imp := archtest.ExportImporter(".")

	for _, tc := range []struct {
		planted string
		want    []string
	}{
		{"", nil},
		{"state.go", []string{"var planted is state"}},
		{"iface.go", []string{"type Planted is an interface"}},
	} {
		var planted []string
		if tc.planted != "" {
			planted = []string{filepath.Join("testdata", "arch", tc.planted)}
		}

		got, err := archtest.Impurities(imp, archtest.Module+"internal/blake3", ".", archtest.Purity{}, planted...)
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(got, tc.want) {
			t.Errorf("planted %q: impurities %q, want %q", tc.planted, got, tc.want)
		}
	}
}
