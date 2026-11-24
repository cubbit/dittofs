package node

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	gobuild "go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// configFields are the fields of Config: what the host supplies and the node
// cannot build.
var configFields = []string{"Bootstrap", "KV", "Clock", "Metrics", "Logger", "Tracer", "Journal"}

// nodeExports returns the violations of the node's surface in the package
// whose sources are in dir: an exported package-level declaration other than
// New, Node, Config and Preflight; an exported method of Node other than Start and Stop,
// or of another exported type, a method of an unexported type being no part
// of the surface;
// a field Node embeds, whose methods it would promote, or exports; a Config
// whose fields are not exactly configFields; a method whose receiver it cannot
// name, which it reports rather than skips; and each of New, Node, Config,
// Start and Stop missing. It reads every .go file but the tests, whatever its
// build tags, so a file behind a tag cannot hide one.
func nodeExports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{"New": false, "Node": false, "Config": false, "Preflight": false, "Node.Start": false, "Node.Stop": false}
	var bad []string
	note := func(name string) {
		if _, ok := want[name]; ok {
			want[name] = true
		} else {
			bad = append(bad, "extra export: "+name)
		}
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					note(d.Name.Name)
					continue
				}
				recv := d.Recv.List[0].Type
				if s, ok := recv.(*ast.StarExpr); ok {
					recv = s.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					if id.IsExported() {
						note(id.Name + "." + d.Name.Name)
					}
				} else {
					bad = append(bad, "unresolved receiver of "+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							note(s.Name.Name)
						}
						bad = append(bad, structViolations(s)...)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								note(n.Name)
							}
						}
					}
				}
			}
		}
	}
	for name, seen := range want {
		if !seen {
			bad = append(bad, "missing export: "+name)
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// structViolations checks the fields of Node and Config: Node embeds nothing
// and exports no field, and Config's fields are exactly configFields.
func structViolations(s *ast.TypeSpec) []string {
	if s.Name.Name != "Node" && s.Name.Name != "Config" {
		return nil
	}
	st, ok := s.Type.(*ast.StructType)
	if !ok || s.TypeParams != nil || s.Assign.IsValid() {
		return []string{s.Name.Name + " is not a plain struct"}
	}
	var bad, names []string
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			bad = append(bad, "embedded field in "+s.Name.Name)
			continue
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
			if s.Name.Name == "Node" && n.IsExported() {
				bad = append(bad, "exported field: Node."+n.Name)
			}
		}
	}
	if s.Name.Name == "Config" && !slices.Equal(names, configFields) {
		bad = append(bad, "Config fields "+strings.Join(names, ","))
	}
	return bad
}

// The node exports New, Node, Config and Preflight, and Node's Start and
// Stop, and nothing else; each planted tree, holding one violation, is caught.
func TestNodeExports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{".", nil},
		{"extra-func", []string{"extra export: Extra"}},
		{"extra-method", []string{"extra export: Node.Pause"}},
		{"embedded", []string{"embedded field in Node"}},
		{"exported-field", []string{"exported field: Node.State"}},
		{"config-field", []string{"Config fields Bootstrap,KV,Clock,Metrics,Logger,Tracer,Journal,Fault"}},
		{"generic", []string{"unresolved receiver of Open"}},
	} {
		dir := tc.dir
		if dir != "." {
			dir = filepath.Join("testdata", "arch", dir)
		}
		got, err := nodeExports(dir)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.dir, got, tc.want)
		}
	}
}

const module = "github.com/cubbit/dittofs/"

// under reports whether p is root or a path below it.
func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// standard reports whether an import path is the standard library's: its
// first element has no dot, and it is not cgo's "C", which links C code.
func standard(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return imp != "C" && !strings.Contains(first, ".")
}

// fileImports is one walk row: a .go file's slash path relative to the walked
// root, the paths it imports, and whether it is a test file — a rule that
// ignores tests skips such a row rather than the walk skipping the file.
type fileImports struct {
	File    string
	Imports []string
	Test    bool
}

// walkSnapshot returns each .go file under root as one fileImports row, test
// files included, each row marked. It leaves out testdata, hidden directories
// and nested modules.
//
// decision: every file is parsed with go/parser, ignoring build constraints
// and GOOS or GOARCH file-name suffixes, rather than listed through go/build,
// which reads only the files of one build; so no file behind a tag or for
// another platform can hide an import from a rule. Its ceiling is the source
// on disk: an import that only a build-time generator writes is not seen.
func walkSnapshot(root string) ([]fileImports, error) {
	var rows []fileImports
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		var imports []string
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			imports = append(imports, ip)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rows = append(rows, fileImports{File: filepath.ToSlash(rel), Imports: imports, Test: strings.HasSuffix(p, "_test.go")})
		return nil
	})
	return rows, err
}

// v1Paths are v1's control plane and its client, which never return at these
// paths. pkg/identity comes back in v2 at the same path, so the compiler, not this
// rule, guards its deletion.
var v1Paths = []string{module + "pkg/controlplane", module + "internal/controlplane", module + "pkg/snapshot", module + "pkg/apiclient"}

