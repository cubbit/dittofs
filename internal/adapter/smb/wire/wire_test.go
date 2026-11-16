package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// The vectors in testdata/golden are written by hand from MS-SMB2's field
// tables, or cut from the reference server's capture, so they check the codec
// against the specification and the reference server, never against itself.
// Each file says where its bytes come from.

// readGolden returns the bytes of testdata/golden/<name>.hex: hex digits, with
// whitespace and each line's comment from # on dropped.
func readGolden(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}

	return b
}

// The cap is 8 MiB and 4 KiB (RFC 22 §3.3), written here and not taken from
// the package, so a cap off by its header room fails.
const wantCap = 8<<20 + 4<<10

func TestFrameLenReadsTheLengthUpToTheCap(t *testing.T) {
	for _, c := range []struct {
		name string
		hdr  []byte
		n    int
		err  error
	}{
		{"the cap", []byte{0x00, 0x80, 0x10, 0x00}, wantCap, nil},
		{"one byte past the cap", []byte{0x00, 0x80, 0x10, 0x01}, 0, errFrameTooLong},
		{"a first byte not zero", []byte{0x01, 0x00, 0x00, 0x40}, 0, errFrameZero},
		{"three bytes", []byte{0x00, 0x00, 0x40}, 0, errFrameShort},
	} {
		n, err := FrameLen(c.hdr)
		if n != c.n || !errors.Is(err, c.err) {
			t.Errorf("%s: FrameLen(% x) = %d, %v; want %d, %v", c.name, c.hdr, n, err, c.n, c.err)
		}
	}

	frame := readGolden(t, "negotiate_request")
	if n, err := FrameLen(frame); n != len(frame)-FrameHeaderLen || err != nil {
		t.Errorf("FrameLen of the capture's frame 4 = %d, %v; want its message's %d bytes", n, err, len(frame)-FrameHeaderLen)
	}
}

func TestAppendFrameReadsBackThroughFrameLen(t *testing.T) {
	for _, n := range []int{0, 64, 0x123456, wantCap} {
		hdr := AppendFrame([]byte{0xaa}, n)
		if len(hdr) != 1+FrameHeaderLen || hdr[0] != 0xaa {
			t.Fatalf("AppendFrame(0xaa, %d) = % x; want 0xaa and 4 bytes after it", n, hdr)
		}
		if got, err := FrameLen(hdr[1:]); got != n || err != nil {
			t.Errorf("FrameLen(AppendFrame(%d)) = %d, %v", n, got, err)
		}
	}
}

// fields are a request header's fields, as its accessors read them.
type fields struct {
	command      uint16
	flags        uint32
	next         uint32
	creditCharge uint16
	credits      uint16
	messageID    uint64
	id           uint64 // the tree ID of a header in the sync form, the async ID of one in the async form
	sessionID    uint64
}

