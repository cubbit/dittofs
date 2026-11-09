package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// surface is core's exported surface as RFC 20 §4.7 states it, with §4.3's
// Handler: types, functions, variables and constants, and Type.Name for each
// exported method, struct field and interface method. A protocol reaches
// nothing else in core; an export missing here is added to §4.7 first.
var surface = []string{
	"Bind", "Call", "Conn", "Conn.Close", "Conn.Established", "Conn.Keep", "Conn.Send", "Conn.Settings",
	"Dispatcher", "Dispatcher.Dispatch", "Dispatcher.Drain",
	"ErrDraining", "ErrExpired", "ErrMoved", "ErrNoFH", "ErrNoSession", "ErrNoTree", "ErrNotLast",
	"ErrNotNegotiated", "ErrNotSupported", "ErrParked", "ErrTooLong", "ErrTooManyPending", "ErrVersion",
	"ErrorRow", "ErrorRow.Err", "ErrorRow.NoReply", "ErrorRow.Ops", "ErrorRow.Status", "ErrorRow.Versions",
	"ErrorTable", "ErrorTable.Map", "Errors",
	"FlowControl", "FlowControl.Admit", "Framer", "Framer.BodyLen", "Framer.HeaderLen",
	"Handler", "Header", "Header.Next", "Header.Op", "Header.Related", "Header.Version",
	"HeaderCodec", "HeaderCodec.Chain", "HeaderCodec.Decode", "HeaderCodec.Encode",
	"HoldCodec", "HoldCodec.Cancels", "HoldCodec.Final", "HoldCodec.Interim", "HoldCodec.Key", "HoldCodec.Pends",
	"Invocation", "Invocation.Bound", "Invocation.Call", "Invocation.Count", "Invocation.Encode",
	"Invocation.Handle", "Invocation.Resolve", "Invocation.Role",
	"Listener", "Listener.Network", "Listener.Port", "Listener.Protocol",
	"Middleware", "Needs", "NeedsFH", "NeedsNegotiated", "NeedsNothing", "NeedsSession", "NeedsTree",
	"NewErrorTable", "NewOpTable", "NewServer",
	"Op", "Op.Cacheable", "Op.Code", "Op.Decode", "Op.Expired", "Op.Members", "Op.Name", "Op.Needs",
	"Op.Role", "Op.Versions",
	"OpTable", "OpTable.Errors", "OpTable.Holds", "OpTable.Ops", "OpTable.ReadOp",
	"Protocol", "Protocol.Framer", "Protocol.NewConn", "Range", "Range.First", "Range.Last",
	"ReplayCache", "ReplayCache.Lookup", "ReplayCache.Store",
	"Request", "Request.Call", "Request.Conn", "Request.Level", "Request.Park", "Request.State", "Request.Version",
	"Role", "RoleContext", "RoleFlush", "RoleNone", "RoleRelease",
	"Router", "Router.Route", "Security", "Security.Unwrap", "Security.Wrap",
	"Server", "Server.Connections", "Server.Health", "Server.Reconfigure", "Server.Start", "Server.Stop",
	"Stages", "Stages.Flow", "Stages.Header", "Stages.Hold", "Stages.MaxPending", "Stages.OnClose",
	"Stages.Replay", "Stages.Router", "Stages.Security",
	"Status", "Version",
}

// exports lists the exported identifiers of core's non-test files.
func exports(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	add := func(owner string, names []*ast.Ident) {
		for _, n := range names {
			if n.IsExported() {
				got = append(got, owner+n.Name)
			}
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				owner := ""
				if d.Recv != nil {
					typ := d.Recv.List[0].Type
					if star, ok := typ.(*ast.StarExpr); ok {
						typ = star.X
					}
					owner = typ.(*ast.Ident).Name + "."
				}
				add(owner, []*ast.Ident{d.Name})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						add("", s.Names)
					case *ast.TypeSpec:
						add("", []*ast.Ident{s.Name})
						if !s.Name.IsExported() {
							continue
						}
						var fields *ast.FieldList
						switch tt := s.Type.(type) {
						case *ast.StructType:
							fields = tt.Fields
						case *ast.InterfaceType:
							fields = tt.Methods
						}
						if fields == nil {
							continue
						}
						for _, fl := range fields.List {
							add(s.Name.Name+".", fl.Names)
						}
					}
				}
			}
		}
	}
	slices.Sort(got)
	return got
}

// core exports exactly what RFC 20 §4.7 names: an export it does not name
// fails here, as does one it names that core lost.
func TestExportsAreTheRFCSurface(t *testing.T) {
	got, want := exports(t), slices.Sorted(slices.Values(surface))
	for _, name := range got {
		if _, found := slices.BinarySearch(want, name); !found {
			t.Errorf("core exports %s, which RFC 20 §4.7 does not name: unexport it, or add it to §4.7 first", name)
		}
	}
	for _, name := range want {
		if _, found := slices.BinarySearch(got, name); !found {
			t.Errorf("core does not export %s, which RFC 20 §4.7 names", name)
		}
	}
}