// v1Imports returns each import of a v1 path by a file of root's rows, tests
// included.
func v1Imports(_ string, files []fileImports) ([]string, error) {
	var bad []string
	for _, r := range files {
		for _, imp := range r.Imports {
			if slices.ContainsFunc(v1Paths, func(v1 string) bool { return under(imp, v1) }) {
				bad = append(bad, r.File+" imports "+imp)
			}
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// nodeImporters returns each file of root's rows, tests included, that
// imports the node but may not: only the server's start command and the test
// harness build a node, and the node's own external tests drive it.
func nodeImporters(_ string, files []fileImports) ([]string, error) {
	var bad []string
	for _, r := range files {
		// decision: the harness under internal/node/nodetest is the one
		// importer besides dfs start, because it drives a whole node in tests
		// as dfs start does in production; the node's own _test.go files are
		// its tests, in package node_test only because they use the harness,
		// which imports the node. No other exemption is named.
		ownTest := path.Dir(r.File) == "internal/node" && strings.HasSuffix(r.File, "_test.go")
		if r.File == "cmd/dfs/commands/start.go" || under(r.File, "internal/node/nodetest") || ownTest {
			continue
		}
		if slices.Contains(r.Imports, module+"internal/node") {
			bad = append(bad, r.File+" imports the node")
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// startPackage is dfs start's package, and startAllowed what of this module
// its files may import: the node, the bootstrap, Badger, the KV interface
// the node's store is typed by, and the metrics registry. The command reads
// the bootstrap and opens the KV and the registry, and the node builds every
// component. start.go is the one file of
// them that imports the node, so nodeImporters names only it.
//
// decision: every non-test file of the package is held to startAllowed, a new
// one included, but those of startExempt: init.go is dfs init's, which builds
// an installation and so imports the installer and the key provider,
// reset_admin.go is dfs reset-admin's, which imports the same two, and
// root.go wires the subcommands, the config command's package among them.
// None runs in dfs start, so a held file may not call a function any of them
// declares, whatever that function imports. The ceiling: only calls are
// flagged, not a variable, type or method an exempt file declares, since the
// start reads root.go's build variables; a method value or a func-typed
// variable declared there would pass. Overturned if one of them must serve
// dfs start, and then by moving that code into a file the rule holds.
const startPackage = "cmd/dfs/commands"

var (
	startAllowed = []string{"internal/node", "pkg/bootstrap", "pkg/metadata/kv", "pkg/metadata/kv/badgerkv", "pkg/metadata/kv/fdbkv", "pkg/metrics"}
	startExempt  = []string{"cmd/dfs/commands/init.go", "cmd/dfs/commands/reset_admin.go", "cmd/dfs/commands/root.go"}
)

// startImports returns each import of this module by a non-test file of
// startPackage outside startExempt that startAllowed does not name, each call
// from such a file to a function one of startExempt declares, and start.go
// and each of startExempt missing under root, so that a file renamed away
// can leave the rule neither passing over nothing nor exempting a name a new
// file could take.
func startImports(root string, files []fileImports) ([]string, error) {
	var bad []string
	seen := map[string]bool{}
	for _, r := range files {
		if r.Test || path.Dir(r.File) != startPackage {
			continue
		}
		seen[r.File] = true
		if slices.Contains(startExempt, r.File) {
			continue
		}
		for _, imp := range r.Imports {
			if rel, ok := strings.CutPrefix(imp, module); ok && !slices.Contains(startAllowed, rel) {
				bad = append(bad, r.File+" imports "+imp)
			}
		}
	}
	for _, f := range append([]string{startPackage + "/start.go"}, startExempt...) {
		if !seen[f] {
			bad = append(bad, f+" is missing")
		}
	}
	calls, err := exemptCalls(root)
	bad = append(bad, calls...)
	slices.Sort(bad)
	return bad, err
}

// exemptCalls returns each call from a non-test file of startPackage under
// root outside startExempt to a function declared in one of startExempt.
// The package is type-checked with its type errors ignored, so a planted
// tree need not compile whole, and with no import resolved: a call by a bare
// name resolves within the package whatever its imports hold, and loading
// them from source would type-check the whole closure of dfs.
func exemptCalls(root string) ([]string, error) {
	fset := token.NewFileSet()
	files, err := parsePackage(fset, filepath.Join(root, filepath.FromSlash(startPackage)))
	if err != nil {
		return nil, err
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: noImports{}, Error: func(error) {}}
	_, _ = conf.Check(module+startPackage, fset, files, info)
	rel := func(pos token.Pos) string {
		return startPackage + "/" + filepath.Base(fset.Position(pos).Filename)
	}
	var bad []string
	for _, f := range files {
		file := rel(f.Pos())
		if slices.Contains(startExempt, file) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := ast.Unparen(call.Fun).(*ast.Ident)
			if !ok {
				return true
			}
			if fn, ok := info.Uses[id].(*types.Func); ok && slices.Contains(startExempt, rel(fn.Pos())) {
				bad = append(bad, file+" calls "+fn.Name()+", declared in "+rel(fn.Pos()))
			}
			return true
		})
	}
	return bad, nil
}

// noImports refuses every import, so a type check resolves only the
// package's own names.
type noImports struct{}

func (noImports) Import(path string) (*types.Package, error) {
	return nil, errors.New("not loaded: " + path)
}

// enginewireImporters returns each non-test file of root's rows that imports
// enginewire outside the two packages its package doc names: the node and
// enginetest.
func enginewireImporters(_ string, files []fileImports) ([]string, error) {
	var bad []string
	for _, r := range files {
		if r.Test {
			continue
		}
		dir := path.Dir(r.File)
		if dir == "internal/node" || dir == "pkg/engine/enginetest" {
			continue
		}
		if slices.Contains(r.Imports, module+"pkg/engine/enginewire") {
			bad = append(bad, r.File+" imports enginewire")
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// cliRoots are where the CLI's dependency closure starts, and cliAllowed what
// of this module it may reach: neither the metadata or block stores, nor the
// control-plane server, nor anything that reads the host's bootstrap.
var (
	cliRoots   = []string{"cmd/dfsctl", "pkg/node/apiv1", "pkg/node/apiv1/client"}
	cliAllowed = []string{"cmd/dfsctl", "pkg/node/apiv1", "pkg/node/apiv1/client", "internal/cli"}
)

// cliClosure returns each package of this module the CLI's binary reaches
// outside cliAllowed, and each import of a package under pkg/node/apiv1
// outside the standard library and pkg/node/apiv1 itself, which its client
// imports. It follows non-test files only, as the
// binary is built from them.
func cliClosure(_ string, files []fileImports) ([]string, error) {
	pkgs := map[string][]string{}
	for _, r := range files {
		if r.Test {
			continue
		}
		dir := path.Dir(r.File)
		pkgs[dir] = append(pkgs[dir], r.Imports...)
	}
	var bad, queue []string
	seen := map[string]bool{}
	for dir := range pkgs {
		if slices.ContainsFunc(cliRoots, func(r string) bool { return under(dir, r) }) {
			seen[dir] = true
			queue = append(queue, dir)
		}
	}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		for _, imp := range pkgs[dir] {
			if under(dir, "pkg/node/apiv1") && !standard(imp) && imp != module+"pkg/node/apiv1" {
				bad = append(bad, dir+" imports "+imp+" outside the standard library")
			}
			dep, ok := strings.CutPrefix(imp, module)
			if !ok || seen[dep] {
				continue
			}
			seen[dep] = true
			if !slices.ContainsFunc(cliAllowed, func(a string) bool { return under(dep, a) }) {
				bad = append(bad, dir+" reaches "+dep)
			}
			queue = append(queue, dep)
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// settingsLeaf is the API's settings section, which imports the standard
// library only, so every package that reads or stores the section can import
// it without reaching anything else.
const settingsLeaf = "internal/node/api/apisettings"

// settingsImports returns each import by a non-test file of settingsLeaf
// outside the standard library.
func settingsImports(_ string, files []fileImports) ([]string, error) {
	var bad []string
	for _, r := range files {
		if r.Test || !under(path.Dir(r.File), settingsLeaf) {
			continue
		}
		for _, imp := range r.Imports {
			if !standard(imp) {
				bad = append(bad, r.File+" imports "+imp)
			}
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// weakKeyCallers returns each non-test file under root that calls
// GenerateWeakKeyFile, whose key file protects its master key at the lowest
// cost a key file may record: only a test, or a package of test helpers
// whose name ends in test, may write one. It walks root itself, ignoring
// files: it reads each file's calls, not its imports, so a shared import
// snapshot serves it nothing — its parse is of whole files.
func weakKeyCallers(root string, _ []fileImports) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if strings.HasSuffix(f.Name.Name, "test") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name == "GenerateWeakKeyFile" {
				bad = append(bad, filepath.ToSlash(rel)+" calls GenerateWeakKeyFile")
			}
			return true
		})
		return nil
	})
	slices.Sort(bad)
	return bad, err
}

// Each import rule reports nothing over the module and exactly the violation
// planted in its testdata tree, beside the allowed look-alikes planted with it.
func TestImportRules(t *testing.T) {
	t.Parallel()
	// One walk of the module, shared by every rule's module check; a rule
	// that reads calls rather than imports walks root itself.
	files, err := walkSnapshot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("over the module: %v", err)
	}
	for _, tc := range []struct {
		name string
		rule func(root string, files []fileImports) ([]string, error)
		tree string
		want []string
	}{
		{"no v1 import", v1Imports, "no-v1",
			[]string{
				"pkg/cli/cli.go imports " + module + "pkg/apiclient",
				"pkg/store/store.go imports " + module + "pkg/controlplane/runtime",
				"pkg/store/store_test.go imports " + module + "pkg/controlplane/models",
			}},
		{"who imports the node", nodeImporters, "node-importers",
			[]string{
				"cmd/dfs/commands/stop.go imports the node",
				"internal/cli/cli_test.go imports the node",
				"internal/node/nodetest2/harness.go imports the node",
			}},
		{"what dfs start imports", startImports, "start-imports",
			[]string{
				"cmd/dfs/commands/kv.go imports " + module + "pkg/metadata/kvstore",
				"cmd/dfs/commands/records.go imports " + module + "pkg/metadata/kvstore",
				"cmd/dfs/commands/start.go calls initRecords, declared in cmd/dfs/commands/init.go",
			}},
		{"a file of dfs start renamed away", startImports, "start-imports-missing",
			[]string{"cmd/dfs/commands/root.go is missing", "cmd/dfs/commands/start.go is missing"}},
		{"who imports enginewire", enginewireImporters, "enginewire-importers",
			[]string{"pkg/vfs/vfs.go imports enginewire"}},
		{"the CLI's closure", cliClosure, "cli-closure",
			[]string{"internal/cli/output reaches pkg/metadata"}},
		{"the wire types' imports", cliClosure, "apiv1-stdlib",
			[]string{
				"pkg/node/apiv1 imports C outside the standard library",
				"pkg/node/apiv1 imports github.com/spf13/cobra outside the standard library",
				"pkg/node/apiv1/client imports github.com/google/uuid outside the standard library",
			}},
		{"who writes a weak key file", weakKeyCallers, "weak-key",
			[]string{"pkg/store/store.go calls GenerateWeakKeyFile"}},
		{"the API settings' imports", settingsImports, "settings-leaf",
			[]string{"internal/node/api/apisettings/settings.go imports " + module + "pkg/node/apiv1"}},
	} {
		got, err := tc.rule(filepath.Join("..", ".."), files)
		if err != nil {
			t.Fatalf("%s over the module: %v", tc.name, err)
		}
		if len(got) > 0 {
			t.Errorf("%s over the module: violations %q, want none", tc.name, got)
		}
		tree := filepath.Join("testdata", "arch", tc.tree)
		rows, err := walkSnapshot(tree)
		if err != nil {
			t.Fatalf("%s over %s: %v", tc.name, tc.tree, err)
		}
		got, err = tc.rule(tree, rows)
		if err != nil {
			t.Fatalf("%s over %s: %v", tc.name, tc.tree, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s over %s: violations %q, want %q", tc.name, tc.tree, got, tc.want)
		}
	}
}

// componentAllowed reports whether a package under internal/node but node
// and nodetest may import the module's package rel: none it allows is a
// component's implementation, the entity and wire packages holding data
// types only, the metadata store's and filesystem's packages of error values,
// which the API answers by name, KV, of which errorsOnly admits only the
// error values, the TLS library the API's listeners load their
// certificate with, the settings table's package, which imports only the
// entity package and whose table the node hands in, and the secret
// provider's refusals, apart from the provider, which the node hands in
// behind an interface the API declares, the names onlyNames admits of
// identity's package, and the SID's encoding, which the domain's routes
// store, apart from identity's server, also handed in behind interfaces the
// API declares; nodetest, which imports Badger, is not allowed. Everything
// else it reaches through an interface it declares or a value the node hands
// it.
func componentAllowed(rel string) bool {
	switch rel {
	case "pkg/health", "pkg/metrics", "pkg/metadata/entity", "pkg/node/apiv1",
		"pkg/metadata/errors", "pkg/metadata/kv", "pkg/vfs/vfserr", "internal/tlsconfig",
		"pkg/settings", "pkg/secrets/secreterr", "pkg/auth/sid":
		return true
	}
	return strings.HasPrefix(rel, "internal/node/") && !strings.HasPrefix(rel, "internal/node/nodetest")
}

// onlyNames are the allowed packages of which a file may use only some
// names: KV, which also declares a store's interface and an implementation
// of it, only its error values, the names starting Err; and identity's
// package, whose server and calls the node hands in behind interfaces, only
// the join credential the API hands identity, the keytab's names, the
// keytab's setting key, which the API seals the keytab under, the NetBIOS
// name's setting key, which the settings route refuses to change while joined,
// and NewHashes and the NTLM setting it takes, the pure derivation of a new
// password's hashes, which the password write seals.
var onlyNames = map[string]func(name string) bool{
	"pkg/metadata/kv": func(n string) bool { return strings.HasPrefix(n, "Err") },
	"pkg/identity": func(n string) bool {
		return n == "JoinCredential" || n == "KeytabNames" || n == "KeytabName" || n == "KeytabSetting" ||
			n == "NetBIOSNameSetting" || n == "NewHashes" || n == "NTLMSetting"
	},
}

// otherUses returns each name f uses from the package it imports as imp
// that admit does not admit: every name, under a dot import.
func otherUses(f *ast.File, imp *ast.ImportSpec, p string, admit func(string) bool) []string {
	name := path.Base(p)
	if imp.Name != nil {
		name = imp.Name.Name
	}
	if name == "." {
		return []string{p + ".*"}
	}
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == name && !admit(sel.Sel.Name) {
				bad = append(bad, p+"."+sel.Sel.Name)
			}
		}
		return true
	})
	return bad
}

// libraries are the third-party packages a package under internal/node may
// import: the metrics and tracing APIs and the JWT library the API signs its
// access tokens with, none of them a component. Only internal/node/api/auth
// is expected to import the JWT library.
var libraries = []string{
	"github.com/golang-jwt/jwt/v5",
	"github.com/prometheus/client_golang/prometheus",
	"go.opentelemetry.io/otel/attribute",
	"go.opentelemetry.io/otel/trace",
	"go.opentelemetry.io/otel/trace/noop",
}

// componentImports returns, for every package below root but root itself,
// nodetest and testdata, each import of its non-test files, whatever their
// build tags, that is neither the standard library nor on componentAllowed.
func componentImports(root string) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "nodetest" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Dir(path) == root || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(root, path)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel, inModule := strings.CutPrefix(p, module)
			std := !strings.Contains(strings.Split(p, "/")[0], ".")
			if admit, ok := onlyNames[rel]; inModule && ok {
				for _, u := range otherUses(f, imp, p, admit) {
					bad = append(bad, filepath.ToSlash(r)+" uses "+u)
				}
				continue
			}
			if std || inModule && componentAllowed(rel) || slices.Contains(libraries, p) {
				continue
			}
			r, _ := filepath.Rel(root, path)
			bad = append(bad, filepath.ToSlash(r)+" imports "+p)
		}
		return nil
	})
	return bad, err
}

