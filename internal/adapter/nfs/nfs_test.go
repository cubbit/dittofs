package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The numbers the tests write and read, written here from the specifications
// rather than taken from the wire package: RFC 5531 section 9 and Appendix A,
// RFC 1813, RFC 7530 and RFC 21 section 11.5.
const (
	progNFS   = 100003
	progMount = 100005
	progNLM   = 100021
	progNSM   = 100024

	authNone = 0
	authSys  = 1

	progUnavail  = 1 // PROG_UNAVAIL
	progMismatch = 2 // PROG_MISMATCH
	procUnavail  = 3 // PROC_UNAVAIL
	garbageArgs  = 4 // GARBAGE_ARGS
	systemErr    = 5 // SYSTEM_ERR
	authBadCred  = 1 // AUTH_BADCRED

	nfs4ErrMinorVersMismatch = 10021 // NFS4ERR_MINOR_VERS_MISMATCH

	lastFragment = 1 << 31 // a record mark's last-fragment bit (RFC 5531 section 11)
	maxRecord    = 1<<20 + 4<<10
)

// words is XDR's encoding of each: four bytes, big-endian (RFC 4506 section
// 4.1).
func words(ws ...uint32) []byte {
	var b []byte
	for _, w := range ws {
		b = binary.BigEndian.AppendUint32(b, w)
	}
	return b
}

// opaque is XDR's variable-length opaque: its length, its bytes and their
// padding to four (RFC 4506 section 4.10).
func opaque(body []byte) []byte {
	b := append(words(uint32(len(body))), body...)
	return append(b, make([]byte, (4-len(body)%4)%4)...)
}

// none is an empty AUTH_NONE credential or verifier.
var none = words(authNone, 0)

// call is an RPC call (RFC 5531 section 9): xid, CALL, RPC version 2, its
// program, version and procedure, the credential cred, an empty AUTH_NONE
// verifier, then args.
func call(xid, prog, vers, proc uint32, cred []byte, args ...byte) []byte {
	b := append(words(xid, 0, 2, prog, vers, proc), cred...)
	return append(append(b, none...), args...)
}

// accepted is an accepted reply to xid: REPLY, MSG_ACCEPTED, an empty AUTH_NONE
// verifier, and then the accept state and what follows it.
func accepted(xid uint32, rest ...uint32) []byte {
	return append(words(xid, 1, 0, authNone, 0), words(rest...)...)
}

// fragments frames body as a record of n fragments, its bytes split as evenly
// as they go, the last one's mark alone carrying the last-fragment bit (RFC
// 5531 section 11).
func fragments(body []byte, n int) []byte {
	var b []byte
	for i := range n {
		piece := body[len(body)*i/n : len(body)*(i+1)/n]
		mark := uint32(len(piece))
		if i == n-1 {
			mark |= lastFragment
		}
		b = append(append(b, words(mark)...), piece...)
	}
	return b
}

// reply reads one reply record and returns its body, failing the test unless
// it is one fragment whose mark carries its length and the last-fragment bit.
func reply(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		t.Fatalf("reading a reply's record mark: %v", err)
	}
	m := binary.BigEndian.Uint32(mark[:])
	if m&lastFragment == 0 {
		t.Fatalf("a reply's record mark %#x lacks the last-fragment bit", m)
	}
	body := make([]byte, m&^lastFragment)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("reading a reply of %d bytes: %v", len(body), err)
	}
	return body
}

// closed reports whether the server closed c rather than answer: a read that
// ends with no byte and an error other than its deadline.
func closed(t *testing.T, c net.Conn) bool {
	t.Helper()
	var b [1]byte
	n, err := c.Read(b[:])
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("neither answered nor closed before the read's deadline")
	}
	return n == 0 && err != nil
}

// nfsSettings is the default NFS settings serving versions, on 127.0.0.1 at a
// port the system picks.
func nfsSettings(versions settings.VersionRange) settings.NFS {
	s := settings.DefaultNFS()
	s.Versions = versions
	s.Port = 0
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

var (
	allVersions = settings.VersionRange{Min: "3", Max: "4.2"}
	only4       = settings.VersionRange{Min: "4.0", Max: "4.2"}
)

// leakChecked is an adapter's Deps over the filesystem over memory, real
// budgets and a registry of its own, its leak check armed: called before the
// test starts anything.
func leakChecked(t *testing.T) adapter.Deps {
	t.Helper()
	b := &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
		Conns:      resources.NewConnLimit(1024),
	}
	adaptertest.CheckLeaks(t, b)
	return adapter.Deps{VFS: memory.New(t), Budgets: b, Metrics: metrics.New("test", "test")}
}

