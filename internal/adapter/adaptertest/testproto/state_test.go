package testproto

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// A session two connections carry outlives the first's close and ends at the
// second's; each connection's keep is released when it leaves.
func TestASessionLivesWhileAConnectionCarriesIt(t *testing.T) {
	g := newRegistry()
	a, b := &connState{}, &connState{}
	ends := time.Unix(100, 0)
	var released []string
	g.join(5, a, ends, func() { released = append(released, "a") })
	g.join(5, b, ends, nil)
	g.addTree(5, 9)

	if carried, expires, tree := g.look(5, a, 9); !carried || !expires.Equal(ends) || !tree {
		t.Errorf("the first connection's view: carried %v, expires %v, tree %v", carried, expires, tree)
	}
	if carried, _, tree := g.look(5, b, 8); !carried || tree {
		t.Errorf("the second connection, a tree never connected: carried %v, tree %v", carried, tree)
	}
	if carried, _, _ := g.look(5, &connState{}, 9); carried {
		t.Error("a connection that never set the session up carries it")
	}

	// The connection joining again replaces its keep, releasing the first once.
	g.join(5, a, ends, func() { released = append(released, "a again") })
	if len(released) != 1 || released[0] != "a" {
		t.Fatalf("keeps released when the connection joined again: %v, want the first one", released)
	}
	released = nil

	g.leave(a)
	if len(released) != 1 || released[0] != "a again" {
		t.Errorf("keeps released at the first connection's close: %v, want its own", released)
	}
	if carried, _, _ := g.look(5, a, 9); carried {
		t.Error("the closed connection still carries the session")
	}
	if carried, _, tree := g.look(5, b, 9); !carried || !tree {
		t.Error("the first connection's close ended a session the second still carries")
	}
	g.leave(b)
	if len(g.m) != 0 {
		t.Error("the session outlived its last connection")
	}
}

// A Logoff ends the session on every connection and releases each keep.
func TestEndReleasesEveryKeep(t *testing.T) {
	g := newRegistry()
	a, b := &connState{}, &connState{}
	n := 0
	g.join(5, a, time.Time{}, func() { n++ })
	g.join(5, b, time.Time{}, func() { n++ })
	g.join(6, a, time.Time{}, func() { n += 100 })
	g.end(5)
	if n != 2 {
		t.Errorf("ending session 5 released %d, want its two keeps and not session 6's", n)
	}
	if carried, _, _ := g.look(5, b, 0); carried {
		t.Error("the session is still carried after it ended")
	}
	if carried, _, _ := g.look(6, a, 0); !carried {
		t.Error("ending one session ended another")
	}
}