// No package under internal/node but node and nodetest imports a component's
// implementation; a planted package importing the engine is caught.
func TestNoComponentImports(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		root string
		want []string
	}{
		{".", nil},
		{"testdata/arch/component-import", []string{
			"api/allowed.go imports github.com/cubbit/dittofs/internal/identity",
			"api/allowed.go imports github.com/dgraph-io/badger/v4",
			"api/identity.go uses github.com/cubbit/dittofs/pkg/identity.CheckKeytab",
			"api/kv.go uses github.com/cubbit/dittofs/pkg/metadata/kv.KV",
			"api/kv.go uses github.com/cubbit/dittofs/pkg/metadata/kv.Counting",
			"api/server.go imports github.com/cubbit/dittofs/pkg/engine",
			"nodetest-import/harness.go imports github.com/cubbit/dittofs/internal/node/nodetest",
		}},
	} {
		got, err := componentImports(tc.root)
		if err != nil {
			t.Fatalf("%s: %v", tc.root, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.root, got, tc.want)
		}
	}
}

// typeCheck type-checks the package whose non-test sources for this host's
// platform are in dir as import path path, with every import read from the
// export data exportImporter lists.
func typeCheck(dir, path string) (*types.Package, []*ast.File, *types.Info, error) {
	fset, imp := exportImporter()
	files, err := parsePackage(fset, dir)
	if err != nil {
		return nil, nil, nil, err
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		return nil, nil, nil, err
	}
	return pkg, files, info, nil
}