// start starts srv, stopping it when the test ends, before its leak check.
func start(t *testing.T, srv *core.Server) {
	t.Helper()
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

// serve builds the adapter with New and starts it.
func serve(t *testing.T, d adapter.Deps, s settings.NFS) *core.Server {
	t.Helper()
	srv, err := New(d, s)
	if err != nil {
		t.Fatal(err)
	}
	start(t, srv)
	return srv
}

// dial opens a connection to srv's TCP listener, closed when the test ends;
// its deadline is a backstop for a server that neither answers nor closes.
func dial(t *testing.T, srv *core.Server) net.Conn {
	t.Helper()
	addrs := srv.Addrs()[adapter.Socket{Service: "nfs", Network: "tcp"}]
	if len(addrs) == 0 {
		t.Fatal("no address serves nfs over tcp")
	}
	c, err := net.Dial("tcp", addrs[0].String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

// ask sends body as one record on c and returns the reply's body.
func ask(t *testing.T, c net.Conn, body []byte) []byte {
	t.Helper()
	if _, err := c.Write(fragments(body, 1)); err != nil {
		t.Fatal(err)
	}
	return reply(t, c)
}

// recorded is the sum of name's series core records under protocol: a
// counter's value, a histogram's count (RFC 20 section 10.1).
func recorded(t *testing.T, m *metrics.Registry, name, protocol string) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != "dittofs_"+name {
			continue
		}
		for _, mt := range mf.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "protocol" && lp.GetValue() == protocol {
					sum += mt.GetCounter().GetValue() + float64(mt.GetHistogram().GetSampleCount())
				}
			}
		}
	}
	return sum
}

// New opens nothing; Start binds one TCP listener at a port the system picks,
// which Addrs reports for nfs, mount, nlm and nsm alike, and nothing over UDP
// (RFC 20 section 3.6, RFC 21 section 2.2).
func TestStartBindsOneTCPPortForEveryProgram(t *testing.T) {
	d := leakChecked(t)
	srv, err := New(d, nfsSettings(allVersions))
	if err != nil {
		t.Fatal(err)
	}
	if st := srv.Health().State; st != adapter.StateConfigured {
		t.Errorf("New's adapter is %s, want configured", st)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("New's adapter reports %v, want nothing bound", got)
	}
	start(t, srv)
	got := srv.Addrs()
	var ports []uint16
	for _, svc := range []string{"nfs", "mount", "nlm", "nsm"} {
		addrs := got[adapter.Socket{Service: svc, Network: "tcp"}]
		if len(addrs) != 1 || addrs[0].Addr() != netip.MustParseAddr("127.0.0.1") {
			t.Errorf("%s over tcp is at %v, want one address on 127.0.0.1", svc, addrs)
			continue
		}
		ports = append(ports, addrs[0].Port())
	}
	if p := slices.Compact(ports); len(p) != 1 || p[0] == 0 || p[0] == 2049 || p[0] == 12049 {
		t.Errorf("the programs are at ports %v, want one the system picked", ports)
	}
	for k := range got {
		if k.Network != "tcp" {
			t.Errorf("Addrs reports %+v, want tcp alone", k)
		}
	}
}

