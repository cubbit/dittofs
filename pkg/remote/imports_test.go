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
	{Pkg: "pkg/remote/codec", Reach: []string{"pkg/remote", "pkg/transform", "pkg/transform/keys", "pkg/metadata/entity/..."}, Libs: archtest.EntityLibs()},
	{Pkg: "pkg/remote/memory/...", Reach: []string{"pkg/remote", "pkg/sim", "pkg/remote/memory/..."}},
	{Pkg: "pkg/remote/remotetest", Reach: []string{"pkg/remote", "internal/prof"}},
	{Pkg: "pkg/remote/s3/...", Reach: []string{"pkg/remote", "pkg/remote/s3/..."}, But: []string{"pkg/remote/s3/s3fault/..."}, Libs: aws},
	{Pkg: "pkg/remote/s3/s3fault/...", Reach: []string{"pkg/remote/s3/s3fault/..."}, Libs: aws},
}

// Every package under pkg/remote reaches, through its production code, only
// what its row allows, and every file under it is built for a platform the
// walk lists.
func TestImportWalk(t *testing.T) {
	graphs, err := archtest.Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range graphs {
		if !slices.Contains(g.Roots, archtest.Module+"pkg/remote") {
			t.Fatalf("the walk listed %q, not pkg/remote", g.Roots)
		}
	}

	for _, v := range archtest.Check(table, graphs...) {
		t.Error(v)
	}

	unwalked, err := archtest.Unwalked(".", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range unwalked {
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
	graphs, err := archtest.Walk("testdata/walk/pkg/remote", false)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"pkg/remote/codec reaches pkg/carver, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/engine through pkg/transform, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/gc, which row pkg/remote/codec does not allow",
		"pkg/remote/codec reaches pkg/journal, which row pkg/remote/codec does not allow",
	}
	if got := archtest.Check(table, graphs...); !slices.Equal(got, want) {
		t.Errorf("violations over the planted module %q, want %q", got, want)
	}
}

// m names a package of the module by its module-relative path.
func m(rel string) string { return archtest.Module + rel }

// The checker, one case per branch.
func TestImportCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    string
		imports map[string][]string
		want    []string
	}{
		{"an allowed reach", m("pkg/remote/codec"), map[string][]string{
			m("pkg/remote/codec"):             {m("pkg/remote"), m("pkg/transform"), "context"},
			m("pkg/transform"):                {m("pkg/metadata/entity/internal")},
			m("pkg/metadata/entity/internal"): {"github.com/google/uuid"},
		}, nil},
		{"a reach the row refuses", m("pkg/remote/codec"), map[string][]string{
			m("pkg/remote/codec"): {m("pkg/remote"), m("pkg/transform/chain")},
		}, []string{"pkg/remote/codec reaches pkg/transform/chain, which row pkg/remote/codec does not allow"}},
		{"a reach through an allowed package", m("pkg/remote/codec"), map[string][]string{
			m("pkg/remote/codec"): {m("pkg/transform")},
			m("pkg/transform"):    {m("pkg/engine")},
		}, []string{"pkg/remote/codec reaches pkg/engine through pkg/transform, which row pkg/remote/codec does not allow"}},
		{"a package with no row", m("pkg/remote/gcs"), nil, []string{"pkg/remote/gcs has no row"}},
		{"a library the contract imports", m("pkg/remote"), map[string][]string{
			m("pkg/remote"): {"github.com/aws/aws-sdk-go-v2/aws", "io"},
		}, []string{"pkg/remote imports github.com/aws/aws-sdk-go-v2/aws, which row pkg/remote does not allow"}},
		{"a library the contract reaches through a module package", m("pkg/remote"), map[string][]string{
			m("pkg/remote"):          {m("pkg/remote/internal")},
			m("pkg/remote/internal"): {"github.com/google/uuid"},
		}, []string{
			"pkg/remote reaches github.com/google/uuid through pkg/remote/internal, which row pkg/remote does not allow",
			"pkg/remote reaches pkg/remote/internal, which row pkg/remote does not allow",
		}},
		{"a library a store imports", m("pkg/remote/s3"), map[string][]string{
			m("pkg/remote/s3"): {m("pkg/remote"), m("pkg/remote/s3/internal/sign"), "github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/smithy-go"},
		}, nil},
		{"a library a store reaches through the SDK", m("pkg/remote/s3"), map[string][]string{
			m("pkg/remote/s3"):                        {"github.com/aws/aws-sdk-go-v2/service/s3"},
			"github.com/aws/aws-sdk-go-v2/service/s3": {"github.com/jmespath/go-jmespath"},
		}, []string{"pkg/remote/s3 reaches github.com/jmespath/go-jmespath through github.com/aws/aws-sdk-go-v2/service/s3, which row pkg/remote/s3/... does not allow"}},
		{"a store reaching its fault transport", m("pkg/remote/s3"), map[string][]string{
			m("pkg/remote/s3"): {m("pkg/remote"), m("pkg/remote/s3/s3fault")},
		}, []string{"pkg/remote/s3 reaches pkg/remote/s3/s3fault, which row pkg/remote/s3/... does not allow"}},
		{"the fault transport by its own row", m("pkg/remote/s3/s3fault"), map[string][]string{
			m("pkg/remote/s3/s3fault"): {m("pkg/remote")},
		}, []string{"pkg/remote/s3/s3fault reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
		{"a package beneath the fault transport by its row", m("pkg/remote/s3/s3fault/wire"), map[string][]string{
			m("pkg/remote/s3/s3fault/wire"): {m("pkg/remote/s3/s3fault"), m("pkg/remote")},
		}, []string{"pkg/remote/s3/s3fault/wire reaches pkg/remote, which row pkg/remote/s3/s3fault/... does not allow"}},
	} {
		g := archtest.Graph{Roots: []string{tc.root}, Imports: tc.imports}
		if got := archtest.Check(table, g); !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
}