// The Request's level is what the connection negotiated and what the header's
// session and tree reach, and its credential ends when the session's does.
func TestTheRequestsLevelFollowsTheSessionAndTree(t *testing.T) {
	p := New(adapter.Deps{})
	st := p.NewConn(nil)
	rt := st.Router.(*router)
	ends := time.Unix(100, 0)
	level := func(m Msg) (*core.Request, core.Needs) {
		t.Helper()
		m.Version = 2
		r, err := rt.request(headerOf(t, m), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r, r.Level
	}

	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9}); got != core.NeedsNothing {
		t.Errorf("before a Negotiate the level is %d, want nothing", got)
	}
	rt.cs.version.Store(2)
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9}); got != core.NeedsNegotiated {
		t.Errorf("negotiated, with no session set up, the level is %d", got)
	}
	p.reg.join(5, rt.cs, ends, nil)
	r, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9})
	if got != core.NeedsSession || !r.Call.Identity.Evidence.Expires.Equal(ends) {
		t.Errorf("under its session the level is %d and the credential ends %v", got, r.Call.Identity.Evidence.Expires)
	}
	p.reg.addTree(5, 9)
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9}); got != core.NeedsTree {
		t.Errorf("under its session and tree the level is %d", got)
	}
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 8}); got != core.NeedsSession {
		t.Errorf("under a tree never connected the level is %d", got)
	}
	if _, got := level(Msg{Op: OpEcho, Session: 6, Tree: 9}); got != core.NeedsNegotiated {
		t.Errorf("under a session this connection never set up the level is %d", got)
	}

	// A related member shares the state of the one before it, so the
	// filehandle a PutFH set is its current one.
	first, _ := level(Msg{Op: OpPutFH, Session: 5, Tree: 9})
	first.State.(*state).fh = vfs.Handle("fh")
	related, err := rt.request(headerOf(t, Msg{Op: OpCreate, Version: 2, Session: 5, Tree: 9, Related: true}), first)
	if err != nil {
		t.Fatal(err)
	}
	if related.Level != core.NeedsTree|core.NeedsFH || related.State != first.State {
		t.Errorf("a related member after a PutFH has level %#x and its own state: %v", related.Level, related.State != first.State)
	}
	if alone, got := level(Msg{Op: OpCreate, Session: 5, Tree: 9}); got != core.NeedsTree || alone.State == first.State {
		t.Errorf("a member that is not related has level %#x", got)
	}

	// Logoff and Tree, through their rows, act on the session and tree the
	// header names.
	call := func(code uint32) core.Invocation {
		t.Helper()
		for _, op := range p.ops() {
			if op.Code == code {
				inv, _, err := op.Decode(nil, 2)
				if err != nil {
					t.Fatal(err)
				}
				return inv
			}
		}
		t.Fatalf("no row for operation %d", code)
		return core.Invocation{}
	}
	treeInv := call(OpTree)
	tr, _ := level(Msg{Op: OpTree, Session: 5, Tree: 12})
	if err := treeInv.Call(t.Context(), tr); err != nil {
		t.Fatal(err)
	}
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 12}); got != core.NeedsTree {
		t.Errorf("after a Tree of tree 12 the level under it is %d", got)
	}
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 13}); got != core.NeedsSession {
		t.Errorf("a Tree of tree 12 connected tree 13: level %d", got)
	}

	// Bare carries no session, whatever the registry holds.
	bare := p.Bare().NewConn(nil).Router.(*router)
	bare.cs.version.Store(1)
	p.reg.join(5, bare.cs, ends, nil)
	br, err := bare.request(headerOf(t, Msg{Op: OpEcho, Version: 1, Session: 5, Tree: 9}), nil)
	if err != nil || br.Level != core.NeedsNegotiated {
		t.Errorf("on Bare the level under a session is %d, %v; want negotiated", br.Level, err)
	}
	p.reg.leave(bare.cs)

	logoffInv := call(OpLogoff)
	lr, _ := level(Msg{Op: OpLogoff, Session: 5, Tree: 9})
	if err := logoffInv.Call(t.Context(), lr); err != nil {
		t.Fatal(err)
	}
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9}); got != core.NeedsNegotiated {
		t.Errorf("after a Logoff the level under the old session is %d", got)
	}
	p.reg.join(5, rt.cs, ends, nil)

	// The connection's close drops its sessions.
	st.OnClose(t.Context())
	if _, got := level(Msg{Op: OpEcho, Session: 5, Tree: 9}); got != core.NeedsNegotiated {
		t.Errorf("after OnClose the level under the old session is %d", got)
	}
}

// NewConn sets every stage on the full protocol, Hold included, and only the
// Header and Router on Bare.
func TestNewConnsStages(t *testing.T) {
	p := New(adapter.Deps{})
	full := p.NewConn(nil)
	if full.Header == nil || full.Router == nil || full.Security == nil || full.Flow == nil || full.Replay == nil || full.Hold == nil || full.OnClose == nil || full.MaxPending != 8 {
		t.Errorf("the full protocol's stages: %+v", full)
	}
	bare := p.Bare().NewConn(nil)
	if bare.Header == nil || bare.Router == nil {
		t.Error("Bare left Header or Router unset")
	}
	if bare.Security != nil || bare.Flow != nil || bare.Replay != nil || bare.Hold != nil || bare.OnClose != nil || bare.MaxPending != 0 {
		t.Errorf("Bare sets a stage core has a default for: %+v", bare)
	}
}

