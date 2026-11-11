package testproto

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var _ settings.Settings = Settings{}

// row is version v's row for code, decoded from args as the pipeline would.
func row(t *testing.T, v core.Version, code uint32, args string) (*core.Op, core.Invocation) {
	t.Helper()
	for _, op := range New(adapter.Deps{}).ops() {
		if op.Code != code {
			continue
		}
		inv, n, err := op.Decode([]byte(args), v)
		if err != nil {
			t.Fatalf("%s: Decode: %v", op.Name, err)
		}
		if n != len(args) {
			t.Fatalf("%s: Decode took %d of %d bytes", op.Name, n, len(args))
		}
		return op, inv
	}
	t.Fatalf("version %d has no row for operation %d", v, code)
	return nil, core.Invocation{}
}

// request is a request as the Router builds one on a connection that
// negotiated nothing yet.
func request(v core.Version) (*core.Request, *state) {
	s := &state{conn: &connState{}}
	return &core.Request{Version: v, State: s}, s
}

func TestEchoAnswersItsPayloadUnderItsOwnRole(t *testing.T) {
	_, inv := row(t, 1, OpEcho, "\x03hello")
	if inv.Role == nil {
		t.Fatal("Echo's invocation has no per-request role")
	}
	if got := inv.Role(nil); got != core.RoleRelease {
		t.Errorf("Role = %d, want the role its arguments carry, %d", got, core.RoleRelease)
	}
	if got := inv.Bound(); got != 5 {
		t.Errorf("Bound = %d, want the payload's 5 bytes", got)
	}
	r, _ := request(1)
	if err := inv.Call(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	out := &resources.Buf{B: []byte("head")}
	if err := inv.Encode(out, 1); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.B, []byte("headhello")) {
		t.Errorf("the reply is %q, want the payload appended after what was there", out.B)
	}
	op, _ := row(t, 1, OpEcho, "\x00")
	if _, _, err := op.Decode(nil, 1); err == nil {
		t.Error("an Echo with no role byte was decoded")
	}
}

// A request refused before its handler ran still encodes: every row's encoder
// takes a nil reply.
func TestEveryRowEncodesWithNoReply(t *testing.T) {
	for _, op := range New(adapter.Deps{}).ops() {
		inv, _, err := op.Decode(make([]byte, 9), 2)
		if err != nil {
			t.Fatalf("%s: %v", op.Name, err)
		}
		out := &resources.Buf{}
		if err := inv.Encode(out, 2); err != nil || len(out.B) != 0 {
			t.Errorf("%s: Encode with no reply = %v, %d bytes; want nothing", op.Name, err, len(out.B))
		}
	}
}

func TestPutFHNamesItsHandleAndSetsTheCurrentOne(t *testing.T) {
	op, inv := row(t, 2, OpPutFH, "handle-bytes")
	if op.Role != core.RoleContext {
		t.Errorf("PutFH's row has role %d, want context", op.Role)
	}
	if inv.Handle == nil {
		t.Fatal("PutFH's invocation names no handle")
	}
	if h, ok := inv.Handle(); !ok || !bytes.Equal(h, vfs.Handle("handle-bytes")) {
		t.Errorf("Handle = %q, %v; want the arguments' bytes", h, ok)
	}
	r, s := request(2)
	if err := inv.Call(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.fh, vfs.Handle("handle-bytes")) {
		t.Errorf("the current filehandle is %q after PutFH", s.fh)
	}
	if r.Level&core.NeedsFH == 0 {
		t.Errorf("the request's level is %#x after PutFH: the members after it in a compound have no current filehandle", r.Level)
	}
}

