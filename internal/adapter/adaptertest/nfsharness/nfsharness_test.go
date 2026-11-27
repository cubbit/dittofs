package nfsharness_test

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

// void is a procedure's empty arguments and empty result body.
type void struct{}

func (*void) DecodeWire([]byte, uint32) (int, error)  { return 0, nil }
func (*void) EncodeWire(uint32, *resources.Buf) error { return nil }
func (*void) MaxWire(uint32) int                      { return 0 }
func (*void) IsZero() bool                            { return true }

var errRefused = errors.New("refused")

func be(ws ...uint32) []byte {
	var b []byte
	for _, w := range ws {
		b = binary.BigEndian.AppendUint32(b, w)
	}
	return b
}

// Row answers a result as a client reads it after the RPC reply header: the
// program's status word, NLM's cookie echoed ahead of it, and nothing for a
// result that carries no status.
func TestRowAnswersTheResultPastTheHeader(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	cookie := append(be(4), "abcd"...) // nlm4_lockargs's cookie
	for _, c := range []struct {
		name             string
		prog, vers, proc uint32
		st               uint32
		want             []byte
	}{
		{"NFSv3 NULL has no status", wire.ProgNFS, wire.VersNFS3, 0, 0, nil},
		{"NFSv3 WRITE NFS3ERR_IO", wire.ProgNFS, wire.VersNFS3, 7, 5, be(5)},
		{"MOUNT MNT MNT3ERR_ACCES", wire.ProgMount, wire.VersMount3, 1, 13, be(13)},
		{"MOUNT EXPORT has no status", wire.ProgMount, wire.VersMount3, 5, 0, nil},
		{"NLM LOCK echoes the cookie", wire.ProgNLM, wire.VersNLM4, 2, 1, append(append([]byte{}, cookie...), be(1)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			tab := core.NewOpTable(core.NewErrorTable(core.Status(wire.NFS3ErrServerFault),
				core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs)},
				core.ErrorRow{Err: errRefused, Status: core.Status(c.st)},
			), false, core.Row(c.proc, "row", core.NeedsNothing, func(context.Context, *core.Request, void) (void, error) {
				if c.st != 0 {
					return void{}, errRefused
				}
				return void{}, nil
			}))
			got := nfsharness.Row(t, rn, tab, c.prog, c.proc, &core.Request{Version: core.Version(c.vers)}, cookie, adaptertest.Expect{})
			if string(got) != string(c.want) {
				t.Errorf("Row answers %x, want %x", got, c.want)
			}
		})
	}
}
