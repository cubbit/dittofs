package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The row's codecs, written by hand as the generated codec's stand-in (the
// generator writes one for every tagged struct): the arguments are a role
// byte, the payload's length and the payload, with trailing bytes a body may
// carry; the reply is the payload. A length past the body's end is refused
// before anything allocates.

var (
	errRowArgs     = errors.New("row: short arguments")
	errRowRefused  = errors.New("row: the payload names none")
	errReadArgs    = errors.New("read row: short arguments")
	errReadRefused = errors.New("read row: its arguments name none")
)

type rowArgs struct {
	role    core.Role
	payload []byte
}

func (a *rowArgs) DecodeWire(b []byte, _ core.Version) (int, error) {
	if len(b) < 5 {
		return 0, errRowArgs
	}
	n := int(binary.BigEndian.Uint32(b[1:]))
	if 5+n > len(b) {
		return 0, errRowArgs
	}
	a.role, a.payload = core.Role(b[0]), b[5:5+n]
	return 5 + n, nil
}

type rowReply struct{ payload []byte }

// EncodeWire writes the payload; a nil receiver, the answer of a request
// refused before its call, writes nothing beside what the header codec wrote.
func (r *rowReply) EncodeWire(_ core.Version, out *resources.Buf) error {
	if r == nil {
		return nil
	}
	out.B = append(out.B, r.payload...)
	return nil
}

// MaxWire is the reply's largest encoding at v under its tags' maxima: the
// payload limit of the version, 64 at version 1 and 128 at version 2.
func (r *rowReply) MaxWire(v core.Version) int {
	if v >= 2 {
		return 128
	}
	return 64
}

func (r *rowReply) IsZero() bool { return r.payload == nil }

// rowHandler is the row's handler in the ops file's form, its request and
// reply by value: it answers the payload.
func rowHandler(_ context.Context, _ *core.Request, req rowArgs) (rowReply, error) {
	return rowReply{payload: req.payload}, nil
}

// rowRefuse is the row's Resolve: it refuses the payload that names none.
func rowRefuse(_ *core.Request, req *rowArgs) error {
	if len(req.payload) > 0 && req.payload[0] == '!' {
		return errRowRefused
	}
	return nil
}

// rowWire is the row's arguments as a served record carries them: the role
// byte, then rowBody's length, payload and trailing bytes. The row's messages
// carry no role of their own, so it is always zero.
func rowWire(data, trailing []byte) []byte {
	return append([]byte{0}, rowBody(data, trailing)...)
}

// rowBody is the row's arguments after the role byte: the payload's length,
// the payload and the trailing bytes a long body carries, an Echo message's
// payload on the wire.
func rowBody(data, trailing []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, data...)
	return append(b, trailing...)
}

// rowCouple is the same row built by hand over Bind, the plain functions of
// what the codecs' methods do, the room-blind bound a nil Bound means and the
// handler's lifted form. The row tests compare the row Row builds against it.
func rowCouple(hooks core.Hooks[rowArgs]) *core.Op {
	bound := func(r *core.Request, _ *rowArgs, _ int) (int, bool) {
		return (&rowReply{}).MaxWire(r.Version), true
	}
	h := func(ctx context.Context, r *core.Request, req *rowArgs) (*rowReply, error) {
		rep, err := rowHandler(ctx, r, *req)
		return &rep, err
	}
	enc := func(rep *rowReply, _ core.Version, out *resources.Buf) error {
		if rep == nil {
			return nil
		}
		out.B = append(out.B, rep.payload...)
		return nil
	}
	return &core.Op{
		Code:  testproto.OpEcho,
		Name:  "Row",
		Needs: core.NeedsNothing,
		Decode: core.Bind(
			func(b []byte, v core.Version, a *rowArgs) (int, error) { return a.DecodeWire(b, v) },
			bound,
			hooks.Resolve,
			hooks.Role,
			hooks.Handle,
			h,
			enc,
		),
	}
}

// Row returns the Op with Code, Name and Needs set from its arguments and
// Decode set over its codecs, and every column it does not take at zero, to
// be set on the Op where a row needs other than their zero value.
func TestRowSetsItsColumns(t *testing.T) {
	op := core.Row[readArgs, readRep, *readArgs, *readRep](42, "Read", core.NeedsSession|core.NeedsFH, readHandler)
	if op.Code != 42 || op.Name != "Read" || op.Needs != core.NeedsSession|core.NeedsFH {
		t.Fatalf("the row's columns: code %d, name %q, needs %v; want 42, Read, session and filehandle", op.Code, op.Name, op.Needs)
	}
	if op.Decode == nil {
		t.Fatal("Decode nil, want the row's")
	}
	if op.Versions != (core.Range{}) || op.Cacheable || op.Role != core.RoleNone || op.Expired {
		t.Errorf("the columns Row does not take changed: %+v", *op)
	}
	if op.Members != nil || op.MaxMembers != 0 {
		t.Errorf("the row is a compound's: members %v, cap %d", op.Members, op.MaxMembers)
	}
}