// parsePackage parses the non-test sources for this host's platform in dir.
func parsePackage(fset *token.FileSet, dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if ok, err := gobuild.Default.MatchFile(dir, e.Name()); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// exportImporter is one importer for every check, so a package two checks
// import is loaded once. It reads the export data the go command compiled for
// each package: type-checking the node's dependencies from source instead
// takes over a minute under the race detector.
var exportImporter = sync.OnceValues(func() (*token.FileSet, types.Importer) {
	fset := token.NewFileSet()
	exports := sync.OnceValues(exportFiles)
	return fset, &lockedImporter{imp: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		files, err := exports()
		if err != nil {
			return nil, err
		}
		if files[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(files[path])
	})}
})

// lockedImporter imports one package at a time: the gc importer fills a
// package map with no lock of its own, and the arch tests run in parallel.
type lockedImporter struct {
	mu  sync.Mutex
	imp types.Importer
}

func (l *lockedImporter) Import(path string) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.imp.Import(path)
}

// exportFiles lists the export data file of each package under
// internal/node, of the metadata store's, and of every package they import,
// by import path. The go command compiles what the build cache lacks, with
// the race detector when this binary has it, so the build of this binary
// already cached them.
func exportFiles() (map[string]string, error) {
	args := []string{"list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}"}
	if prof.RaceEnabled {
		args = append(args, "-race")
	}
	cmd := exec.Command("go", append(args, "./...", "../../pkg/metadata/kvstore")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export: %w: %s", err, stderr.String())
	}
	files := map[string]string{}
	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "=")
		files[path] = file
	}
	return files, nil
}

// kvUpdate is the KV's write transaction, through which every write of the
// store commits.
const kvUpdate = "(" + module + "pkg/metadata/kv.KV).Update"

// storeWriters returns every exported method of the metadata store's package
// that writes the store, by its full name, with its signature less the
// receiver: one whose body, or a function or method of the package it calls,
// opens the KV's write transaction. Write, which commits a change with its
// audit record, is the one write left out.
func storeWriters() (map[string]*types.Signature, error) {
	_, files, info, err := typeCheck("../../pkg/metadata/kvstore", module+"pkg/metadata/kvstore")
	if err != nil {
		return nil, err
	}
	calls := map[string][]string{} // a function's full name: the full names it calls
	var methods []*types.Func
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := info.Defs[fd.Name].(*types.Func)
			if fd.Recv != nil && fn.Exported() && fn.Name() != "Write" {
				methods = append(methods, fn)
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					var callee *ast.Ident
					switch f := c.Fun.(type) {
					case *ast.Ident:
						callee = f
					case *ast.SelectorExpr:
						callee = f.Sel
					}
					if g, ok := info.Uses[callee].(*types.Func); ok {
						calls[fn.FullName()] = append(calls[fn.FullName()], g.Origin().FullName())
					}
				}
				return true
			})
		}
	}
	writes := map[string]bool{kvUpdate: true}
	for grew := true; grew; {
		grew = false
		for fn, called := range calls {
			if !writes[fn] && slices.ContainsFunc(called, func(c string) bool { return writes[c] }) {
				writes[fn], grew = true, true
			}
		}
	}
	out := map[string]*types.Signature{}
	for _, m := range methods {
		if writes[m.FullName()] {
			s := m.Signature()
			out[m.FullName()] = types.NewSignatureType(nil, nil, nil, s.Params(), s.Results(), s.Variadic())
		}
	}
	return out, nil
}