func TestNegotiatePicksTheLowerOfTheOfferAndTheCap(t *testing.T) {
	for _, c := range []struct {
		offer, max, want core.Version
	}{{2, 2, 2}, {2, 1, 1}, {1, 2, 1}} {
		_, inv := row(t, c.offer, OpNegotiate, "")
		r, s := request(c.offer)
		s.max = c.max
		if err := inv.Call(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if got := core.Version(s.conn.version.Load()); got != c.want {
			t.Errorf("an offer of %d under a cap of %d negotiated %d, want %d", c.offer, c.max, got, c.want)
		}
		out := &resources.Buf{}
		if err := inv.Encode(out, c.offer); err != nil || !bytes.Equal(out.B, []byte{uint8(c.want)}) {
			t.Errorf("the reply is % x, %v; want the version picked, %d", out.B, err, c.want)
		}
	}
}

func TestPanicPanics(t *testing.T) {
	_, inv := row(t, 1, OpPanic, "")
	r, _ := request(1)
	defer func() {
		if recover() == nil {
			t.Error("Panic's handler returned")
		}
	}()
	_ = inv.Call(t.Context(), r)
}

// The rows: each code once, served in both versions, with the preconditions
// the pipeline checks for it.
func TestRows(t *testing.T) {
	both, second := core.Range{First: 1, Last: 2}, core.Range{First: 2, Last: 2}
	want := map[uint32]struct {
		versions  core.Range
		needs     core.Needs
		role      core.Role
		expired   bool
		cacheable bool
	}{
		OpNegotiate: {both, core.NeedsNothing, core.RoleNone, false, false},
		OpSession:   {both, core.NeedsNegotiated, core.RoleNone, true, false},
		OpLogoff:    {both, core.NeedsSession, core.RoleRelease, true, false},
		OpTree:      {both, core.NeedsSession, core.RoleNone, false, false},
		OpPutFH:     {both, core.NeedsTree, core.RoleContext, false, false},
		OpEcho:      {both, core.NeedsNothing, core.RoleNone, false, false},
		OpCreate:    {both, core.NeedsTree | core.NeedsFH, core.RoleNone, false, true},
		OpPanic:     {both, core.NeedsNothing, core.RoleNone, false, false},
		OpPark:      {second, core.NeedsTree | core.NeedsFH, core.RoleNone, false, false},
		OpNotify:    {both, core.NeedsNothing, core.RoleNone, false, false},
		OpViolate:   {both, core.NeedsNothing, core.RoleNone, false, false},
	}
	seen := map[uint32]bool{}
	for _, op := range New(adapter.Deps{}).ops() {
		w, ok := want[op.Code]
		switch {
		case !ok:
			t.Errorf("a row for operation %d, %s, which the table of rows does not list", op.Code, op.Name)
		case seen[op.Code]:
			t.Errorf("operation %d has two rows", op.Code)
		case op.Versions != w.versions || op.Needs != w.needs || op.Role != w.role || op.Expired != w.expired || op.Cacheable != w.cacheable:
			t.Errorf("%s: versions %v needs %#x role %d expired %v cacheable %v; want %+v", op.Name, op.Versions, op.Needs, op.Role, op.Expired, op.Cacheable, w)
		}
		seen[op.Code] = true
	}
	if len(seen) != len(want) {
		t.Errorf("%d of the %d rows", len(seen), len(want))
	}
	if seen[OpCancel] || seen[OpCompound] {
		t.Error("Cancel is never routed, and the compound row is the first version's alone, over its members")
	}

	// A row served in the second version alone sits in both tables, so the
	// first refuses it by its Versions column; the compound row has none of
	// the preconditions, and no member negotiates.
	c := compound(nil)
	if c.Versions != (core.Range{First: 1, Last: 1}) || c.Needs != core.NeedsNothing || c.Role != core.RoleNone || c.Expired || c.Cacheable {
		t.Errorf("the compound row: %+v", c)
	}
	members := memberRows(New(adapter.Deps{}).ops())
	if len(members) != len(want)-1 {
		t.Errorf("%d member rows, want every row but Negotiate's", len(members))
	}
	for _, m := range members {
		if m.Code == OpNegotiate {
			t.Error("a compound's members can negotiate")
		}
	}
}

// Every refusal of core's has a row, the two drain errors answer nothing, and
// only the table that holds holds, for HoldFor from the hold's start.
func TestErrorRows(t *testing.T) {
	for _, holds := range []bool{false, true} {
		rows := errorRows(holds)
		find := func(err error) *core.ErrorRow {
			var found *core.ErrorRow
			for i := range rows {
				if errors.Is(err, rows[i].Err) {
					if found != nil {
						t.Errorf("two rows match %v", err)
					}
					found = &rows[i]
				}
			}
			return found
		}
		statuses := map[core.Status]error{}
		for _, err := range append(core.Errors(), vfs.ErrDelay, context.Canceled) {
			r := find(err)
			if r == nil {
				t.Errorf("holds %v: no row maps %v", holds, err)
				continue
			}
			noReply := errors.Is(err, core.ErrDraining) || errors.Is(err, core.ErrMoved)
			if r.NoReply != noReply {
				t.Errorf("holds %v: %v has NoReply %v, want %v", holds, err, r.NoReply, noReply)
			}
			if noReply {
				continue
			}
			if r.Status == StatusOK || r.Status == StatusFault {
				t.Errorf("holds %v: %v maps to status %d, want one of its own", holds, err, r.Status)
			}
			if other, dup := statuses[r.Status]; dup {
				t.Errorf("holds %v: %v and %v share status %d", holds, err, other, r.Status)
			}
			statuses[r.Status] = err
			if isDelay := errors.Is(err, vfs.ErrDelay); (r.Hold != nil) != (holds && isDelay) {
				t.Errorf("holds %v: %v has a Hold: %v", holds, err, r.Hold != nil)
			}
		}
		if r := find(vfs.ErrDelay); holds && r != nil && r.Hold != nil {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if got := r.Hold(vfs.ErrDelay, start); !got.Equal(start.Add(HoldFor)) {
				t.Errorf("the hold ends at %v, want %v after its start", got, HoldFor)
			}
		}
		if find(vfs.ErrNotFound) != nil {
			t.Errorf("holds %v: a row maps ErrNotFound; the tables leave service errors to the fault status", holds)
		}
	}
}

func TestSettingsValidateRefusesAVersionWithNoTable(t *testing.T) {
	for v, ok := range map[core.Version]bool{0: true, 1: true, 2: true, 3: false} {
		if err := (Settings{MaxVersion: v}).Validate(); (err == nil) != ok {
			t.Errorf("Validate with MaxVersion %d = %v, want accepted: %v", v, err, ok)
		}
	}
}

// The Router refuses a version with no table before it builds one, and Bare
// serves version 1 alone.
func TestRouteRefusesAVersionItDoesNotServe(t *testing.T) {
	header := func(v core.Version) core.Header {
		b := Append(nil, Msg{Op: OpEcho, Version: v})[lengthLen:]
		h, err := headerCodec{}.Decode(&resources.Buf{B: b}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	full := New(adapter.Deps{})
	for _, c := range []struct {
		name string
		p    core.Protocol
		v    core.Version
	}{{"version 0", full, 0}, {"version 3", full, 3}, {"version 2 on Bare", full.Bare(), 2}} {
		st := c.p.NewConn(nil)
		if _, _, err := st.Router.Route(header(c.v), nil); !errors.Is(err, core.ErrVersion) {
			t.Errorf("%s: Route = %v, want ErrVersion", c.name, err)
		}
	}
}
