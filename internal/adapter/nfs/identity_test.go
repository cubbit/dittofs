package nfs

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// seen is what a recording row's handler received: the call the router built
// for it and the protocol label its context carried.
type seen struct {
	call  vfs.Call
	proto vfs.Protocol
	label bool
}

// recorder is the handlers of the recording rows, each call it receives in
// the order it arrived.
type recorder struct {
	mu   sync.Mutex
	seen []seen
}

// row is a recording row of code, for the versions vers: its handler keeps
// what it receives and answers with nothing.
func (rc *recorder) row(code uint32, name string, vers uint32) *core.Op {
	op := core.Row[void, void](code, name, core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			p, ok := vfs.ProtocolFrom(ctx)

			rc.mu.Lock()
			rc.seen = append(rc.seen, seen{call: r.Call, proto: p, label: ok})
			rc.mu.Unlock()

			return void{}, nil
		})
	op.Versions = core.Range{First: vers, Last: vers}

	return op
}

// take is what the rows received since the last take.
func (rc *recorder) take() []seen {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	got := rc.seen
	rc.seen = nil

	return got
}

// recording serves NFSv3's GETATTR, MOUNT's MNT, UMNT and EXPORT, NLM's TEST,
// NSM's SM_NOTIFY and, as NFSv4 COMPOUND's member at opAccess, a recording row
// each; it returns the recorder and the address to dial.
func recording(t *testing.T) (*recorder, netip.AddrPort) {
	t.Helper()
	d := leakChecked(t)
	rc := new(recorder)
	ts := tables(d)
	build := func(old *core.OpTable, ops ...*core.Op) *core.OpTable {
		tab := core.NewOpTable(old.Errors, false, ops...)
		tab.Protocol = "nfs3"

		return tab
	}
	ts.V3 = build(ts.V3, rc.row(1, "getattr", 3))
	ts.Mount = build(ts.Mount, rc.row(1, "mnt", 3), rc.row(3, "umnt", 3), rc.row(5, "export", 3))
	ts.NLM = build(ts.NLM, rc.row(1, "test", 4))
	ts.NSM = build(ts.NSM, rc.row(6, "sm_notify", 1))

	members := ts.V4.Ops[1].Members
	if _, taken := members.Ops[opAccess]; taken {
		t.Fatalf("the members' table holds a row at %d: pick a code no row holds", opAccess)
	}

	members.Ops[opAccess] = rc.row(opAccess, "access", 0)

	srv, err := core.NewServer(d, nfsSettings(allVersions), core.Listener{
		Services: []string{
			"nfs",
			"mount",
			"nlm",
			"nsm",
		}, Network: "tcp", Protocol: protocol.New(ts, d.Budgets),
	})
	if err != nil {
		t.Fatal(err)
	}

	start(t, srv)

	return rc, srv.Addrs()[adapter.Socket{Service: "nfs", Network: "tcp"}][0]
}

// send makes one call and fails t unless it is answered.
func send(t *testing.T, c *nfsclient.Client, call nfsclient.Call) {
	t.Helper()

	if _, err := c.Do(within(t), call); err != nil {
		t.Fatal(err)
	}
}

// An AUTH_SYS call reaches its handler as the Identity it claims: its uid,
// gid and groups, the address it came from, a claim and no more. The address
// is the source the client bound, a COMPOUND's members read the one call its
// route built.
func TestAHandlerReceivesTheCallersIdentity(t *testing.T) {
	rc, addr := recording(t)
	cred := nfsclient.AuthSys(1000, 1000, 10, 20, 30)
	want := func(client string) vfs.Identity {
		return vfs.Identity{
			Proof:  vfs.ProofClaimed,
			Claims: vfs.Claims{UID: 1000, GID: 1000, GIDs: []uint32{10, 20, 30}},
			Client: netip.MustParseAddr(client),
		}
	}
	getattr := nfsclient.Call{Prog: progNFS, Vers: 3, Proc: 1, Cred: cred}

	for _, from := range []string{"127.0.0.1", "127.0.0.2"} {
		t.Run(from, func(t *testing.T) {
			// Only Linux routes all of 127/8: where the host binds no such
			// address, this row is skipped and the others still run.
			probe, err := net.Listen("tcp", net.JoinHostPort(from, "0"))
			if err != nil {
				t.Skipf("%s: this host binds no such loopback address", from)
			}

			_ = probe.Close()

			c, err := nfsclient.DialFrom(within(t), netip.MustParseAddr(from), addr)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = c.Close() })
			send(t, c, getattr)

			got := rc.take()
			if len(got) != 1 || !reflect.DeepEqual(got[0].call.Identity, want(from)) {
				t.Errorf(
					"from %s the handler received %+v, want the Identity %+v",
					from,
					got,
					want(from),
				)
			}
		})
	}

	c := client(t, addr)
	send(
		t,
		c,
		nfsclient.Call{
			Prog: progNFS,
			Vers: 4,
			Proc: 1,
			Cred: cred,
			Args: words(0, 0, 2, opAccess, opAccess),
		},
	)

	got := rc.take()
	if len(got) != 2 || !reflect.DeepEqual(got[0].call, got[1].call) ||
		!reflect.DeepEqual(got[0].call.Identity, want("127.0.0.1")) {
		t.Errorf("a COMPOUND's members received %+v, want two equal calls of the Identity %+v",
			got, want("127.0.0.1"))
	}
}

