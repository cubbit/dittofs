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
	"github.com/cubbit/dittofs/pkg/vfs"
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
// and 10.1, RFC 21 sections 2.1, 11.1 and 11.5). Each program's NULL is a
// call a row serves now, against the captured and hand-written reply files:
// an accepted success, an empty AUTH_NONE verifier, no result, no refusal of
// core's counted, under AUTH_NONE and under AUTH_SYS, and the service's
// recorded calls empty (RFC 5531 section 9, RFC 21 section 11.5); a NULL row's
// handler holds no service today, so that last check guards a later change. NFSv4's COMPOUNDs,
// against the reply files written from RFC 8881 and RFC 7530, echo their tag
// in every reply that carries their status: an empty one served, an
// undefined operation code answered NFS4ERR_OP_ILLEGAL at it, at the cap
// too, one past the cap refused NFS4ERR_TOO_MANY_OPS and over NFSv4.0, on
// the call after, NFS4ERR_RESOURCE, a member that does not decode
// NFS4ERR_BADXDR, a defined code no row serves NFS4ERR_NOTSUPP, and a
// COMPOUND whose own arguments do not decode GARBAGE_ARGS. A call a row
// serves counts no refusal of core's, but records under its label.
func TestEachRefusalIsAnswered(t *testing.T) {
	d := leakChecked(t)
	c := dial(t, serve(t, d, nfsSettings(allVersions)))
	sys := append(words(0), opaque(nil)...) // stamp, machine name
	sys = append(sys, words(0, 0, 17)...)   // uid, gid, 17 groups, one past RFC 5531's 16
	sys = append(sys, make([]byte, 17*4)...)
	inRule := words(1000, 100, 0) // uid, gid, no groups, within RFC 5531 Appendix A
	inRuleCred := append(
		words(authSys),
		opaque(append(append(words(0), opaque(nil)...), inRule...))...)
	tag := opaque([]byte("abcde"))
	for _, tc := range []struct {
		name, label string
		call, want  []byte
		served      bool // a row served the call: no refusal of core's counted
	}{
		{
			name: "NFSv3's NULL as knfsd answers it", label: "nfs3", served: true,
			call: golden(t, "knfsd-nfs3-nfs3-v3-null-call"),
			want: golden(t, "knfsd-nfs3-nfs3-v3-null-reply"),
		},
		{
			name: "MOUNT's NULL as knfsd answers it", label: "nfs3", served: true,
			call: golden(t, "knfsd-nfs3-nfs3-mount-null-call"),
			want: golden(t, "knfsd-nfs3-nfs3-mount-null-reply"),
		},
		{
			name: "NLM's NULL as the hand-written vector answers it", label: "nfs3", served: true,
			call: golden(t, "rfc-nlm4-null-call"),
			want: golden(t, "rfc-nlm4-null-reply"),
		},
		{
			name: "NSM's NULL as knfsd answers it", label: "nfs3", served: true,
			call: golden(t, "knfsd-nfs3-nlm-nsm-null-call"),
			want: golden(t, "knfsd-nfs3-nlm-nsm-null-reply"),
		},
		{
			name: "NFSv4's NULL as knfsd answers it", label: "nfs4",
			served: true,
			call:   golden(t, "knfsd-nfs4-nfs40-null-call"),
			want:   golden(t, "knfsd-nfs4-nfs40-null-reply"),
		},
		{
			name:  "MOUNT's NULL under AUTH_SYS, the hand-written vector",
			label: "nfs3", served: true,
			call: golden(t, "mount-v3-null-call")[4:],
			want: accepted(0x11223344, 0),
		},
		{
			"NFSv3's NULL under AUTH_SYS", "nfs3",
			call(30, progNFS, 3, 0, inRuleCred), accepted(30, 0), true,
		},
		{
			"NLM's NULL under AUTH_SYS", "nfs3",
			call(33, progNLM, 4, 0, inRuleCred), accepted(33, 0), true,
		},
		{
			"NSM's NULL under AUTH_SYS", "nfs3",
			call(34, progNSM, 1, 0, inRuleCred), accepted(34, 0), true,
		},
		{
			"NFSv4's NULL under AUTH_SYS", "nfs4",
			call(35, progNFS, 4, 0, inRuleCred), accepted(35, 0), true,
		},
		{
			"MOUNT's procedure 999, no table holds it", "nfs3",
			call(36, progMount, 3, 999, none), accepted(36, procUnavail), false,
		},
		{
			"NLM's procedure 999, no table holds it", "nfs3",
			call(37, progNLM, 4, 999, none), accepted(37, procUnavail), false,
		},
		{
			"NSM's procedure 999, no table holds it", "nfs3",
			call(38, progNSM, 1, 999, none), accepted(38, procUnavail), false,
		},
		{"NFS v2 from the stages' table", "nfs3",
			call(2, progNFS, 2, 0, none), accepted(2, progMismatch, 3, 4), false},
		{"an AUTH_SYS credential of 17 groups", "nfs3",
			call(3, progNFS, 3, 0, append(words(authSys), opaque(sys)...)),
			words(3, 1, 1, 1, authBadCred), false}, // REPLY, MSG_DENIED, AUTH_ERROR
		{"a COMPOUND of minor version 3 from the version 4 table", "nfs4",
			call(4, progNFS, 4, 1, none, append(tag, words(3, 0)...)...),                          // COMPOUND: tag, minor version, no operations
			append(append(accepted(4, 0, nfs4ErrMinorVersMismatch), tag...), words(0)...), false}, // SUCCESS, the status, the tag, no results
		{"a verifier past the record", "nfs3",
			words(5, 0, 2, progNFS, 3, 0, authNone, 0, authNone, 8), accepted(5, garbageArgs), false},
		{
			"a procedure 999 after it, no table holds it", "nfs3",
			call(6, progNFS, 3, 999, none), accepted(6, procUnavail), false,
		},
		{name: "a COMPOUND of no operation", label: "nfs4", served: true,
			call: golden(t, "rfc-nfs4-empty-call"), want: golden(t, "rfc-nfs4-empty-reply")},
		{name: "a COMPOUND whose first operation is code 9999", label: "nfs4", served: true,
			call: golden(t, "rfc-nfs4-illegal-call"), want: golden(t, "rfc-nfs4-illegal-reply")},
		{name: "the same COMPOUND counting 128 operations", label: "nfs4", served: true,
			call: golden(t, "rfc-nfs4-illegal-at-cap-call"), want: golden(t, "rfc-nfs4-illegal-at-cap-reply")},
		{name: "a COMPOUND of 129 operations over NFSv4.1", label: "nfs4",
			call: golden(t, "rfc-nfs4-too-many-ops-call"), want: golden(t, "rfc-nfs4-too-many-ops-reply")},
		{name: "a COMPOUND of 129 operations over NFSv4.0", label: "nfs4",
			call: golden(t, "rfc-nfs4-resource-call"), want: golden(t, "rfc-nfs4-resource-reply")},
		{name: "a COMPOUND whose count runs past its bytes", label: "nfs4",
			call: golden(t, "rfc-nfs4-badxdr-call"), want: golden(t, "rfc-nfs4-badxdr-reply")},
		{name: "a COMPOUND of a READ no row serves", label: "nfs4",
			call: golden(t, "rfc-nfs4-read-call"), want: golden(t, "rfc-nfs4-read-reply")},
		{name: "a COMPOUND cut before its count", label: "nfs4",
			call: golden(t, "rfc-nfs4-no-count-call"), want: golden(t, "rfc-nfs4-no-count-reply")},
	} {
		other := map[string]string{"nfs3": "nfs4", "nfs4": "nfs3"}[tc.label]
		before, otherBefore := recorded(t, d.Metrics, "adapter_refusals_total", tc.label), recorded(t, d.Metrics, "adapter_refusals_total", other)
		stagesBefore := recorded(t, d.Metrics, "adapter_wire_seconds", tc.label)
		if got := ask(t, c, tc.call); !bytes.Equal(got, tc.want) {
			t.Errorf("%s: answered %x, want %x", tc.name, got, tc.want)
		}
		want := 1.0
		if tc.served {
			want = 0
		}
		if got := recorded(t, d.Metrics, "adapter_refusals_total", tc.label) - before; got != want {
			t.Errorf("%s: %v refusals under %s, want %v", tc.name, got, tc.label, want)
		}
		if tc.served && recorded(t, d.Metrics, "adapter_wire_seconds", tc.label) == stagesBefore {
			t.Errorf("%s: recorded no stage under %s", tc.name, tc.label)
		}
		if got := recorded(t, d.Metrics, "adapter_refusals_total", other) - otherBefore; got != 0 {
			t.Errorf("%s: %v refusals under %s, want none", tc.name, got, other)
		}
		// vfstest's Mutations record: no case reaches the service. No row's
		// handler holds the service today, so this cannot fail for a row as
		// written; it fails the day one is given the service and calls it.
		if ops := d.VFS.(*memory.Service).Ops(); len(ops) != 0 {
			t.Errorf("%s: the service saw %v, want no call", tc.name, ops)
		}
	}
}

