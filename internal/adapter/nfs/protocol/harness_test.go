package protocol_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// nfs3ErrIO is NFS3ERR_IO (RFC 1813 section 2.6).
const nfs3ErrIO = 5

// write3 is a test row's handler set: NFSv3's WRITE over the service, whose
// one call is a Write, as RFC 20 section 4.3's example writes it.
type write3 struct{ svc vfs.Service }

func (h write3) write(ctx context.Context, r *core.Request, a Write3Args) (Write3Res, error) {
	w, err := h.svc.Write(ctx, r.Call, vfs.Anonymous(vfs.Handle(a.File)), int64(a.Offset), a.Data, vfs.Unstable)
	if err != nil {
		return Write3Res{}, err
	}
	return Write3Res{Ok: &Write3ResOk{Count: uint32(w.N), Committed: a.Stable, Verf: w.Verifier}}, nil
}

// An NFSv3 row runs on nfsharness against the in-memory service: its one
// Write reaches the file at the offset the request names, and its result is
// NFS3_OK and WRITE3resok; a fault injected on Write answers the status the
// table maps it to, with a failed WRITE3's empty file_wcc, and is recorded,
// with the injected error as its answer, as the request's one call.
func TestAnNFSv3RowOnTheHarness(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	tab := core.NewOpTable(core.NewErrorTable(core.Status(wire.NFS3ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs)},
		core.ErrorRow{Err: vfs.ErrIO, Status: nfs3ErrIO},
	), false, core.Row(7, "write", core.NeedsNothing, write3{svc: rn.Rec}.write))
	admin := rn.Mem.Admin()
	made, err := rn.Mem.Create(t.Context(), admin, rn.Mem.Store.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fh := made.Handle
	var args resources.Buf
	if err := (&Write3Args{File: fh, Offset: 8192, Count: 5, Data: []byte("hello")}).EncodeWire(3, &args); err != nil {
		t.Fatal(err)
	}
	write := adaptertest.Expect{Call: vfs.OpWrite}

	t.Run("a success", func(t *testing.T) {
		got := nfsharness.Row(t, rn, tab, 7, &core.Request{Call: admin, Version: 3}, args.B, write)
		verf, err := rn.Mem.Commit(t.Context(), admin, vfs.Anonymous(fh), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]byte{
			0, 0, 0, 0, // NFS3_OK
			0, 0, 0, 0, 0, 0, 0, 0, // file_wcc: no pre-op or post-op attributes
			0, 0, 0, 5, // count
			0, 0, 0, 0, // UNSTABLE
		}, verf[:]...)
		if !bytes.Equal(got, want) {
			t.Errorf("the result is %x, want %x", got, want)
		}
		buf := make([]byte, 8)
		if n, _, err := rn.Mem.Read(t.Context(), admin, vfs.Anonymous(fh), 8192, buf); err != nil || string(buf[:n]) != "hello" {
			t.Errorf("the file holds %q at 8192, %v; want %q", buf[:n], err, "hello")
		}
	})

	t.Run("a fault on Write", func(t *testing.T) {
		rn.Faults.Fail(vfs.OpWrite, vfs.ErrIO)
		got := nfsharness.Row(t, rn, tab, 7, &core.Request{Call: admin, Version: 3}, args.B, write)
		want := []byte{0, 0, 0, nfs3ErrIO, 0, 0, 0, 0, 0, 0, 0, 0} // NFS3ERR_IO, then file_wcc's two FALSEs
		if !bytes.Equal(got, want) {
			t.Errorf("the result is %x, want %x", got, want)
		}
		if calls := rn.Rec.Calls(); len(calls) != 1 || calls[0].Op != vfs.OpWrite || calls[0].Err != vfs.ErrIO {
			t.Errorf("the record is %+v, want the one Write answered the injected %v", calls, vfs.ErrIO)
		}
	})
}
