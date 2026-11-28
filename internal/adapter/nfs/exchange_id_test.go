package nfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/ops/v4"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
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
		Node:         [16]byte{3, 15: 9},
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

		// The node's minor ID is the first eight bytes of the SHA-256 of its
		// ID, worked out here from the bytes.
		sum := sha256.Sum256(d.Server.Node[:])
		if m, want := binary.BigEndian.Uint64(b[replyMinor:]), binary.BigEndian.Uint64(sum[:8]); m != want {
			t.Errorf("uid %d: the minor ID is %#x, want %#x", uid, m, want)
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

// A client whose lease expires while its CREATE_SESSION is being written has
// the session ended at once, and the node sends the Disconnect the service
// is owed for a node that lists itself and holds nothing: its last session,
// at the Gen of the entry the write listed. Without it the record would list
// the node for a session that no longer exists, and the client could never
// be destroyed (RFC 21 section 3.3).
func TestLeaseExpiryDuringCreateSessionSendsTheLastDisconnect(t *testing.T) {
	d := leakChecked(t)

	mem := d.VFS.(*memory.Service)
	rn := &adaptertest.Runner{Mem: mem, Faults: vfstest.Faults(mem)}
	rn.Rec = vfstest.RecordMutations(rn.Faults)

	d.VFS = rn.Rec

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

	rep, err := c.Do(within(t), exchangeIDCall(t, 100))
	if err != nil {
		t.Fatal(err)
	}

	cl := binary.BigEndian.Uint64(rep.Body[replyClientID:])
	seq := binary.BigEndian.Uint32(rep.Body[replySeqID:])

	hold := rn.Faults.Hold(vfs.OpAddSession)

	done := make(chan error, 1)

	go func() {
		_, err := c.Do(within(t), createSessionCall(100, cl, seq))
		done <- err
	}()

	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		hold.Release()
		t.Fatal("the AddSession never arrived")
	}

	h.Callbacks(vfs.ClientID(cl), vfs.ProtocolNFSv4).Revoked(
		t.Context(),
		vfs.Revocation{Client: vfs.ClientID(cl), Reason: vfs.RevokedLeaseExpired},
	)
	hold.Release()

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	rec, err := rn.Mem.Record(t.Context(), rn.Mem.Admin(), vfs.ClientID(cl))
	if err != nil {
		t.Fatal(err)
	}

	var sent []vfstest.Mutation

	for _, m := range rn.Rec.Calls() {
		if m.Op == vfs.OpDisconnect {
			sent = append(sent, m)
		}
	}

	if len(sent) != 1 {
		t.Fatalf("the service saw %d Disconnect calls, want 1", len(sent))
	}

	if !sent[0].End.Last || sent[0].End.Gen == 0 || sent[0].End.Gen != rec.Reach.Gen {
		t.Errorf(
			"Disconnect was sent %+v, want Last at the entry's Gen, %d",
			sent[0].End,
			rec.Reach.Gen,
		)
	}

	if len(rec.Reach.Sessions) != 0 {
		t.Errorf("the record lists %v, want no node after the last Disconnect", rec.Reach.Sessions)
	}

	t.Cleanup(func() {
		h.Sessions.Drop(vfs.ClientID(cl))
	})
}