// Row's Decode is Bind's over the wire codecs: the same bytes consumed, the
// same refusal and no Invocation for a body its DecodeWire refuses (catches a
// Row that drops the bytes DecodeWire consumed).
func TestRowDecodesAsBind(t *testing.T) {
	row := core.Row[rowArgs, rowReply, *rowArgs, *rowReply](testproto.OpEcho, "Row", core.NeedsNothing, rowHandler)
	couple := rowCouple(core.Hooks[rowArgs]{})
	for _, c := range []struct {
		name string
		body []byte
		n    int // the bytes the codecs consume; 0 where they refuse
		err  error
	}{
		{name: "full", body: rowWire([]byte("hi"), []byte("XY")), n: 7},
		{name: "exact", body: rowWire([]byte("payload"), nil), n: 12},
		{name: "short", body: nil, err: errRowArgs},
		{name: "length past the body", body: append([]byte{0}, binary.BigEndian.AppendUint32(nil, 0xffffff01)...), err: errRowArgs},
	} {
		rInv, rN, rErr := row.Decode(c.body, 1)
		bInv, bN, bErr := couple.Decode(c.body, 1)
		if rN != c.n || bN != c.n {
			t.Errorf("%s: Row consumed %d, Bind's %d, want %d", c.name, rN, bN, c.n)
		}
		if (rErr == nil) != (c.err == nil) || (rErr != nil && !errors.Is(rErr, c.err)) {
			t.Errorf("%s: Row refused with %v, want %v", c.name, rErr, c.err)
		}
		if !errors.Is(bErr, rErr) {
			t.Errorf("%s: Bind's refusal %v differs from Row's %v", c.name, bErr, rErr)
		}
		if c.err != nil {
			if rInv.Call != nil || rInv.Resolve != nil || rInv.Encode != nil || rInv.Bound != nil ||
				bInv.Call != nil || bInv.Resolve != nil || bInv.Encode != nil || bInv.Bound != nil {
				t.Errorf("%s: a refused body built an Invocation", c.name)
			}
		}
	}
}

// A nil Bound means the reply's MaxWire at the request's version: the room
// ignored, ok always true, at every room and version.
func TestRowNilBoundIsTheReplyMaxWire(t *testing.T) {
	row := core.Row[rowArgs, rowReply, *rowArgs, *rowReply](testproto.OpEcho, "Row", core.NeedsNothing, rowHandler)
	for _, c := range []struct {
		v    core.Version
		want int // the reply's MaxWire at v, from rowReply's tags
	}{
		{1, 64},
		{2, 128},
	} {
		inv, _, err := row.Decode(rowWire([]byte("hi"), nil), c.v)
		if err != nil {
			t.Fatalf("version %d: decode: %v", c.v, err)
		}
		for _, room := range []int{1 << 20, 130, 64, 13, 4, 1, 0} {
			if n, ok := inv.Bound(&core.Request{Version: c.v}, room); n != c.want || !ok {
				t.Errorf("version %d, room %d: Bound %d, %v; want the reply's %d, true", c.v, room, n, ok, c.want)
			}
		}
	}
}

// The read row is a reply sized by its request: a count clamped to the room
// in its Bound hook, never below one byte, ok false where the room cannot
// hold one (a READ's row, RFC 20 §4.3). Its other hooks are a refused
// resolve, a per-request role and a filehandle.
type readArgs struct {
	refuse bool
	count  int
}

func (a *readArgs) DecodeWire(b []byte, _ core.Version) (int, error) {
	if len(b) != 5 {
		return 0, errReadArgs
	}
	a.refuse, a.count = b[0] != 0, int(binary.BigEndian.Uint32(b[1:]))
	return 5, nil
}

type readRep struct {
	eof  bool
	data []byte
}

func (r *readRep) EncodeWire(_ core.Version, out *resources.Buf) error {
	if r == nil {
		return nil
	}
	var eof byte
	if r.eof {
		eof = 1
	}
	out.B = append(out.B, eof)
	out.B = append(out.B, r.data...)
	return nil
}

