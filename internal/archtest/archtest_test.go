package archtest

import (
	"go/constant"
	"go/types"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// The importer reads a dependency from export data built in this binary's
// race mode: prof.RaceEnabled comes from race.go or norace.go, so its imported
// value differs when the mode is not mirrored.
func TestExportImporterReadsThisBinarysBuild(t *testing.T) {
	pkg, err := ExportImporter(".").Import("github.com/cubbit/dittofs/internal/prof")
	if err != nil {
		t.Fatalf("import internal/prof: %v", err)
	}

	c, ok := pkg.Scope().Lookup("RaceEnabled").(*types.Const)
	if !ok {
		t.Fatalf("internal/prof.RaceEnabled: got %v, want a constant", pkg.Scope().Lookup("RaceEnabled"))
	}

	if got := constant.BoolVal(c.Val()); got != prof.RaceEnabled {
		t.Errorf("imported prof.RaceEnabled = %t, want %t, this binary's", got, prof.RaceEnabled)
	}
}

// A package outside the patterns' dependencies fails its import rather than
// type-checking to an empty package.
func TestExportImporterRefusesAPackageOutsideItsPatterns(t *testing.T) {
	_, err := ExportImporter(".").Import("github.com/cubbit/dittofs/pkg/settings")
	if err == nil || !strings.Contains(err.Error(), "no export data for github.com/cubbit/dittofs/pkg/settings") {
		t.Errorf("import pkg/settings: got %v, want no export data", err)
	}
}
