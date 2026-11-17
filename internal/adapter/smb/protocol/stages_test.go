package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Command codes and the related flag, written from MS-SMB2 2.2.1.2's tables,
// not taken from wire, so a code read wrong there fails here.
const (
	cmdNegotiate = 0x0000
	cmdCreate    = 0x0005
	cmdClose     = 0x0006
	cmdRead      = 0x0008
	cmdCancel    = 0x000C
	related      = 0x00000004 // SMB2_FLAGS_RELATED_OPERATIONS
)

// statusInternalError is STATUS_INTERNAL_ERROR (MS-ERREF 2.3.1), the fault
// status of the test's error table (RFC 22 §16.5); statusInvalidParameter is
// STATUS_INVALID_PARAMETER (MS-ERREF 2.3.1), its ErrDecode row's answer.
const (
	statusInternalError    = 0xC00000E5
	statusInvalidParameter = 0xC000000D
)

// request is an SMB2 request in the sync form, its header written field by
// field from MS-SMB2 2.2.1.2 and independent of wire, then args bytes of fill.
// It asks for 8 credits, so a reply that echoes the request's ask is told
// from one that grants 1.
func request(cmd uint16, flags, next uint32, id uint64, args int, fill byte) []byte {
	b := make([]byte, 64, 64+args)
	copy(b, "\xfeSMB")                           // ProtocolId
	binary.LittleEndian.PutUint16(b[4:], 64)     // StructureSize
	binary.LittleEndian.PutUint16(b[6:], 1)      // CreditCharge
	binary.LittleEndian.PutUint16(b[12:], cmd)   // Command
	binary.LittleEndian.PutUint16(b[14:], 8)     // CreditRequest
	binary.LittleEndian.PutUint32(b[16:], flags) // Flags
	binary.LittleEndian.PutUint32(b[20:], next)  // NextCommand
	binary.LittleEndian.PutUint64(b[24:], id)    // MessageId
	binary.LittleEndian.PutUint32(b[36:], 5)     // TreeId
	binary.LittleEndian.PutUint64(b[40:], 7)     // SessionId
	return append(b, bytes.Repeat([]byte{fill}, args)...)
}

// chainOfThree is a CREATE, a related READ and a related CLOSE in one record,
// at offsets 0, 80 and 168: each member's arguments, filled with a byte of its
// own, padding to 8, then the next member's header. The READ's NextCommand,
// 88, is relative to its own header, so it differs from its successor's
// offset.
func chainOfThree() []byte {
	b := request(cmdCreate, 0, 80, 10, 12, 0xa0)
	b = append(b, 0, 0, 0, 0)
	b = append(b, request(cmdRead, related, 88, 11, 20, 0xa1)...)
	b = append(b, 0, 0, 0, 0)
	return append(b, request(cmdClose, related, 0, 12, 24, 0xa2)...)
}

// decodeAll decodes each member of rec's chain in turn, from offset 0.
func decodeAll(t *testing.T, rec *resources.Buf) []core.Header {
	t.Helper()
	var hs []core.Header
	for off := 0; ; {
		h, err := headerStage{}.Decode(rec, off)
		if h == nil || err != nil {
			t.Fatalf("Decode at %d = %v, %v", off, h, err)
		}
		hs = append(hs, h)
		if off = h.Next(); off == 0 {
			return hs
		}
	}
}