// Under the default range, each path a refusal takes is answered over one
// connection, the call after a malformed one too, and counts under the label
// of the table that answered it, or nfs3 before Route (RFC 20 sections 4.8
// and 10.1, RFC 21 sections 2.1, 11.1 and 11.5).
func TestEachRefusalIsAnswered(t *testing.T) {
	d := leakChecked(t)
	c := dial(t, serve(t, d, nfsSettings(allVersions)))
	sys := append(words(0), opaque(nil)...) // stamp, machine name
	sys = append(sys, words(0, 0, 17)...)   // uid, gid, 17 groups, one past RFC 5531's 16
	sys = append(sys, make([]byte, 17*4)...)
	tag := opaque([]byte("abcde"))
	for _, tc := range []struct {
		name, label string
		call, want  []byte
	}{
		{"MOUNT's NULL from its empty table", "nfs3",
			call(1, progMount, 3, 0, none), accepted(1, procUnavail)},
		{"NLM's NULL from its empty table", "nfs3",
			call(31, progNLM, 4, 0, none), accepted(31, procUnavail)},
		{"NSM's NULL from its empty table", "nfs3",
			call(32, progNSM, 1, 0, none), accepted(32, procUnavail)},
		{"NFS v2 from the stages' table", "nfs3",
			call(2, progNFS, 2, 0, none), accepted(2, progMismatch, 3, 4)},
		{"an AUTH_SYS credential of 17 groups", "nfs3",
			call(3, progNFS, 3, 0, append(words(authSys), opaque(sys)...)),
			words(3, 1, 1, 1, authBadCred)}, // REPLY, MSG_DENIED, AUTH_ERROR
		{"a COMPOUND of minor version 3 from the version 4 table", "nfs4",
			call(4, progNFS, 4, 1, none, append(tag, words(3, 0)...)...),                   // COMPOUND: tag, minor version, no operations
			append(append(accepted(4, 0, nfs4ErrMinorVersMismatch), tag...), words(0)...)}, // SUCCESS, the status, the tag, no results
		{"a verifier past the record", "nfs3",
			words(5, 0, 2, progNFS, 3, 0, authNone, 0, authNone, 8), accepted(5, garbageArgs)},
		{"a NULL after it", "nfs3",
			call(6, progNFS, 3, 0, none), accepted(6, procUnavail)},
	} {
		other := map[string]string{"nfs3": "nfs4", "nfs4": "nfs3"}[tc.label]
		before, otherBefore := recorded(t, d.Metrics, "adapter_refusals_total", tc.label), recorded(t, d.Metrics, "adapter_refusals_total", other)
		if got := ask(t, c, tc.call); !bytes.Equal(got, tc.want) {
			t.Errorf("%s: answered %x, want %x", tc.name, got, tc.want)
		}
		if got := recorded(t, d.Metrics, "adapter_refusals_total", tc.label) - before; got != 1 {
			t.Errorf("%s: %v refusals under %s, want 1", tc.name, got, tc.label)
		}
		if got := recorded(t, d.Metrics, "adapter_refusals_total", other) - otherBefore; got != 0 {
			t.Errorf("%s: %v refusals under %s, want none", tc.name, got, other)
		}
	}
}

// A record in fragments is read whole up to RFC 21 section 2.3's 64 fragments
// and 1 MiB and 4 KiB together, and answered; past either, the connection
// closes with nothing answered (RFC 20 section 4.7's Framer).
func TestFragmentsUpToTheCaps(t *testing.T) {
	d := leakChecked(t)
	srv := serve(t, d, nfsSettings(allVersions))
	null := call(7, progMount, 3, 0, none)
	padded := func(n int) []byte { return append(slices.Clone(null), make([]byte, n-len(null))...) }
	for _, tc := range []struct {
		name   string
		record []byte
		answer bool
	}{
		{"a NULL in 2 fragments", fragments(null, 2), true},
		{"a NULL in 64 fragments", fragments(null, 64), true},
		{"a NULL in 65 fragments", fragments(null, 65), false},
		{"a call of the cap in 2 fragments", fragments(padded(maxRecord), 2), true},
		// The second fragment's body is not sent: the reader closes at its mark.
		{"a call a byte past the cap in 2 fragments", fragments(padded(maxRecord+1), 2)[:4+(maxRecord+1)/2+4], false},
		{"a mark a byte past the cap", words(lastFragment | (maxRecord + 1)), false},
	} {
		c := dial(t, srv)
		if _, err := c.Write(tc.record); err != nil {
			t.Fatal(err)
		}
		if !tc.answer {
			if !closed(t, c) {
				t.Errorf("%s: answered, want the connection closed", tc.name)
			}
			continue
		}
		if got, want := reply(t, c), accepted(7, procUnavail); !bytes.Equal(got, want) {
			t.Errorf("%s: answered %x, want %x", tc.name, got, want)
		}
	}
}

// golden reads one of the wire package's golden records, hex with # comments.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("wire/testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return b
}

