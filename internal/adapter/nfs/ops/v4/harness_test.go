package v4

import (
	"bytes"
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The NFSv4 codes and status the test member answers with: OP_WRITE (RFC
// 8881 section 18.32) and NFS4ERR_IO (RFC 8881 section 15.1).
const (
	opWrite   = 38
	nfs4ErrIO = 5
)

// write4 is a test member's handler set: NFSv4's WRITE through the current
// filehandle the members before it left, whose one call is a Write.
type write4 struct{ svc vfs.Service }

func (h write4) write(ctx context.Context, r *core.Request, a write4Args) (write4Res, error) {
	fh := r.State.(*protocol.State).Cur.FH
	w, err := h.svc.Write(ctx, r.Call, vfs.Anonymous(fh), int64(a.Offset), a.Data, vfs.Unstable)
	if err != nil {
		return write4Res{}, err
	}
	return write4Res{Ok: &write4ResOk{Count: uint32(w.N), Committed: a.Stable, Verf: w.Verifier}}, nil
}

// An NFSv4 member runs on nfsharness against the in-memory service, with the
// current filehandle a state the test builds holds: its one Write reaches
// that file, and its result starts with the members' table's head, its
// operation code and status, before WRITE4resok; a fault injected on Write
// answers the status the table maps it to, with no reply past the head, and
// is recorded, with the injected error as its answer, as the request's one
// call.
func TestAnNFSv4MemberOnTheHarness(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	members := core.NewOpTable(core.NewErrorTable(core.Status(wire.NFS4ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.NFS4ErrBadXDR)},
		core.ErrorRow{Err: vfs.ErrIO, Status: nfs4ErrIO},
	), false, core.Row[write4Args, write4Res](opWrite, "WRITE", core.NeedsFH, write4{svc: rn.Rec}.write))
	members.ReadOp, members.EncodeResult = readOp, encodeResult
	admin := rn.Mem.Admin()
	made, err := rn.Mem.Create(t.Context(), admin, rn.Mem.Store.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fh := made.Handle
	var args resources.Buf
	if err := (&write4Args{Offset: 4096, Data: []byte("hello")}).EncodeWire(1, &args); err != nil {
		t.Fatal(err)
	}
	request := func() *core.Request {
		return &core.Request{Call: admin, Version: 1, Level: core.NeedsFH, State: &protocol.State{Cur: protocol.Current{FH: fh}}}
	}
	write := adaptertest.Expect{Call: vfs.OpWrite}

	t.Run("a success", func(t *testing.T) {
		got := nfsharness.Member(t, rn, members, opWrite, request(), args.B, write)
		verf, err := rn.Mem.Commit(t.Context(), admin, vfs.Anonymous(fh), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]byte{
			0, 0, 0, opWrite, 0, 0, 0, 0, // resop OP_WRITE, NFS4_OK
			0, 0, 0, 5, // count
			0, 0, 0, 0, // UNSTABLE4
		}, verf[:]...)
		if !bytes.Equal(got, want) {
			t.Errorf("the result is %x, want %x", got, want)
		}
		buf := make([]byte, 8)
		if n, _, err := rn.Mem.Read(t.Context(), admin, vfs.Anonymous(fh), 4096, buf); err != nil || string(buf[:n]) != "hello" {
			t.Errorf("the file holds %q at 4096, %v; want %q", buf[:n], err, "hello")
		}
	})

	t.Run("a fault on Write", func(t *testing.T) {
		rn.Faults.Fail(vfs.OpWrite, vfs.ErrIO)
		got := nfsharness.Member(t, rn, members, opWrite, request(), args.B, write)
		want := []byte{0, 0, 0, opWrite, 0, 0, 0, nfs4ErrIO} // resop OP_WRITE, NFS4ERR_IO, and nothing past it
		if !bytes.Equal(got, want) {
			t.Errorf("the result is %x, want %x", got, want)
		}
		if calls := rn.Rec.Calls(); len(calls) != 1 || calls[0].Op != vfs.OpWrite || calls[0].Err != vfs.ErrIO {
			t.Errorf("the record is %+v, want the one Write answered the injected %v", calls, vfs.ErrIO)
		}
	})
}
