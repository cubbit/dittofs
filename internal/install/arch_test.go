package install_test

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The package reaches the node's tree only for auth: the initialisation
// step's signing-key seal and the reset's ResetAccount. auth is its one
// import under internal/node, so the node's import rules keep holding for
// what it builds.
func TestImportsUnderTheNode(t *testing.T) {
	const node = "github.com/cubbit/dittofs/internal/node"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if (p == node || strings.HasPrefix(p, node+"/")) && !slices.Contains(got, p) {
				got = append(got, p)
			}
		}
	}
	if want := []string{node + "/api/auth"}; !slices.Equal(got, want) {
		t.Errorf("imports under internal/node: got %q, want %q", got, want)
	}
}
