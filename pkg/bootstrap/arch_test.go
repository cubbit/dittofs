package bootstrap

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

// frozenExports is pkg/bootstrap's surface as RFC 13's configuration
// freeze leaves it: what the node, the API and dfs call, and the types a
// kept field needs. A change here is a change to the frozen surface.
var frozenExports = []string{
	"func GetDefaultConfigPath() string",
	"func InitLogger(cfg *Bootstrap) error",
	"func InitNodeID(b *Bootstrap, installation [16]byte, dir string) (entity.NodeID, error)",
	"func NodeID(b *Bootstrap) (entity.NodeID, error)",
	"func WriteMissingNodeIDs(b *Bootstrap, id entity.NodeID, " +
		"recorded func(dir string) (bool, error)) error",
	"func GetDefaultConfig() *Bootstrap",
	"func Load(configPath string, environ []string) (*Bootstrap, error)",
	"func MustLoad(configPath string, environ []string) (*Bootstrap, error)",
	"type BlockstoreConfig struct{Journal BlockstoreJournalConfig}",
	"type BlockstoreJournalConfig struct{Path string}",
	"type Bootstrap struct{Logging LoggingConfig; Blockstore BlockstoreConfig; " +
		"Node struct{ID entity.NodeID; Roles []string}; " +
		"Metadata struct{Backend string; Badger struct{Path string}; " +
		"FoundationDB struct{ClusterFile string; " +
		"TLS struct{CertFile string; KeyFile string; CAFile string}}}; " +
		"Journals []Journal; Adapters struct{Listen []string}; " +
		"API struct{Listen []string; TLS struct{CertFile string; KeyFile string}; " +
		"Socket string}; " +
		"Metrics struct{Listen string}; Keys struct{Protocol WrappingKey; Storage WrappingKey}}",
	"type Journal struct{Path string}",
	"type LogRotationConfig struct{MaxSize int; MaxBackups int; MaxAge int; Compress bool}",
	"type LoggingConfig struct{Level string; Format string; Output string; " +
		"Rotation LogRotationConfig}",
	"type WrappingKey struct{Kind keyprovider.Kind; " +
		"Local struct{File string; RetiredFiles []string}; " +
		"KMIP struct{Endpoint string; ServerCA string; ClientCert string; ClientKey string; " +
		"KeyUID string; Timeout time.Duration; RetiredKeyUIDs []string}}",
	"func (WrappingKey).Provider() keyprovider.Config",
}

// sourceImporter reads every import from source, once for all the cases.
var sourceImporter = importer.ForCompiler(token.NewFileSet(), "source", nil)

// exports type-checks the package made of dir's non-test .go files, whatever
// their build tags, so a file behind a tag cannot hide an export, plus the
// planted files, and prints each exported declaration on one line: a
// function or method with its signature and receiver, a constant with its
// value, a struct with its exported fields only, since a later epic adds
// unexported ones to an opaque struct.
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

	pkg, err := conf.Check("github.com/cubbit/dittofs/pkg/bootstrap", fset, files, nil)
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
		if named, ok := tn.Type().(*types.Named); ok {
			for m := range named.Methods() {
				if m.Exported() {
					got = append(got, types.ObjectString(m, qualify))
				}
			}
		}
	}

	return got, nil
}

// typeString prints t as types.TypeString does, but a struct with its
// exported fields only and no tags, nested anonymous structs included.
func typeString(t types.Type, qualify types.Qualifier) string {
	st, ok := t.(*types.Struct)
	if !ok {
		return types.TypeString(t, qualify)
	}

	var fields []string

	for f := range st.Fields() {
		if f.Exported() {
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

// pkg/bootstrap exports exactly its frozen surface; each planted file, one
// change each, is caught.
func TestBootstrapExports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		gone    string
		want    []string
	}{
		{"the package", "", "", nil},
		{"extra function", "extra_func.go", "", []string{"extra export: func Extra()"}},
		{"extra method", "extra_method.go", "", []string{"extra export: func (Bootstrap).Extra()"}},
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