// storeWrites type-checks the package in dir, as import path path, and
// returns each way it holds a store write: a method of a type it uses that is
// one of writers or the KV's write transaction, and an interface method, a
// func-typed variable, field or parameter whose signature is a writer's, which
// the node could fill with the store's method whatever its name.
func storeWrites(dir, path string, writers map[string]*types.Signature) ([]string, error) {
	_, _, info, err := typeCheck(dir, path)
	if err != nil {
		return nil, err
	}
	isWriter := func(t types.Type) bool {
		s, ok := t.Underlying().(*types.Signature)
		return ok && slices.ContainsFunc(slices.Collect(maps.Values(writers)), func(w *types.Signature) bool { return types.Identical(s, w) })
	}
	used := map[types.Type]bool{}
	for _, tv := range info.Types {
		used[tv.Type] = true
	}
	seen := map[string]bool{}
	for _, objs := range []map[*ast.Ident]types.Object{info.Defs, info.Uses} {
		for _, o := range objs {
			if o == nil {
				continue
			}
			used[o.Type()] = true
			if v, ok := o.(*types.Var); ok && isWriter(v.Type()) {
				seen["var "+v.Name()] = true
			}
		}
	}
	for t := range used {
		if st, ok := t.Underlying().(*types.Struct); ok {
			for f := range st.Fields() {
				if isWriter(f.Type()) {
					seen["var "+f.Name()] = true
				}
			}
		}
		for _, t := range []types.Type{t, types.NewPointer(t)} {
			ms := types.NewMethodSet(t)
			for i := range ms.Len() {
				m := ms.At(i).Obj().(*types.Func)
				_, iface := m.Signature().Recv().Type().Underlying().(*types.Interface)
				if iface && isWriter(m.Signature()) || writers[m.Origin().FullName()] != nil || m.FullName() == kvUpdate {
					seen[m.FullName()] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// methodValues type-checks the package in dir, as import path path, and
// returns each store writer it takes as a method value rather than calls,
// which the node could hand to the api as a function.
func methodValues(dir, path string, writers map[string]*types.Signature) ([]string, error) {
	_, files, info, err := typeCheck(dir, path)
	if err != nil {
		return nil, err
	}
	called := map[ast.Expr]bool{}
	var sels []*ast.SelectorExpr
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				called[n.Fun] = true
			case *ast.SelectorExpr:
				sels = append(sels, n)
			}
			return true
		})
	}
	var bad []string
	for _, s := range sels {
		if fn, ok := info.Uses[s.Sel].(*types.Func); ok && !called[s] && writers[fn.Origin().FullName()] != nil {
			bad = append(bad, fn.FullName())
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// No package under internal/node/api has a way to write the store but Write,
// and the node hands it no store write as a function value; planted packages
// holding a store method in an interface of their own, under its name or
// another, in a func field, the store's view itself, and a node taking a
// store method as a value are caught.
func TestAPIWritesOnlyThroughWrite(t *testing.T) {
	t.Parallel()
	writers, err := storeWriters()
	if err != nil {
		t.Fatal(err)
	}
	cp := "(" + module + "pkg/metadata/kvstore.ControlPlane)."
	if writers[cp+"PutShare"] == nil || writers[cp+"Shares"] != nil {
		t.Fatalf("store writers %v: want ControlPlane's PutShare in and Shares out", slices.Sorted(maps.Keys(writers)))
	}
	var dirs []string
	if err := filepath.WalkDir("api", func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		got, err := storeWrites(dir, module+"internal/node/"+filepath.ToSlash(dir), writers)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(got) > 0 {
			t.Errorf("%s: violations %q, want none", dir, got)
		}
	}
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"interface", []string{"(interface).PutShare"}},
		{"renamed", []string{"(interface).Forget"}},
		{"func-field", []string{"var Drop"}},
		{"assert", []string{"(interface).PutUser"}},
	} {
		got, err := storeWrites(filepath.Join("testdata/arch/store-write", tc.dir), module+"internal/node/api", writers)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.dir, got, tc.want)
		}
	}
	got, err := storeWrites("testdata/arch/store-write/view", module+"internal/node/api", writers)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, cp+"PutShare") || slices.Contains(got, cp+"Shares") {
		t.Errorf("the store's view: violations %q, want PutShare among them and Shares not", got)
	}
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{".", nil},
		{"testdata/arch/store-write/method-value", []string{cp + "DeleteNetgroup"}},
	} {
		got, err := methodValues(tc.dir, module+"internal/node", writers)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: store writers taken as values %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// writeAdapters are the functions of package api that may call the store's
// Write, each named by its receiver and name: the write adapter and the
// refusal's audit.
var writeAdapters = []string{"write", "(*Server).refuse"}

// signInCounts are auth's sign-in count and the takeback of an attempt the
// store could not check, the functions of auth's whose Write sets no audit
// record: the attempt is audited when it settles, in the settle's own Write,
// and an attempt that never settles, or whose password the store could not
// check, checked none, so it is counted and not recorded.
var signInCounts = []string{"reserve", "unreserve"}

// writeCalls type-checks the package in dir, as import path path, and
// returns each way it breaks the write rule: a use of the store's Write
// other than as the function of a call, a method value that could be handed
// anywhere; a call of it from a function writeAdapters does not name, unless
// the package is auth, whose every function may call it; and a call whose
// WriteMeta is not a literal setting Audit to &x or to a call's result, but
// auth's signInCounts. A
// Write is any method so named whose second parameter is entity.WriteMeta.
func writeCalls(dir, path string) ([]string, error) {
	_, files, info, err := typeCheck(dir, path)
	if err != nil {
		return nil, err
	}
	isAuth := under(path, module+"internal/node/api/auth")
	calls := map[*ast.Ident]*ast.CallExpr{}
	var funcs []*ast.FuncDecl
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					calls[sel.Sel] = n
				}
			case *ast.FuncDecl:
				funcs = append(funcs, n)
			}
			return true
		})
	}
	// in names the function declaring the identifier at pos, by its
	// receiver and name.
	in := func(pos token.Pos) string {
		for _, fd := range funcs {
			if fd.Pos() <= pos && pos < fd.End() {
				if fd.Recv == nil {
					return fd.Name.Name
				}
				return "(" + types.ExprString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
			}
		}
		return "package scope"
	}
	var bad []string
	for id, o := range info.Uses {
		fn, ok := o.(*types.Func)
		if !ok || fn.Name() != "Write" || fn.Signature().Params().Len() < 2 ||
			fn.Signature().Params().At(1).Type().String() != module+"pkg/metadata/entity.WriteMeta" {
			continue
		}
		name := in(id.Pos())
		c := calls[id]
		switch {
		case c == nil:
			bad = append(bad, name+" takes Write as a value")
			continue
		case !isAuth && !slices.Contains(writeAdapters, name):
			bad = append(bad, name+" calls Write outside the write adapter")
		}
		if exempt := isAuth && slices.Contains(signInCounts, name); !exempt && (len(c.Args) < 2 || !setsAudit(c.Args[1])) {
			bad = append(bad, name+" calls Write with no audit record")
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// signingKeyFiles are the files of the API, by their path under it, that may
// reach the signing key: the tokens, which seal and open it, and the
// rotation step, its one writer after the installation's first version.
var signingKeyFiles = []string{"auth/rotate.go", "auth/tokens.go"}

// credentialWrites are the API's functions outside signingKeyFiles that may
// write a secret version, each by its file, its receiver's type ("" for
// none) and its name: secretSetting.put, which writes a remote store's
// credential and the identity keytab, handOver, an offline join's machine
// password, WritePassword, a password's hash and NT hash, and createStore,
// which writes a remote store's first credential beside the settings rows
// it is judged with. Only a row with prunes set may also call PruneSecret,
// the versions below the ones it writes; WritePassword's is the one such
// row. The store refuses each under the signing key's kind or owner, and
// every secretSetting literal is checked to name a constant kind other
// than the signing key's.
//
// decision: the exemption names the functions rather than checking the
// written secret's kind and owner, which the type checker cannot see: the
// secret reaches PutSecret as a value, sealed in another function. So it is
// scoped to those declarations: a closure at package level, a method on
// another type, or a function of the same name in another file is checked.
// Overturned once a secret write takes its kind and owner as constants a
// check can read.
var credentialWrites = []struct {
	file, recv, name string
	prunes           bool
}{
	{"res_secrets.go", "secretSetting", "put", false},
	{"res_domain.go", "", "handOver", false},
	{"auth/password.go", "", "WritePassword", true},
	{"res_stores.go", "", "createStore", false},
}

// isCredentialWrite reports whether fd, declared in file, a path under the
// API, is one of credentialWrites, and whether it may also prune.
func isCredentialWrite(file string, fd *ast.FuncDecl) (write, prune bool) {
	recv := ""
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		t := fd.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if id, ok := t.(*ast.Ident); ok {
			recv = id.Name
		}
	}
	for _, w := range credentialWrites {
		if w.file == file && w.recv == recv && w.name == fd.Name.Name {
			return true, w.prunes
		}
	}
	return false, false
}

// signingKeyReaches type-checks the package in dir, as import path path, an
// API package, and returns each identifier outside signingKeyFiles that
// could reach the signing key: a use of the installation's SigningKey field,
// which names the key's reference; a use of entity.SecretKey, the key's
// kind; and a call of a PutSecret, PruneSecret or SetSigningKey method,
// which writes or drops a version of any secret or names another secret as
// the key, but a PutSecret in one of credentialWrites, and a PruneSecret in
// one of them that prunes. Every
// way to write the key needs one of these, so a route writing it is caught
// whatever it is named; one reaching the reference through another package's
// function, which none of the API's imports has, is not.
func signingKeyReaches(dir, path string) ([]string, error) {
	_, files, info, err := typeCheck(dir, path)
	if err != nil {
		return nil, err
	}
	fset, imp := exportImporter()
	ent, err := imp.Import(module + "pkg/metadata/entity")
	if err != nil {
		return nil, err
	}
	var field types.Object
	if o := ent.Scope().Lookup("Installation"); o != nil {
		if inst, ok := o.Type().Underlying().(*types.Struct); ok {
			for f := range inst.Fields() {
				if f.Name() == "SigningKey" {
					field = f
				}
			}
		}
	}
	kind := ent.Scope().Lookup("SecretKey")
	hashes := []types.Object{ent.Scope().Lookup("SecretPassword"), ent.Scope().Lookup("SecretNTHash")}
	if field == nil || kind == nil || hashes[0] == nil || hashes[1] == nil {
		return nil, errors.New("entity declares no Installation struct with a SigningKey field, or no SecretKey, SecretPassword or SecretNTHash")
	}
	refused := append([]types.Object{kind}, hashes...)
	sub := strings.TrimPrefix(strings.TrimPrefix(path, module+"internal/node/api"), "/")
	var bad []string
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		if sub != "" {
			file = sub + "/" + file
		}
		if slices.Contains(signingKeyFiles, file) {
			continue
		}
		for _, d := range f.Decls {
			var exempt, prunes bool
			if fd, ok := d.(*ast.FuncDecl); ok {
				exempt, prunes = isCredentialWrite(file, fd)
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok && sub == "" && !secretKindAllowed(info, lit, refused) {
					bad = append(bad, file+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+" builds a secretSetting whose kind is SecretKey, a password hash's or no constant")
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				at := file + ":" + strconv.Itoa(fset.Position(id.Pos()).Line)
				switch o := info.Uses[id]; {
				case o == nil:
				case o == field:
					bad = append(bad, at+" uses Installation.SigningKey")
				case o == kind:
					bad = append(bad, at+" uses entity.SecretKey")
				case (o.Name() == "PutSecret" || o.Name() == "PruneSecret" || o.Name() == "SetSigningKey") && isMethod(o):
					if o.Name() == "PutSecret" && exempt || o.Name() == "PruneSecret" && prunes {
						break
					}
					bad = append(bad, at+" calls "+o.Name())
				}
				return true
			})
		}
	}
	slices.Sort(bad)
	return bad, nil
}

