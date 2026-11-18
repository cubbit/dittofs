package smb_test

//go:generate go run ./wire/gen -o codec_gen_test.go smb_test.go

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/smb"
	"github.com/cubbit/dittofs/internal/adapter/smb/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// Command codes (MS-SMB2 2.2.1.2), flags (MS-SMB2 2.2.1.1) and statuses
// (MS-ERREF 2.3.1), written from the specifications, not taken from wire, so
// a value read wrong there fails here.
const (
	cmdNegotiate           = 0x0000
	cmdCancel              = 0x000C
	cmdEcho                = 0x000D
	flagServerToRedir      = 0x00000001
	flagAsyncCommand       = 0x00000002
	statusInvalidParameter = 0xC000000D
	statusNotSupported     = 0xC00000BB
	statusInternalError    = 0xC00000E5
)

// errorBody is the ERROR response's body a status alone carries: its
// StructureSize of 9, ErrorContextCount, Reserved, a ByteCount of 0 and one
// byte of ErrorData (MS-SMB2 2.2.2).
var errorBody = []byte{9, 0, 0, 0, 0, 0, 0, 0, 0}

// newDeps is the adapter's dependencies: the filesystem over memory and real
// budgets, the receive budget wide enough for a record past the cap, so the
// framer alone refuses one.
func newDeps(t *testing.T) adapter.Deps {
	return adapter.Deps{VFS: memory.New(t), Budgets: &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 16<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}}
}

// loopback is SMB's default settings on 127.0.0.1, at a port the system
// picks.
func loopback() settings.SMB {
	s := settings.DefaultSMB()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	s.Port = 0
	return s
}

// serve builds the adapter with smb.New over d, on loopback's settings, and
// starts it; it returns the server and its one address.
func serve(t *testing.T, d adapter.Deps) (*core.Server, netip.AddrPort) {
	t.Helper()
	srv, err := smb.New(d, loopback())
	if err != nil {
		t.Fatal(err)
	}
	return srv, start(t, srv)
}

// start starts srv and returns its one smb address over TCP. srv stops at
// the test's end, before the leak check reads what is still open.
func start(t *testing.T, srv *core.Server) netip.AddrPort {
	t.Helper()
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	as := srv.Addrs()[adapter.Socket{Service: "smb", Network: "tcp"}]
	if len(as) != 1 {
		t.Fatalf("Addrs reports smb over tcp at %v, want one address", as)
	}
	return as[0]
}

// dial connects to a, every read and write bounded by a deadline well short
// of the settings' handshake and read timeouts, 30 s each: a read that meets
// it is a server that kept the connection and sent nothing, never one that
// either timeout closed.
func dial(t *testing.T, a netip.AddrPort) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", a.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

// request is an SMB2 request in the sync form, its header written field by
// field from MS-SMB2 2.2.1.2, independent of wire, then body. It asks for 8
// credits, so a reply that echoes the ask is told from one that grants 1.
func request(cmd uint16, flags, next uint32, id uint64, body ...byte) []byte {
	b := make([]byte, 64, 64+len(body))
	copy(b, "\xfeSMB")                           // ProtocolId
	binary.LittleEndian.PutUint16(b[4:], 64)     // StructureSize
	binary.LittleEndian.PutUint16(b[6:], 1)      // CreditCharge
	binary.LittleEndian.PutUint16(b[12:], cmd)   // Command
	binary.LittleEndian.PutUint16(b[14:], 8)     // CreditRequest
	binary.LittleEndian.PutUint32(b[16:], flags) // Flags
	binary.LittleEndian.PutUint32(b[20:], next)  // NextCommand
	binary.LittleEndian.PutUint64(b[24:], id)    // MessageId
	return append(b, body...)
}

// frame is msg in a record of its own: its Direct TCP header, a zero byte and
// msg's length in three big-endian bytes (MS-SMB2 2.1), then msg.
func frame(msg []byte) []byte {
	n := len(msg)
	return append([]byte{0, byte(n >> 16), byte(n >> 8), byte(n)}, msg...)
}