// Next is the record's offset of the next member, not NextCommand, which
// counts from the member's own header: core's walk follows Next.
func TestDecodeWalksAChainByTheRecordsOffsets(t *testing.T) {
	h, err := headerStage{}.Decode(&resources.Buf{B: request(cmdNegotiate, 0, 0, 1, 36, 0)}, 0)
	if h == nil || err != nil || h.Op() != cmdNegotiate || h.Next() != 0 || h.Related() || h.Version() != 0 {
		t.Fatalf("a single NEGOTIATE: Decode = %v, %v; want Op 0, Next 0, not related, version 0", h, err)
	}

	hs := decodeAll(t, &resources.Buf{B: chainOfThree()})
	want := []struct {
		op      uint32
		next    int
		related bool
	}{{cmdCreate, 80, false}, {cmdRead, 168, true}, {cmdClose, 0, true}}
	if len(hs) != len(want) {
		t.Fatalf("the walk reached %d members, want %d", len(hs), len(want))
	}
	for i, w := range want {
		if h := hs[i]; h.Op() != w.op || h.Next() != w.next || h.Related() != w.related || h.Version() != 0 {
			t.Errorf("member %d: Op %#x, Next %d, Related %v, Version %d; want %#x, %d, %v, 0", i, h.Op(), h.Next(), h.Related(), h.Version(), w.op, w.next, w.related)
		}
	}
}

// A header no reply can be addressed to comes back with none; a malformed one
// that can be answered comes back with its error. A refused NextCommand
// reports a Next past the record's end, so the walk ends there; a header
// refused for anything else keeps its step.
func TestDecodeKeepsTheHeaderOfWhatCanBeAnswered(t *testing.T) {
	ok := request(cmdNegotiate, 0, 0, 1, 36, 0)
	patched := func(b []byte, at int, v ...byte) []byte {
		b = bytes.Clone(b)
		copy(b[at:], v)
		return b
	}
	chain := chainOfThree()
	for _, c := range []struct {
		name string
		rec  []byte
		off  int
	}{
		{"63 bytes", ok[:63], 0},
		{"a member with 63 bytes left", chain[:80+63], 80},
		{"command 0x0013", patched(ok, 12, 0x13), 0},
		{"protocol ID 0xFC 'SMB'", patched(ok, 0, 0xfc), 0},
	} {
		if h, err := (headerStage{}).Decode(&resources.Buf{B: c.rec}, c.off); h != nil || err == nil {
			t.Errorf("%s: Decode = %v, %v; want no header and an error", c.name, h, err)
		}
	}

	// A NEGOTIATE with room for 200 bytes after its header, so a step of 68
	// lands inside the record.
	roomy := request(cmdNegotiate, 0, 0, 1, 200, 0)
	second := append(request(cmdCreate, 0, 80, 10, 16, 0), roomy...)
	for _, c := range []struct {
		name      string
		rec       []byte
		off       int
		next      int  // -1: past the record's end
		keepsStep bool // refused for something other than its NextCommand
	}{
		{"StructureSize 65", patched(ok, 4, 0x41), 0, 0, true},
		{"NextCommand 68", patched(roomy, 20, 68), 0, -1, false},
		{"NextCommand 68, past the first", patched(second, 80+20, 68), 80, -1, false},
		{"StructureSize 65 and NextCommand 80, past the first", patched(patched(second, 80+20, 80), 80+4, 0x41), 80, 160, true},
	} {
		h, err := headerStage{}.Decode(&resources.Buf{B: c.rec}, c.off)
		if h == nil || err == nil || errors.Is(err, wire.ErrNextCommand) == c.keepsStep {
			t.Errorf("%s: Decode = %v, %v; want the header and its error", c.name, h, err)
			continue
		}
		if c.next < 0 && h.Next() <= len(c.rec) {
			t.Errorf("%s: Next %d lies inside the record of %d bytes; the walk would follow it", c.name, h.Next(), len(c.rec))
		}
		if c.next >= 0 && h.Next() != c.next {
			t.Errorf("%s: Next %d, want %d", c.name, h.Next(), c.next)
		}
	}
}

