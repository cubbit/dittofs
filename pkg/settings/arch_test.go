package settings

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// frozenExports is pkg/settings' surface as RFC 13's configuration freeze
// leaves it: the binding classes, the scope forms, Of and its options, the
// table, the snapshot and its reads, the write, its refusal and its check, and the
// errors. A change here is a change to the frozen surface.
var frozenExports = []string{
	"type Class uint8",
	"const Live Class = 1",
	"const Restart Class = 2",
	"const NextWrite Class = 3",
	"const Bound Class = 4",
	"type Scope struct{}",
	"func At(scope entity.ScopeKind) Scope",
	"func PerThing(scope entity.ScopeKind, thing string) Scope",
	"func Override(wide entity.ScopeKind, narrow entity.ScopeKind) Scope",
	"type Section struct{}",
	"func Of[T interface{Validate() error}](prefix string, scope Scope, def T, " +
		"opts ...Option) Section",
	"type Option struct{}",
	"func WriteRole(role entity.APIRole) Option",
	"func Required(fields ...string) Option",
	"func CheckTx[T any](check func(tx entity.ControlPlaneTx, w Write, effective T) error) Option",
	"func SecretRef(field string, kind entity.SecretKind) Option",
	"func FormatVersion(field string) Option",
	"func OverrideOnly(fields ...string) Option",
	"type Table []Section",
	"func (Table).Check(tx entity.ControlPlaneTx, caller entity.APIRole, w Write, " +
		"holds HoldsContent) error",
	"type Snapshot struct{}",
	"func (*Snapshot).Keep(section string, prev *Snapshot) *Snapshot",
	"func (*Snapshot).Stored(section string) bool",
	"func RefusedRows(err error) []entity.SettingKey",
	"func Read(ctx context.Context, r entity.SettingsReader, " +
		"table Table, node entity.NodeID) (*Snapshot, map[string]error, error)",
	"func Get[T any](snap *Snapshot, section string, node entity.NodeID) (T, error)",
	"type Change struct{Section string; Key string; Class Class; " +
		"Scope entity.SettingScope}",
	"func Diff(prev *Snapshot, cur *Snapshot) []Change",
	"type Write struct{Scope entity.SettingScope; Key string}",
	"func Put(s entity.Setting) Write",
	"func Delete(scope entity.SettingScope, key string) Write",
	"func (Write).Refuse(rule string) error",
	"type HoldsContent func(scope entity.SettingScope) (bool, error)",
	"var ErrBound error",
	"var ErrForbidden error",
	"var ErrFormatVersion error",
	"var ErrInvalid error",
	"var ErrNewerSchema error",
	"var ErrRequired error",
	"var ErrSecretRef error",
	"var ErrUnknownKey error",
	"var ErrUnproven error",
	"var ErrWrongScope error",
	"func Versions() map[entity.Format]entity.VersionRange",
}

// sourceImporter reads every import from source, once for all the cases.
var sourceImporter = importer.ForCompiler(token.NewFileSet(), "source", nil)

// exports type-checks the package made of dir's non-test .go files, whatever
// their build tags, so a file behind a tag cannot hide an export, plus the
// planted files, and prints each exported declaration on one line: a
// function or method with its signature and receiver, a constant with its
// value, a struct with its exported and embedded fields only, since an opaque
// struct may gain unexported fields.
func exports(dir string, planted ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	paths := slices.Clone(planted)

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}

	fset := token.NewFileSet()

	files := make([]*ast.File, 0, len(paths))
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}

		files = append(files, f)
	}

	conf := types.Config{Importer: sourceImporter}

	pkg, err := conf.Check("github.com/cubbit/dittofs/pkg/settings", fset, files, nil)
	if err != nil {
		return nil, err
	}

	qualify := func(p *types.Package) string {
		if p == pkg {
			return ""
		}

		return p.Name()
	}

	var got []string

	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}

		if c, ok := obj.(*types.Const); ok {
			got = append(got, types.ObjectString(c, qualify)+" = "+c.Val().ExactString())

			continue
		}

		tn, ok := obj.(*types.TypeName)
		if !ok {
			got = append(got, types.ObjectString(obj, qualify))

			continue
		}

		got = append(got, "type "+name+" "+typeString(tn.Type().Underlying(), qualify))
		// An interface prints every method in typeString; any other type
		// prints the whole method set, promoted methods of an embedded type
		// included, since a method promoted that way is public too.
		if _, isIface := tn.Type().Underlying().(*types.Interface); !isIface {
			for sel := range types.NewMethodSet(types.NewPointer(tn.Type())).Methods() {
				if sel.Obj().Exported() {
					got = append(got, types.ObjectString(sel.Obj(), qualify))
				}
			}
		}
	}

	return got, nil
}

// typeString prints t as types.TypeString does, but a struct with its
// exported and embedded fields only and no tags, nested anonymous structs included.
func typeString(t types.Type, qualify types.Qualifier) string {
	st, ok := t.(*types.Struct)
	if !ok {
		return types.TypeString(t, qualify)
	}

	var fields []string

	for f := range st.Fields() {
		if f.Exported() || f.Embedded() {
			fields = append(fields, f.Name()+" "+typeString(f.Type(), qualify))
		}
	}

	return "struct{" + strings.Join(fields, "; ") + "}"
}

// hiddenCase names the case whose planted file sits in the package's
// directory behind a build tag, so it reaches the pin through the directory
// scan, as a real file would.
const hiddenCase = "hidden.go"

// withHidden returns a directory holding a copy of the package's non-test
// .go files and testdata/arch/hidden.go, an export behind a build tag.
func withHidden(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{filepath.Join("testdata", "arch", hiddenCase)}

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, name)
		}
	}

	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, filepath.Base(p)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// violations compares got with want, whatever their order: each line of got
// not in want is an extra export, each line of want not in got a missing one.
func violations(got, want []string) []string {
	var bad []string

	for _, g := range got {
		if !slices.Contains(want, g) {
			bad = append(bad, "extra export: "+g)
		}
	}

	for _, w := range want {
		if !slices.Contains(got, w) {
			bad = append(bad, "missing export: "+w)
		}
	}

	slices.Sort(bad)

	return bad
}

// pkg/settings exports exactly its frozen surface; each planted file, one
// change each, is caught.
func TestSettingsExports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		gone    string
		want    []string
	}{
		{"the package", "", "", nil},
		{"extra function", "extra_func.go", "", []string{"extra export: func Extra()"}},
		{"extra method", "extra_method.go", "", []string{"extra export: func (Table).Extra()"}},
		{"promoted method", "extra_embed.go", "", []string{
			"extra export: func (embedded).Promoted()",
			"extra export: type Leaky struct{embedded embedded}",
		}},
		{"extra type", "extra_type.go", "", []string{"extra export: type Extra struct{A int}"}},
		{"missing export", "", "func Gone()", []string{"missing export: func Gone()"}},
		{"behind a build tag", hiddenCase, "", []string{"extra export: func Hidden()"}},
	} {
		dir := "."

		var planted []string

		switch tc.planted {
		case "":
		case hiddenCase:
			dir = withHidden(t)
		default:
			planted = append(planted, filepath.Join("testdata", "arch", tc.planted))
		}

		want := frozenExports
		if tc.gone != "" {
			want = append(slices.Clone(want), tc.gone)
		}

		got, err := exports(dir, planted...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if bad := violations(got, want); !slices.Equal(bad, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, bad, tc.want)
		}
	}
}
