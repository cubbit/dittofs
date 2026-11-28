package kmip

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// The transport imports the standard library and the KMIP library only, no
// package of the module, so both key providers can import it and it imports
// neither.
func TestImports(t *testing.T) {
	graphs, err := archtest.Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}
	self := archtest.Module + "internal/kmip"
	for _, g := range graphs {
		imps, ok := g.Imports[self]
		if !ok {
			t.Fatalf("the walk did not list %s", self)
		}
		for _, imp := range imps {
			first, _, _ := strings.Cut(imp, "/")
			if strings.Contains(first, ".") && !strings.HasPrefix(imp, "github.com/gemalto/kmip-go") {
				t.Errorf("internal/kmip imports %s, want the standard library and github.com/gemalto/kmip-go only", imp)
			}
		}
	}
}