// A table of one row, NULL on version 4, its void arguments and result
// encoded by the generated codec, answers the reference server's capture
// byte for byte: its NULL call (frame 4) with its reply (frame 6), counted
// under nfs4. A procedure is its structs and one core.Row line.
func TestANullRowAnswersAsTheReferenceServer(t *testing.T) {
	d := leakChecked(t)
	v4 := core.NewOpTable(tables(d).V4.Errors, false,
		core.Row[void, void](0, "null", core.NeedsNothing,
			func(context.Context, *core.Request, void) (void, error) { return void{}, nil }))
	v4.Protocol = "nfs4"
	srv, err := core.NewServer(d, nfsSettings(allVersions), core.Listener{
		Services: []string{"nfs"}, Network: "tcp", Protocol: protocol.New(protocol.Tables{V4: v4}),
	})
	if err != nil {
		t.Fatal(err)
	}
	start(t, srv)
	c := dial(t, srv)
	frame4, frame6 := golden(t, "nfsv4-null-call"), golden(t, "nfsv4-null-reply")
	if _, err := c.Write(frame4); err != nil {
		t.Fatal(err)
	}
	if got := reply(t, c); !bytes.Equal(got, frame6[4:]) {
		t.Errorf("frame 4 answered %x, want frame 6's %x", got, frame6[4:])
	}
	if recorded(t, d.Metrics, "adapter_wire_seconds", "nfs4") == 0 {
		t.Error("the NULL recorded no stage under nfs4")
	}
}

// Of two connections accepted together, the one that sends nothing is closed
// at the handshake deadline, and the one whose first call was routed still
// answers after it (RFC 20 section 6.4). core's clock is set only from core's
// own tests, so the deadline passes in real time: the silent connection's
// close is the event the test waits on, its read's deadline the bound.
func TestTheFirstRoutedCallEndsTheHandshake(t *testing.T) {
	d := leakChecked(t)
	s := nfsSettings(allVersions)
	s.HandshakeTimeout = time.Second
	srv := serve(t, d, s)
	talker, silent := dial(t, srv), dial(t, srv)
	if got, want := ask(t, talker, call(8, progNFS, 3, 0, none)), accepted(8, procUnavail); !bytes.Equal(got, want) {
		t.Fatalf("the first NULL answered %x, want %x", got, want)
	}
	if !closed(t, silent) {
		t.Fatal("the silent connection answered something")
	}
	if got, want := ask(t, talker, call(9, progNFS, 3, 0, none)), accepted(9, procUnavail); !bytes.Equal(got, want) {
		t.Errorf("past the deadline a NULL answered %x, want %x", got, want)
	}
}

// A connection serves the range of the settings it was accepted with: one
// accepted before a Reconfigure keeps the range, one accepted after takes the
// new one, narrower or wider, from tables New built whatever the range (RFC 20
// section 4.8).
func TestAConnectionKeepsTheRangeItWasAcceptedWith(t *testing.T) {
	d := leakChecked(t)
	t.Run("narrowed", func(t *testing.T) {
		srv := serve(t, d, nfsSettings(allVersions))
		before := dial(t, srv)
		v2 := func(xid uint32) []byte { return call(xid, progNFS, 2, 0, none) }
		if got, want := ask(t, before, v2(10)), accepted(10, progMismatch, 3, 4); !bytes.Equal(got, want) {
			t.Fatalf("NFS v2 before answered %x, want %x", got, want)
		}
		if err := srv.Reconfigure(t.Context(), nfsSettings(only4)); err != nil {
			t.Fatal(err)
		}
		after := dial(t, srv)
		for _, tc := range []struct {
			name       string
			c          net.Conn
			call, want []byte
		}{
			{"MOUNT after", after, call(11, progMount, 3, 0, none), accepted(11, progUnavail)},
			{"NFS v3 after", after, call(12, progNFS, 3, 0, none), accepted(12, progMismatch, 4, 4)},
			{"NFS v2 before", before, v2(13), accepted(13, progMismatch, 3, 4)},
		} {
			if got := ask(t, tc.c, tc.call); !bytes.Equal(got, tc.want) {
				t.Errorf("%s: answered %x, want %x", tc.name, got, tc.want)
			}
		}
	})
	t.Run("widened", func(t *testing.T) {
		srv := serve(t, d, nfsSettings(only4))
		if err := srv.Reconfigure(t.Context(), nfsSettings(allVersions)); err != nil {
			t.Fatal(err)
		}
		if got, want := ask(t, dial(t, srv), call(14, progMount, 3, 0, none)), accepted(14, procUnavail); !bytes.Equal(got, want) {
			t.Errorf("MOUNT after answered %x, want %x", got, want)
		}
	})
}

