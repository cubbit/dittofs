package nfs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/ops/v4"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The offsets of EXCHANGE_ID's result inside a COMPOUND's reply (RFC 8881
// sections 18.35.2 and 16.2.2): the compound's status, its empty tag and the
// count of results, then the member's operation and status, and its fields.
const (
	replyStatus   = 0
	replyMember   = 12
	replyOp       = replyMember
	replyOpStatus = replyMember + 4
	replyClientID = replyMember + 8
	replyFlags    = replyMember + 20
	replyMinor    = replyMember + 28
	replyMajor    = replyMember + 40
	opExchangeID  = 42
	flagUseNonPNF = 0x00010000
)

// exchangeIDCall is a COMPOUND of minor version 1 holding the one EXCHANGE_ID
// of the golden arguments, under AUTH_SYS uid.
func exchangeIDCall(t *testing.T, uid uint32) nfsclient.Call {
	t.Helper()

	args := append(words(0, 1, 1, opExchangeID), golden(t, "rfc-nfs4-exchange-id-args")...)

	return nfsclient.Call{
		Prog: progNFS,
		Vers: 4,
		Proc: 1,
		Cred: nfsclient.AuthSys(uid, uid),
		Args: args,
	}
}

// EXCHANGE_ID reaches its handler through a socket: a COMPOUND of minor
// version 1 holding it is answered a client ID, USE_NON_PNFS, and the major
// ID of the installation with the node's own minor ID; two principals asking
// for one name before any confirm are each answered their own client ID; and
// the same COMPOUND at minor version 0, which has no EXCHANGE_ID, is refused
// as an operation it does not define (RFC 8881 section 18.35; RFC 21 sections
// 3.3 and 3.5).
func TestExchangeIDOverTheWire(t *testing.T) {
	d := leakChecked(t)
	d.Server = adapter.Server{
		Installation: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Node:         3,
	}

	mem := d.VFS.(*memory.Service)
	adaptertest.OtherUser(t, mem)

	// The proposals stay charged to the connection budget until the node's
	// table drops them, so the test builds the server over handlers it keeps.
	h := handlers(d)
	ts := tables(d)
	ts.V4 = v4.Table(h)

	srv, err := core.NewServer(d, nfsSettings(only4), core.Listener{
		Services: []string{"nfs"},
		Network:  "tcp",
		Protocol: protocol.New(ts, d.Budgets),
	})
	if err != nil {
		t.Fatal(err)
	}

	start(t, srv)

	c := client(t, srv.Addrs()[adapter.Socket{Service: "nfs", Network: "tcp"}][0])
	ids := map[uint32]uint64{}

	t.Cleanup(func() {
		for _, id := range ids {
			h.Sessions.Drop(vfs.ClientID(id))
		}
	})

	for _, uid := range []uint32{100, 101} {
		rep, err := c.Do(within(t), exchangeIDCall(t, uid))
		if err != nil {
			t.Fatal(err)
		}

		b := rep.Body
		if len(b) < replyMajor+20 ||
			binary.BigEndian.Uint32(b[replyStatus:]) != 0 ||
			binary.BigEndian.Uint32(b[replyOp:]) != opExchangeID ||
			binary.BigEndian.Uint32(b[replyOpStatus:]) != 0 {
			t.Fatalf("uid %d: the reply is %x, want EXCHANGE_ID answered NFS4_OK", uid, b)
		}

		if f := binary.BigEndian.Uint32(b[replyFlags:]); f != flagUseNonPNF {
			t.Errorf("uid %d: the flags are %#x, want %#x", uid, f, flagUseNonPNF)
		}

		if m := binary.BigEndian.Uint64(b[replyMinor:]); m != 3 {
			t.Errorf("uid %d: the minor ID is %d, want 3", uid, m)
		}

		if !bytes.Equal(b[replyMajor:replyMajor+16], d.Server.Installation[:]) {
			t.Errorf(
				"uid %d: the major ID is %x, want the installation's",
				uid,
				b[replyMajor:replyMajor+16],
			)
		}

		ids[uid] = binary.BigEndian.Uint64(b[replyClientID:])
	}

	if ids[100] == ids[101] {
		t.Errorf("two principals were both answered client %d, want their own", ids[100])
	}

	v0 := exchangeIDCall(t, 100)
	v0.Args = append(words(0, 0, 1, opExchangeID), golden(t, "rfc-nfs4-exchange-id-args")...)

	rep, err := c.Do(within(t), v0)
	if err != nil {
		t.Fatal(err)
	}

	if st := binary.BigEndian.Uint32(rep.Body[replyStatus:]); st != 10044 {
		t.Errorf("minor version 0 answered status %d, want NFS4ERR_OP_ILLEGAL, 10044", st)
	}
}
