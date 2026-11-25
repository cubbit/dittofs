package remote

import (
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
	"List(ctx context.Context, after Cursor, n int) (page []Info, next Cursor, done bool, err error); " +
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
	"func (Role).String() string",
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
	"type Cursor string",
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

// hiddenCase names the case whose planted file sits in the package's
// directory behind a build tag, so it reaches the pin through the directory
// scan, as a real file would.
const hiddenCase = "hidden.go"

// errorVars are the package-level variables the contract may hold: its error
// set, and nothing else (RFC 4 §1.2).
var errorVars = []string{
	"ErrNotFound", "ErrInvalid", "ErrDenied", "ErrTransient", "ErrCorrupt", "ErrThrottled", "ErrMalformed",
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
		{"extra alias", "extra_alias.go", nil, []string{"extra export: type Label = string"}},
		{"extra generic type", "extra_generic.go", nil, []string{"extra export: type Box[T any] struct{V T}"}},
		{"a defined type made an alias", "", func(want []string) []string {
			return slices.Replace(want, slices.Index(want, "type Version string"), slices.Index(want, "type Version string")+1, "type Version = string")
		}, []string{"extra export: type Version string", "missing export: type Version = string"}},
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
			var err error

			dir, err = archtest.WithHidden(t.TempDir(), ".", filepath.Join("testdata", "arch", hiddenCase))
			if err != nil {
				t.Fatal(err)
			}
		default:
			extra = planted(tc.planted)
		}

		want := slices.Clone(frozenExports)
		if tc.edit != nil {
			want = tc.edit(want)
		}

		got, err := archtest.Exports(exportImporter, "github.com/cubbit/dittofs/pkg/remote", dir, extra...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		bad := archtest.Violations(got, want)

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
			var err error

			dir, err = archtest.WithHidden(t.TempDir(), ".", filepath.Join("testdata", "arch", hiddenCase))
			if err != nil {
				t.Fatal(err)
			}
		default:
			extra = planted(tc.planted)
		}

		bad, err := archtest.Impurities(exportImporter, "github.com/cubbit/dittofs/pkg/remote", dir, archtest.Purity{Vars: errorVars, Interfaces: true}, extra...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if !slices.Equal(bad, tc.want) {
			t.Errorf("%s: impurities %q, want %q", tc.name, bad, tc.want)
		}
	}
}