// secretKindAllowed reports whether lit, when it builds a secretSetting,
// whose put credentialWrites exempts, sets its kind to a constant other than
// each of refused, the signing key's kind and a password's two hash kinds,
// which only the password write writes; a literal of any other type is
// allowed. A kind left unset is the zero kind, which is no secret's.
func secretKindAllowed(info *types.Info, lit *ast.CompositeLit, refused []types.Object) bool {
	named, ok := info.Types[lit].Type.(*types.Named)
	if !ok || named.Obj().Name() != "secretSetting" {
		return true
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if k, isID := kv.Key.(*ast.Ident); !ok || !isID || k.Name != "kind" {
			continue
		}
		v := info.Types[kv.Value].Value
		return v != nil && !slices.ContainsFunc(refused, func(k types.Object) bool {
			return constant.Compare(v, token.EQL, k.(*types.Const).Val())
		})
	}
	return true
}

// isMethod reports whether o is a method.
func isMethod(o types.Object) bool {
	fn, ok := o.(*types.Func)
	return ok && fn.Signature().Recv() != nil
}

// No package under api reaches the signing key outside the tokens and the
// rotation step, so no route writes it (RFC 23 §5.2); a planted route
// rewriting it through the installation's field, the key's kind and
// PutSecret, dropping it through PruneSecret and repointing it is caught at
// each, and so is a PutSecret outside the credential write's own
// declaration: in a function of its name in another file, in a method of
// its name, and in a closure at package level after it; so is a handOver
// method in the offline join's file, a secretSetting.put in another file, a
// PruneSecret in a credential write that may not prune, and a secretSetting
// literal whose kind is the signing key's, a password hash's or no
// constant.
func TestNoRouteWritesTheSigningKey(t *testing.T) {
	t.Parallel()
	var dirs []string
	if err := filepath.WalkDir("api", func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		got, err := signingKeyReaches(dir, module+"internal/node/"+filepath.ToSlash(dir))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(got) > 0 {
			t.Errorf("%s: reaches the signing key %q, want none", dir, got)
		}
	}
	got, err := signingKeyReaches(filepath.Join("testdata", "arch", "signing-key"), module+"internal/node/api")
	if err != nil {
		t.Fatalf("the planted route: %v", err)
	}
	want := []string{
		"res_domain.go:18 calls PutSecret",
		"res_domain.go:8 calls PruneSecret",
		"res_secrets.go:10 calls PruneSecret",
		"res_secrets.go:18 calls PutSecret",
		"res_secrets.go:25 calls PutSecret",
		"res_secrets.go:30 calls PutSecret",
		"res_secrets.go:37 builds a secretSetting whose kind is SecretKey, a password hash's or no constant",
		"res_secrets.go:37 uses entity.SecretKey",
		"res_secrets.go:38 builds a secretSetting whose kind is SecretKey, a password hash's or no constant",
		"res_secrets.go:40 builds a secretSetting whose kind is SecretKey, a password hash's or no constant",
		"res_secrets.go:41 builds a secretSetting whose kind is SecretKey, a password hash's or no constant",
		"routes.go:12 uses Installation.SigningKey",
		"routes.go:16 uses entity.SecretKey",
		"routes.go:18 calls PutSecret",
		"routes.go:23 calls PruneSecret",
		"routes.go:29 calls SetSigningKey",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the planted route: reaches %q, want %q", got, want)
	}
	got, err = signingKeyReaches(filepath.Join("testdata", "arch", "signing-key-elsewhere"), module+"internal/node/api")
	if err != nil {
		t.Fatalf("the credential write's name in another file: %v", err)
	}
	if want := []string{"res_stores.go:16 calls PutSecret", "res_stores.go:8 calls PutSecret"}; !slices.Equal(got, want) {
		t.Errorf("the credential write's name in another file: reaches %q, want %q", got, want)
	}
}