// send writes each of msgs to c in a record of its own.
func send(t *testing.T, c net.Conn, msgs ...[]byte) {
	t.Helper()
	var b []byte
	for _, m := range msgs {
		b = append(b, frame(m)...)
	}
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

// reply is what a test reads of a reply: its header's fields, from MS-SMB2
// 2.2.1.2's layout, and its body, the bytes past its header to the next reply
// or the record's end.
type reply struct {
	status  uint32
	credits uint16
	flags   uint32
	id      uint64
	body    []byte
}

// recv reads one record from c, as long as its Direct TCP header says, and
// returns the replies its NextCommand fields chain in it.
func recv(t *testing.T, c net.Conn) []reply {
	t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("reading a record's Direct TCP header: %v", err)
	}
	if hdr[0] != 0 {
		t.Fatalf("a Direct TCP header whose first byte is %#x, want 0", hdr[0])
	}
	rec := make([]byte, int(hdr[1])<<16|int(hdr[2])<<8|int(hdr[3]))
	if _, err := io.ReadFull(c, rec); err != nil {
		t.Fatalf("reading a record of %d bytes: %v", len(rec), err)
	}
	var rs []reply
	for off := 0; ; {
		m := rec[off:]
		if len(m) < 64 || string(m[:4]) != "\xfeSMB" {
			t.Fatalf("no SMB2 header at offset %d of a %d-byte record", off, len(rec))
		}
		next := int(binary.LittleEndian.Uint32(m[20:]))
		if next != 0 && (next < 64 || next > len(m)) {
			t.Fatalf("a reply at offset %d whose NextCommand %d lies outside the record", off, next)
		}
		end := len(m)
		if next != 0 {
			end = next
		}
		rs = append(rs, reply{
			status:  binary.LittleEndian.Uint32(m[8:]),
			credits: binary.LittleEndian.Uint16(m[14:]),
			flags:   binary.LittleEndian.Uint32(m[16:]),
			id:      binary.LittleEndian.Uint64(m[24:]),
			body:    m[64:end],
		})
		if next == 0 {
			return rs
		}
		off += next
	}
}

// closed is nil when c's server closed it with nothing sent: a read meets the
// stream's end before a byte. A read that meets the deadline dial set instead
// is a server that kept it open.
func closed(c net.Conn) error {
	var b [1]byte
	if n, err := c.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %d bytes, %v; want the end of the stream", n, err)
	}
	return nil
}

// With Port 0, Start binds smb over TCP alone, at 127.0.0.1 on a port the
// system picked, neither the well-known 445 nor the default 12445.
func TestPortZeroBindsAPortTheSystemPicks(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	srv, a := serve(t, d)
	if got := srv.Addrs(); len(got) != 1 || a.Addr() != netip.MustParseAddr("127.0.0.1") || a.Port() == 0 || a.Port() == 445 || a.Port() == 12445 {
		t.Errorf("Addrs after Start = %v; want smb over tcp alone, at 127.0.0.1 on a port the system picked", got)
	}
}

// A NEGOTIATE, which the empty table lacks, is answered STATUS_NOT_SUPPORTED
// under its own message ID, in a record whose Direct TCP header states its
// length: the reply's header, granting one credit and flagged a response,
// then the ERROR response's body.
func TestANegotiateIsNotSupported(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	_, a := serve(t, d)
	c := dial(t, a)
	send(t, c, request(cmdNegotiate, 0, 0, 7))
	rs := recv(t, c)
	if len(rs) != 1 {
		t.Fatalf("a NEGOTIATE was answered %d replies, want 1", len(rs))
	}
	r := rs[0]
	if r.status != statusNotSupported || r.id != 7 || r.credits != 1 || r.flags&flagServerToRedir == 0 || string(r.body) != string(errorBody) {
		t.Errorf("the reply to a NEGOTIATE: status %#x, message ID %d, %d credits, flags %#x, body % x; want %#x, 7, 1, SERVER_TO_REDIR set, % x",
			r.status, r.id, r.credits, r.flags, r.body, statusNotSupported, errorBody)
	}
}