// MaxWire is the eof byte and the data limit of the version, as rowReply's.
func (r *readRep) MaxWire(v core.Version) int {
	return (&rowReply{}).MaxWire(v) + 1
}

func (r *readRep) IsZero() bool { return r == nil || len(r.data) == 0 }

func readHandler(_ context.Context, _ *core.Request, req readArgs) (readRep, error) {
	data := make([]byte, req.count)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	return readRep{data: data}, nil
}

var readHooks = core.Hooks[readArgs]{
	Bound: func(_ *core.Request, req *readArgs, room int) (int, bool) {
		if 1+req.count > room {
			if room < 2 {
				return 1 + req.count, false
			}
			req.count = room - 1
		}
		return 1 + req.count, true
	},
	Resolve: func(_ *core.Request, req *readArgs) error {
		if req.refuse {
			return errReadRefused
		}
		return nil
	},
	Role: func(_ *core.Request, req *readArgs) core.Role {
		if req.refuse {
			return core.RoleRelease
		}
		return core.RoleContext
	},
	Handle: func(req *readArgs) (vfs.Handle, bool) { return vfs.Handle{byte(req.count)}, true },
}

// readBytes is the read row's arguments: the refuse byte, then the count.
func readBytes(refuse bool, count int) []byte {
	b := make([]byte, 5)
	if refuse {
		b[0] = 1
	}
	binary.BigEndian.PutUint32(b[1:], uint32(count))
	return b
}

// readCouple is the read row built by hand over Bind with the same hooks.
func readCouple(hooks core.Hooks[readArgs]) *core.Op {
	h := func(ctx context.Context, r *core.Request, req *readArgs) (*readRep, error) {
		rep, err := readHandler(ctx, r, *req)
		return &rep, err
	}
	return &core.Op{
		Code:  42,
		Name:  "Read",
		Needs: core.NeedsNothing,
		Decode: core.Bind(
			func(b []byte, v core.Version, a *readArgs) (int, error) { return a.DecodeWire(b, v) },
			hooks.Bound,
			hooks.Resolve,
			hooks.Role,
			hooks.Handle,
			h,
			func(rep *readRep, v core.Version, out *resources.Buf) error {
				return rep.EncodeWire(v, out)
			},
		),
	}
}

// The hooks are Bind's arguments: a row built with Hooks behaves as the row
// Bind builds with the same functions - a Bound that measures the room and
// writes its clamp into the request, a Resolve that refuses, a Role, a Handle
// (catches a Row that drops or miswires a hook).
func TestRowHooksAreBinds(t *testing.T) {
	row := core.Row[readArgs, readRep, *readArgs, *readRep](42, "Read", core.NeedsNothing, readHandler, readHooks)
	couple := readCouple(readHooks)
	rInv, _, err := row.Decode(readBytes(false, 9), 1)
	if err != nil {
		t.Fatal(err)
	}
	bInv, _, err := couple.Decode(readBytes(false, 9), 1)
	if err != nil {
		t.Fatal(err)
	}
	rB, bB := &core.Request{}, &core.Request{}
	if rInv.Handle == nil || bInv.Handle == nil {
		t.Fatal("Handle dropped, want the row's handle hook on both rows")
	}
	rh, rok := rInv.Handle()
	bh, bok := bInv.Handle()
	if rok != bok || !bytes.Equal(rh, bh) || rh[0] != 9 {
		t.Errorf("Handle: Row's %v, %v, Bind's %v, %v; want the decoded 9", rh, rok, bh, bok)
	}
	if (rInv.Role == nil) != (bInv.Role == nil) {
		t.Fatalf("Role: the hook reached one row and not the other (Row nil %v, Bind nil %v)", rInv.Role == nil, bInv.Role == nil)
	}
	if rInv.Role == nil || rInv.Role(rB) != bInv.Role(bB) {
		t.Errorf("Role: Row's %v, Bind's %v", rInv.Role(rB), bInv.Role(bB))
	}
	for _, room := range []int{1 << 20, 155, 5, 3, 1} {
		rn, rok := rInv.Bound(rB, room)
		bn, bok := bInv.Bound(bB, room)
		if rn != bn || rok != bok {
			t.Errorf("room %d: Bound over Row %d, %v, over Bind %d, %v", room, rn, rok, bn, bok)
		}
	}
	// The sweep's smallest rooms wrote their clamp into the decoded count, so
	// a Bound past them reads the count the room left.
	if rn, rok := rInv.Bound(rB, 1<<20); rn != 3 || !rok {
		t.Errorf("after the sweep Bound %d, %v, want the clamped 3, true", rn, rok)
	}
	rBad, _, err := row.Decode(readBytes(true, 4), 1)
	if err != nil {
		t.Fatal(err)
	}
	bBad, _, err := couple.Decode(readBytes(true, 4), 1)
	if err != nil {
		t.Fatal(err)
	}
	if rErr, bErr := rBad.Resolve(rB), bBad.Resolve(bB); !errors.Is(rErr, errReadRefused) || rErr != bErr {
		t.Errorf("Resolve: Row's %v, Bind's %v, want %v", rErr, bErr, errReadRefused)
	}
	if rBad.Role(rB) != bBad.Role(bB) || rBad.Role(rB) != core.RoleRelease {
		t.Errorf("Role: Row's %v, Bind's %v, want the release", rBad.Role(rB), bBad.Role(bB))
	}
}

