package nfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
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
		}, Network: "tcp", Protocol: protocol.New(ts),
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

// The numbers the tests below read, from RFC 5531 section 9, RFC 7530
// section 13 and RFC 8881 sections 15.1.2.4 and 15.1.2.5.
const (
	serverFault = 10006 // NFS3ERR_SERVERFAULT and NFS4ERR_SERVERFAULT
	nfs4Moved   = 10019 // NFS4ERR_MOVED
	nfs4NoFH    = 10020 // NFS4ERR_NOFILEHANDLE
	authTooWeak = 5     // AUTH_TOOWEAK
)

// Each row of these tables answers through the running server from a table row
// returning its sentinel, so no service call runs and the sentinels reach the
// table's rows as they are: this table holds nothing pending that core could
// swap them against (one mutating call per handler, and none here). The
// refused flavour and the four statuses the row answers at its table's fault
// or at AUTH_TOOWEAK are answered on one connection;
// ErrMoved and ErrDraining answer nothing; and NFSv4's members answer inside
// a COMPOUND. On the version 4 table's procedures 2 to 7, whose result has no
// place for a status, the four fault statuses drop out of the reply, so only
// the unmapped counter guards their rows (RFC 5531 section 9;
// RFC 8881 section 15; RFC 21 sections 10 and 11.5).
func TestEachNewRowAnswersThroughTheServer(t *testing.T) {
	d := leakChecked(t)
	mem := d.VFS.(*memory.Service)
	ts := tables(d)

	// refusals is one row per sentinel at code on, each named for its
	// answer, unique in the table it lands in, its handler returning the
	// sentinel itself.
	type refusal struct {
		name string
		err  error
	}
	// A row's name is unique over the server, so each list carries its
	// table's prefix.
	refusals := func(prefix string, code uint32, versions core.Range, rs []refusal) []*core.Op {
		rows := make([]*core.Op, len(rs))
		for i, f := range rs {
			r := core.Row[void, void](code+uint32(i), prefix+"_"+f.name, core.NeedsNothing,
				func(context.Context, *core.Request, void) (void, error) {
					return void{}, f.err
				})
			r.Versions = versions
			rows[i] = r
		}

		return rows
	}
	everyMinor := core.Range{First: 0, Last: 2} // as ops/v4's own rows name it

	// The version 3 family's rows: a refused flavour, the four at the fault
	// and the two that answer nothing, one program each.
	family := []refusal{
		{"wrong_security", vfs.ErrWrongSecurity},
		{"not_last", core.ErrNotLast},
		{"too_many_pending", core.ErrTooManyPending},
		{"cancelled", core.ErrCancelled},
		{"failed_logon", vfs.ErrLogonFailure},
		{"moved", core.ErrMoved},
		{"draining", core.ErrDraining},
	}
	v3 := core.NewOpTable(
		ts.V3.Errors,
		false,
		refusals("v3", 10, core.Range{First: 3, Last: 3}, family)...)
	v3.Protocol = "nfs3"
	mount := core.NewOpTable(
		ts.Mount.Errors,
		false,
		refusals("mount", 10, core.Range{First: 3, Last: 3}, family)...)
	mount.Protocol = "nfs3"
	nlm := core.NewOpTable(
		ts.NLM.Errors,
		false,
		refusals("nlm", 10, core.Range{First: 4, Last: 4}, family)...)
	nlm.Protocol = "nfs3"
	nsm := core.NewOpTable(
		ts.NSM.Errors,
		false,
		refusals("nsm", 10, core.Range{First: 1, Last: 1}, family)...)
	nsm.Protocol = "nfs3"
	ts.V3, ts.Mount, ts.NLM, ts.NSM = v3, mount, nlm, nsm

	// NFSv4's members: the four at the fault, ErrMoved over DELEGRETURN, whose
	// row answers NFS4ERR_MOVED, ErrMoved over DELEGPURGE, outside that row's
	// operation list, and ErrDraining, which answer nothing.
	members := ts.V4.Ops[1].Members
	for _, r := range refusals("member", 3, everyMinor, []refusal{
		{"not_last", core.ErrNotLast},
		{"too_many_pending", core.ErrTooManyPending},
		{"cancelled", core.ErrCancelled},
		{"failed_logon", vfs.ErrLogonFailure},
		{"moved_over_delegpurge", core.ErrMoved},
		{"moved_over_delegreturn", core.ErrMoved},
		{"draining", core.ErrDraining},
	}) {
		if _, taken := members.Ops[r.Code]; taken {
			t.Fatalf("the members' table holds a row at %d: pick a code no row holds", r.Code)
		}

		members.Ops[r.Code] = r
	}

	// The version 4 table's procedures 2 to 7, reached as calls apart from COMPOUND.
	for _, r := range refusals("proc", 2, everyMinor, []refusal{
		{"not_last", core.ErrNotLast},
		{"too_many_pending", core.ErrTooManyPending},
		{"cancelled", core.ErrCancelled},
		{"failed_logon", vfs.ErrLogonFailure},
		{"moved", core.ErrMoved},
		{"draining", core.ErrDraining},
	}) {
		if _, taken := ts.V4.Ops[r.Code]; taken {
			t.Fatalf("the version 4 table holds a row at %d: pick a code no row holds", r.Code)
		}

		ts.V4.Ops[r.Code] = r
	}

	srv, err := core.NewServer(d, nfsSettings(allVersions), core.Listener{
		Services: []string{
			"nfs",
			"mount",
			"nlm",
			"nsm",
		}, Network: "tcp", Protocol: protocol.New(ts),
	})
	if err != nil {
		t.Fatal(err)
	}

	start(t, srv)

	c := dial(t, srv)
	for _, tc := range []struct {
		name       string
		call, want []byte
	}{
		{
			"NFSv3's refused flavour", call(10, progNFS, 3, 10, none),
			words(10, 1, 1, 1, authTooWeak),
		},
		{"NFSv3's not last", call(11, progNFS, 3, 11, none), accepted(11, 0, serverFault)},
		{"NFSv3's too many pending", call(12, progNFS, 3, 12, none), accepted(12, 0, serverFault)},
		{"NFSv3's cancelled", call(13, progNFS, 3, 13, none), accepted(13, 0, serverFault)},
		{"NFSv3's failed logon", call(14, progNFS, 3, 14, none), accepted(14, 0, serverFault)},
		{
			"MOUNT's refused flavour", call(20, progMount, 3, 10, none),
			words(20, 1, 1, 1, authTooWeak),
		},
		{"MOUNT's not last", call(21, progMount, 3, 11, none), accepted(21, systemErr)},
		{"MOUNT's too many pending", call(22, progMount, 3, 12, none), accepted(22, systemErr)},
		{"MOUNT's cancelled", call(23, progMount, 3, 13, none), accepted(23, systemErr)},
		{"MOUNT's failed logon", call(24, progMount, 3, 14, none), accepted(24, systemErr)},
		{"NLM's refused flavour", call(30, progNLM, 4, 10, none), words(30, 1, 1, 1, authTooWeak)},
		{"NLM's not last", call(31, progNLM, 4, 11, none), accepted(31, systemErr)},
		{"NLM's too many pending", call(32, progNLM, 4, 12, none), accepted(32, systemErr)},
		{"NLM's cancelled", call(33, progNLM, 4, 13, none), accepted(33, systemErr)},
		{"NLM's failed logon", call(34, progNLM, 4, 14, none), accepted(34, systemErr)},
		{"NSM's refused flavour", call(40, progNSM, 1, 10, none), words(40, 1, 1, 1, authTooWeak)},
		{"NSM's not last", call(41, progNSM, 1, 11, none), accepted(41, systemErr)},
		{"NSM's too many pending", call(42, progNSM, 1, 12, none), accepted(42, systemErr)},
		{"NSM's cancelled", call(43, progNSM, 1, 13, none), accepted(43, systemErr)},
		{"NSM's failed logon", call(44, progNSM, 1, 14, none), accepted(44, systemErr)},
		{
			"an NFSv4 member's not last", call(50, progNFS, 4, 1, none, compound(3)...),
			accepted(50, 0, serverFault, 0, 1, 3, serverFault),
		},
		{
			"an NFSv4 member's too many pending", call(51, progNFS, 4, 1, none, compound(4)...),
			accepted(51, 0, serverFault, 0, 1, 4, serverFault),
		},
		{
			"an NFSv4 member's cancelled", call(52, progNFS, 4, 1, none, compound(5)...),
			accepted(52, 0, serverFault, 0, 1, 5, serverFault),
		},
		{
			"an NFSv4 member's failed logon", call(53, progNFS, 4, 1, none, compound(6)...),
			accepted(53, 0, serverFault, 0, 1, 6, serverFault),
		},
		{
			"an NFSv4 member's moved over DELEGRETURN",
			call(54, progNFS, 4, 1, none, compound(8)...),
			accepted(54, 0, nfs4Moved, 0, 1, 8, nfs4Moved),
		},
		// Procedure 2's result has no place for a status, so the header's
		// accepted answer closes the reply: the fault rows are guarded by the
		// unmapped counter.
		{
			"NFSv4's procedure 2 not last, its status dropped",
			call(60, progNFS, 4, 2, none), accepted(60, 0),
		},
		{
			"NFSv4's procedure 3 too many pending, its status dropped",
			call(61, progNFS, 4, 3, none), accepted(61, 0),
		},
		{
			"NFSv4's procedure 4 cancelled, its status dropped",
			call(62, progNFS, 4, 4, none), accepted(62, 0),
		},
		{
			"NFSv4's procedure 5 failed logon, its status dropped",
			call(63, progNFS, 4, 5, none), accepted(63, 0),
		},
	} {
		if got := ask(t, c, tc.call); !bytes.Equal(got, tc.want) {
			t.Errorf("%s: answered %x, want %x", tc.name, got, tc.want)
		}
	}

	// The calls that answer nothing, each on its own connection, which answers
	// the refused flavour after its silence. No clock can be injected into a
	// TCP read, so the read's deadline is the assertion that nothing is
	// answered.
	for _, tc := range []struct {
		name string
		call []byte
	}{
		{"NFSv3's moved", call(70, progNFS, 3, 15, none)},
		{"NFSv3's draining", call(71, progNFS, 3, 16, none)},
		{"MOUNT's moved", call(72, progMount, 3, 15, none)},
		{"MOUNT's draining", call(73, progMount, 3, 16, none)},
		{"NLM's moved", call(74, progNLM, 4, 15, none)},
		{"NLM's draining", call(75, progNLM, 4, 16, none)},
		{"NSM's moved", call(76, progNSM, 1, 15, none)},
		{"NSM's draining", call(77, progNSM, 1, 16, none)},
		{"an NFSv4 member's moved over DELEGPURGE", call(78, progNFS, 4, 1, none, compound(7)...)},
		{"an NFSv4 member's draining", call(79, progNFS, 4, 1, none, compound(9)...)},
		{"NFSv4's procedure 6 moved", call(80, progNFS, 4, 6, none)},
		{"NFSv4's procedure 7 draining", call(81, progNFS, 4, 7, none)},
	} {
		silent := dial(t, srv)

		if _, err := silent.Write(fragments(tc.call, 1)); err != nil {
			t.Fatal(err)
		}

		if err := silent.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}

		var b [256]byte

		if n, err := silent.Read(b[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s: %d bytes %v, want nothing", tc.name, n, err)
			continue // the connection is derailed by the answer itself
		}

		if err := silent.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}

		got := ask(t, silent, call(82, progNFS, 3, 10, none))

		want := words(82, 1, 1, 1, authTooWeak)

		if !bytes.Equal(got, want) {
			t.Errorf("%s: answered %x after its silence, want %x", tc.name, got, want)
		}
	}

	for _, label := range []string{"nfs3", "nfs4"} {
		if got := recorded(t, d.Metrics, "adapter_unmapped_errors_total", label); got != 0 {
			t.Errorf("%v unmapped errors under %s, want none", got, label)
		}
	}

	if ops := mem.Ops(); len(ops) != 0 {
		t.Errorf("the refusal rows called the service %d times, want none", len(ops))
	}
}

