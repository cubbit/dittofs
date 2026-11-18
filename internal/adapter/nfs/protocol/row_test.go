package protocol_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// core.Row takes the generated codec as the generator writes it, its version a
// uint32 that core.Version aliases, and the row decodes the request, calls the
// handler with it and encodes its reply past the status, whose zero value is
// its fail arm (RFC 20 section 4.7): a failed ACCESS writes nothing, a failed
// WRITE3 file_wcc's two FALSEs, and a success its ok part too, within the
// reply the row reserves, and decodes back to what the handler returned.
func TestRowOverTheGeneratedCodec(t *testing.T) {
	access := func(fail error) *core.Op {
		return core.Row[AccessArgs, AccessRes, *AccessArgs, *AccessRes](3, "access", core.NeedsFH,
			func(_ context.Context, _ *core.Request, req AccessArgs) (AccessRes, error) {
				if fail != nil {
					return AccessRes{}, fail
				}
				return AccessRes{Ok: &AccessResOk{Supported: 0x3f, Access: req.Access & 0x3f}}, nil
			})
	}
	write := func(fail error) *core.Op {
		return core.Row[Write3Args, Write3Res, *Write3Args, *Write3Res](7, "write", core.NeedsFH,
			func(_ context.Context, _ *core.Request, req Write3Args) (Write3Res, error) {
				if fail != nil {
					return Write3Res{}, fail
				}
				return Write3Res{Ok: &Write3ResOk{Count: req.Count, Committed: 2, Verf: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}}, nil
			})
	}
	accessArgs := []byte{0, 0, 0, 0x41, 0xff}                                 // ACCESS4args, and a byte past it
	writeArgs := append(make([]byte, 12), 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0) // no handle, offset 0, count 5, UNSTABLE, no data
	okAccess := func(b []byte) bool {
		var r AccessRes
		_, err := r.DecodeWire(b, 0)
		return err == nil && r.Ok != nil && *r.Ok == AccessResOk{Supported: 0x3f, Access: 1}
	}
	okWrite := func(b []byte) bool {
		var r Write3Res
		_, err := r.DecodeWire(b, 0)
		return err == nil && r.FileWcc == WccData{} && r.Ok != nil && *r.Ok == Write3ResOk{Count: 5, Committed: 2, Verf: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}
	}
	for _, c := range []struct {
		name string
		op   *core.Op
		args []byte
		n    int    // the request's bytes
		err  error  // the handler's
		want []byte // past the status
		back func([]byte) bool
	}{
		{"a failed ACCESS", access(vfs.ErrIO), accessArgs, 4, vfs.ErrIO, []byte{}, nil},
		{"a successful ACCESS", access(nil), accessArgs, 4, nil, []byte{0, 0, 0, 0x3f, 0, 0, 0, 0x01}, okAccess},
		{"a failed WRITE3", write(vfs.ErrIO), writeArgs, 24, vfs.ErrIO, make([]byte, 8), nil},
		{"a successful WRITE3", write(nil), writeArgs, 24, nil,
			[]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 2, 1, 2, 3, 4, 5, 6, 7, 8}, okWrite},
	} {
		t.Run(c.name, func(t *testing.T) {
			inv, n, err := c.op.Decode(c.args, 0)
			if err != nil || n != c.n {
				t.Fatalf("decoded %d bytes, %v; want %d", n, err, c.n)
			}
			r := &core.Request{}
			if err := inv.Call(context.Background(), r); !errors.Is(err, c.err) {
				t.Fatalf("the call answered %v, want %v", err, c.err)
			}
			var out resources.Buf
			if err := inv.Encode(&out, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.B, c.want) {
				t.Errorf("encoded %x after the status, want %x", out.B, c.want)
			}
			if bound, _ := inv.Bound(r, 1<<20); len(out.B) > bound {
				t.Errorf("encoded %d bytes past the %d the row reserves", len(out.B), bound)
			}
			if c.back != nil && !c.back(out.B) {
				t.Errorf("%x does not decode back to the handler's reply", out.B)
			}
		})
	}
}
