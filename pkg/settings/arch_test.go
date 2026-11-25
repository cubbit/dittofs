package settings

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
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
	"func KeyRole(field string, role entity.APIRole) Option",
	"func Required(fields ...string) Option",
	"func CheckTx[T any](check func(tx entity.ControlPlaneTx, w Write, effective T) error) Option",
	"func SecretRef(field string, kind entity.SecretKind) Option",
	"func FormatVersion(field string) Option",
	"func OverrideOnly(fields ...string) Option",
	"func SelfKey() Option",
	"type Table []Section",
	"func (Table).Check(tx entity.ControlPlaneTx, caller entity.APIRole, w Write, " +
		"holds HoldsContent) error",
	"func (Table).Bound(rows []entity.Setting) ([]byte, error)",
	"func (Table).SecretKind(key string) (entity.SecretKind, bool)",
	"func (Table).DecodeBound(b []byte) (map[string]json.RawMessage, error)",
	"func Refusals(err error) iter.Seq2[entity.SettingScope, string]",
	"func (Table).PrefixOf(key string) string",
	"func (Table).WithMetrics(m *metrics.Registry) Table",
	"func (Table).Reads(s entity.Setting) (prefix string, ok bool)",
	"type Snapshot struct{}",
	"type SecretUse struct{Section string; Key string; Scope entity.SettingScope; " +
		"Kind entity.SecretKind; Ref entity.SecretRef}",
	"func (*Snapshot).Keep(section string, prev *Snapshot) *Snapshot",
	"func (*Snapshot).Stored(section string) bool",
	"func (*Snapshot).Secrets() []SecretUse",
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

// exportImporter reads each import from compiled export data; the package
// itself is always parsed from its files, the planted ones included.
var exportImporter = archtest.ExportImporter(".")

// hiddenCase names the case whose planted file sits in the package's
// directory behind a build tag, so it reaches the pin through the directory
// scan, as a real file would.
const hiddenCase = "hidden.go"

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
			var err error

			dir, err = archtest.WithHidden(t.TempDir(), ".", filepath.Join("testdata", "arch", hiddenCase))
			if err != nil {
				t.Fatal(err)
			}
		default:
			planted = append(planted, filepath.Join("testdata", "arch", tc.planted))
		}

		want := frozenExports
		if tc.gone != "" {
			want = append(slices.Clone(want), tc.gone)
		}

		got, err := archtest.Exports(exportImporter, "github.com/cubbit/dittofs/pkg/settings", dir, planted...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if bad := archtest.Violations(got, want); !slices.Equal(bad, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, bad, tc.want)
		}
	}
}
