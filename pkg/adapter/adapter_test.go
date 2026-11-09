package adapter

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowed is all pkg/adapter may import beside the standard library: of the
// core only the service, the metrics registry, the budgets and its own
// settings, and the tracing API Deps.Tracer names. An import of the adapters'
// implementation, under internal/, would close a cycle through it back here.
var allowed = []string{
	"github.com/cubbit/dittofs/pkg/vfs",
	"github.com/cubbit/dittofs/pkg/metrics",
	"github.com/cubbit/dittofs/pkg/resources",
	"github.com/cubbit/dittofs/pkg/adapter/config",
	"go.opentelemetry.io/otel/trace",
}

func TestImportsOnlyTheContractsItNames(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			stdlib := !strings.Contains(strings.Split(p, "/")[0], ".")
			if !stdlib && !slices.Contains(allowed, p) {
				t.Errorf("%s imports %s; pkg/adapter imports only the standard library and %q", f, p, allowed)
			}
		}
	}
}