// Two adapters from New each serve their own range, whichever of their
// connections was accepted last (RFC 20 section 4.7: nothing is looked up
// while serving).
func TestTwoAdaptersServeTheirOwnRanges(t *testing.T) {
	d := leakChecked(t)
	wide, narrow := serve(t, d, nfsSettings(allVersions)), serve(t, d, nfsSettings(only4))
	cw := dial(t, wide)
	for i, tc := range []struct {
		c         net.Conn
		low, high uint32
	}{{cw, 3, 4}, {dial(t, narrow), 4, 4}, {cw, 3, 4}} {
		xid := uint32(20 + i)
		if got, want := ask(t, tc.c, call(xid, progNFS, 2, 0, none)), accepted(xid, progMismatch, tc.low, tc.high); !bytes.Equal(got, want) {
			t.Errorf("call %d: NFS v2 answered %x, want %x", i, got, want)
		}
	}
}

// Each program's table maps what the loopback tests leave unreached, each
// row by minor version where NFSv4's differ: arguments that do not decode,
// which only a row decodes, an error no row names, which is its program's
// fault, a procedure NFSv4's table lacks, a COMPOUND past the cap or whose
// reply is too big, NFSv4.0 answering NFS4ERR_RESOURCE for both, and, in
// NFSv4's members' table, a member that does not decode, names a code no row
// serves or one its minor version does not define (RFC 21 section 11.5).
func TestEachTableMapsCoresRefusals(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)
	// accept is an RPC accept state's status, as the header stage encodes it:
	// 1 in the two high bits.
	accept := func(state uint32) core.Status { return core.Status(1<<30 | state) }
	// The nfsstat4 values, from RFC 7530 section 13 and RFC 8881 section 15.
	const (
		serverFault  = 10006 // NFS3ERR_SERVERFAULT and NFS4ERR_SERVERFAULT
		notSupp      = 10004 // NFS4ERR_NOTSUPP
		resource     = 10018 // NFS4ERR_RESOURCE
		badXDR       = 10036 // NFS4ERR_BADXDR
		opIllegal    = 10044 // NFS4ERR_OP_ILLEGAL
		repTooBig    = 10066 // NFS4ERR_REP_TOO_BIG
		tooManyOps   = 10070 // NFS4ERR_TOO_MANY_OPS
		compoundProc = 1
	)
	ts := tables(adapter.Deps{})
	members := ts.V4.Ops[compoundProc].Members
	other := errors.New("an error no row names")
	for _, tc := range []struct {
		name  string
		table *core.OpTable
		err   error
		minor core.Version
		want  core.Status
	}{
		{"NFSv3's decode", ts.V3, core.ErrDecode, 0, accept(garbageArgs)},
		{"NFSv3's fault", ts.V3, other, 0, serverFault},
		{"NFSv4's not supported", ts.V4, core.ErrNotSupported, 0, accept(procUnavail)},
		{"NFSv4's decode", ts.V4, core.ErrDecode, 1, accept(garbageArgs)},
		{"NFSv4's fault", ts.V4, other, 1, serverFault},
		{"NFSv4's minor version", ts.V4, core.ErrVersion, 3, nfs4ErrMinorVersMismatch},
		{"NFSv4.0's cap", ts.V4, core.ErrTooLong, 0, resource},
		{"NFSv4.1's cap", ts.V4, core.ErrTooLong, 1, tooManyOps},
		{"NFSv4.2's cap", ts.V4, core.ErrTooLong, 2, tooManyOps},
		{"NFSv4.0's reply too big", ts.V4, core.ErrReplyTooBig, 0, resource},
		{"NFSv4.1's reply too big", ts.V4, core.ErrReplyTooBig, 1, repTooBig},
		{"NFSv4.2's reply too big", ts.V4, core.ErrReplyTooBig, 2, repTooBig},
		{"an NFSv4.0 member's decode", members, core.ErrDecode, 0, badXDR},
		{"an NFSv4.2 member's decode", members, core.ErrDecode, 2, badXDR},
		{"an NFSv4 member no row serves", members, core.ErrNotSupported, 1, notSupp},
		{"an NFSv4 member its minor version lacks", members, core.ErrVersion, 0, opIllegal},
		{"an NFSv4 member's fault", members, other, 1, serverFault},
		{"MOUNT's decode", ts.Mount, core.ErrDecode, 0, accept(garbageArgs)},
		{"MOUNT's fault", ts.Mount, other, 0, accept(systemErr)},
		{"NLM's fault", ts.NLM, other, 0, accept(systemErr)},
		{"NSM's fault", ts.NSM, other, 0, accept(systemErr)},
	} {
		if got, _, _ := tc.table.Errors.Map(tc.err, nil, tc.minor); got != tc.want {
			t.Errorf("%s: %#x, want %#x", tc.name, uint32(got), uint32(tc.want))
		}
	}
}