// setsAudit reports whether e is a composite literal whose Audit is set to
// &x or to a call's result, either of which names a record the call built;
// a variable, nil or not, does not show that one exists.
func setsAudit(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Audit" {
			switch v := kv.Value.(type) {
			case *ast.UnaryExpr:
				return v.Op == token.AND
			case *ast.CallExpr:
				return true
			}
			return false
		}
	}
	return false
}

// Every package under api calls the store's Write only from the write
// adapter and auth, each call with its audit record set, and takes it as no
// value; planted packages calling Write from a resource handler or from a
// method named like the adapter, taking it as a method value, and calling it
// from auth with no audit record, a nil one or a variable that may be nil
// are caught; auth's sign-in count, writing none, is not.
func TestWritesThroughTheAdapter(t *testing.T) {
	t.Parallel()
	var dirs []string
	if err := filepath.WalkDir("api", func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		got, err := writeCalls(dir, module+"internal/node/"+filepath.ToSlash(dir))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(got) > 0 {
			t.Errorf("%s: violations %q, want none", dir, got)
		}
	}
	for _, tc := range []struct {
		dir, path string
		want      []string
	}{
		{"testdata/arch/write-calls/handler", "internal/node/api", []string{
			"(shares).write calls Write outside the write adapter",
			"hand takes Write as a value",
			"putShare calls Write outside the write adapter",
		}},
		{"testdata/arch/write-calls/auth", "internal/node/api/auth", []string{
			"drop calls Write with no audit record",
			"forget calls Write with no audit record",
			"revoke calls Write with no audit record",
		}},
	} {
		got, err := writeCalls(tc.dir, module+tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// lifecycleCalls type-checks the package in dir, as import path path, and
// returns every reference to a start or a stop of open state or the engine,
// by its full name: enginewire's Start and Stop, and the table's and the
// engine's own, called or taken as a value.
func lifecycleCalls(dir, path string) ([]string, error) {
	_, _, info, err := typeCheck(dir, path)
	if err != nil {
		return nil, err
	}
	watched := map[string]bool{
		module + "pkg/engine/enginewire.Start":       true,
		module + "pkg/engine/enginewire.Stop":        true,
		"(*" + module + "pkg/openstate.Table).Start": true,
		"(*" + module + "pkg/openstate.Table).Stop":  true,
		"(*" + module + "pkg/engine.Engine).Start":   true,
		"(*" + module + "pkg/engine.Engine).Close":   true,
	}
	var got []string
	for _, o := range info.Uses {
		if fn, ok := o.(*types.Func); ok && watched[fn.FullName()] {
			got = append(got, fn.FullName())
		}
	}
	slices.Sort(got)
	return got, nil
}

// The node starts and stops open state and the engine only through
// enginewire's Start and Stop, once each, so the order between the two is
// the one enginewire ships; a node calling the table's or the engine's own
// Start, Stop or Close is caught.
func TestNodeStartsThroughEnginewire(t *testing.T) {
	t.Parallel()
	wire := module + "pkg/engine/enginewire."
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{".", []string{wire + "Start", wire + "Stop"}},
		{"testdata/arch/start-direct", []string{
			"(*" + module + "pkg/engine.Engine).Close",
			"(*" + module + "pkg/engine.Engine).Start",
			"(*" + module + "pkg/openstate.Table).Start",
			"(*" + module + "pkg/openstate.Table).Stop",
		}},
	} {
		got, err := lifecycleCalls(tc.dir, module+"internal/node")
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: starts and stops of open state and the engine %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// apiExports returns the api package's exported names, each exported type's
// with its exported methods, the pointer's included, as Type.Method.
func apiExports(dir, path string) ([]string, error) {
	pkg, _, _, err := typeCheck(dir, module+path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, name := range pkg.Scope().Names() {
		o := pkg.Scope().Lookup(name)
		if !o.Exported() {
			continue
		}
		seen[name] = true
		if _, ok := o.(*types.TypeName); !ok {
			continue
		}
		for _, t := range []types.Type{o.Type(), types.NewPointer(o.Type())} {
			ms := types.NewMethodSet(t)
			for i := range ms.Len() {
				if m := ms.At(i).Obj(); m.Exported() {
					seen[name+"."+m.Name()] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// The api package exports New, Deps and Server, Server's Start and Stop, and
// OpenAPI, the document cmd/genopenapi writes; and its auth package Caller,
// CallerFrom, the With its authentication step calls, the local socket's
// mark, WithLocal and Local, the role table's Right, its rights and Allowed,
// the ResetAccount the break-glass reset calls, the SealSigningKey the
// initialisation step seals the first signing key with, and the access
// tokens and the signing key's rotation the node builds: Tokens, NewTokens,
// Issue, Verify, Close, the SigningKey the API keys its idempotency digests
// with and their Claims, the sign-in service the node builds, Service,
// NewService and its Deps, SignIn, Refresh, Authenticate and Refused, the
// password writes, ChangePassword, SealPassword, SetPassword and the
// PasswordSet between the last two, the shared seal and write the
// break-glass reset also makes, SealPassword, WritePassword and its
// WriteOpts, and identity.ntlm's key, NTLMKey, its decoder, NTLM, and its
// read, ReadNTLM, the
// Audited they answer, the credential a caller presented and the Session it
// names, ErrThrottled, which the problem table answers,
// the KeyVersion the node opens the key's
// versions as, RotationStep and RotationProgress, and the interfaces their
// parameters take, Clock, KeyVersions, Sealer and Writer; and nothing else;
// a planted extra method is caught.
func TestAPIExports(t *testing.T) {
	t.Parallel()
	want := []string{"Deps", "New", "OpenAPI", "Server", "Server.Start", "Server.Stop"}
	for _, tc := range []struct {
		dir, path string
		want      []string
	}{
		{"api", "internal/node/api", want},
		{"api/auth", "internal/node/api/auth", []string{
			"Accounts", "Allowed", "Audit", "Audited", "AuditedApplied", "AuditedNothing", "AuditedRefused", "Caller", "Caller.Session",
			"CallerFrom", "Claims", "Clock", "Clock.Now", "CredentialAPIToken", "CredentialKind", "CredentialNone", "CredentialSession", "Deps", "Drain",
			"ErrThrottled", "Installation",
			"KeyVersion", "KeyVersion.Format", "KeyVersion.LogValue", "KeyVersion.MarshalJSON", "KeyVersion.MarshalYAML",
			"KeyVersion.String", "KeyVersions", "KeyVersions.Versions", "Local", "NTLM", "NTLMKey", "NewService", "NewTokens", "PasswordChange", "PasswordSet",
			"Principals", "Public",
			"QuotaEvents", "ReadNTLM", "ResetAccount", "Right", "RotationProgress", "RotationStep", "SealPassword", "SealSigningKey", "Sealer",
			"Sealer.Seal", "Self", "Service", "Service.Authenticate", "Service.ChangePassword", "Service.CreateAPIToken", "Service.Refresh", "Service.Refused",
			"Service.SealPassword", "Service.SetPassword", "Service.SignIn", "Shares", "Stores",
			"Tokens", "Tokens.Close", "Tokens.Format", "Tokens.Issue", "Tokens.LogValue", "Tokens.SigningKey",
			"Tokens.String", "Tokens.Verify", "View", "With", "WithLocal", "WriteOpts", "WritePassword", "Writer", "Writer.Write",
		}},
		{"testdata/arch/api-exports", "internal/node/api", []string{"Deps", "New", "Server", "Server.Handler", "Server.Start", "Server.Stop"}},
	} {
		got, err := apiExports(tc.dir, tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: exports %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// importName returns the name file f imports the package at import path p
// under, its last element unless renamed, or "" when f does not import it.
func importName(f *ast.File, p string) string {
	for _, imp := range f.Imports {
		if ip, _ := strconv.Unquote(imp.Path.Value); ip == p {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return path.Base(p)
		}
	}
	return ""
}

// composeCalls counts, in the non-test files in dir, the references to
// enginewire's Compose and the engine.Deps composite literals, each package
// under whatever name each file imports it.
func composeCalls(dir string) (compose, deps int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return 0, 0, err
		}
		wire := importName(f, module+"pkg/engine/enginewire")
		eng := importName(f, module+"pkg/engine")
		named := func(x ast.Expr, pkg, sel string) bool {
			s, ok := x.(*ast.SelectorExpr)
			if !ok || s.Sel.Name != sel {
				return false
			}
			id, ok := s.X.(*ast.Ident)
			return ok && pkg != "" && id.Name == pkg
		}
		ast.Inspect(f, func(x ast.Node) bool {
			switch x := x.(type) {
			case *ast.SelectorExpr:
				if named(x, wire, "Compose") {
					compose++
				}
			case *ast.CompositeLit:
				if named(x.Type, eng, "Deps") {
					deps++
				}
			}
			return true
		})
	}
	return compose, deps, nil
}

// The node references enginewire's Compose exactly once and writes no
// engine.Deps literal: a node building Deps itself instead of composing, one
// composing twice, and one composing once but handing engine.New Deps it built
// itself are caught. A Deps the node builds without a literal, from a
// declared variable or a constructor of its own, is not.
func TestNodeComposesOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dir           string
		compose, deps int
	}{
		{".", 1, 0},
		{"testdata/arch/compose-inline", 0, 1},
		{"testdata/arch/compose-twice", 2, 0},
		{"testdata/arch/compose-once-inline-deps", 1, 1},
	} {
		compose, deps, err := composeCalls(tc.dir)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if compose != tc.compose {
			t.Errorf("%s: enginewire.Compose referenced %d times, want %d", tc.dir, compose, tc.compose)
		}
		if deps != tc.deps {
			t.Errorf("%s: engine.Deps literals %d, want %d", tc.dir, deps, tc.deps)
		}
	}
}

// apiClosureBanned are the module's packages the management API's import
// closure must not reach: the metadata store's implementation, which it
// reaches only through the interfaces it declares, and the key providers,
// which only the node builds and hands to the secret provider.
var apiClosureBanned = []string{"pkg/metadata/kvstore", "pkg/keyprovider"}

// The management API's packages, followed through every import of the
// module's own, its non-test files', reach no package apiClosureBanned
// names: a direct import the import rule refuses, and this catches one an
// allowed package brings in, as the secret provider would.
func TestAPIClosure(t *testing.T) {
	t.Parallel()
	seen := map[string]string{} // each package reached, and the one that imported it
	var walk func(imp, from string)
	walk = func(imp, from string) {
		if _, ok := seen[imp]; ok || !strings.HasPrefix(imp, module) {
			return
		}
		seen[imp] = from
		p, err := gobuild.Import(imp, ".", 0)
		if err != nil {
			t.Fatalf("import %s: %v", imp, err)
		}
		for _, i := range p.Imports {
			walk(i, imp)
		}
	}
	walk(module+"internal/node/api", "")
	walk(module+"internal/node/api/auth", "")
	for _, b := range apiClosureBanned {
		if from, ok := seen[module+b]; ok {
			t.Errorf("the API's closure reaches %s, imported by %s", b, from)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("walked %d packages, want the API's whole closure", len(seen))
	}
}