// calls is one call to each recording row, with the label and the profile its
// handler's call must carry.
var calls = []struct {
	name  string
	call  nfsclient.Call
	proto vfs.Protocol
	caps  vfs.Caps
}{
	{
		"NFSv3 GETATTR",
		nfsclient.Call{Prog: progNFS, Vers: 3, Proc: 1},
		vfs.ProtocolNFSv3,
		vfs.NFSv3Caps,
	},
	{
		"MOUNT MNT",
		nfsclient.Call{Prog: progMount, Vers: 3, Proc: 1},
		vfs.ProtocolNFSv3,
		vfs.MountCaps,
	},
	{
		"MOUNT EXPORT",
		nfsclient.Call{Prog: progMount, Vers: 3, Proc: 5},
		vfs.ProtocolNFSv3,
		vfs.MountCaps,
	},
	{
		"MOUNT UMNT",
		nfsclient.Call{Prog: progMount, Vers: 3, Proc: 3},
		vfs.ProtocolNFSv3,
		vfs.NFSv3Caps,
	},
	{
		"NLM TEST",
		nfsclient.Call{Prog: progNLM, Vers: 4, Proc: 1, Args: words(0)},
		vfs.ProtocolNLM,
		vfs.NLMCaps,
	},
	{
		"NSM SM_NOTIFY",
		nfsclient.Call{Prog: progNSM, Vers: 1, Proc: 6},
		vfs.ProtocolNLM,
		vfs.NLMCaps,
	},
	{
		"a COMPOUND's member",
		nfsclient.Call{Prog: progNFS, Vers: 4, Proc: 1, Args: words(0, 0, 1, opAccess)},
		vfs.ProtocolNFSv4, vfs.NFSv4Caps,
	},
}

// Each call's context carries the label of its protocol: NFSv3 and MOUNT
// NFSv3, NLM and NSM NLM, whose metric label is nfs3 as NFSv3's, and a
// COMPOUND's member NFSv4. The value is compared, not its label, which NFSv3
// and NLM share.
func TestEachCallCarriesItsProtocolLabel(t *testing.T) {
	rc, addr := recording(t)

	c := client(t, addr)
	for _, tc := range calls {
		send(t, c, tc.call)

		got := rc.take()
		if len(got) != 1 || !got[0].label || got[0].proto != tc.proto {
			t.Errorf(
				"%s: the handler's context carries %+v, want the label %v",
				tc.name,
				got,
				tc.proto,
			)
		}
	}
}

// Each call states the profile of its program and procedure, beside its
// Identity: NLM's for NLM
// and NSM, MOUNT's for MNT and EXPORT, NFSv3's for MOUNT's other procedures and
// for NFSv3, NFSv4's for a COMPOUND's member.
func TestEachCallStatesItsProgramsProfile(t *testing.T) {
	rc, addr := recording(t)
	c := client(t, addr)

	for _, tc := range calls {
		tc.call.Cred = nfsclient.AuthSys(7, 8)
		send(t, c, tc.call)

		got := rc.take()
		if len(got) != 1 || got[0].call.Caps != tc.caps {
			t.Errorf("%s: the handler's call states %+v, want %q", tc.name, got, tc.caps)
		}
		// A profile never comes without the Identity beside it.
		if len(got) == 1 && got[0].call.Identity.Proof != vfs.ProofClaimed {
			t.Errorf("%s: the call carries the proof %v beside its profile, want a claim",
				tc.name, got[0].call.Identity.Proof)
		}
	}
}
