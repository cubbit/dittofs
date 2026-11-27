package protocol

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Each COMPOUND Route builds carries a state of its own, new and empty, so a
// member of one compound never sees another's filehandle, stateid or budget,
// routed one after the other on one connection; a
// call that is no COMPOUND, NFSv4's NULL among them, carries none (RFC 20
// section 4.7, RFC 21 section 2.1).
func TestEachCompoundGetsItsOwnState(t *testing.T) {
	s := stageOf(settings.VersionRange{Min: "3", Max: "4.2"})
	route := func(body []byte) *core.Request {
		t.Helper()
		h, err := decode(t, s, body)
		if err != nil {
			t.Fatal(err)
		}
		_, r, err := s.Route(h, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := route(compoundCall(1, "a"))
	st, ok := first.State.(*State)
	if !ok || !reflect.DeepEqual(*st, State{}) {
		t.Fatalf("the first COMPOUND's state is %#v, want an empty *State", first.State)
	}
	st.Cur = Current{FH: vfs.Handle("fh"), HasStateid: true, Stateid: Stateid{Seqid: 1}}
	st.Saved, st.MaxReply, st.MaxCached = st.Cur, 4096, 512
	second := route(compoundCall(1, "a"))
	if got, ok := second.State.(*State); !ok || got == st || !reflect.DeepEqual(*got, State{}) {
		t.Errorf("the second COMPOUND's state is %#v, want a new, empty *State", second.State)
	}
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"NFSv4's NULL", call(wire.ProgNFS, wire.VersNFS4)},
		{"NFSv3's NULL", call(wire.ProgNFS, wire.VersNFS3)},
		{"MOUNT's NULL", call(wire.ProgMount, wire.VersMount3)},
		{"NLM's NULL", call(wire.ProgNLM, wire.VersNLM4)},
		{"NSM's NULL", call(wire.ProgNSM, wire.VersNSM1)},
	} {
		if r := route(c.body); r.State != nil {
			t.Errorf("%s carries the state %#v, want none", c.name, r.State)
		}
	}
}

// SetFH sets the current filehandle, makes the current stateid the all-zeros
// one and
// raises NeedsFH in the Level core's needs check reads, the bits already
// there kept (RFC 20 section 4.7, RFC 8881 section 16.2.3.1.2).
func TestSetFHRaisesTheFilehandleBit(t *testing.T) {
	st := &State{Cur: Current{FH: vfs.Handle("old"), HasStateid: true, Stateid: Stateid{Seqid: 7, Other: [12]byte{1}}}}
	r := &core.Request{Level: core.NeedsSession, State: st}
	SetFH(r, vfs.Handle("new"))
	if r.Level != core.NeedsSession|core.NeedsFH {
		t.Errorf("Level is %#x, want NeedsSession|NeedsFH %#x", r.Level, core.NeedsSession|core.NeedsFH)
	}
	if !bytes.Equal(st.Cur.FH, []byte("new")) {
		t.Errorf("the current filehandle is %q, want %q", st.Cur.FH, "new")
	}
	if st.Cur.HasStateid || st.Cur.Stateid != (Stateid{}) {
		t.Errorf("the current stateid is %+v, set %v, want the all-zeros stateid, not set", st.Cur.Stateid, st.Cur.HasStateid)
	}
}