// A refusal is the request's header with MS-SMB2 3.3.4.4's changes, one
// credit, then the ERROR response's 9 bytes; a success is the header alone,
// its body the command's.
func TestEncodeAnswersAFailureWithTheErrorBody(t *testing.T) {
	req := request(cmdNegotiate, 0, 0, 42, 36, 0)
	h, err := headerStage{}.Decode(&resources.Buf{B: req}, 0)
	if err != nil {
		t.Fatal(err)
	}
	reply := func(status []byte) []byte {
		b := bytes.Clone(req[:64])
		copy(b[8:], status)              // Status
		copy(b[14:], []byte{1, 0})       // CreditResponse: one credit
		copy(b[16:], []byte{1, 0, 0, 0}) // Flags: SERVER_TO_REDIR
		return b
	}
	errorBody := []byte{9, 0, 0, 0, 0, 0, 0, 0, 0} // MS-SMB2 2.2.2, no error data
	for _, c := range []struct {
		name string
		s    core.Status
		want []byte
	}{
		{"STATUS_NOT_SUPPORTED", wire.StatusNotSupported, append(reply([]byte{0xbb, 0, 0, 0xc0}), errorBody...)},
		{"STATUS_NO_MORE_FILES, a warning", core.Status(0x80000006), append(reply([]byte{0x06, 0, 0, 0x80}), errorBody...)},
		{"STATUS_SUCCESS", 0, reply([]byte{0, 0, 0, 0})},
	} {
		out := &resources.Buf{}
		if err := (headerStage{}).Encode(h, c.s, out); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !bytes.Equal(out.B, c.want) {
			t.Errorf("%s: Encode = % x\nwant % x", c.name, out.B, c.want)
		}
	}
}

// Three replies chained in one buffer each start at a multiple of 8, each
// NextCommand stepping to the next, the last's 0, each under its request's
// message ID.
func TestChainedRepliesLieAtMultiplesOfEight(t *testing.T) {
	hs := decodeAll(t, &resources.Buf{B: chainOfThree()})
	statuses := []core.Status{wire.StatusNotSupported, 0, statusInvalidParameter}
	out := &resources.Buf{}
	var starts []int
	for i, h := range hs {
		if i > 0 {
			if err := (headerStage{}).Chain(out, starts[i-1]); err != nil {
				t.Fatal(err)
			}
		}
		starts = append(starts, len(out.B))
		if err := (headerStage{}).Encode(h, statuses[i], out); err != nil {
			t.Fatal(err)
		}
		if statuses[i] == 0 {
			out.B = append(out.B, 1, 2, 3, 4, 5) // a command's body of odd length
		}
	}
	for i, at := range starts {
		next := binary.LittleEndian.Uint32(out.B[at+20:])
		id := binary.LittleEndian.Uint64(out.B[at+24:])
		want := uint32(0)
		if i < len(starts)-1 {
			want = uint32(starts[i+1] - at)
		}
		if at%8 != 0 || next != want || id != uint64(10+i) {
			t.Errorf("reply %d at %d: NextCommand %d, MessageId %d; want a multiple of 8, %d, %d", i, at, next, id, want, 10+i)
		}
	}
}

// Each member's arguments are its own bytes and padding, up to the next
// member's header.
func TestArgsAreEachMembersOwn(t *testing.T) {
	rec := &resources.Buf{B: chainOfThree()}
	want := [][]byte{
		append(bytes.Repeat([]byte{0xa0}, 12), 0, 0, 0, 0),
		append(bytes.Repeat([]byte{0xa1}, 20), 0, 0, 0, 0),
		bytes.Repeat([]byte{0xa2}, 24),
	}
	for i, h := range decodeAll(t, rec) {
		if got := (headerStage{}).Args(rec, h); !bytes.Equal(got, want[i]) {
			t.Errorf("member %d: Args = % x\nwant % x", i, got, want[i])
		}
	}
}

// What Encode writes for any status, padded by Chain, fits in ReplyLen.
func TestReplyLenBoundsEncode(t *testing.T) {
	for i, h := range decodeAll(t, &resources.Buf{B: chainOfThree()}) {
		for _, s := range []core.Status{0, wire.StatusNotSupported, statusInvalidParameter, statusInternalError} {
			out := &resources.Buf{}
			if err := (headerStage{}).Encode(h, s, out); err != nil {
				t.Fatal(err)
			}
			if err := (headerStage{}).Chain(out, 0); err != nil {
				t.Fatal(err)
			}
			if n := (headerStage{}).ReplyLen(h); len(out.B) > n {
				t.Errorf("member %d, status %#x: Encode and Chain write %d bytes, past ReplyLen %d", i, uint32(s), len(out.B), n)
			}
		}
	}
}

