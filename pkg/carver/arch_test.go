package carver

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/archtest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// exportImporter reads each import from compiled export data; a package
// under test is always parsed from its files, the planted ones included.
var exportImporter = archtest.ExportImporter("./...")

// surface is one package of the carver as an importer sees it: what it
// exports, frozen until the interface is agreed again, and what it may
// declare beyond types, constants and methods.
type surface struct {
	dir     string // relative to pkg/carver; its planted files sit in testdata/arch/<base of dir>
	exports []string
	purity  archtest.Purity
}

func (s surface) path() string {
	if s.dir == "." {
		return archtest.Module + "pkg/carver"
	}

	return archtest.Module + "pkg/carver/" + s.dir
}

func (s surface) planted(name string) []string {
	base := filepath.Base(s.dir)
	if s.dir == "." {
		base = "root"
	}

	return []string{filepath.Join("testdata", "arch", base, name)}
}

// surfaces are the carver's packages: the root, the contract, with no
// function, no interface and no state but its error (RFC 0 §1.2, RFC 2 §1.1,
// §1.3); the settings value it aliases, data only; carve and the fold, which
// declare functions but no interface and keep nothing between calls.
var surfaces = []surface{
	{
		dir: ".",
		exports: []string{
			"type AssemblerConfig struct{BlockTarget int64; MaxChunks int; KeyBudget int; BlockKeys int; FileKeys int; MaxAge time.Duration}",
			"type BlockPlan struct{Carried []ChunkInfo; Refs []ChunkInfo}",
			"const Carried Disposition = 1",
			"type Chunk struct{Offset int64; Length int32; Hash entity.ChunkHash}",
			"type ChunkInfo struct{File entity.FileID; Chunk Chunk; Zero bool; Keys int; Oldest time.Time}",
			"type Disposition uint8",
			"var ErrInvalidSettings error",
			"const MaxBlockTarget untyped int = 67108864",
			"const MinBlockTarget untyped int = 1048576",
			// No exported field: outside pkg/carver a value can be held and
			// passed, never built with fields or read beyond these methods.
			"type Settings = struct{}",
			"func (params.Settings).Max() int",
			"func (params.Settings).Min() int",
			"func (params.Settings).Target() int",
			"const ZeroRef Disposition = 2",
		},
		purity: archtest.Purity{Vars: []string{"ErrInvalidSettings"}},
	},
	{
		dir: "internal/params",
		exports: []string{
			"type Settings struct{}",
			"func (Settings).Max() int",
			"func (Settings).Min() int",
			"func (Settings).Target() int",
		},
		// Data only: methods over the value. The function that builds one widens this rule when it lands.
		purity: archtest.Purity{},
	},
	{
		dir: "carve",
		exports: []string{
			"func BlockName(scope entity.NamespaceID, nonce [16]byte, chain [32]byte, ids []entity.ChunkHash) entity.BlockName",
			"func CheckTarget(target int) error",
			"func ChunkID(s carver.Settings, p []byte) entity.ChunkHash",
			"func Cut(r io.Reader, base int64, s carver.Settings, buf []byte, realEnd bool, emit func(carver.Chunk, []byte) error) (consumed int64, err error)",
			"func NewSettings(target int, encrypting bool, chunkingKey [32]byte, chunkIDKey [32]byte) (carver.Settings, error)",
		},
		purity: archtest.Purity{Funcs: true},
	},
	{
		dir: "assembler",
		exports: []string{
			"type Fold struct{}",
			"func (*Fold).Add(c carver.ChunkInfo) ([]carver.BlockPlan, carver.Disposition)",
			"func (*Fold).Finish(now time.Time) (last *carver.BlockPlan, held bool)",
			"func New(c carver.AssemblerConfig) (*Fold, error)",
		},
		purity: archtest.Purity{Funcs: true},
	},
}

// Each package exports exactly its frozen surface, and a planted extra export
// in each is caught.
func TestCarverExports(t *testing.T) {
	for _, s := range surfaces {
		got, err := archtest.Exports(exportImporter, s.path(), s.dir)
		if err != nil {
			t.Fatalf("%s: %v", s.dir, err)
		}

		if bad := archtest.Violations(got, s.exports); bad != nil {
			t.Errorf("%s: violations %q, want none", s.dir, bad)
		}
	}

	for _, tc := range []struct {
		dir, planted, want string
	}{
		{".", "iface.go", "extra export: type Chunker interface{Next() int}"},
		{"internal/params", "iface.go", "extra export: type Deriver interface{Derive(target int) Settings}"},
		{"carve", "iface.go", "extra export: type Cutter interface{Cut(s carver.Settings) error}"},
		{"assembler", "iface.go", "extra export: type Assembler interface{Add(c carver.ChunkInfo) ([]carver.BlockPlan, carver.Disposition)}"},
	} {
		s := surfaces[slices.IndexFunc(surfaces, func(s surface) bool { return s.dir == tc.dir })]

		got, err := archtest.Exports(exportImporter, s.path(), s.dir, s.planted(tc.planted)...)
		if err != nil {
			t.Fatalf("%s with %s: %v", tc.dir, tc.planted, err)
		}

		if bad := archtest.Violations(got, s.exports); !slices.Equal(bad, []string{tc.want}) {
			t.Errorf("%s with %s: violations %q, want %q", tc.dir, tc.planted, bad, tc.want)
		}
	}
}