// A header whose StructureSize is 65 is answered STATUS_INVALID_PARAMETER,
// and the connection stays: a NEGOTIATE after it is answered.
func TestAMalformedHeaderIsAnsweredAndTheConnectionKept(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	_, a := serve(t, d)
	c := dial(t, a)
	bad := request(cmdNegotiate, 0, 0, 1)
	binary.LittleEndian.PutUint16(bad[4:], 65) // StructureSize
	send(t, c, bad)
	if rs := recv(t, c); len(rs) != 1 || rs[0].status != statusInvalidParameter || rs[0].id != 1 {
		t.Fatalf("a header whose StructureSize is 65 was answered %+v, want STATUS_INVALID_PARAMETER under message ID 1", rs)
	}
	send(t, c, request(cmdNegotiate, 0, 0, 2))
	if rs := recv(t, c); len(rs) != 1 || rs[0].status != statusNotSupported || rs[0].id != 2 {
		t.Errorf("a NEGOTIATE after the malformed header was answered %+v, want STATUS_NOT_SUPPORTED under message ID 2", rs)
	}
}

// A CANCEL, sync and then async, is answered nothing, and the connection
// stays: the next record read is the reply to a NEGOTIATE sent after them,
// and nothing follows it (MS-SMB2 3.3.5.16).
func TestACancelIsAnsweredNothing(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	srv, a := serve(t, d)
	c := dial(t, a)
	async := request(cmdCancel, flagAsyncCommand, 0, 2)
	binary.LittleEndian.PutUint64(async[32:], 9) // AsyncId, in the async form's place of Reserved and TreeId
	send(t, c, request(cmdCancel, 0, 0, 1), async, request(cmdNegotiate, 0, 0, 3))
	if rs := recv(t, c); len(rs) != 1 || rs[0].status != statusNotSupported || rs[0].id != 3 {
		t.Fatalf("the first record read after two CANCELs and a NEGOTIATE holds %+v, want the NEGOTIATE's STATUS_NOT_SUPPORTED under message ID 3 alone", rs)
	}
	// Stop drains: it returns once the CANCELs, read before the NEGOTIATE,
	// have ended and whatever they were answered is written, then closes.
	if err := srv.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := closed(c); err != nil {
		t.Errorf("after the NEGOTIATE's reply: %v", err)
	}
}

// In a chain of a NEGOTIATE and a member whose NextCommand is 68, not a
// multiple of 8, both are answered, STATUS_NOT_SUPPORTED then
// STATUS_INVALID_PARAMETER, and the connection then closes: the refused step
// ends the chain, so the header 68 bytes past the member's is never read
// (RFC 22 §16.5).
func TestARefusedNextCommandEndsTheChainAndTheConnection(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	_, a := serve(t, d)
	c := dial(t, a)
	// The second member, at offset 64, carries 4 bytes and then a NEGOTIATE
	// header at 132, its offset plus 68, which a walk past the refused step
	// would answer.
	second := request(cmdNegotiate, 0, 68, 2, append(make([]byte, 4), request(cmdNegotiate, 0, 0, 3)...)...)
	send(t, c, append(request(cmdNegotiate, 0, 64, 1), second...))
	var rs []reply
	for len(rs) < 2 {
		rs = append(rs, recv(t, c)...)
	}
	if len(rs) != 2 || rs[0].status != statusNotSupported || rs[0].id != 1 || rs[1].status != statusInvalidParameter || rs[1].id != 2 {
		t.Fatalf("the chain was answered %+v; want STATUS_NOT_SUPPORTED under message ID 1, then STATUS_INVALID_PARAMETER under 2", rs)
	}
	if err := closed(c); err != nil {
		t.Errorf("after the refused step: %v", err)
	}
}

// A record no reply can be addressed to closes its connection, nothing
// answered: a message of 63 bytes, a command code of 0x0013, a protocol ID of
// 0xFC 'SMB' (MS-SMB2 3.3.5.2.6, RFC 22 §16.5), a Direct TCP header whose
// first byte is not zero (MS-SMB2 2.1), and one whose length is past the
// 8 MiB and 4 KiB cap (RFC 22 §3.3).
func TestUnaddressableRecordsCloseTheConnection(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	_, a := serve(t, d)
	foreign := request(cmdNegotiate, 0, 0, 1)
	foreign[0] = 0xFC
	notZero := frame(request(cmdNegotiate, 0, 0, 1))
	notZero[0] = 1
	for _, cs := range []struct {
		name string
		b    []byte
	}{
		{"a message of 63 bytes", frame(request(cmdNegotiate, 0, 0, 1)[:63])},
		{"a command code of 0x0013", frame(request(0x0013, 0, 0, 1))},
		{"a protocol ID of 0xFC 'SMB'", frame(foreign)},
		{"a Direct TCP header whose first byte is not zero", notZero},
		{"a Direct TCP header past the cap", []byte{0, 0x80, 0x10, 0x01}},
	} {
		c := dial(t, a)
		if _, err := c.Write(cs.b); err != nil {
			t.Fatal(err)
		}
		if err := closed(c); err != nil {
			t.Errorf("%s: %v", cs.name, err)
		}
	}
}