func TestSessionDecodesItsKeepAndExpiry(t *testing.T) {
	ends := time.Unix(1234, 5678)
	args := Append(nil, Msg{Op: OpSession, Version: 2, Keep: true, Expires: ends})[lengthLen+headerLen:]
	var req sessionReq
	if n, err := decodeSession(args, 2, &req); err != nil || n != 9 || !req.keep || !req.expires.Equal(ends) {
		t.Errorf("decoded %d bytes, %v: keep %v, expires %v", n, err, req.keep, req.expires)
	}
	args = Append(nil, Msg{Op: OpSession, Version: 2})[lengthLen+headerLen:]
	req = sessionReq{}
	if _, err := decodeSession(args, 2, &req); err != nil || req.keep || !req.expires.IsZero() {
		t.Errorf("a Session with no keep and no expiry: %v, keep %v, expires %v", err, req.keep, req.expires)
	}
	if _, err := decodeSession(args[:8], 2, &req); err == nil {
		t.Error("a Session with 8 bytes of arguments was decoded")
	}

	var target targetReq
	notify := Append(nil, Msg{Op: OpNotify, Version: 2, Target: 0x0102030405060708})[lengthLen+headerLen:]
	if n, err := decodeTarget(notify, 2, &target); err != nil || n != 8 || target.target != 0x0102030405060708 {
		t.Errorf("a Notify's target decodes to %#x in %d bytes, %v", target.target, n, err)
	}
	if _, err := decodeTarget(notify[:7], 2, &target); err == nil {
		t.Error("a Notify with 7 bytes of arguments was decoded")
	}
}

// A park core refuses makes no service call: the handler answers the refusal,
// and with no service given a call would panic.
func TestARefusedParkMakesNoCall(t *testing.T) {
	op, inv := row(t, 2, OpPark, "name")
	if op.Versions != (core.Range{First: 2, Last: 2}) {
		t.Fatalf("Park is served in %v", op.Versions)
	}
	r, _ := request(2)
	err := inv.Call(t.Context(), r)
	if err == nil || errors.Is(err, core.ErrParked) {
		t.Errorf("Park on a request core refuses to park = %v, want that refusal", err)
	}
}

// The compound row decodes its count and takes those bytes alone; ReadOp and
// the members' rows then walk each member, and refuse one past the record.
func TestTheCompoundRowAndItsMembers(t *testing.T) {
	p := New(adapter.Deps{})
	args := Append(nil, Msg{Op: OpCompound, Version: 1, Members: []Msg{
		{Op: OpPutFH, Handle: vfs.Handle("fh")},
		{Op: OpEcho, Role: core.RoleFlush, Payload: []byte("pay")},
		{Op: OpPanic},
	}})[lengthLen+headerLen:]

	c := compound(nil)
	if c.Versions != (core.Range{First: 1, Last: 1}) {
		t.Errorf("the compound row is served in %v, want version 1 alone", c.Versions)
	}
	inv, n, err := c.Decode(args, 1)
	if err != nil || n != 4 || inv.Count != 3 {
		t.Fatalf("the compound's Decode took %d bytes with a count of %d, %v; want 4 and 3", n, inv.Count, err)
	}
	if inv.Bound == nil || inv.Resolve == nil || inv.Call == nil || inv.Encode == nil {
		t.Fatal("the compound's invocation has a nil step")
	}

	members := map[uint32]*core.Op{}
	for _, m := range memberRows(p.ops()) {
		members[m.Code] = m
	}
	rest := args[n:]
	var codes []uint32
	for range inv.Count {
		code, took, err := readOp(rest)
		if err != nil || took != 4 {
			t.Fatalf("ReadOp = %d, %d, %v", code, took, err)
		}
		m, used, err := members[code].Decode(rest[took:], 1)
		if err != nil {
			t.Fatalf("member %d: %v", code, err)
		}
		switch code {
		case OpPutFH:
			if h, ok := m.Handle(); !ok || string(h) != "fh" {
				t.Errorf("the PutFH member's handle is %q", h)
			}
		case OpEcho:
			if m.Role(nil) != core.RoleFlush || m.Bound() != 3 {
				t.Errorf("the Echo member's role is %d and its bound %d", m.Role(nil), m.Bound())
			}
		}
		codes = append(codes, code)
		rest = rest[took+used:]
	}
	if len(codes) != 3 || codes[0] != OpPutFH || codes[1] != OpEcho || codes[2] != OpPanic || len(rest) != 0 {
		t.Errorf("walked members %v with %d bytes left; want PutFH, Echo, Panic and none", codes, len(rest))
	}

	if _, _, err := c.Decode(args[:3], 1); !errors.Is(err, errMember) {
		t.Errorf("a compound with 3 bytes of arguments: %v", err)
	}
	if _, _, err := c.Decode([]byte{0x00, 0x10, 0x00, 0x01}, 1); !errors.Is(err, errMember) {
		t.Errorf("a compound counting more members than a record has bytes: %v", err)
	}
	if _, _, err := members[OpPutFH].Decode([]byte{0xff, 0xff, 0xff, 0xff, 'x'}, 1); !errors.Is(err, errMember) {
		t.Errorf("a member whose length is past every record: %v", err)
	}
	if _, _, err := readOp(args[4:7]); !errors.Is(err, errMember) {
		t.Errorf("ReadOp of a code that runs past the record: %v", err)
	}
	short := args[4+4 : 4+4+5] // the PutFH member's length and one byte of its two
	if _, _, err := members[OpPutFH].Decode(short, 1); !errors.Is(err, errMember) {
		t.Errorf("a member whose arguments run past the record: %v", err)
	}
}

