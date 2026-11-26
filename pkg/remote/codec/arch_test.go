package codec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// newPutExport is NewPut as the pin prints it.
const newPutExport = "func (*Store).NewPut(name remote.Name, nonce [16]byte, pin transform.Attempt, " +
	"src iter.Seq2[remote.Chunk, error]) *Put"

// frozenExports is the codec's surface (RFC 4 §4.3): Store with Get
// and Health, its put, its constructor with what the content wiring and the
// root hand it, and the header bound. The encoder and decoder are not
// exported, and GetVersion lands with its first caller. A change here is a
// change to what the syncer, the engine and the content wiring build on.
var frozenExports = []string{
	"type Config struct{Namespace entity.NamespaceID; Chain transform.Chain; " +
		"ChunkID func(plain []byte) [32]byte; " +
		"Name func(nonce [16]byte, chain [32]byte, ids [][32]byte) remote.Name; " +
		"Census func(ctx context.Context, name remote.Name) ([]transform.Encoded, error); " +
		"MaxChunk int; Version uint8}",
	"type Recorder interface{MeasuringPass(); Took(d remote.Direction, took time.Duration); " +
		"Verified(n int); VerifyFailed(kind string)}",
	"type CPU interface{Acquire(ctx context.Context) (release func(), err error)}",
	"type Deps struct{Logger *slog.Logger; Recorder Recorder; CPU CPU}",
	"type Store struct{}",
	"func New(st store, cfg Config, d Deps) (*Store, error)",
	"func (*Store).Get(ctx context.Context, name remote.Name, want []remote.ChunkRange, m remote.Meter) " +
		"iter.Seq2[remote.Chunk, error]",
	"func (*Store).Health(ctx context.Context, d remote.Direction) error",
	newPutExport,
	"type Put struct{}",
	"func (*Put).Attempt(ctx context.Context, m remote.Meter) error",
	"func (*Put).Stored() (ranges []remote.ChunkRange, census []transform.Encoded)",
	"func MaxHeaderLen(v uint8) (n int, ok bool)",
}

// exportImporter reads each import from compiled export data; the package
// itself is always parsed from its files, the planted ones included.
var exportImporter = archtest.ExportImporter(".")

const path = "github.com/cubbit/dittofs/pkg/remote/codec"

// source returns the directory and the extra files a case's planted file
// reaches the checks through. A file named hidden* sits behind a build tag, so
// it is copied into the package's directory and reached through the directory
// scan, as a real file would be.
func source(t *testing.T, planted string) (string, []string) {
	t.Helper()

	switch {
	case planted == "":
		return ".", nil
	case strings.HasPrefix(planted, "hidden"):
		dir, err := archtest.WithHidden(t.TempDir(), ".", filepath.Join("testdata", "arch", planted))
		if err != nil {
			t.Fatal(err)
		}

		return dir, nil
	default:
		return ".", []string{filepath.Join("testdata", "arch", planted)}
	}
}

// The codec exports exactly its frozen surface; each planted change is
// caught: GetVersion declared before its caller, an exported error of the
// codec's own, an extra export behind a build tag, a missing export and a
// changed signature.
func TestCodecExports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		edit    func(want []string) []string
		want    []string
	}{
		{"the package", "", nil, nil},
		{"GetVersion declared", "get_version.go", nil, []string{"extra export: func (*Store).GetVersion(ctx context.Context, name remote.Name, " +
			"v remote.Version, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error]"}},
		{"an exported error", "exported_error.go", nil, []string{"extra export: var ErrHeader error"}},
		{"behind a build tag", "hidden.go", nil, []string{"extra export: func Hidden()"}},
		{"missing export", "", func(want []string) []string {
			return append(want, "func Gone()")
		}, []string{"missing export: func Gone()"}},
		{"NewPut without its nonce", "", func(want []string) []string {
			return slices.Replace(want, slices.Index(want, newPutExport), slices.Index(want, newPutExport)+1,
				"func (*Store).NewPut(name remote.Name, pin transform.Attempt, src iter.Seq2[remote.Chunk, error]) *Put")
		}, []string{
			"extra export: " + newPutExport,
			"missing export: func (*Store).NewPut(name remote.Name, pin transform.Attempt, src iter.Seq2[remote.Chunk, error]) *Put",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, extra := source(t, tc.planted)

			want := slices.Clone(frozenExports)
			if tc.edit != nil {
				want = tc.edit(want)
			}

			got, err := archtest.Exports(exportImporter, path, dir, extra...)
			if err != nil {
				t.Fatal(err)
			}

			if bad := archtest.Violations(got, want); !slices.Equal(bad, tc.want) {
				t.Errorf("violations %q, want %q", bad, tc.want)
			}
		})
	}
}

// errorTypes names each type that declares an Error method in the package's
// non-test files in dir, whatever their build tags, plus planted ones: an
// error type of the codec's own, which a constant or a return site can carry
// with no variable declared.
func errorTypes(dir string, planted ...string) ([]string, error) {
	paths, err := archtest.GoFiles(dir)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, p := range append(paths, planted...) {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}

		if f.Name.Name != filepath.Base(path) {
			continue
		}

		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "Error" {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}

				if id, ok := recv.(*ast.Ident); ok {
					names = append(names, "type "+id.Name+" is an error")
				}
			}
		}
	}

	return names, nil
}

// The codec declares no error of its own: it returns RFC 4 §4.8's set and the
// transforms' material errors as they are (RFC 4 §4.3). So it holds no
// package-level variable, which would be the home of an error value, and no
// type with an Error method. A table the encoder needs is named in the Vars
// of the purity rule when it lands. Each planted error is caught.
func TestCodecDeclaresNoError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		want    []string
	}{
		{"the package", "", nil},
		{"an error variable", "error_var.go", []string{"var errShortHeader is state"}},
		{"errors in a map", "error_map.go", []string{"var errsByKind is state"}},
		{"a table", "state.go", []string{"var table is state"}},
		{"an error type and its constant", "error_type.go", []string{"type codecError is an error"}},
		{"an error type behind a build tag", "hidden_error.go", []string{"type taggedError is an error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, extra := source(t, tc.planted)

			bad, err := archtest.Impurities(exportImporter, path, dir, archtest.Purity{Funcs: true, Interfaces: true}, extra...)
			if err != nil {
				t.Fatal(err)
			}

			types, err := errorTypes(dir, extra...)
			if err != nil {
				t.Fatal(err)
			}

			if got := append(bad, types...); !slices.Equal(got, tc.want) {
				t.Errorf("errors declared %q, want %q", got, tc.want)
			}
		})
	}
}