func TestDecodeHeaderReadsEachFieldInPlace(t *testing.T) {
	negotiate := readGolden(t, "negotiate_request")[FrameHeaderLen:]
	sync := readGolden(t, "sync_request")
	async := readGolden(t, "async_request")
	chain := readGolden(t, "chain_request")

	for _, c := range []struct {
		name  string
		msg   []byte
		off   int
		async bool
		want  fields
	}{
		{"the capture's NEGOTIATE request, frame 4", negotiate, 0, false, fields{
			command: 0x0000, credits: 31,
		}},
		{"a sync header", sync, 0, false, fields{
			command: 0x0008, flags: 0x10000000, creditCharge: 2, credits: 10,
			messageID: 0x0807060504030201, id: 0x14131211, sessionID: 0x2827262524232221,
		}},
		{"an async header", async, 0, true, fields{
			command: 0x000c, flags: 0x0000000a, creditCharge: 1,
			messageID: 0x3837363534333231, id: 0x4847464544434241, sessionID: 0x2827262524232221,
		}},
		{"a chain's first member", chain, 0, false, fields{
			command: 0x0005, next: 80, creditCharge: 1, credits: 2,
			messageID: 10, id: 5, sessionID: 7,
		}},
		{"a chain's second member, at the first's NextCommand", chain, 80, false, fields{
			command: 0x0006, flags: 0x00000004, creditCharge: 1, credits: 1,
			messageID: 11, id: 0xffffffff, sessionID: 0xffffffffffffffff,
		}},
	} {
		h, err := DecodeHeader(c.msg, c.off)
		if h == nil || err != nil {
			t.Errorf("%s: DecodeHeader = %v, %v; want a header", c.name, h, err)
			continue
		}
		if &h[0] != &c.msg[c.off] {
			t.Errorf("%s: the header is a copy, not a view of the message at %d", c.name, c.off)
		}
		got := fields{
			command: h.Command(), flags: h.Flags(), next: h.NextCommand(), creditCharge: h.creditCharge(),
			credits: h.credits(), messageID: h.messageID(), id: uint64(h.treeID()), sessionID: h.sessionID(),
		}
		if c.async {
			got.id = h.asyncID()
		}
		if got != c.want {
			t.Errorf("%s: decoded to\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

// patched returns a copy of msg with b written at off.
func patched(msg []byte, off int, b ...byte) []byte {
	c := bytes.Clone(msg)
	copy(c[off:], b)

	return c
}

func TestDecodeHeaderRefusesWithNoHeaderWhatCannotBeAnswered(t *testing.T) {
	sync := readGolden(t, "sync_request")
	chain := readGolden(t, "chain_request")
	for _, c := range []struct {
		name string
		msg  []byte
		off  int
		err  error
	}{
		{"63 bytes", sync[:63], 0, errShort},
		{"a member with 63 bytes left", chain[:80+63], 80, errShort},
		{"command 0x0013", patched(sync, 12, 0x13, 0x00), 0, errCommand},
		{"protocol ID 0xFF 'SMB', SMB1's", patched(sync, 0, 0xff), 0, errProtocolID},
		{"protocol ID 0xFD 'SMB', the transform header's", patched(sync, 0, 0xfd), 0, errProtocolID},
		{"protocol ID 0xFC 'SMB', the compression transform header's", patched(sync, 0, 0xfc), 0, errProtocolID},
	} {
		if h, err := DecodeHeader(c.msg, c.off); h != nil || !errors.Is(err, c.err) {
			t.Errorf("%s: DecodeHeader = %v, %v; want no header and %v", c.name, h, err, c.err)
		}
	}
}

func TestDecodeHeaderReturnsAMalformedHeaderWithItsError(t *testing.T) {
	sync := readGolden(t, "sync_request")
	// room is sync with n bytes after its header and NextCommand set to next.
	room := func(next uint32, n int) []byte {
		msg := append(bytes.Clone(sync), make([]byte, n)...)
		binary.LittleEndian.PutUint32(msg[20:], next) // NextCommand, MS-SMB2 2.2.1.2

		return msg
	}
	for _, c := range []struct {
		name string
		msg  []byte
		err  error
	}{
		{"StructureSize 65", patched(sync, 4, 0x41, 0x00), errStructureSize},
		{"NextCommand 68, not a multiple of 8", room(68, 68), errNextCommand},
		{"NextCommand 72, its header one byte past the message", room(72, 72-1), errNextCommand},
		{"NextCommand 8, inside this header", room(8, 64), errNextCommand},
		{"NextCommand 68 and StructureSize 65: the step's refusal first", patched(room(68, 68), 4, 0x41), errNextCommand},
	} {
		h, err := DecodeHeader(c.msg, 0)
		if h == nil || &h[0] != &c.msg[0] || !errors.Is(err, c.err) {
			t.Errorf("%s: DecodeHeader = %v, %v; want the header and %v", c.name, h, err, c.err)
		}
	}

	if h, err := DecodeHeader(room(72, 72), 0); h == nil || err != nil {
		t.Errorf("NextCommand 72, its header ending at the message's end: DecodeHeader = %v, %v; want the header", h, err)
	}
}

func TestAppendReplyGivesTheCapturesResponseHeader(t *testing.T) {
	req := readGolden(t, "negotiate_request")
	resp := readGolden(t, "negotiate_response")
	h, err := DecodeHeader(req[FrameHeaderLen:], 0)
	if err != nil {
		t.Fatal(err)
	}

	// Frame 6's status is STATUS_SUCCESS and it grants 1 credit. No field of
	// its header is the server's own choice: it is the request's header with
	// MS-SMB2 3.3.4.4's changes, as AppendReply writes it.
	got := AppendReply(nil, h, 0, 1)
	if want := resp[FrameHeaderLen : FrameHeaderLen+HeaderLen]; !bytes.Equal(got, want) {
		t.Errorf("AppendReply over frame 4's header =\n% x\nwant frame 6's\n% x", got, want)
	}
	if got, want := AppendFrame(nil, len(resp)-FrameHeaderLen), resp[:FrameHeaderLen]; !bytes.Equal(got, want) {
		t.Errorf("AppendFrame for frame 6's message = % x; want frame 6's Direct TCP header % x", got, want)
	}
}

func TestAppendReplyKeepsTheAsyncForm(t *testing.T) {
	h, err := DecodeHeader(readGolden(t, "async_request"), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := AppendReply([]byte{0xaa}, h, StatusNotSupported, 0)
	if want := append([]byte{0xaa}, readGolden(t, "async_reply")...); !bytes.Equal(got, want) {
		t.Errorf("AppendReply after one byte =\n% x\nwant\n% x", got, want)
	}
}

func TestAppendErrorWritesTheNineByteBody(t *testing.T) {
	got := AppendError([]byte{0xaa})
	if want := append([]byte{0xaa}, readGolden(t, "error_body")...); !bytes.Equal(got, want) {
		t.Errorf("AppendError after one byte = % x; want % x", got, want)
	}
}

func TestChainPadsToEightAndPointsAtTheNextReply(t *testing.T) {
	chain := readGolden(t, "chain_request")
	first, err := DecodeHeader(chain, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DecodeHeader(chain, 80)
	if err != nil {
		t.Fatal(err)
	}

	// Each reply is a 64-byte header and the 9-byte error body: 73 bytes,
	// padded to 80.
	out := AppendError(AppendReply(nil, first, StatusNotSupported, 1))
	if out, err = Chain(out, 0); err != nil {
		t.Fatal(err)
	}
	out = AppendError(AppendReply(out, second, StatusNotSupported, 1))
	if out, err = Chain(out, 80); err != nil {
		t.Fatal(err)
	}

	if len(out) != 160 {
		t.Fatalf("two chained 73-byte replies take %d bytes, want 160", len(out))
	}
	nextCommand := func(off int) uint32 { return binary.LittleEndian.Uint32(out[off+20:]) } // MS-SMB2 2.2.1.2
	if nextCommand(0) != 80 || nextCommand(80) != 80 {
		t.Errorf("the replies' NextCommand are %d and %d, want 80 and 80", nextCommand(0), nextCommand(80))
	}
	if pad := out[73:80]; !bytes.Equal(pad, make([]byte, 7)) {
		t.Errorf("the padding after the first reply is % x, want zeros", pad)
	}
	if flags := binary.LittleEndian.Uint32(out[80+16:]); flags != 0x00000005 {
		t.Errorf("the related member's reply has Flags %#x, want SERVER_TO_REDIR and RELATED_OPERATIONS", flags)
	}

	if _, err := Chain(out, len(out)-HeaderLen+1); !errors.Is(err, errChain) {
		t.Errorf("Chain of a reply with 63 bytes in the buffer = %v, want %v", err, errChain)
	}
}

// A refusal of a claimed length allocates nothing for it: a buffer allocated
// before its length is checked would be megabytes, or gigabytes.
func TestRefusalsOfAClaimedLengthAllocateNoBuffer(t *testing.T) {
	msg := patched(readGolden(t, "sync_request"), 20, 0xf8, 0xff, 0xff, 0xff) // NextCommand 2^32 - 8
	for _, c := range []struct {
		name   string
		refuse func() error
	}{
		{"a frame of 2^24 - 1 bytes", func() error {
			_, err := FrameLen([]byte{0x00, 0xff, 0xff, 0xff})
			return err
		}},
		{"a NextCommand of 2^32 - 8", func() error {
			_, err := DecodeHeader(msg, 0)
			return err
		}},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := c.refuse()
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n >= 4<<10 {
			t.Errorf("%s: refusing it allocated %d bytes, want under 4 KiB", c.name, n)
		}
	}
}
