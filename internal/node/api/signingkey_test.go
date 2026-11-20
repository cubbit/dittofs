package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// signingKeyWriters are the files under the API that may name what writes
// the signing key: the tokens, which declare its owner and its seal, and the
// rotation step, the one writer after the installation's first version.
var signingKeyWriters = []string{"auth/rotate.go", "auth/tokens.go"}

// No route writes the signing key: outside the tokens and the rotation step,
// no file under the API names its seal, its owner, the installation's field
// naming it or that field's key, so no handler can seal a version of it or
// point the installation at another. A version written under another kind
// or owner is refused by the store, so a seal under the key's owner is the
// only way to write it.
func TestNoRouteWritesTheSigningKey(t *testing.T) {
	names := []string{"SealSigningKey", "signingKeyOwner", "SetSigningKey"}
	var bad []string
	seen := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if slices.Contains(signingKeyWriters, filepath.ToSlash(p)) {
			seen[filepath.ToSlash(p)] = true
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if slices.Contains(names, n.Name) {
					bad = append(bad, fset.Position(n.Pos()).String()+" names "+n.Name)
				}
			case *ast.BasicLit:
				if s, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING && strings.Contains(s, "signing_key") {
					bad = append(bad, fset.Position(n.Pos()).String()+" names the signing_key field")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the API's files: %v", err)
	}
	for _, w := range signingKeyWriters {
		if !seen[w] {
			t.Errorf("%s is missing, so the walk read none of the files it exempts", w)
		}
	}
	if len(bad) > 0 {
		t.Errorf("files under the API outside %v that could write the signing key: %q, want none", signingKeyWriters, bad)
	}
}