// echoRequest is MS-SMB2 2.2.28's ECHO request.
type echoRequest struct {
	_ uint16 `smb2:"size=4"`
	_ uint16 // Reserved
}

// echoResponse is MS-SMB2 2.2.29's ECHO response.
type echoResponse struct {
	_ uint16 `smb2:"size=4"`
	_ uint16 // Reserved
}

func echo(context.Context, *core.Request, echoRequest) (echoResponse, error) {
	return echoResponse{}, nil
}

// A command is its tagged structs, its handler and one core.Row line: a table
// of the ECHO row alone, served through protocol.New and core.NewServer,
// answers an ECHO STATUS_SUCCESS with the response's 4 bytes after the header
// and no ERROR body.
func TestAnEchoRowIsServed(t *testing.T) {
	d := newDeps(t)
	adaptertest.CheckLeaks(t, d.Budgets)
	errs := core.NewErrorTable(statusInternalError, core.ErrorRow{Err: core.ErrDecode, Status: statusInvalidParameter})
	tb := core.NewOpTable(errs, false, core.Row(cmdEcho, "ECHO", core.NeedsNothing, echo))
	tb.Protocol = "smb"
	srv, err := core.NewServer(d, loopback(), core.Listener{Services: []string{"smb"}, Network: "tcp", Protocol: protocol.New(tb)})
	if err != nil {
		t.Fatal(err)
	}
	c := dial(t, start(t, srv))
	send(t, c, request(cmdEcho, 0, 0, 5, 4, 0, 0, 0))
	if rs := recv(t, c); len(rs) != 1 || rs[0].status != 0 || rs[0].id != 5 || string(rs[0].body) != "\x04\x00\x00\x00" {
		t.Errorf("an ECHO was answered %+v; want STATUS_SUCCESS under message ID 5 with the body 04 00 00 00", rs)
	}
}

// refusals is each adapter_refusals_total series of m that counted a
// refusal, keyed by its protocol and stage labels as protocol/stage.
func refusals(t *testing.T, m *metrics.Registry) map[string]float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "dittofs_adapter_refusals_total" {
			continue
		}
		for _, mt := range mf.GetMetric() {
			var protocol, stage string
			for _, lp := range mt.GetLabel() {
				switch lp.GetName() {
				case "protocol":
					protocol = lp.GetValue()
				case "stage":
					stage = lp.GetValue()
				}
			}
			if v := mt.GetCounter().GetValue(); v != 0 {
				got[protocol+"/"+stage] = v
			}
		}
	}
	return got
}

// Every answer counts under protocol="smb": a malformed header's, refused
// before Route under the stages' label, and a NEGOTIATE's, refused at Route
// under the table's (RFC 20 §10.1).
func TestAnswersCountUnderSMB(t *testing.T) {
	d := newDeps(t)
	d.Metrics = metrics.New("v", "c")
	adaptertest.CheckLeaks(t, d.Budgets)
	_, a := serve(t, d)
	c := dial(t, a)
	bad := request(cmdNegotiate, 0, 0, 1)
	binary.LittleEndian.PutUint16(bad[4:], 65) // StructureSize
	send(t, c, bad, request(cmdNegotiate, 0, 0, 2))
	recv(t, c)
	recv(t, c)
	want := map[string]float64{"smb/decode_header": 1, "smb/route": 1}
	if got := refusals(t, d.Metrics); !maps.Equal(got, want) {
		t.Errorf("adapter_refusals_total by protocol/stage = %v, want %v", got, want)
	}
}
