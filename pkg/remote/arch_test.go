package remote

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// storeExport is the Store interface as the pin prints it, every method with
// its signature.
const storeExport = "type Store interface{" +
	"Checksum() Checksum; " +
	"Close() error; " +
	"Delete(ctx context.Context, names []Name) []error; " +
	"Get(ctx context.Context, name Name, r Range) (body io.ReadCloser, stored StoredSum, err error); " +
	"GetControl(ctx context.Context, role Role) (body []byte, v Version, err error); " +
	"Health(ctx context.Context, d Direction) error; " +
	"List(ctx context.Context, after Name, n int) (page []Info, next Name, done bool, err error); " +
	"Put(ctx context.Context, name Name, body io.Reader, size int64, sum []byte) error; " +
	"PutControl(ctx context.Context, role Role, body []byte) error}"

// frozenExports is pkg/remote's surface for v1.1 (RFC 4 §4.1): the calls v1.1
// has a caller for, the types and constants they carry, and the error set. A
// change here is a change to the contract every store and consumer shares.
var frozenExports = []string{
	"type Name [32]byte",
	"type Role uint8",
	"const RoleClaim Role = 1",
	"const RoleHealth Role = 2",
	"const RoleKeys Role = 3",
	"type Mode uint8",
	"const Mutable Mode = 1",
	"const Immutable Mode = 2",
	"type Version string",
	"type Checksum uint8",
	"const ChecksumNone Checksum = 0",
	"const ChecksumCRC32C Checksum = 1",
	"const ChecksumMD5 Checksum = 2",
	"type StoredSum struct{Kind Checksum; Sum []byte}",
	"type Direction uint8",
	"const DirPut Direction = 1",
	"const DirGet Direction = 2",
	"type Range struct{Off int64; Len int64}",
	"type Chunk struct{Hash [32]byte; Bytes []byte}",
	"type ChunkRange struct{Hash [32]byte; Range Range}",
	"type Meter struct{Moved func(n int64); Sent func()}",
	"type Info struct{Name Name; Size int64; ModTime time.Time}",
	storeExport,
	"var ErrNotFound error",
	"var ErrInvalid error",
	"var ErrDenied error",
	"var ErrTransient error",
	"var ErrCorrupt error",
	"var ErrThrottled error",
	"var ErrMalformed error",
	"type Cause uint8",
	"const CauseAccess Cause = 1",
	"const CauseQuota Cause = 2",
	"const CauseSize Cause = 3",
	"func (Cause).String() string",
	"type DeniedError struct{Cause Cause; Code string}",
	"func (*DeniedError).Error() string",
	"func (*DeniedError).Unwrap() error",
}

// exportImporter reads each import from compiled export data; the package
// itself is always parsed from its files, the planted ones included.
var exportImporter = archtest.ExportImporter(".")

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

	conf := types.Config{Importer: exportImporter}

	pkg, err := conf.Check("github.com/cubbit/dittofs/pkg/remote", fset, files, nil)
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

// errorVars are the package-level variables the contract may hold: its error
// set, and nothing else (RFC 4 §1.2).
var errorVars = []string{
	"ErrNotFound", "ErrInvalid", "ErrDenied", "ErrTransient", "ErrCorrupt", "ErrThrottled", "ErrMalformed",
}

// impurities parses dir's non-test .go files, whatever their build tags, plus
// the planted ones, and reports each package-level var other than the error
// set and each function that is not a method, init included: the contract
// holds no state and no behaviour beyond methods over its own values (RFC 4
// §1.2), which neither the import walk nor the export pin can see.
func impurities(dir string, planted ...string) ([]string, error) {
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

	var bad []string

	for _, p := range paths {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					bad = append(bad, "func "+d.Name.Name+" is not a method")
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}

				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if !slices.Contains(errorVars, n.Name) {
							bad = append(bad, "var "+n.Name+" is state")
						}
					}
				}
			}
		}
	}

	slices.Sort(bad)

	return bad, nil
}

// planted returns each name as a path under testdata/arch.
func planted(names ...string) []string {
	var paths []string

	for _, n := range names {
		if n != "" {
			paths = append(paths, filepath.Join("testdata", "arch", n))
		}
	}

	return paths
}

// pkg/remote exports exactly its frozen surface; each planted change is
// caught: an extra export of each kind, a missing one, a changed signature of
// a Store method, and an export behind a build tag.
func TestRemoteExports(t *testing.T) {
	// changedStore is the Store line with Put's sum dropped.
	changedStore := strings.Replace(storeExport, "size int64, sum []byte) error", "size int64) error", 1)

	for _, tc := range []struct {
		name    string
		planted string
		edit    func(want []string) []string
		want    []string
	}{
		{"the package", "", nil, nil},
		{"extra function", "extra_func.go", nil, []string{"extra export: func Extra()"}},
		{"extra method", "extra_method.go", nil, []string{"extra export: func (Name).Extra()"}},
		{"extra type", "extra_type.go", nil, []string{"extra export: type Extra struct{A int}"}},
		{"extra variable", "extra_var.go", nil, []string{"extra export: var Leak int"}},
		{"missing export", "", func(want []string) []string {
			return append(want, "func Gone()")
		}, []string{"missing export: func Gone()"}},
		{"changed Store signature", "", func(want []string) []string {
			return slices.Replace(want, slices.Index(want, storeExport), slices.Index(want, storeExport)+1, changedStore)
		}, []string{"extra export: " + storeExport, "missing export: " + changedStore}},
		{"behind a build tag", hiddenCase, nil, []string{"extra export: func Hidden()"}},
	} {
		dir := "."

		var extra []string

		switch tc.planted {
		case hiddenCase:
			dir = withHidden(t)
		default:
			extra = planted(tc.planted)
		}

		want := slices.Clone(frozenExports)
		if tc.edit != nil {
			want = tc.edit(want)
		}

		got, err := exports(dir, extra...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		bad := violations(got, want)

		if !slices.Equal(bad, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, bad, tc.want)
		}
	}
}

// pkg/remote holds no package-level state but its errors and no function but
// methods; each planted file, one impurity each, is caught.
func TestRemoteIsPure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planted string
		want    []string
	}{
		{"the package", "", nil},
		{"unexported state", "state.go", []string{"var cache is state"}},
		{"exported state", "extra_var.go", []string{"var Leak is state"}},
		{"free function", "free_func.go", []string{"func helper is not a method"}},
		{"init", "init.go", []string{"func init is not a method"}},
		{"behind a build tag", hiddenCase, []string{"func Hidden is not a method"}},
	} {
		dir := "."

		var extra []string

		switch tc.planted {
		case hiddenCase:
			dir = withHidden(t)
		default:
			extra = planted(tc.planted)
		}

		bad, err := impurities(dir, extra...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if !slices.Equal(bad, tc.want) {
			t.Errorf("%s: impurities %q, want %q", tc.name, bad, tc.want)
		}
	}
}
