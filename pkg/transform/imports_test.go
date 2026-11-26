package transform

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// entities is the metadata entities' contract, the namespace ID's home, with
// the packages beneath it that only it imports.
const entities = "pkg/metadata/entity/..."

// treeReach is what every package under pkg/transform other than the two
// contracts may reach of the module: the component itself, the entities'
// contract, the remote tier's contract (the provider reads a census), and the
// key file and KMIP transport the provider builds on.
var treeReach = []string{"pkg/transform/...", entities, "pkg/remote", "internal/keyfile", "internal/kmip"}

// treeLibs is what every package under pkg/transform other than the two
// contracts may reach outside the module: what the entities' contract brings,
// with the stages' and the provider's libraries.
var treeLibs = archtest.EntityLibs(
	"golang.org/x/crypto/...",           // the encrypt stage's ciphers and key derivation
	"github.com/klauspost/compress/...", // the zstd stage
	"github.com/pierrec/lz4/v4",         // the lz4 stage
	"github.com/gemalto/kmip-go/...",    // the provider's KMIP client
	"github.com/google/uuid",            // key and material IDs
)

// table is every package of the transform component, landed or not, and what
// its production code may reach, transitively (RFC 0 §1.2, RFC 5 §3.1). The
// root's row comes before the tree's: both name pkg/transform equally
// closely, and the first wins.
//
//   - pkg/transform, the contract: pkg/transform/keys and the entities'
//     contract, never chain, a stage or the provider.
//   - pkg/transform/keys, the material contract: the entities' contract
//     only, never pkg/transform.
//   - every other package: treeReach and treeLibs.
//   - pkg/transform/transformtest and pkg/transform/keystest besides reach
//     internal/prof, whose leak check their suites run under.
//
// Left out on purpose, since each is or holds an implementation: pkg/settings,
// pkg/metrics and pkg/health, which keep a contract and its implementation
// together; pkg/keyprovider, v1's provider; pkg/metadata/kvstore, a store; and
// pkg/remote/s3, the remote tier's implementation.
//
// A row is an upper bound: a package that reaches less passes.
var table = []archtest.Row{
	{Pkg: "pkg/transform", Reach: []string{"pkg/transform/keys", entities}, Libs: archtest.EntityLibs()},
	{Pkg: "pkg/transform/keys", Reach: []string{entities}, Libs: archtest.EntityLibs()},
	{Pkg: "pkg/transform/...", Reach: treeReach, Libs: treeLibs},
	{Pkg: "pkg/transform/transformtest", Reach: append(slices.Clone(treeReach), "internal/prof"), Libs: treeLibs},
	{Pkg: "pkg/transform/keystest", Reach: append(slices.Clone(treeReach), "internal/prof"), Libs: treeLibs},
}

// contracts are the packages that import no library directly: what a
// contract's caller reaches through it is the entities' contract and what that
// brings, nothing the contract picked.
var contracts = []string{"pkg/transform", "pkg/transform/keys"}

// violations returns every way graphs break the transform component's import
// rule: a package reaching, transitively, what its row in table does not
// allow, and a contract importing a library outside the standard library
// directly.
//
// decision: only production files are held to the rule, since amended RFC 0
// §1.2 binds production code and lets tests import implementations, and every
// v1 package is deleted on this branch, so the compiler refuses a test that
// imports one. Withdraw this if a v1 package is restored, or if RFC 0 starts
// holding tests to the rule.
func violations(graphs ...archtest.Graph) []string {
	bad := archtest.Check(table, graphs...)

	for _, g := range graphs {
		for _, root := range g.Roots {
			pkg := strings.TrimPrefix(root, archtest.Module)
			if !slices.Contains(contracts, pkg) {
				continue
			}

			for _, imp := range g.Imports[root] {
				if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") && !strings.HasPrefix(imp, archtest.Module) {
					bad = append(bad, fmt.Sprintf("%s imports %s directly, which a contract may not", pkg, imp))
				}
			}
		}
	}

	slices.Sort(bad)

	return slices.Compact(bad)
}