// swappedProto is testproto's stages with one row served in a code's place:
// the routed table's row for op's code gives way to op, so the row a test
// builds is served on testproto's own tables, error table and stages.
type swappedProto struct {
	inner core.Protocol
	op    *core.Op
}

func (p swappedProto) Framer() core.Framer { return p.inner.Framer() }

func (p swappedProto) NewConn(c *core.Conn) core.Stages {
	st := p.inner.NewConn(c)
	st.Router = swappedRouter{inner: st.Router, op: p.op}
	return st
}

// swappedRouter serves the inner router's table with the swapped row in its
// code's place. The table is copied shallowly and its map cloned, so
// testproto's own tables and rows are never touched.
type swappedRouter struct {
	inner core.Router
	op    *core.Op
}

func (rt swappedRouter) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	tb, r, err := rt.inner.Route(h, prev)
	if err != nil {
		return tb, r, err
	}
	st := *tb
	st.Ops = maps.Clone(tb.Ops)
	st.Ops[rt.op.Code] = rt.op
	return &st, r, nil
}

// sameReply says whether two answers are the same reply, every field of it.
func sameReply(a, b testproto.Reply) bool {
	return a.ID == b.ID && a.Status == b.Status && a.Interim == b.Interim &&
		a.Credits == b.Credits && a.Session == b.Session && a.Signed == b.Signed &&
		a.Checksum == b.Checksum && bytes.Equal(a.Body, b.Body)
}

// The row Row builds over codecs written by hand answers as the row Bind
// builds over the same functions, served on testproto's stages: the same
// reply bytes, and its statuses - the decode status for a body its DecodeWire
// refuses, the fault for one its Resolve refuses (catches a Row that encodes
// through a path of its own, lifts the handler badly, or loses a request the
// codecs refused).
func TestRowAnswersAsBind(t *testing.T) {
	inner := testproto.New(adapter.Deps{}).Bare()
	hooks := core.Hooks[rowArgs]{Resolve: rowRefuse}
	row := core.Row[rowArgs, rowReply, *rowArgs, *rowReply](
		testproto.OpEcho, "Row", core.NeedsNothing, rowHandler, hooks)
	row.Versions = core.Range{First: 1, Last: 2}
	couple := rowCouple(hooks)
	couple.Versions = core.Range{First: 1, Last: 2}
	rows, hand := newSeam(t, swappedProto{inner, row}, 1<<20), newSeam(t, swappedProto{inner, couple}, 1<<20)
	for _, c := range []struct {
		name   string
		msg    testproto.Msg
		status core.Status
		body   []byte
	}{
		{name: "answered its payload", msg: testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: rowBody([]byte("hi"), []byte("XY"))}, status: testproto.StatusOK, body: []byte("hi")},
		{name: "no arguments", msg: testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 8}, status: testproto.StatusDecode},
		{name: "length past the body", msg: testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 9, Payload: binary.BigEndian.AppendUint32(nil, 0xffffff01)}, status: testproto.StatusDecode},
		{name: "resolved to none", msg: testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 10, Payload: rowBody([]byte("!hi"), nil)}, status: testproto.StatusFault},
	} {
		rows.serve(c.msg)
		hand.serve(c.msg)
		r, b := rows.read(), hand.read()
		if !sameReply(r, b) {
			t.Errorf("%s: Row answered %+v, the row Bind builds %+v", c.name, r, b)
		}
		if r.ID != c.msg.ID {
			t.Errorf("%s: answered message %d, want its own %d", c.name, r.ID, c.msg.ID)
		}
		if r.Status != c.status {
			t.Errorf("%s: answered status %d, want %d", c.name, r.Status, c.status)
		}
		if !bytes.Equal(r.Body, c.body) {
			t.Errorf("%s: answered body %q, want %q", c.name, r.Body, c.body)
		}
	}
}
