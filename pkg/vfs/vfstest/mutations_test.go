package vfstest_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// Each Service method, called once in Op order, is recorded as its own op
// exactly when that op may change state.
func TestMutationsRecordsEveryMethodAsItsOp(t *testing.T) {
	rec := vfstest.RecordMutations(vfstest.Null{})
	v := reflect.ValueOf(rec)
	var want []vfs.Op
	for op := vfs.Op(1); op.String() != "Op(?)"; op++ {
		m := v.MethodByName(op.String())
		if !m.IsValid() {
			t.Fatalf("Mutations has no method %s", op)
		}
		args := make([]reflect.Value, m.Type().NumIn())
		for i := range args {
			args[i] = reflect.Zero(m.Type().In(i))
		}
		args[0] = reflect.ValueOf(context.Background())
		if m.Type().IsVariadic() {
			m.CallSlice(args)
		} else {
			m.Call(args)
		}
		if vfs.Mutates(op) {
			want = append(want, op)
		}
	}
	if got := rec.Ops(); !slices.Equal(got, want) {
		t.Errorf("Ops = %v, want %v", got, want)
	}
}

// The wrapper records each call that may change state, a refused one
// included, and none that cannot; every answer, and every change, is the
// filesystem's it passes the call to.
func TestMutationsRecordsEachMutatingCallItPasses(t *testing.T) {
	s := memory.New(t)
	ctx, admin := t.Context(), s.Admin()
	h, _ := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
	if w, err := s.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable); err != nil || w.N != 5 {
		t.Fatalf("Write: %d bytes, %v; want 5", w.N, err)
	}
	buf := make([]byte, 8)
	if n, _, err := s.Read(ctx, admin, vfs.Anonymous(h), 0, buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read: %q, %v; want %q", buf[:n], err, "hello")
	}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(2)}); err != nil {
		t.Fatalf("SetAttr of size 2: %v", err)
	}
	if f, err := s.GetAttr(ctx, admin, h); err != nil || f.Size != 2 {
		t.Fatalf("GetAttr: size %d, %v; want 2", f.Size, err)
	}
	if _, err := s.Unlink(ctx, admin, vfs.Target{H: s.Store.Root}, []byte("missing")); !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("Unlink of a name never made: %v, want ErrNotFound", err)
	}
	want := []vfs.Op{vfs.OpWrite, vfs.OpSetAttr, vfs.OpUnlink}
	if got := s.Ops(); !slices.Equal(got, want) {
		t.Errorf("Ops = %v, want %v", got, want)
	}
	s.Reset()
	if got := s.Ops(); len(got) != 0 {
		t.Errorf("Ops after Reset = %v, want none", got)
	}
}

// smbCall is the administrator's call on SMB session id, with SMB's capabilities.
func smbCall(s *memory.Service, id uint64) vfs.Call {
	c := s.Admin()
	c.Caps, c.Session = vfs.SMBCaps, vfs.SessionRef{ID: id}
	return c
}

// A last Disconnect is recorded with its client, its session, its Last and
// the Gen it was given, and answered with the newer Gen the filesystem
// returned once a second session's Connect listed the node again.
func TestMutationsRecordsADisconnectsGivenAndAnsweredGen(t *testing.T) {
	s := memory.New(t)
	ctx := t.Context()
	info := vfs.ClientInfo{Protocol: entity.ProtocolSMB, Name: []byte("pc"), Boot: [8]byte{1}, Recovery: entity.RecoverReconnect}
	id, first, err := s.Connect(ctx, smbCall(s, 1), info, &vfstest.Callbacks{})
	if err != nil {
		t.Fatalf("Connect on session 1: %v", err)
	}
	if _, _, err = s.Connect(ctx, smbCall(s, 2), info, &vfstest.Callbacks{}); err != nil {
		t.Fatalf("Connect on session 2: %v", err)
	}
	s.Reset()
	end := vfs.SessionEnd{Last: true, Gen: first}
	gen, err := s.Disconnect(ctx, smbCall(s, 1), id, vfs.SessionRef{ID: 1}, end)
	if err != nil || gen == 0 || gen == first {
		t.Fatalf("Disconnect at the stale Gen %d: Gen %d, %v; want a newer Gen", first, gen, err)
	}
	want := []vfstest.Mutation{{Op: vfs.OpDisconnect, Client: id, Session: vfs.SessionRef{ID: 1}, End: end, Gen: gen}}
	if got := s.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("Calls = %+v, want %+v", got, want)
	}
}