// Every package under pkg/transform reaches, through its production code,
// only what its row allows, on every platform the walk lists, and every file
// under it is built for one of them.
func TestImportRule(t *testing.T) {
	graphs, err := archtest.Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range graphs {
		for _, want := range []string{"pkg/transform", "pkg/transform/keys", "pkg/transform/chain"} {
			if !slices.Contains(g.Roots, archtest.Module+want) {
				t.Fatalf("the walk listed %q, not %s", g.Roots, want)
			}
		}
	}

	for _, v := range violations(graphs...) {
		t.Error(v)
	}

	unwalked, err := archtest.Unwalked(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range unwalked {
		t.Error(v)
	}
}

// m names a package of the module by its module-relative path.
func m(rel string) string { return archtest.Module + rel }

// The rule over planted graphs, each break refused with the package, the
// import and the path it was reached by: a rule that checks direct imports
// only misses the transitive cases, one that lets the root reach chain misses
// the first, and one that admits pkg/metrics misses the chain's.
func TestImportRuleRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    string
		imports map[string][]string
		want    []string
	}{
		{"the root as it is", m("pkg/transform"), map[string][]string{
			m("pkg/transform"):       {m("pkg/transform/keys"), m("pkg/metadata/entity"), "context", "errors"},
			m("pkg/transform/keys"):  {m("pkg/metadata/entity"), "context"},
			m("pkg/metadata/entity"): {"github.com/google/uuid", "lukechampine.com/blake3"},
		}, nil},
		{"the chain as it is", m("pkg/transform/chain"), map[string][]string{
			m("pkg/transform/chain"): {m("pkg/transform"), m("pkg/transform/keys"), m("pkg/metadata/entity"), "log/slog"},
			m("pkg/transform"):       {m("pkg/transform/keys"), m("pkg/metadata/entity")},
		}, nil},
		{"the root importing the chain", m("pkg/transform"), map[string][]string{
			m("pkg/transform"): {m("pkg/transform/keys"), m("pkg/transform/chain")},
		}, []string{"pkg/transform reaches pkg/transform/chain, which row pkg/transform does not allow"}},
		{"the root reaching the provider through a dependency's dependency", m("pkg/transform"), map[string][]string{
			m("pkg/transform"):       {m("pkg/transform/keys")},
			m("pkg/transform/keys"):  {m("pkg/metadata/entity")},
			m("pkg/metadata/entity"): {m("pkg/transform/keys/provider")},
		}, []string{"pkg/transform reaches pkg/transform/keys/provider through pkg/transform/keys -> pkg/metadata/entity, which row pkg/transform does not allow"}},
		{"keys importing the root", m("pkg/transform/keys"), map[string][]string{
			m("pkg/transform/keys"): {m("pkg/metadata/entity"), m("pkg/transform")},
		}, []string{"pkg/transform/keys reaches pkg/transform, which row pkg/transform/keys does not allow"}},
		{"the chain importing pkg/metrics", m("pkg/transform/chain"), map[string][]string{
			m("pkg/transform/chain"): {m("pkg/transform"), m("pkg/metrics")},
		}, []string{"pkg/transform/chain reaches pkg/metrics, which row pkg/transform/... does not allow"}},
		{"a stage reaching a store through the entities' contract", m("pkg/transform/compress/zstd"), map[string][]string{
			m("pkg/transform/compress/zstd"): {m("pkg/transform"), m("pkg/metadata/entity")},
			m("pkg/metadata/entity"):         {m("pkg/metadata/kvstore")},
		}, []string{"pkg/transform/compress/zstd reaches pkg/metadata/kvstore through pkg/metadata/entity, which row pkg/transform/... does not allow"}},
		{"a stage importing a module off the list", m("pkg/transform/compress/lz"), map[string][]string{
			m("pkg/transform/compress/lz"): {m("pkg/transform"), "github.com/pierrec/lz4"},
		}, []string{"pkg/transform/compress/lz imports github.com/pierrec/lz4, which row pkg/transform/... does not allow"}},
		{"a stage reaching the test suites' leak check", m("pkg/transform/compress/zstd"), map[string][]string{
			m("pkg/transform/compress/zstd"): {m("internal/prof")},
		}, []string{"pkg/transform/compress/zstd reaches internal/prof, which row pkg/transform/... does not allow"}},
		{"the root importing BLAKE3 directly", m("pkg/transform"), map[string][]string{
			m("pkg/transform"): {m("pkg/metadata/entity"), "lukechampine.com/blake3"},
		}, []string{"pkg/transform imports lukechampine.com/blake3 directly, which a contract may not"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := violations(archtest.Graph{Roots: []string{tc.root}, Imports: tc.imports})
			if !slices.Equal(got, tc.want) {
				t.Errorf("violations %q, want %q", got, tc.want)
			}
		})
	}
}
