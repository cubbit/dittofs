package kmip

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// The transport imports the standard library and the KMIP library only, and
// reaches no package of the module, so both key providers can import it and
// it imports neither. Libs lists what the KMIP library brings with it.
func TestImports(t *testing.T) {
	graphs, err := archtest.Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}
	row := archtest.Row{Pkg: "internal/kmip", Libs: []string{
		"github.com/gemalto/kmip-go/...",
		"github.com/ansel1/merry/...",
		"github.com/google/uuid",
		"github.com/gemalto/flume/...",
		"go.uber.org/zap/...",
		"go.uber.org/multierr/...",
		"golang.org/x/text/...",
		"golang.org/x/sys/...",
		"github.com/mattn/go-colorable/...",
		"github.com/mattn/go-isatty/...",
		"github.com/mgutz/ansi/...",
	}}
	for _, v := range archtest.Check([]archtest.Row{row}, graphs...) {
		t.Error(v)
	}
	for _, g := range graphs {
		for _, imp := range g.Imports[archtest.Module+"internal/kmip"] {
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") && !strings.HasPrefix(imp, "github.com/gemalto/kmip-go") {
				t.Errorf("internal/kmip imports %s directly, want the standard library and github.com/gemalto/kmip-go only", imp)
			}
		}
	}
}