// newRouter is a connection's router over an empty table, with the error
// rows the stages' table carries: ErrDecode's answer and CANCEL's row with
// no reply (MS-SMB2 3.3.5.16).
func newRouter() *router {
	errs := core.NewErrorTable(statusInternalError,
		core.ErrorRow{Err: core.ErrDecode, Status: statusInvalidParameter},
		core.ErrorRow{Err: ErrCancel, NoReply: true},
	)
	return &router{table: core.NewOpTable(errs, false), conn: &core.Conn{}}
}

// Every command, related or not, is routed to the one table, at version 0 and
// level NeedsNothing, on the router's connection, with no state.
func TestRouteGivesTheOneTableAtNoLevel(t *testing.T) {
	r := newRouter()
	for cmd := uint16(0); cmd <= 0x0012; cmd++ {
		if cmd == cmdCancel {
			continue // answered nothing, in its own test
		}
		for _, flags := range []uint32{0, related} {
			h, err := headerStage{}.Decode(&resources.Buf{B: request(cmd, flags, 0, 1, 8, 0)}, 0)
			if err != nil {
				t.Fatal(err)
			}
			tbl, req, err := r.Route(h, &core.Request{Level: core.NeedsTree})
			if err != nil || tbl != r.table || req == nil {
				t.Fatalf("command %#x: Route = table %p, a request %v, %v; want the router's table %p and a request", cmd, tbl, req != nil, err, r.table)
			}
			if req.Version != 0 || req.Level != core.NeedsNothing || req.Conn != r.conn || req.State != nil {
				t.Errorf("command %#x: the request is at version %d, level %d, on Conn %p, with state %v; want 0, NeedsNothing, %p, none", cmd, req.Version, req.Level, req.Conn, req.State, r.conn)
			}
		}
	}
}

