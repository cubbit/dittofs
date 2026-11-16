package apiv1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Each closed set is RFC 23's literal list, each value once, both as the
// constants of its type declared in the package and as its unexported list: a
// value missing, extra, repeated or misspelt in either fails.
func TestClosedSetsAreRFC23s(t *testing.T) {
	consts := constsByType(t)
	for _, tc := range []struct {
		typ  string
		list []string
		want []string
	}{
		{"Code", strs(apiCodes), []string{
			"unauthenticated", "forbidden", "generation_mismatch", "precondition_required",
			"idempotency_mismatch", "managed", "operation_running", "not_cancellable",
			"last_admin", "not_a_copy", "password_change_required", "throttled", "starting",
			"unavailable",
		}},
		{"OpKind", strs(opKinds), []string{
			"installation.rotate-signing-key", "installation.resolve-principal",
			"installation.test-directory", "installation.join-domain",
			"installation.rotate-machine-password", "share.delete", "user.delete",
			"group.delete", "identity.nt-hash-delete",
		}},
		{"Role", strs(roles), []string{"viewer", "operator", "admin"}},
		{"OpState", strs(opStates), []string{
			"pending", "running", "succeeded", "failed", "cancelling", "cancelled",
		}},
	} {
		want := slices.Sorted(slices.Values(tc.want))
		if got := slices.Sorted(slices.Values(consts[tc.typ])); !slices.Equal(got, want) {
			t.Errorf("constants of type %s: got %q, want %q", tc.typ, got, want)
		}
		if got := slices.Sorted(slices.Values(tc.list)); !slices.Equal(got, want) {
			t.Errorf("list of %s: got %q, want %q", tc.typ, got, want)
		}
	}
}

// constsByType returns the value of every string constant declared with an
// explicit type in the package's own source, by the type's name.
func constsByType(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				typ, ok := v.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, val := range v.Values {
					lit, ok := val.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("a constant of type %s is not a string literal", typ.Name)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[typ.Name] = append(out[typ.Name], s)
				}
			}
		}
	}
	return out
}

func strs[S ~string](s []S) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = string(v)
	}
	return out
}
