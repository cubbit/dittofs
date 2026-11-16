package node

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
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

// componentAllowed reports whether a package under internal/node but node
// and nodetest may import the module's package rel: none it allows is a
// component's implementation, the entity and wire packages holding data
// types only, and nodetest, which imports Badger, is not allowed. Everything
// else it reaches through an interface it declares or a value the node hands
// it.
func componentAllowed(rel string) bool {
	switch rel {
	case "pkg/health", "pkg/metrics", "pkg/metadata/entity", "pkg/node/apiv1":
		return true
	}
	return strings.HasPrefix(rel, "internal/node/") && !strings.HasPrefix(rel, "internal/node/nodetest")
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
		{"testdata/arch/component-import", []string{
			"api/server.go imports github.com/cubbit/dittofs/pkg/block/engine",
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

// typeCheck type-checks the package whose non-test sources are in dir as
// import path path, with every import read from source.
func typeCheck(dir, path string) (*types.Package, []*ast.File, *types.Info, error) {
	fset, imp := sourceImporter()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, nil, nil, err
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(path, fset, files, info)
	if err != nil {
		return nil, nil, nil, err
	}
	return pkg, files, info, nil
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

// apiExports returns the api package's exported names, each exported type's
// with its exported methods, the pointer's included, as Type.Method.
func apiExports(dir string) ([]string, error) {
	pkg, _, _, err := typeCheck(dir, module+"internal/node/api")
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

// The api package exports New, Deps and Server, and Server's Start and Stop,
// and nothing else; a planted extra method is caught.
func TestAPIExports(t *testing.T) {
	want := []string{"Deps", "New", "Server", "Server.Start", "Server.Stop"}
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"api", want},
		{"testdata/arch/api-exports", []string{"Deps", "New", "Server", "Server.Handler", "Server.Start", "Server.Stop"}},
	} {
		got, err := apiExports(tc.dir)
		if err != nil {
			t.Fatalf("%s: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: exports %q, want %q", tc.dir, got, tc.want)
		}
	}
}