// A COMPOUND whose member needs the current filehandle answers
// NFS4ERR_NOFILEHANDLE in the member's result and as the COMPOUND's status,
// the tag echoed, the call the hand-written vector rfc-nfs4-nofh-call (RFC
// 8881 sections 15.1.2.5 and 18.1; RFC 21 section 11.5). The row maps the
// refusal without reaching the service, so nothing is counted unmapped.
func TestACompoundWithNoFilehandleAnswersNoFileHandle(t *testing.T) {
	d := leakChecked(t)
	mem := d.VFS.(*memory.Service)
	ts := tables(d)
	members := ts.V4.Ops[1].Members

	if _, taken := members.Ops[opAccess]; taken {
		t.Fatalf("the members' table holds a row at %d: pick a code no row holds", opAccess)
	}

	row := core.Row[void, void](
		opAccess, "access", core.NeedsFH,
		func(context.Context, *core.Request, void) (void, error) {
			return void{}, nil
		},
	)
	row.Versions = core.Range{First: 0, Last: 2}
	members.Ops[opAccess] = row

	srv, err := core.NewServer(d, nfsSettings(allVersions), core.Listener{
		Services: []string{
			"nfs",
			"mount",
			"nlm",
			"nsm",
		}, Network: "tcp", Protocol: protocol.New(ts),
	})
	if err != nil {
		t.Fatal(err)
	}

	start(t, srv)

	tag := opaque([]byte("abcde"))

	want := append(append(accepted(0x30, 0, nfs4NoFH), tag...), words(1, opAccess, nfs4NoFH)...)

	if got := ask(t, dial(t, srv), golden(t, "rfc-nfs4-nofh-call")); !bytes.Equal(got, want) {
		t.Errorf("the COMPOUND answered %x, want %x", got, want)
	}

	if got := recorded(t, d.Metrics, "adapter_unmapped_errors_total", "nfs4"); got != 0 {
		t.Errorf("%v unmapped errors under nfs4, want none", got)
	}

	if ops := mem.Ops(); len(ops) != 0 {
		t.Errorf("the row called the service %d times, want none", len(ops))
	}
}
