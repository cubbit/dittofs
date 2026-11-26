package archtest

import (
	"slices"
	"testing"
)

// A function declared once per platform is checked in each file rather than
// failing the type check as redeclared, and a generator of another package is
// not part of the package.
func TestImpuritiesReadsEveryPlatformsFile(t *testing.T) {
	got, err := Impurities(ExportImporter("."), "example.com/platforms", "testdata/platforms", Purity{})
	if err != nil {
		t.Fatalf("Impurities over testdata/platforms: %v", err)
	}

	if want := []string{"func cut is not a method", "func cut is not a method"}; !slices.Equal(got, want) {
		t.Errorf("impurities: got %q, want %q", got, want)
	}
}