// A CancelLock is recorded with the waiter it names: its open, owner and
// range, and the answer the filesystem gave.
func TestMutationsRecordsACancelLocksWaiter(t *testing.T) {
	s := memory.New(t)
	h, _ := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
	ref := vfs.Anonymous(h)
	owner := vfs.LockOwner{Client: 3, Name: []byte("owner")}
	r := vfs.ByteRange{Offset: 10, Length: 20}
	_, err := s.CancelLock(t.Context(), s.Admin(), ref, owner, r)
	want := []vfstest.Mutation{{Op: vfs.OpCancelLock, Open: &ref, Owner: owner, Range: r, Err: err}}
	got := s.Calls()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls = %+v, want %+v", got, want)
	}
	if got[0].Err != err {
		t.Errorf("CancelLock recorded the answer %v, want the %v it returned", got[0].Err, err)
	}
}

// A FreeState answered a revoked reference by a fault under the wrapper is
// recorded with its client, its reference and that very answer.
func TestMutationsRecordsAFreeStatesReferenceAndRevokedAnswer(t *testing.T) {
	faults := vfstest.Faults(memory.New(t))
	rec := vfstest.RecordMutations(faults)
	revoked := &vfs.StateError{Kind: vfs.StateRevoked}
	faults.Fail(vfs.OpFreeState, revoked)
	ref := vfs.StateRef{Type: vfs.StateOpen, Shard: 1, Instance: 2, Token: 7, Seq: 1}
	if err := rec.FreeState(t.Context(), vfs.Call{}, 5, ref); err != revoked {
		t.Fatalf("FreeState: %v, want the fault's %v", err, revoked)
	}
	got := rec.Calls()
	want := []vfstest.Mutation{{Op: vfs.OpFreeState, Client: 5, State: ref, Err: revoked}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls = %+v, want %+v", got, want)
	}
	if got[0].Err != revoked {
		t.Errorf("FreeState recorded the answer %v, want the fault's own %p", got[0].Err, revoked)
	}
}

// Two calls returning in the opposite order to the one they started in are
// recorded in the order they started, each with its own answer: the first
// holds its place, with no answer, until it returns.
func TestMutationsKeepsStartOrderWhenCallsReturnOutOfOrder(t *testing.T) {
	mem := memory.New(t)
	faults := vfstest.Faults(mem)
	rec := vfstest.RecordMutations(faults)
	ctx, admin := t.Context(), mem.Admin()
	h, _ := mem.Store.Create(t, mem.Store.Admin, mem.Store.Root, "f", kvstore.Regular(0o644))
	hold := faults.Hold(vfs.OpUnlink)
	unlinked := make(chan error, 1)
	go func() {
		_, err := rec.Unlink(ctx, admin, vfs.Target{H: mem.Store.Root}, []byte("missing"))
		unlinked <- err
	}()
	<-hold.Reached()
	if _, err := rec.SetAttr(ctx, admin, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(2)}); err != nil {
		t.Fatalf("SetAttr of size 2: %v", err)
	}
	running := []vfstest.Mutation{{Op: vfs.OpUnlink}, {Op: vfs.OpSetAttr}}
	if got := rec.Calls(); !reflect.DeepEqual(got, running) {
		t.Errorf("Calls while Unlink is held = %+v, want %+v", got, running)
	}
	hold.Release()
	err := <-unlinked
	if !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("Unlink of a name never made: %v, want ErrNotFound", err)
	}
	want := []vfstest.Mutation{{Op: vfs.OpUnlink, Err: err}, {Op: vfs.OpSetAttr}}
	if got := rec.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("Calls = %+v, want %+v", got, want)
	}
}
