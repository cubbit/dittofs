package nfs

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsclient"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The numbers the loopback rows read, from RFC 5531 section 9.2 and RFC 8881
// section 16.2.
const (
	authBadVerf = 3 // AUTH_BADVERF
	authDH      = 3 // the AUTH_DH flavour, which no server serves
	unknownAuth = 99
	opAccess    = 3 // OP_ACCESS, a code no member row holds yet
	rpcMismatch = 0 // the reject_stat RPC_MISMATCH
)

// mutatingRow is a table's one row, NULL: a handler whose one call is a Create
// on the service, so the Mutations record holds one entry for each call that
// reached it and none for a call the stages refused. The service's own answer
// is not the row's concern, only that the call was made.
func mutatingRow(mem *memory.Service) *core.Op {
	return core.Row[void, void](0, "null", core.NeedsNothing,
		func(ctx context.Context, r *core.Request, _ void) (void, error) {
			_, _ = mem.Create(
				ctx,
				r.Call,
				mem.Store.Root,
				[]byte("f"),
				vfs.CreateArgs{Kind: vfs.CreateRegular},
			)

			return void{}, nil
		})
}

// mutating serves the default tables but for NFSv3's, whose NULL is the
// mutating row, and NFSv4's COMPOUND, whose members gain it at code opAccess,
// over the range versions; it returns the service whose Mutations the row
// fills and the address to dial.
func mutating(t *testing.T, versions settings.VersionRange) (*memory.Service, netip.AddrPort) {
	t.Helper()
	d := leakChecked(t)
	mem := d.VFS.(*memory.Service)
	ts := tables(d)
	nfs3 := mutatingRow(mem)
	nfs3.Versions = core.Range{First: 3, Last: 3} // NFSv3's request carries version 3
	v3 := core.NewOpTable(ts.V3.Errors, false, nfs3)
	v3.Protocol = "nfs3"

	members := ts.V4.Ops[1].Members
	if _, taken := members.Ops[opAccess]; taken {
		t.Fatalf("the members' table holds a row at %d: pick a code no row holds", opAccess)
	}

	row := mutatingRow(mem)
	row.Code, row.Name = opAccess, "access"
	members.Ops[opAccess] = row
	ts.V3 = v3

	srv, err := core.NewServer(d, nfsSettings(versions), core.Listener{
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

	return mem, srv.Addrs()[adapter.Socket{Service: "nfs", Network: "tcp"}][0]
}

// within is a context that ends a few seconds from now: a hang guard for a
// call or a read, so a server that stops answering fails the test with the
// call's own message instead of holding the package to the test timeout.
func within(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// client dials addr with the scripted client, closed when the test ends.
func client(t *testing.T, addr netip.AddrPort) *nfsclient.Client {
	t.Helper()

	c, err := nfsclient.Dial(within(t), addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = c.Close()
	})

	return c
}

// expectReply fails t unless the call c sends under xid is answered want, to
// which it gives xid.
func expectReply(
	t *testing.T,
	name string,
	c *nfsclient.Client,
	xid uint32,
	call nfsclient.Call,
	want nfsclient.Reply,
) {
	t.Helper()

	call.XID, want.XID = &xid, xid

	got, err := c.Do(within(t), call)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: answered %+v, want %+v", name, got, want)
	}
}

// ids is an AUTH_SYS body's fixed part, from its stamp to the group count:
// uid 1000, gid 1000 (RFC 5531 Appendix A).
func ids() []byte {
	return words(0, 0, 1000, 1000)
}

// refusedCredentials are RFC 21 section 13's five: a body cut inside a whole
// credential, one of 17 groups, one with a 256-byte machine name, AUTH_DH and
// a flavour no one defines.
func refusedCredentials() map[string]nfsclient.Auth {
	name256 := append(words(0, 256), make([]byte, 256)...)

	return map[string]nfsclient.Auth{
		"a truncated AUTH_SYS": {Flavour: authSys, Body: words(0, 0, 1000)},
		"17 groups": {
			Flavour: authSys,
			Body:    append(append(ids(), words(17)...), make([]byte, 17*4)...),
		},
		"a 256-byte name": {
			Flavour: authSys,
			Body:    append(name256, words(1000, 1000, 0)...),
		},
		"AUTH_DH":            {Flavour: authDH, Body: make([]byte, 8)},
		"an unknown flavour": {Flavour: unknownAuth},
	}
}

// Each of RFC 21 section 13's refused credentials, sent to a row whose one
// call is a Create, is answered AUTH_BADCRED and leaves the Mutations record
// empty: none is served as anonymous. The same call under AUTH_NONE reaches
// the row and leaves one entry, so an empty record is not an unreachable row.
func TestTheRefusedCredentialsRunNothing(t *testing.T) {
	mem, addr := mutating(t, allVersions)
	c := client(t, addr)

	xid := uint32(100)
	for name, cred := range refusedCredentials() {
		xid++
		expectReply(t, name, c, xid, nfsclient.Call{Prog: progNFS, Vers: 3, Cred: cred},
			nfsclient.Reply{Denied: true, Stat: authBadCred})

		if ops := mem.Ops(); len(ops) != 0 {
			t.Errorf("%s: the service served %v, want nothing", name, ops)
		}
	}

	expectReply(t, "AUTH_NONE", c, 200, nfsclient.Call{Prog: progNFS, Vers: 3}, nfsclient.Reply{})

	if ops := mem.Ops(); !reflect.DeepEqual(ops, []vfs.Op{vfs.OpCreate}) {
		t.Errorf("an AUTH_NONE call made %v, want one Create", ops)
	}
}

// compound is a COMPOUND of minor version 0 whose one member is code, with no
// arguments of its own (RFC 7530 section 16.2): its tag of no bytes, the minor
// version, the count and the member's code.
func compound(code uint32) []byte {
	return words(0, 0, 1, code)
}

// The refusals of a call's header, over a socket, run nothing and are answered
// as RFC 5531 and RFC 21 section 11.5 say, whichever the range: under the
// default range, RPC version 3, a credential of 401 bytes, an AUTH_SYS
// verifier and NFS version 2; under 4.x alone, NFS version 3 and MOUNT, the
// range read from the settings and not the connection. Each fixture then makes
// one valid call to the row that mutates, which leaves one entry.
func TestTheHeaderRefusalsOverLoopback(t *testing.T) {
	t.Run("the default range", func(t *testing.T) {
		mem, addr := mutating(t, allVersions)

		c := client(t, addr)
		if err := c.Send(words(300, 0, 3, progNFS, 3, 0, 0, 0, 0, 0)); err != nil { // RPC version 3
			t.Fatal(err)
		}

		if got, err := c.Read(within(t)); err != nil ||
			!bytes.Equal(got, words(300, 1, 1, rpcMismatch, 2, 2)) {
			t.Errorf("RPC version 3 answered %x, %v, want RPC_MISMATCH 2 to 2", got, err)
		}

		for i, tc := range []struct {
			name string
			call nfsclient.Call
			want nfsclient.Reply
		}{
			{"a credential of 401 bytes", nfsclient.Call{
				Prog: progNFS, Vers: 3,
				Cred: nfsclient.Auth{Flavour: authSys, Body: make([]byte, 401)},
			}, nfsclient.Reply{Stat: garbageArgs}},
			{"an AUTH_SYS verifier", nfsclient.Call{
				Prog: progNFS, Vers: 3, Verf: nfsclient.Auth{Flavour: authSys},
			}, nfsclient.Reply{Denied: true, Stat: authBadVerf}},
			{
				"NFS version 2",
				nfsclient.Call{Prog: progNFS, Vers: 2},
				nfsclient.Reply{Stat: progMismatch, Low: 3, High: 4},
			},
		} {
			expectReply(t, tc.name, c, uint32(301+i), tc.call, tc.want)
		}

		if ops := mem.Ops(); len(ops) != 0 {
			t.Errorf("the refused calls made %v, want nothing", ops)
		}

		expectReply(
			t,
			"a valid NFS v3 call",
			c,
			310,
			nfsclient.Call{Prog: progNFS, Vers: 3},
			nfsclient.Reply{},
		)

		if ops := mem.Ops(); !reflect.DeepEqual(ops, []vfs.Op{vfs.OpCreate}) {
			t.Errorf("the valid call made %v, want one Create", ops)
		}
	})
	t.Run("the 4.x range", func(t *testing.T) {
		mem, addr := mutating(t, only4)
		c := client(t, addr)
		expectReply(t, "NFS version 3", c, 320, nfsclient.Call{Prog: progNFS, Vers: 3},
			nfsclient.Reply{Stat: progMismatch, Low: 4, High: 4})
		expectReply(
			t,
			"MOUNT",
			c,
			321,
			nfsclient.Call{Prog: progMount, Vers: 3},
			nfsclient.Reply{Stat: progUnavail},
		)

		if ops := mem.Ops(); len(ops) != 0 {
			t.Errorf("the refused calls made %v, want nothing", ops)
		}

		expectReply(t, "a valid COMPOUND", c, 322,
			nfsclient.Call{Prog: progNFS, Vers: 4, Proc: 1, Args: compound(opAccess)},
			nfsclient.Reply{Body: words(0, 0, 1, opAccess, 0)})

		if ops := mem.Ops(); !reflect.DeepEqual(ops, []vfs.Op{vfs.OpCreate}) {
			t.Errorf("the valid COMPOUND made %v, want one Create", ops)
		}
	})
}

// A record of seven bytes, too short to hold a transaction ID and a message
// type, closes the connection with nothing answered: the client's Read ends
// with io.EOF (RFC 21 section 11.5). It guards core's close of a connection
// whose record does not decode a header (the drop in pipeline.go's serve) and
// the client's reading of that close; the stage's own refusal is held by
// TestTheNonCalls.
func TestASevenByteRecordCloses(t *testing.T) {
	_, addr := mutating(t, allVersions)

	c := client(t, addr)
	if err := c.Send(make([]byte, 7)); err != nil {
		t.Fatal(err)
	}

	if got, err := c.Read(within(t)); err != io.EOF {
		t.Errorf("Read answered %x, %v, want io.EOF", got, err)
	}
}

// A call written as two fragments by Write is answered as the same call sent
// whole by Send: the client adds no record mark of its own to what it writes.
func TestTwoFragmentsAnswerAsOne(t *testing.T) {
	_, addr := mutating(t, allVersions)
	c := client(t, addr)

	null := call(7, progNFS, 3, 0, none)
	if err := c.Send(null); err != nil {
		t.Fatal(err)
	}

	whole, err := c.Read(within(t))
	if err != nil {
		t.Fatalf("Read of the whole call: %v", err)
	}

	if err := c.Write(fragments(null, 2)); err != nil {
		t.Fatal(err)
	}

	split, err := c.Read(within(t))
	if err != nil {
		t.Fatalf("Read of the fragmented call: %v", err)
	}

	if want := accepted(7, 0); !bytes.Equal(whole, want) || !bytes.Equal(split, whole) {
		t.Errorf("whole answered %x, in two fragments %x, want both %x", whole, split, want)
	}
}
