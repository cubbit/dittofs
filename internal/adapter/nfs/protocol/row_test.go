package protocol_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

// core.Row takes the generated codec as the generator writes it, its version a
// uint32 that core.Version aliases (RFC 20 section 4.7), and the row decodes
// the request, calls the handler with it and encodes the handler's reply.
func TestRowOverTheGeneratedCodec(t *testing.T) {
	access := func(_ context.Context, _ *core.Request, req AccessArgs) (AccessRes, error) {
		return AccessRes{Supported: 0x3f, Access: req.Access & 0x3f}, nil
	}
	op := core.Row[AccessArgs, AccessRes, *AccessArgs, *AccessRes](3, "access", core.NeedsFH, access)
	inv, n, err := op.Decode([]byte{0, 0, 0, 0x41, 0xff}, 0)
	if err != nil || n != 4 {
		t.Fatalf("decoded %d bytes, %v; want the 4 of ACCESS4args", n, err)
	}
	if err := inv.Call(context.Background(), &core.Request{}); err != nil {
		t.Fatal(err)
	}
	var out resources.Buf
	if err := inv.Encode(&out, 0); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0, 0, 0, 0x3f, 0, 0, 0, 0x01}; !bytes.Equal(out.B, want) {
		t.Errorf("encoded %x, want ACCESS4resok %x", out.B, want)
	}
}
