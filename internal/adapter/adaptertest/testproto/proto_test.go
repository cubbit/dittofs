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
	for _, op := range New(adapter.Deps{}).ops(v) {
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
	for _, op := range New(adapter.Deps{}).ops(2) {
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
	want := map[uint32]struct {
		needs     core.Needs
		cacheable bool
	}{
		OpNegotiate: {core.NeedsNothing, false},
		OpPutFH:     {core.NeedsTree, false},
		OpEcho:      {core.NeedsNothing, false},
		OpCreate:    {core.NeedsTree | core.NeedsFH, true},
		OpPanic:     {core.NeedsNothing, false},
		OpSession:   {core.NeedsNegotiated, false},
		OpLogoff:    {core.NeedsSession, false},
		OpTree:      {core.NeedsSession, false},
		OpNotify:    {core.NeedsNothing, false},
		OpViolate:   {core.NeedsNothing, false},
	}
	for _, v := range []core.Version{1, 2} {
		seen := map[uint32]bool{}
		for _, op := range New(adapter.Deps{}).ops(v) {
			w, ok := want[op.Code]
			switch {
			case !ok:
				continue
			case seen[op.Code]:
				t.Errorf("version %d: operation %d has two rows", v, op.Code)
			case op.Needs != w.needs || op.Cacheable != w.cacheable:
				t.Errorf("version %d, %s: needs %#x cacheable %v; want %#x, %v", v, op.Name, op.Needs, op.Cacheable, w.needs, w.cacheable)
			case v < op.Versions.First || v > op.Versions.Last:
				t.Errorf("version %d's table holds %s, served in %v", v, op.Name, op.Versions)
			}
			seen[op.Code] = true
		}
		if len(seen) != len(want) {
			t.Errorf("version %d has %d of the %d rows both versions serve", v, len(seen), len(want))
		}
	}

	// Park is version 2's alone, and Session and Logoff run on an expired
	// credential, Logoff as a release.
	byCode := func(v core.Version) map[uint32]*core.Op {
		m := map[uint32]*core.Op{}
		for _, op := range New(adapter.Deps{}).ops(v) {
			m[op.Code] = op
		}
		return m
	}
	v1, v2 := byCode(1), byCode(2)
	if v1[OpPark] != nil || v2[OpPark] == nil || v2[OpPark].Versions != (core.Range{First: 2, Last: 2}) || v2[OpPark].Needs != core.NeedsTree|core.NeedsFH {
		t.Errorf("Park: version 1 has it: %v; version 2's row: %+v", v1[OpPark] != nil, v2[OpPark])
	}
	if !v2[OpSession].Expired || !v2[OpLogoff].Expired || v2[OpLogoff].Role != core.RoleRelease || v2[OpEcho].Expired {
		t.Errorf("the expired-credential columns: Session %v, Logoff %v with role %d, Echo %v", v2[OpSession].Expired, v2[OpLogoff].Expired, v2[OpLogoff].Role, v2[OpEcho].Expired)
	}
	if v1[OpCancel] != nil || v2[OpCancel] != nil {
		t.Error("Cancel has a row: it is never routed")
	}
}

// Every refusal of core's has a row, the two drain errors answer nothing, and
// only the table that holds holds, for holdFor from the hold's start.
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
			if got := r.Hold(vfs.ErrDelay, start); !got.Equal(start.Add(holdFor)) {
				t.Errorf("the hold ends at %v, want %v after its start", got, holdFor)
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
