package secrets

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// frozenExports is pkg/secrets' surface as RFC 13's configuration freeze
// leaves it: RFC 13 §7's provider, its roles, Meta, Version and the errors.
// A change here is a change to the frozen surface.
var frozenExports = []string{
	"func New(cp kvstore.ControlPlane, keys map[Role]keyprovider.KeyProvider) (*Provider, error)",
	"type Meta struct{KeyVersion uint32; HandedOver bool}",
	"type Provider struct{}",
	"func (*Provider).Delete(ctx context.Context, kind entity.SecretKind, " +
		"owner entity.SecretOwner, ref entity.SecretRef) error",
	"func (*Provider).Prune(ctx context.Context, kind entity.SecretKind, " +
		"owner entity.SecretOwner, ref entity.SecretRef) error",
	"func (*Provider).Resolve(ctx context.Context, role Role, kind entity.SecretKind, " +
		"owner entity.SecretOwner, ref entity.SecretRef) ([]byte, error)",
	"func (*Provider).Seal(ctx context.Context, role Role, kind entity.SecretKind, " +
		"owner entity.SecretOwner, ref entity.SecretRef, value []byte, m Meta) " +
		"(entity.Secret, error)",
	"func (*Provider).Versions(ctx context.Context, role Role, kind entity.SecretKind, " +
		"owner entity.SecretOwner, ref entity.SecretRef) ([]Version, error)",
	"type Role uint8",
	"func (Role).String() string",
	"const Protocol Role = 1",
	"const Storage Role = 2",
	"type Version struct{Number uint64; Value []byte; Meta Meta}",
	"func (Version).Format(f fmt.State, verb rune)",
	"func (Version).LogValue() slog.Value",
	"func (Version).MarshalJSON() ([]byte, error)",
	"func (Version).MarshalYAML() (any, error)",
	"func (Version).String() string",
	"var ErrKindChanged error",
	"var ErrUnproven error",
	"var ErrWrongKind error",
	"var ErrWrongRole error",
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

	pkg, err := conf.Check("github.com/cubbit/dittofs/pkg/secrets", fset, files, nil)
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

// pkg/secrets exports exactly its frozen surface; each planted file, one
// change each, is caught.
func TestSecretsExports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		gone    string
		want    []string
	}{
		{"the package", "", "", nil},
		{"extra function", "extra_func.go", "", []string{"extra export: func Extra()"}},
		{"extra method", "extra_method.go", "", []string{"extra export: func (*Provider).Extra()"}},
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

// storePackages are the packages whose symbols the freeze pins one by one,
// not as a whole set, since the store's other exports are not this surface.
var storePackages = map[string]string{
	"entity":  "github.com/cubbit/dittofs/pkg/metadata/entity",
	"kvstore": "github.com/cubbit/dittofs/pkg/metadata/kvstore",
}

// storeSymbol prints one symbol named as package.Name or
// package.Type.Method, as exports prints a declaration; false when the
// package has no such symbol.
func storeSymbol(name string) (string, bool, error) {
	parts := strings.Split(name, ".")

	path, ok := storePackages[parts[0]]
	if !ok || len(parts) < 2 || len(parts) > 3 {
		return "", false, nil
	}

	pkg, err := sourceImporter.Import(path)
	if err != nil {
		return "", false, err
	}

	qualify := func(p *types.Package) string {
		return p.Name()
	}

	obj := pkg.Scope().Lookup(parts[1])
	if obj == nil {
		return "", false, nil
	}

	if len(parts) == 3 {
		m, _, _ := types.LookupFieldOrMethod(obj.Type(), true, pkg, parts[2])
		if fn, ok := m.(*types.Func); ok {
			return types.ObjectString(fn, qualify), true, nil
		}

		return "", false, nil
	}

	switch obj := obj.(type) {
	case *types.Const:
		return types.ObjectString(obj, qualify) + " = " + obj.Val().ExactString(), true, nil
	case *types.TypeName:
		return "type " + name + " " + typeString(obj.Type().Underlying(), qualify), true, nil
	default:
		return types.ObjectString(obj, qualify), true, nil
	}
}

// frozenStore is what the secrets surface freezes outside pkg/secrets: the
// store view's settings delete and secret methods, their
// transaction forms, and the Secret record's types with each kind's value.
var frozenStore = map[string]string{
	"kvstore.ControlPlane.DeleteSetting": "func (kvstore.ControlPlane).DeleteSetting(" +
		"ctx context.Context, scope entity.SettingScope, key string, expect uint64) error",
	"kvstore.ControlPlane.Secret": "func (kvstore.ControlPlane).Secret(" +
		"ctx context.Context, id entity.SecretRef, version uint64) (entity.Secret, error)",
	"kvstore.ControlPlane.PutSecret": "func (kvstore.ControlPlane).PutSecret(" +
		"ctx context.Context, s entity.Secret, expect uint64) error",
	"kvstore.ControlPlane.PruneSecret": "func (kvstore.ControlPlane).PruneSecret(" +
		"ctx context.Context, id entity.SecretRef, below uint64) error",
	"entity.ControlPlaneTx.DeleteSetting": "func (entity.ControlPlaneTx).DeleteSetting(" +
		"scope entity.SettingScope, key string, expect uint64) error",
	"entity.ControlPlaneTx.Secret": "func (entity.ControlPlaneTx).Secret(" +
		"id entity.SecretRef, version uint64) (entity.Secret, bool, error)",
	"entity.ControlPlaneTx.PutSecret": "func (entity.ControlPlaneTx).PutSecret(" +
		"s entity.Secret, expect uint64) error",
	"entity.ControlPlaneTx.PruneSecret": "func (entity.ControlPlaneTx).PruneSecret(" +
		"id entity.SecretRef, below uint64) error",
	"entity.SettingKey": "type entity.SettingKey struct{Scope entity.SettingScope; Key string}",
	"entity.SettingsReader": "type entity.SettingsReader interface{AllSettings(ctx context.Context, " +
		"after entity.SettingKey) iter.Seq2[entity.Setting, error]}",
	"entity.Secret": "type entity.Secret struct{ID entity.SecretRef; Kind entity.SecretKind; " +
		"Version uint64; KeyID string; Owner entity.SecretOwner; Value []byte; " +
		"KeyVersion uint32; HandedOver bool}",
	"entity.SecretOwner": "type entity.SecretOwner struct{Record entity.ResourceRef; " +
		"Scope entity.SettingScope; Key string}",
	"entity.SecretKind":             "type entity.SecretKind uint8",
	"entity.SecretKind.String":      "func (entity.SecretKind).String() string",
	"entity.SecretKind.MarshalText": "func (entity.SecretKind).MarshalText() ([]byte, error)",
	"entity.SecretKind.UnmarshalText": "func (*entity.SecretKind).UnmarshalText(" +
		"b []byte) error",
	"entity.SecretPassword":         "const entity.SecretPassword entity.SecretKind = 1",
	"entity.SecretNTHash":           "const entity.SecretNTHash entity.SecretKind = 2",
	"entity.SecretKeytab":           "const entity.SecretKeytab entity.SecretKind = 3",
	"entity.SecretMachinePassword":  "const entity.SecretMachinePassword entity.SecretKind = 4",
	"entity.SecretRemoteCredential": "const entity.SecretRemoteCredential entity.SecretKind = 5",
	"entity.SecretKey":              "const entity.SecretKey entity.SecretKind = 6",
}

// Each frozen store symbol is present with its signature; a symbol the
// store does not have is named.
func TestStoreSurface(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrong map[string]string
		want  []string
	}{
		{"the store", nil, nil},
		{
			"missing symbol",
			map[string]string{"entity.ControlPlaneTx.Gone": "func Gone()"},
			[]string{"missing: entity.ControlPlaneTx.Gone"},
		},
		{
			"changed value and signature",
			map[string]string{
				"entity.SecretKey": "const entity.SecretKey entity.SecretKind = 7",
				"kvstore.ControlPlane.DeleteSetting": "func (kvstore.ControlPlane).DeleteSetting(" +
					"ctx context.Context, scope entity.SettingScope, key string) error",
			},
			[]string{
				"changed: entity.SecretKey: got const entity.SecretKey entity.SecretKind = 6, " +
					"want const entity.SecretKey entity.SecretKind = 7",
				"changed: kvstore.ControlPlane.DeleteSetting: got " +
					"func (kvstore.ControlPlane).DeleteSetting(ctx context.Context, " +
					"scope entity.SettingScope, key string, expect uint64) error, " +
					"want func (kvstore.ControlPlane).DeleteSetting(ctx context.Context, " +
					"scope entity.SettingScope, key string) error",
			},
		},
	} {
		want := maps.Clone(frozenStore)
		maps.Copy(want, tc.wrong)

		var bad []string

		for _, name := range slices.Sorted(maps.Keys(want)) {
			got, ok, err := storeSymbol(name)
			if err != nil {
				t.Fatalf("%s: %s: %v", tc.name, name, err)
			}

			switch {
			case !ok:
				bad = append(bad, "missing: "+name)
			case got != want[name]:
				bad = append(bad, "changed: "+name+": got "+got+", want "+want[name])
			}
		}

		if !slices.Equal(bad, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, bad, tc.want)
		}
	}
}