// A record in fragments is read whole up to RFC 21 section 2.3's 64 fragments
// and 1 MiB and 4 KiB together, and answered; past either, the connection
// closes with nothing answered (RFC 20 section 4.7's Framer).
func TestFragmentsUpToTheCaps(t *testing.T) {
	d := leakChecked(t)
	srv := serve(t, d, nfsSettings(allVersions))
	refusal := call(7, progMount, 3, 999, none) // a procedure no table holds

	padded := func(n int) []byte {
		return append(slices.Clone(refusal), make([]byte, n-len(refusal))...)
	}

	for _, tc := range []struct {
		name   string
		record []byte
		answer bool
	}{
		{"a call in 2 fragments", fragments(refusal, 2), true},
		{"a call in 64 fragments", fragments(refusal, 64), true},
		{"a call in 65 fragments", fragments(refusal, 65), false},
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

// The version 4 table answers the reference server's capture byte for byte:
// its NULL call (frame 4) with its reply (frame 6), counted under nfs4, the
// NULL a row serves, an accepted success, no result (RFC 5531 section 9;
// RFC 21 sections 2.1 and 7).
func TestANullRowAnswersAsTheReferenceServer(t *testing.T) {
	d := leakChecked(t)
	srv, err := core.NewServer(d, nfsSettings(allVersions), core.Listener{
		Services: []string{"nfs"}, Network: "tcp",
		Protocol: protocol.New(protocol.Tables{V4: tables(d).V4}),
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
	got := ask(t, talker, call(8, progNFS, 3, 999, none))

	want := accepted(8, procUnavail)

	if !bytes.Equal(got, want) {
		t.Fatalf("the first routed call answered %x, want %x", got, want)
	}
	if !closed(t, silent) {
		t.Fatal("the silent connection answered something")
	}

	got2 := ask(t, talker, call(9, progNFS, 3, 999, none))

	want2 := accepted(9, procUnavail)

	if !bytes.Equal(got2, want2) {
		t.Errorf("past the deadline a call answered %x, want %x", got2, want2)
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

		got := ask(t, dial(t, srv), call(14, progMount, 3, 999, none))

		want := accepted(14, procUnavail)

		if !bytes.Equal(got, want) {
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

// Each program's table maps what the loopback tests leave unreached, each row
// by minor version where NFSv4's differ: arguments that do not decode, which
// only a row decodes, an error no row names, which is its program's fault,
// ErrNotLast, ErrTooManyPending and ErrCancelled, which never reach NFS, and a
// failed logon, all at the fault, a security flavour the share refuses,
// AUTH_ERROR with AUTH_TOOWEAK, and the calls that
// answer nothing, a move and the adapter draining, a procedure NFSv4's table
// lacks, a COMPOUND past the cap or whose reply is too big, NFSv4.0 answering
// NFS4ERR_RESOURCE for both, and, in NFSv4's members' table, a member that does
// not decode, names a code no row serves or one its minor version does not
// define, the moved and no-filehandle rows by their operation lists (RFC 21
// sections 10 and 11.5; RFC 5531 section 9; RFC 8881 sections 15.1.2.4 and
// 15.1.2.5). A row left out of a table lands on the fault status, which this
// test cannot tell from the same row answering the fault, so the loopback
// test's adapter_unmapped_errors_total counters are what catch a missing row:
// keep both.
func TestEachTableMapsCoresRefusals(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)
	// accept is an RPC accept state's status, as the header stage encodes it:
	// 1 in the two high bits; auth an AUTH_ERROR's, 3 in them.
	accept := func(state uint32) core.Status { return core.Status(1<<30 | state) }
	auth := func(stat uint32) core.Status {
		return core.Status(3<<30 | stat)
	}
	// The nfsstat4 values, from RFC 7530 section 13 and RFC 8881 section 15,
	// and the auth_stat of AUTH_TOOWEAK from RFC 5531 section 9.
	const (
		serverFault  = 10006 // NFS3ERR_SERVERFAULT and NFS4ERR_SERVERFAULT
		notSupp      = 10004 // NFS4ERR_NOTSUPP
		resource     = 10018 // NFS4ERR_RESOURCE
		moved        = 10019 // NFS4ERR_MOVED
		noFileHandle = 10020 // NFS4ERR_NOFILEHANDLE
		badXDR       = 10036 // NFS4ERR_BADXDR
		opIllegal    = 10044 // NFS4ERR_OP_ILLEGAL
		repTooBig    = 10066 // NFS4ERR_REP_TOO_BIG
		tooManyOps   = 10070 // NFS4ERR_TOO_MANY_OPS
		authTooWeak  = 5     // AUTH_TOOWEAK
		compoundProc = 1
	)
	ts := tables(adapter.Deps{})
	members := ts.V4.Ops[compoundProc].Members
	other := errors.New("an error no row names")
	// A wrapped refusal, the error a row maps through errors.Is.
	wrapped := func(err error) error {
		return errors.Join(err, other)
	}
	for _, tc := range []struct {
		name    string
		table   *core.OpTable
		err     error
		op      *core.Op // an operation a row names: nil where none does
		minor   core.Version
		want    core.Status
		noreply bool // true sends nothing: no reply at all
	}{
		{"NFSv3's decode", ts.V3, core.ErrDecode, nil, 0, accept(garbageArgs), false},
		{"NFSv3's fault", ts.V3, other, nil, 0, serverFault, false},
		{"NFSv3's not supported", ts.V3, core.ErrNotSupported, nil, 0, accept(procUnavail), false},
		{"NFSv3's not last", ts.V3, core.ErrNotLast, nil, 0, serverFault, false},
		{"NFSv3's too many pending", ts.V3, core.ErrTooManyPending, nil, 0, serverFault, false},
		{"NFSv3's cancelled", ts.V3, core.ErrCancelled, nil, 0, serverFault, false},
		{"NFSv3's failed logon", ts.V3, wrapped(vfs.ErrLogonFailure), nil, 0, serverFault, false},
		{"NFSv3's wrong security", ts.V3, vfs.ErrWrongSecurity, nil, 0, auth(authTooWeak), false},
		{"NFSv3's moved", ts.V3, core.ErrMoved, nil, 0, 0, true},
		{"NFSv3's draining", ts.V3, core.ErrDraining, nil, 0, 0, true},
		{"NFSv4's not supported", ts.V4, core.ErrNotSupported, nil, 0, accept(procUnavail), false},
		{"NFSv4's decode", ts.V4, core.ErrDecode, nil, 1, accept(garbageArgs), false},
		{"NFSv4's fault", ts.V4, other, nil, 1, serverFault, false},
		{"NFSv4's minor version", ts.V4, core.ErrVersion, nil, 3, nfs4ErrMinorVersMismatch, false},
		{"NFSv4's not last", ts.V4, core.ErrNotLast, nil, 1, serverFault, false},
		{"NFSv4's too many pending", ts.V4, core.ErrTooManyPending, nil, 1, serverFault, false},
		{"NFSv4's cancelled", ts.V4, core.ErrCancelled, nil, 1, serverFault, false},
		{"NFSv4's failed logon", ts.V4, wrapped(vfs.ErrLogonFailure), nil, 1, serverFault, false},
		{"NFSv4's moved", ts.V4, core.ErrMoved, nil, 1, 0, true},
		{"NFSv4's draining", ts.V4, core.ErrDraining, nil, 1, 0, true},
		{"NFSv4.0's cap", ts.V4, core.ErrTooLong, nil, 0, resource, false},
		{"NFSv4.1's cap", ts.V4, core.ErrTooLong, nil, 1, tooManyOps, false},
		{"NFSv4.2's cap", ts.V4, core.ErrTooLong, nil, 2, tooManyOps, false},
		{"NFSv4.0's reply too big", ts.V4, core.ErrReplyTooBig, nil, 0, resource, false},
		{"NFSv4.1's reply too big", ts.V4, core.ErrReplyTooBig, nil, 1, repTooBig, false},
		{"NFSv4.2's reply too big", ts.V4, core.ErrReplyTooBig, nil, 2, repTooBig, false},
		{"an NFSv4.0 member's decode", members, core.ErrDecode, nil, 0, badXDR, false},
		{"an NFSv4.2 member's decode", members, core.ErrDecode, nil, 2, badXDR, false},
		{"an NFSv4 member no row serves", members, core.ErrNotSupported, nil, 1, notSupp, false},
		{
			"an NFSv4 member its minor version lacks", members, core.ErrVersion,
			nil, 0, opIllegal, false,
		},
		{"an NFSv4 member's fault", members, other, nil, 1, serverFault, false},
		{"an NFSv4 member's not last", members, core.ErrNotLast, nil, 1, serverFault, false},
		{
			"an NFSv4 member's too many pending", members, core.ErrTooManyPending,
			nil, 1, serverFault, false,
		},
		{
			"an NFSv4 member's cancelled", members, core.ErrCancelled,
			nil, 1, serverFault, false,
		},
		{
			"an NFSv4 member's failed logon", members, wrapped(vfs.ErrLogonFailure),
			nil, 1, serverFault, false,
		},
		{
			"an NFSv4 member's moved over DELEGRETURN", members, core.ErrMoved,
			&core.Op{Code: 8}, 1, moved, false,
		},
		{
			"an NFSv4 member's moved over SEQUENCE", members, core.ErrMoved,
			&core.Op{Code: 53}, 1, 0, true,
		},
		{"an NFSv4 member's draining", members, core.ErrDraining, nil, 1, 0, true},
		{
			"an NFSv4.0 member with no filehandle", members, core.ErrNoFH,
			nil, 0, noFileHandle, false,
		},
		{
			"an NFSv4.1 member with no filehandle", members, core.ErrNoFH,
			nil, 1, noFileHandle, false,
		},
		{
			"an NFSv4.2 member with no filehandle", members, core.ErrNoFH,
			nil, 2, noFileHandle, false,
		},
		{"MOUNT's decode", ts.Mount, core.ErrDecode, nil, 0, accept(garbageArgs), false},
		{"MOUNT's fault", ts.Mount, other, nil, 0, accept(systemErr), false},
		{
			"MOUNT's not supported", ts.Mount, core.ErrNotSupported,
			nil, 0, accept(procUnavail), false,
		},
		{"MOUNT's not last", ts.Mount, core.ErrNotLast, nil, 0, accept(systemErr), false},
		{
			"MOUNT's too many pending", ts.Mount, core.ErrTooManyPending,
			nil, 0, accept(systemErr), false,
		},
		{"MOUNT's cancelled", ts.Mount, core.ErrCancelled, nil, 0, accept(systemErr), false},
		{
			"MOUNT's failed logon", ts.Mount, wrapped(vfs.ErrLogonFailure),
			nil, 0, accept(systemErr), false,
		},
		{
			"MOUNT's wrong security", ts.Mount, vfs.ErrWrongSecurity,
			nil, 0, auth(authTooWeak), false,
		},
		{"MOUNT's moved", ts.Mount, core.ErrMoved, nil, 0, 0, true},
		{"MOUNT's draining", ts.Mount, core.ErrDraining, nil, 0, 0, true},
		{"NLM's decode", ts.NLM, core.ErrDecode, nil, 0, accept(garbageArgs), false},
		{"NLM's fault", ts.NLM, other, nil, 0, accept(systemErr), false},
		{"NLM's not supported", ts.NLM, core.ErrNotSupported, nil, 0, accept(procUnavail), false},
		{"NLM's not last", ts.NLM, core.ErrNotLast, nil, 0, accept(systemErr), false},
		{
			"NLM's too many pending", ts.NLM, core.ErrTooManyPending,
			nil, 0, accept(systemErr), false,
		},
		{"NLM's cancelled", ts.NLM, core.ErrCancelled, nil, 0, accept(systemErr), false},
		{
			"NLM's failed logon", ts.NLM, wrapped(vfs.ErrLogonFailure),
			nil, 0, accept(systemErr), false,
		},
		{"NLM's wrong security", ts.NLM, vfs.ErrWrongSecurity, nil, 0, auth(authTooWeak), false},
		{"NLM's moved", ts.NLM, core.ErrMoved, nil, 0, 0, true},
		{"NLM's draining", ts.NLM, core.ErrDraining, nil, 0, 0, true},
		{"NSM's decode", ts.NSM, core.ErrDecode, nil, 0, accept(garbageArgs), false},
		{"NSM's fault", ts.NSM, other, nil, 0, accept(systemErr), false},
		{"NSM's not supported", ts.NSM, core.ErrNotSupported, nil, 0, accept(procUnavail), false},
		{"NSM's not last", ts.NSM, core.ErrNotLast, nil, 0, accept(systemErr), false},
		{
			"NSM's too many pending", ts.NSM, core.ErrTooManyPending,
			nil, 0, accept(systemErr), false,
		},
		{"NSM's cancelled", ts.NSM, core.ErrCancelled, nil, 0, accept(systemErr), false},
		{
			"NSM's failed logon", ts.NSM, wrapped(vfs.ErrLogonFailure),
			nil, 0, accept(systemErr), false,
		},
		{"NSM's wrong security", ts.NSM, vfs.ErrWrongSecurity, nil, 0, auth(authTooWeak), false},
		{"NSM's moved", ts.NSM, core.ErrMoved, nil, 0, 0, true},
		{"NSM's draining", ts.NSM, core.ErrDraining, nil, 0, 0, true},
	} {
		if got, replied, _ := tc.table.Errors.Map(tc.err, tc.op, tc.minor); got != tc.want ||
			tc.noreply == replied {
			t.Errorf(
				"%s: %#x, replied %v, want %#x replying %v",
				tc.name,
				uint32(got),
				replied,
				uint32(tc.want),
				!tc.noreply,
			)
		}
	}
}