// Requests on one connection run at once: core decodes each on the reader
// and routes and answers it on a goroutine of its own. Each keeps its own
// header and Request, so each reply carries its own message ID.
func TestConcurrentRequestsOnOneConnectionKeepTheirOwn(t *testing.T) {
	const n = 8
	var hs headerStage
	r := newRouter()
	headers := make([]core.Header, n)
	for i := range headers {
		h, err := hs.Decode(&resources.Buf{B: request(cmdNegotiate, 0, 0, uint64(100+i), 36, 0)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		headers[i] = h
	}
	replies := make([][]byte, n)
	reqs := make([]*core.Request, n)
	var wg sync.WaitGroup
	for i, h := range headers {
		wg.Go(func() {
			_, req, err := r.Route(h, nil)
			if err != nil {
				t.Error(err)
				return
			}
			out := &resources.Buf{}
			if err := hs.Encode(h, wire.StatusNotSupported, out); err != nil {
				t.Error(err)
				return
			}
			replies[i], reqs[i] = out.B, req
		})
	}
	wg.Wait()
	seen := map[*core.Request]bool{}
	for i, b := range replies {
		if id := binary.LittleEndian.Uint64(b[24:]); id != uint64(100+i) {
			t.Errorf("request %d answered under message ID %d, want %d", i, id, 100+i)
		}
		if seen[reqs[i]] {
			t.Errorf("request %d shares its Request with another", i)
		}
		seen[reqs[i]] = true
	}
}

// The header stage decodes and encodes a single message, and decodes each
// refusal with a header, with no allocation; a member past the first costs
// one, for its offsets.
func TestHeaderStageAllocs(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	var hs headerStage
	single := &resources.Buf{B: request(cmdNegotiate, 0, 0, 1, 36, 0)}
	out := &resources.Buf{B: make([]byte, 0, replyLen)}
	if n := testing.AllocsPerRun(100, func() {
		h, _ := hs.Decode(single, 0)
		for _, s := range []core.Status{0, wire.StatusNotSupported} {
			out.B = out.B[:0]
			_ = hs.Encode(h, s, out)
		}
	}); n != 0 {
		t.Errorf("a single message's decode and encodes allocate %v times, want 0", n)
	}

	structure := request(cmdNegotiate, 0, 0, 1, 200, 0)
	structure[4] = 0x41 // StructureSize 65
	step := request(cmdNegotiate, 0, 68, 1, 200, 0)
	for name, b := range map[string][]byte{"StructureSize 65": structure, "NextCommand 68": step} {
		rec := &resources.Buf{B: b}
		if n := testing.AllocsPerRun(100, func() { _, _ = hs.Decode(rec, 0) }); n != 0 {
			t.Errorf("the refusal of %s allocates %v times, want 0", name, n)
		}
	}

	chain := &resources.Buf{B: chainOfThree()}
	if n := testing.AllocsPerRun(100, func() { _, _ = hs.Decode(chain, 80) }); n != 1 {
		t.Errorf("a member past the first allocates %v times, want 1", n)
	}
}

// A CANCEL, in either form, is refused with the table and its sentinel, and
// no request: the stages' table maps the sentinel to its row with no reply,
// so nothing is answered (MS-SMB2 3.3.5.16).
func TestACancelIsAnsweredNothing(t *testing.T) {
	r := newRouter()
	for _, flags := range []uint32{0, wire.FlagAsyncCommand} {
		req := request(cmdCancel, flags, 0, 42, 36, 0)
		h, err := headerStage{}.Decode(&resources.Buf{B: req}, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, rq, err := r.Route(h, nil)
		if !errors.Is(err, ErrCancel) || rq != nil || got != r.table {
			t.Errorf("a CANCEL of flags %#x routes %p, %v, %v; want the table, no request and ErrCancel", flags, got, rq, err)
		}
	}
	if _, send := r.table.Errors.Map(ErrCancel, nil, 0); send {
		t.Error("the table sends a CANCEL a reply, which MS-SMB2 3.3.5.16 answers nothing")
	}
}

// otherHeader is a header of another protocol, not a double of one: it exists
// only to reach the stage's type checks.
type otherHeader struct{}

func (otherHeader) Op() uint32            { return 0 }
func (otherHeader) Version() core.Version { return 0 }
func (otherHeader) Next() int             { return 0 }
func (otherHeader) Related() bool         { return false }

// A header the stage did not decode is refused by Encode and has no arguments.
func TestAForeignHeader(t *testing.T) {
	out := &resources.Buf{}
	if err := (headerStage{}).Encode(otherHeader{}, 0, out); !errors.Is(err, errForeign) {
		t.Errorf("Encode of a foreign header returns %v", err)
	}
	if args := (headerStage{}).Args(&resources.Buf{B: make([]byte, 64)}, otherHeader{}); args != nil {
		t.Errorf("Args of a foreign header is %v", args)
	}
}

// The reply to an async-form request grants no credit (MS-SMB2 3.3.4.4),
// where every other reply grants one, and takes no async form: ASYNC_COMMAND
// is not a bit a reply echoes, the async reply being the hold stage's with
// the AsyncId it chose, and no request is put off here (MS-SMB2 2.2.1.1's
// flags table, 3.3.4.4).
func TestAnAsyncRequestsReplyGrantsNoCredit(t *testing.T) {
	req := request(cmdRead, wire.FlagAsyncCommand, 0, 42, 0, 0)
	h, err := headerStage{}.Decode(&resources.Buf{B: req}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := &resources.Buf{}
	if err := (headerStage{}).Encode(h, wire.StatusNotSupported, out); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(out.B[14:]); got != 0 {
		t.Errorf("the reply to an async request grants %d credits, want none", got)
	}
	if got := binary.LittleEndian.Uint32(out.B[16:]); got != wire.FlagServerToRedir {
		t.Errorf("the reply to an async request carries flags %#x, want SERVER_TO_REDIR alone", got)
	}
}
