package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// configFields are the fields of Config: what the host supplies and the node
// cannot build.
var configFields = []string{"Bootstrap", "KV", "Clock", "Metrics", "Logger"}

// nodeExports returns the violations of the node's surface in the package
// whose sources are in dir: an exported package-level declaration other than
// New, Node and Config; an exported method of Node other than Start and Stop;
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
	want := map[string]bool{"New": false, "Node": false, "Config": false, "Node.Start": false, "Node.Stop": false}
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
					note(id.Name + "." + d.Name.Name)
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

// The node exports New, Node and Config, and Node's Start and Stop, and
// nothing else; each planted tree, holding one violation, is caught.
func TestNodeExports(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{".", nil},
		{"extra-func", []string{"extra export: Extra"}},
		{"extra-method", []string{"extra export: Node.Pause"}},
		{"embedded", []string{"embedded field in Node"}},
		{"exported-field", []string{"exported field: Node.State"}},
		{"config-field", []string{"Config fields Bootstrap,KV,Clock,Metrics,Logger,Fault"}},
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

// walkImports calls fn with each .go file under root, as a slash path relative
// to root, and the paths it imports; test files are read only when tests is
// set. It leaves out testdata, hidden directories and nested modules.
//
// decision: every file is parsed with go/parser, ignoring build constraints
// and GOOS or GOARCH file-name suffixes, rather than listed through go/build,
// which reads only the files of one build; so no file behind a tag or for
// another platform can hide an import from a rule. Its ceiling is the source
// on disk: an import that only a build-time generator writes is not seen.
func walkImports(root string, tests bool, fn func(file string, imports []string)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		if !strings.HasSuffix(p, ".go") || (!tests && strings.HasSuffix(p, "_test.go")) {
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
		fn(filepath.ToSlash(rel), imports)
		return nil
	})
}

// v1Paths are v1's control plane, which never returns at these paths.
// pkg/identity and pkg/apiclient come back in v2 at the same paths, so the
// compiler, not this rule, guards their deletion.
var v1Paths = []string{module + "pkg/controlplane", module + "internal/controlplane", module + "pkg/snapshot"}

// v1Imports returns each import of a v1 path by a file under root, tests
// included.
func v1Imports(root string) ([]string, error) {
	var bad []string
	err := walkImports(root, true, func(file string, imports []string) {
		for _, imp := range imports {
			if slices.ContainsFunc(v1Paths, func(v1 string) bool { return under(imp, v1) }) {
				bad = append(bad, file+" imports "+imp)
			}
		}
	})
	slices.Sort(bad)
	return bad, err
}

// nodeImporters returns each file under root, tests included, that imports
// the node but may not: only the server's start command and the test harness
// build a node, and the node's own external tests drive it.
func nodeImporters(root string) ([]string, error) {
	var bad []string
	err := walkImports(root, true, func(file string, imports []string) {
		// decision: the harness under internal/node/nodetest is the one
		// importer besides dfs start, because it drives a whole node in tests
		// as dfs start does in production; the node's own _test.go files are
		// its tests, in package node_test only because they use the harness,
		// which imports the node. No other exemption is named.
		ownTest := path.Dir(file) == "internal/node" && strings.HasSuffix(file, "_test.go")
		if file == "cmd/dfs/commands/start.go" || under(file, "internal/node/nodetest") || ownTest {
			return
		}
		if slices.Contains(imports, module+"internal/node") {
			bad = append(bad, file+" imports the node")
		}
	})
	slices.Sort(bad)
	return bad, err
}

// cliRoots are where the CLI's dependency closure starts, and cliAllowed what
// of this module it may reach: neither the metadata or block stores, nor the
// control-plane server, nor anything that reads the host's bootstrap.
var (
	cliRoots   = []string{"cmd/dfsctl", "pkg/node/apiv1", "pkg/apiclient"}
	cliAllowed = []string{"cmd/dfsctl", "pkg/apiclient", "pkg/node/apiv1", "internal/cli"}
)

// cliClosure returns each package of this module the CLI's binary reaches
// outside cliAllowed, and each import of a package under pkg/node/apiv1
// outside the standard library. It follows non-test files only, as the
// binary is built from them.
func cliClosure(root string) ([]string, error) {
	pkgs := map[string][]string{}
	err := walkImports(root, false, func(file string, imports []string) {
		dir := path.Dir(file)
		pkgs[dir] = append(pkgs[dir], imports...)
	})
	if err != nil {
		return nil, err
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
			if under(dir, "pkg/node/apiv1") && !standard(imp) {
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

// Each import rule reports nothing over the module and exactly the violation
// planted in its testdata tree, beside the allowed look-alikes planted with it.
func TestImportRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule func(root string) ([]string, error)
		tree string
		want []string
	}{
		{"no v1 import", v1Imports, "no-v1",
			[]string{
				"pkg/store/store.go imports " + module + "pkg/controlplane/runtime",
				"pkg/store/store_test.go imports " + module + "pkg/controlplane/models",
			}},
		{"who imports the node", nodeImporters, "node-importers",
			[]string{
				"cmd/dfs/commands/stop.go imports the node",
				"internal/cli/cli_test.go imports the node",
				"internal/node/nodetest2/harness.go imports the node",
			}},
		{"the CLI's closure", cliClosure, "cli-closure",
			[]string{"internal/cli/output reaches pkg/metadata"}},
		{"the wire types' imports", cliClosure, "apiv1-stdlib",
			[]string{
				"pkg/node/apiv1 imports C outside the standard library",
				"pkg/node/apiv1 imports github.com/spf13/cobra outside the standard library",
			}},
	} {
		got, err := tc.rule(filepath.Join("..", ".."))
		if err != nil {
			t.Fatalf("%s over the module: %v", tc.name, err)
		}
		if len(got) > 0 {
			t.Errorf("%s over the module: violations %q, want none", tc.name, got)
		}
		got, err = tc.rule(filepath.Join("testdata", "arch", tc.tree))
		if err != nil {
			t.Fatalf("%s over %s: %v", tc.name, tc.tree, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s over %s: violations %q, want %q", tc.name, tc.tree, got, tc.want)
		}
	}
}