// exempt is the exported names of core that testproto, a protocol, neither
// implements nor calls, each with why.
var exempt = map[string]string{
	"NewServer":  "builds the adapter: a test's, and New's in a real adapter",
	"Server":     "the adapter NewServer returns",
	"Listener":   "handed to NewServer by whoever builds the adapter",
	"Errors":     "read by a real protocol's totality test",
	"ErrorTable": "built with NewErrorTable; the type is named by core alone",
	"Call":       "a middleware's type; the pipeline builds the chain",
	"Middleware": "the pipeline's",
	"Handler":    "named through Bind's type parameters, never written out",
	"Needs":      "its values are used; the type itself is never written out",
	"RoleNone":   "the zero role: a row that sets none has it",
	"RoleFlush":  "a role Echo carries from its arguments, never names",
}

// Every exported name of core, and every method of Conn and Request, is
// referenced from testproto's own code, so core has no symbol a protocol uses
// that no test protocol exercises; the names a protocol has no use for are
// listed with why.
//
// ponytail: the check matches names, so core.X counts wherever it is written
// and a called method of another type with the same name counts as Conn's or
// Request's. Resolve the uses with go/types if a gap is ever found hidden
// that way.
func TestTestprotoReferencesEveryCoreSymbol(t *testing.T) {
	fset := token.NewFileSet()
	parse := func(dir string) []*ast.File {
		t.Helper()
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(names) == 0 {
			t.Fatalf("no Go file in %s: %v", dir, err)
		}
		var files []*ast.File
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		return files
	}

	want := map[string]bool{} // "Name" for a package-level name, ".Name" for a method of Conn or Request
	for _, f := range parse(filepath.Join("..", "..", "core")) {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					want[d.Name.Name] = true
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok && (id.Name == "Conn" || id.Name == "Request") {
					want["."+d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							want[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								want[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	if len(want) < 40 {
		t.Fatalf("found only %d exported names in core: the walk is reading the wrong directory", len(want))
	}

	used := map[string]bool{}
	for _, f := range parse(".") {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "core" {
					used[n.Sel.Name] = true
				}
			case *ast.CallExpr: // a method counts only where it is called, not where a field of its name is read
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					used["."+sel.Sel.Name] = true
				}
			}
			return true
		})
	}

	var missing, stale []string
	for name := range want {
		_, exempted := exempt[strings.TrimPrefix(name, ".")]
		switch {
		case used[name] && exempted:
			stale = append(stale, name)
		case !used[name] && !exempted:
			missing = append(missing, name)
		}
	}
	for name := range exempt {
		if !want[name] && !want["."+name] {
			stale = append(stale, name+" (not in core)")
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("core exports these and testproto's code references none of them: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("exempted, but referenced or gone from core: %v", stale)
	}
}
