package remote

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// aws is the AWS SDK for Go v2 with its runtime.
var aws = []string{"github.com/aws/aws-sdk-go-v2/...", "github.com/aws/smithy-go/..."}

// table is every package of the remote tier, landed or not, and what its
// production code may reach (RFC 0 §1.2, RFC 4 §1.2): the contract nothing but
// the standard library; the codec the contract and the transforms' and the
// metadata entities' contracts, never a transform, the carver, the engine or a
// store; a store the contract and its own packages, never the codec. A row is
// an upper bound: a package that reaches less passes.
var table = []archtest.Row{
	{Pkg: "pkg/remote"},
	{Pkg: "pkg/remote/codec", Reach: []string{"pkg/remote", "pkg/transform", "pkg/transform/keys", "pkg/metadata/entity/..."}},
	{Pkg: "pkg/remote/memory/...", Reach: []string{"pkg/remote", "pkg/sim", "pkg/remote/memory/..."}},
	{Pkg: "pkg/remote/remotetest", Reach: []string{"pkg/remote", "internal/prof"}},
	{Pkg: "pkg/remote/s3/...", Reach: []string{"pkg/remote", "pkg/remote/s3/..."}, But: []string{"pkg/remote/s3/s3fault/..."}, Libs: aws},
	{Pkg: "pkg/remote/s3/s3fault/...", Reach: []string{"pkg/remote/s3/s3fault/..."}, Libs: aws},
}

// Every package under pkg/remote reaches, through its production code, only
// what its row allows.
func TestImportWalk(t *testing.T) {
	pkgs, err := archtest.Walk(".")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(pkgs, func(p archtest.Listed) bool { return p.Path == archtest.Module+"pkg/remote" }) {
		t.Fatalf("the walk listed %d packages, none of them pkg/remote", len(pkgs))
	}

	for _, v := range archtest.Check(pkgs, table) {
		t.Error(v)
	}
}

// The walk over a planted module, whose codec reaches the engine through an
// intermediate package, and the carver, GC and the journal from files built
// only for Windows, only for arm64 and only with cgo, and whose codec test
// imports the syncer, reports exactly the production reaches: a walk of direct
// imports only misses the engine, a walk of one platform or without cgo misses
// the others, and a walk that binds tests reports the syncer too.
func TestImportWalkFollowsProductionImportsTransitively(t *testing.T) {
	pkgs, err := archtest.Walk("testdata/walk/pkg/remote")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"pkg/remote/codec reaches pkg/carver, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/engine, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/gc, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/journal, which row pkg/remote/codec does not allow",
	}
	if got := archtest.Check(pkgs, table); !slices.Equal(got, want) {
		t.Errorf("violations over the planted module %q, want %q", got, want)
	}
}

// The checker, one case per branch.
func TestImportCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkg  archtest.Listed
		want []string
	}{
		{"an allowed reach", archtest.Listed{
			Path:    archtest.Module + "pkg/remote/codec",
			Deps:    []string{archtest.Module + "pkg/remote", archtest.Module + "pkg/transform", archtest.Module + "pkg/metadata/entity/internal", "context"},
			Imports: []string{archtest.Module + "pkg/transform", "context"},
		}, nil},
		{"a reach the row refuses", archtest.Listed{
			Path: archtest.Module + "pkg/remote/codec",
			Deps: []string{archtest.Module + "pkg/remote", archtest.Module + "pkg/transform/chain"},
		}, []string{"pkg/remote/codec reaches pkg/transform/chain, which row pkg/remote/codec does not allow"}},
		{"a package with no row", archtest.Listed{Path: archtest.Module + "pkg/remote/gcs"}, []string{"pkg/remote/gcs has no row"}},
		{"a library the contract imports", archtest.Listed{
			Path:    archtest.Module + "pkg/remote",
			Imports: []string{"github.com/aws/aws-sdk-go-v2/aws", "io"},
		}, []string{"pkg/remote imports github.com/aws/aws-sdk-go-v2/aws, which row pkg/remote does not allow"}},
		{"a library a store imports", archtest.Listed{
			Path:    archtest.Module + "pkg/remote/s3",
			Deps:    []string{archtest.Module + "pkg/remote", archtest.Module + "pkg/remote/s3/internal/sign"},
			Imports: []string{"github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/smithy-go"},
		}, nil},
		{"a store reaching its fault transport", archtest.Listed{
			Path: archtest.Module + "pkg/remote/s3",
			Deps: []string{archtest.Module + "pkg/remote", archtest.Module + "pkg/remote/s3/s3fault"},
		}, []string{"pkg/remote/s3 reaches pkg/remote/s3/s3fault, which row pkg/remote/s3/... does not allow"}},
		{"the fault transport by its own row", archtest.Listed{
			Path: archtest.Module + "pkg/remote/s3/s3fault",
			Deps: []string{archtest.Module + "pkg/remote"},
		}, []string{"pkg/remote/s3/s3fault reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
		{"a package beneath the fault transport by its row", archtest.Listed{
			Path: archtest.Module + "pkg/remote/s3/s3fault/wire",
			Deps: []string{archtest.Module + "pkg/remote/s3/s3fault", archtest.Module + "pkg/remote"},
		}, []string{"pkg/remote/s3/s3fault/wire reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
	} {
		if got := archtest.Check([]archtest.Listed{tc.pkg}, table); !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}