// Each package declares no state, the root no function either, and none an
// interface (RFC 2 §1.1, §1.3); each planted impurity is caught.
func TestCarverIsPure(t *testing.T) {
	for _, tc := range []struct {
		dir     string
		planted string
		want    []string
	}{
		{".", "", nil},
		{".", "func.go", []string{"func Helper is not a method"}},
		{".", "init.go", []string{"func init is not a method"}},
		{".", "state.go", []string{"var cache is state"}},
		{".", "iface.go", []string{"type Chunker is an interface"}},
		{".", "alias_iface.go", []string{"type source is an interface"}},
		{".", "defined_iface.go", []string{"type sink is an interface"}},
		{"internal/params", "", nil},
		{"internal/params", "state.go", []string{"var defaults is state"}},
		{"internal/params", "func.go", []string{"func derive is not a method"}},
		{"internal/params", "iface.go", []string{"type Deriver is an interface"}},
		{"carve", "", nil},
		{"carve", "state.go", []string{"var tables is state"}},
		{"carve", "iface.go", []string{"type Cutter is an interface"}},
		{"carve", "init.go", []string{"func init is not a method"}},
		{"assembler", "", nil},
		{"assembler", "state.go", []string{"var open is state"}},
		{"assembler", "iface.go", []string{"type Assembler is an interface"}},
	} {
		s := surfaces[slices.IndexFunc(surfaces, func(s surface) bool { return s.dir == tc.dir })]

		var planted []string
		if tc.planted != "" {
			planted = s.planted(tc.planted)
		}

		bad, err := archtest.Impurities(exportImporter, s.path(), s.dir, s.purity, planted...)
		if err != nil {
			t.Fatalf("%s with %q: %v", tc.dir, tc.planted, err)
		}

		if !slices.Equal(bad, tc.want) {
			t.Errorf("%s with %q: impurities %q, want %q", tc.dir, tc.planted, bad, tc.want)
		}
	}
}

// byteless reports each field reachable from t that could hold chunk bytes,
// by its path from t. A plan may hold integers, booleans, byte arrays (the
// entity package's hashes and IDs, none longer than a hash), time.Time and time.Duration, and structs
// of pkg/carver and slices of them, walked recursively; anything else, a
// []byte, a string, a pointer, a map, an interface, fails (RFC 2 §5.5).
func byteless(t reflect.Type, path string) []string {
	switch {
	case t == reflect.TypeFor[time.Time]():
		return nil
	case t.Kind() == reflect.Bool, t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		return nil
	case t.Kind() == reflect.Array && t.Elem().Kind() == reflect.Uint8 && t.Len() <= len(entity.ChunkHash{}):
		return nil
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		return byteless(t.Elem(), path+"[]")
	case t.Kind() == reflect.Struct && t.PkgPath() == reflect.TypeFor[BlockPlan]().PkgPath():
		var bad []string
		for f := range t.Fields() {
			bad = append(bad, byteless(f.Type, path+"."+f.Name)...)
		}

		return bad
	}

	return []string{path + " is " + t.String()}
}

// Planted plans, each with one field that could hold bytes.
type (
	plantedBytes     struct{ Data []byte }
	plantedString    struct{ Data string }
	plantedAny       struct{ Data any }
	plantedPointer   struct{ Data *ChunkInfo }
	plantedInterface struct{ Data error }
	plantedNested    struct{ Chunks []plantedBytes }
	plantedForeign   struct{ Ref entity.ChunkRef }
	plantedArray     struct{ Data [4096]byte }
)

// A plan holds hashes and positions, never chunk bytes: BlockPlan passes, and
// each planted plan fails on its one field.
func TestPlanHoldsNoBytes(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeFor[BlockPlan](), nil},
		{reflect.TypeFor[plantedBytes](), []string{"plan.Data is []uint8"}},
		{reflect.TypeFor[plantedString](), []string{"plan.Data is string"}},
		{reflect.TypeFor[plantedAny](), []string{"plan.Data is interface {}"}},
		{reflect.TypeFor[plantedPointer](), []string{"plan.Data is *carver.ChunkInfo"}},
		{reflect.TypeFor[plantedInterface](), []string{"plan.Data is error"}},
		{reflect.TypeFor[plantedNested](), []string{"plan.Chunks[].Data is []uint8"}},
		{reflect.TypeFor[plantedForeign](), []string{"plan.Ref is entity.ChunkRef"}},
		{reflect.TypeFor[plantedArray](), []string{"plan.Data is [4096]uint8"}},
	} {
		if got := byteless(tc.typ, "plan"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: fields that could hold bytes %q, want %q", tc.typ, got, tc.want)
		}
	}
}

// funcDoc returns the doc comment of the function name in dir's non-test .go
// files, as go/doc reads it.
func funcDoc(dir, name string) (string, error) {
	paths, err := archtest.GoFiles(dir)
	if err != nil {
		return "", err
	}

	fset := token.NewFileSet()

	var files []*ast.File

	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return "", err
		}

		files = append(files, f)
	}

	pkg, err := doc.NewFromFiles(fset, files, "example.com/doc")
	if err != nil {
		return "", err
	}

	for _, f := range pkg.Funcs {
		if f.Name == name {
			return f.Doc, nil
		}
	}

	return "", fmt.Errorf("no func %s in %s", name, dir)
}

// Cut's doc comment, the one place its caller reads, says that emit's bytes
// are borrowed and that a caller keeping them copies them (RFC 2 §2.2); a
// planted Cut whose comment says neither is caught.
func TestCutSaysEmitsBytesAreBorrowed(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"carve", nil},
		{filepath.Join("testdata", "doc"), []string{"borrowed", "copies"}},
	} {
		d, err := funcDoc(tc.dir, "Cut")
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}

		var missing []string

		for _, w := range []string{"borrowed", "copies"} {
			if !strings.Contains(d, w) {
				missing = append(missing, w)
			}
		}

		if !slices.Equal(missing, tc.want) {
			t.Errorf("%s: Cut's doc lacks %q, want it to lack %q", tc.dir, missing, tc.want)
		}
	}
}
