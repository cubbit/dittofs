package node

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// configFields are the fields of Config: what the host supplies and the node
// cannot build.
var configFields = []string{"Bootstrap", "KV", "Clock", "Metrics", "Logger"}

// nodeExports returns the violations of the node's surface in the package
// whose sources are in dir: an exported package-level declaration other than
// New, Node and Config; an exported method of Node other than Start and Stop,
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
// build a node.
func nodeImporters(root string) ([]string, error) {
	var bad []string
	err := walkImports(root, true, func(file string, imports []string) {
		// decision: the harness under internal/node/nodetest is the one
		// importer besides dfs start, because it drives a whole node in tests
		// as dfs start does in production; no other exemption is named.
		if file == "cmd/dfs/commands/start.go" || under(file, "internal/node/nodetest") {
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

// componentAllowed reports whether a package under internal/node but node
// and nodetest may import the module's package rel: none it allows is a
// component's implementation. Everything else it reaches through an interface
// it declares or a value the node hands it.
func componentAllowed(rel string) bool {
	return strings.HasPrefix(rel, "internal/node/") || rel == "pkg/health" || rel == "pkg/metrics"
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
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			rel, inModule := strings.CutPrefix(p, module)
			std := !strings.Contains(strings.Split(p, "/")[0], ".")
			if std || inModule && componentAllowed(rel) {
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
	for _, tc := range []struct {
		root string
		want []string
	}{
		{".", nil},
		{"testdata/arch/component-import", []string{"api/server.go imports github.com/cubbit/dittofs/pkg/block/engine"}},
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

// typeCheck type-checks the package whose non-test sources are in dir as
// import path path, with every import read from source.
func typeCheck(dir, path string) ([]*ast.File, *types.Info, error) {
	fset, imp := sourceImporter()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp}
	if _, err := conf.Check(path, fset, files, info); err != nil {
		return nil, nil, err
	}
	return files, info, nil
}

// sourceImporter is one importer from source for every check, so a package
// two checks import is type-checked once.
var sourceImporter = sync.OnceValues(func() (*token.FileSet, types.Importer) {
	fset := token.NewFileSet()
	return fset, importer.ForCompiler(fset, "source", nil)
})

// kvUpdate is the KV's write transaction, through which every write of the
// store commits.
const kvUpdate = "(" + module + "pkg/metadata/kv.KV).Update"

// storeWriters returns every exported method of the metadata store's package
// that writes the store, by its full name: one whose body, or a
// function or method of the package it calls, opens the KV's write
// transaction. Write, which commits a change with its audit record, is the
// one write left out.
func storeWriters() (map[string]bool, error) {
	files, info, err := typeCheck("../../pkg/metadata/kvstore", module+"pkg/metadata/kvstore")
	if err != nil {
		return nil, err
	}
	calls := map[string][]string{} // a function's full name: the full names it calls
	var methods []string
	for fd := range funcDecls(files) {
		fn := info.Defs[fd.Name].(*types.Func)
		if fd.Recv != nil && fn.Exported() && fn.Name() != "Write" {
			methods = append(methods, fn.FullName())
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
	writes := map[string]bool{kvUpdate: true}
	for grew := true; grew; {
		grew = false
		for fn, called := range calls {
			if !writes[fn] && slices.ContainsFunc(called, func(c string) bool { return writes[c] }) {
				writes[fn], grew = true, true
			}
		}
	}
	out := map[string]bool{}
	for _, m := range methods {
		if writes[m] {
			out[m] = true
		}
	}
	return out, nil
}

// funcDecls yields every function and method with a body in files.
func funcDecls(files []*ast.File) iter.Seq[*ast.FuncDecl] {
	return func(yield func(*ast.FuncDecl) bool) {
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && !yield(fd) {
					return
				}
			}
		}
	}
}

// storeWrites type-checks the package in dir and returns each method that
// writes the store on a type the package uses: one of writers, the KV's write
// transaction, or a method of an interface named as one of writers, which
// the node could fill with the store's.
func storeWrites(dir string, writers map[string]bool) ([]string, error) {
	_, info, err := typeCheck(dir, module+"internal/node/api")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{"Update": true}
	for w := range writers {
		names[w[strings.LastIndex(w, ".")+1:]] = true
	}
	used := map[types.Type]bool{}
	for _, tv := range info.Types {
		used[tv.Type] = true
	}
	for _, objs := range []map[*ast.Ident]types.Object{info.Defs, info.Uses} {
		for _, o := range objs {
			if o != nil {
				used[o.Type()] = true
			}
		}
	}
	seen := map[string]bool{}
	for t := range used {
		for _, t := range []types.Type{t, types.NewPointer(t)} {
			ms := types.NewMethodSet(t)
			for i := range ms.Len() {
				m := ms.At(i).Obj().(*types.Func)
				_, iface := m.Signature().Recv().Type().Underlying().(*types.Interface)
				if iface && names[m.Name()] || writers[m.Origin().FullName()] || m.FullName() == kvUpdate {
					seen[m.FullName()] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// No type internal/node/api uses has a method that writes the store but
// Write; planted packages holding a store method in an interface of their
// own, and the store's view itself, are caught.
func TestAPIWritesOnlyThroughWrite(t *testing.T) {
	writers, err := storeWriters()
	if err != nil {
		t.Fatal(err)
	}
	cp := "(" + module + "pkg/metadata/kvstore.ControlPlane)."
	if !writers[cp+"PutShare"] || writers[cp+"Shares"] {
		t.Fatalf("store writers %v: want ControlPlane's PutShare in and Shares out", slices.Sorted(maps.Keys(writers)))
	}
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"api", nil},
		{"testdata/arch/store-write/interface", []string{"(interface).PutShare"}},
	} {
		got, err := storeWrites(tc.dir, writers)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.dir, got, tc.want)
		}
	}
	got, err := storeWrites("testdata/arch/store-write/view", writers)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, cp+"PutShare") || slices.Contains(got, cp+"Shares") {
		t.Errorf("the store's view: violations %q, want PutShare among them and Shares not", got)
	}
}
