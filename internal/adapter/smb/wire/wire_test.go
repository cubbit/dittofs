package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
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
		hdr, err := AppendFrame([]byte{0xaa}, n)
		if err != nil {
			t.Fatalf("AppendFrame(0xaa, %d): %v", n, err)
		}
		if len(hdr) != 1+FrameHeaderLen || hdr[0] != 0xaa {
			t.Fatalf("AppendFrame(0xaa, %d) = % x; want 0xaa and 4 bytes after it", n, hdr)
		}
		if got, ferr := FrameLen(hdr[1:]); got != n || ferr != nil {
			t.Errorf("FrameLen(AppendFrame(%d)) = %d, %v", n, got, ferr)
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
	create := readGolden(t, "create_request")[FrameHeaderLen:]
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
		{"the capture's CREATE request, frame 126", create, 0, false, fields{
			command: 0x0005, flags: 0x00000010, creditCharge: 1, credits: 1,
			messageID: 14, id: 0x0988b3b3, sessionID: 0xa6d965dc,
		}},
		{"a sync header", sync, 0, false, fields{
			command: 0x0008, flags: 0x10000000, creditCharge: 2, credits: 10,
			messageID: 0x0807060504030201, id: 0x14131211, sessionID: 0x2827262524232221,
		}},
		{"an async header", async, 0, true, fields{
			command: 0x000c, flags: 0x0000000a, creditCharge: 1, credits: 1,
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
			command: h.Command(), flags: h.Flags(), next: h.NextCommand(), creditCharge: h.CreditCharge(),
			credits: h.Credits(), messageID: h.MessageID(), id: uint64(h.TreeID()), sessionID: h.SessionID(),
		}
		if c.async {
			got.id = h.AsyncID()
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

// decode is DecodeHeader, with a panic reported as the test's failure.
func decode(t *testing.T, msg []byte, off int) (h *Header, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("DecodeHeader of %d bytes at %d panicked: %v", len(msg), off, p)
		}
	}()

	return DecodeHeader(msg, off)
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
		{"offset -1", sync, -1, errShort},
		{"command 0x0013", patched(sync, 12, 0x13, 0x00), 0, errCommand},
		{"protocol ID 0xFF 'SMB', SMB1's", patched(sync, 0, 0xff), 0, errProtocolID},
		{"protocol ID 0xFD 'SMB', the transform header's", patched(sync, 0, 0xfd), 0, errProtocolID},
		{"protocol ID 0xFC 'SMB', the compression transform header's", patched(sync, 0, 0xfc), 0, errProtocolID},
		{"protocol ID 0xFE 'SMC'", patched(sync, 3, 'C'), 0, errProtocolID},
	} {
		h, err := decode(t, c.msg, c.off)
		if h != nil || !errors.Is(err, c.err) || !errors.Is(err, ErrUnaddressable) {
			t.Errorf("%s: DecodeHeader = %v, %v; want no header and %v, which is ErrUnaddressable", c.name, h, err, c.err)
		}
	}

	if h, err := DecodeHeader(patched(sync, 12, 0x12, 0x00), 0); h == nil || err != nil {
		t.Errorf("command 0x0012, OPLOCK_BREAK, the last MS-SMB2 2.2.1.2 defines: DecodeHeader = %v, %v; want the header", h, err)
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
		{"StructureSize 63", patched(sync, 4, 0x3f, 0x00), errStructureSize},
		{"NextCommand 68, not a multiple of 8", room(68, 68), ErrNextCommand},
		{"NextCommand 72, its header one byte past the message", room(72, 72-1), ErrNextCommand},
		{"NextCommand 8, inside this header", room(8, 64), ErrNextCommand},
		{"NextCommand 56, its header overlapping this one", room(56, 64), ErrNextCommand},
		{"NextCommand 68 and StructureSize 65: the step's refusal first", patched(room(68, 68), 4, 0x41), ErrNextCommand},
	} {
		h, err := DecodeHeader(c.msg, 0)
		if h == nil || &h[0] != &c.msg[0] || !errors.Is(err, c.err) || errors.Is(err, ErrUnaddressable) {
			t.Errorf("%s: DecodeHeader = %v, %v; want the header and %v, which is not ErrUnaddressable", c.name, h, err, c.err)
		}
	}

	if h, err := DecodeHeader(room(72, 72), 0); h == nil || err != nil {
		t.Errorf("NextCommand 72, its header ending at the message's end: DecodeHeader = %v, %v; want the header", h, err)
	}
	// 64 steps to a whole header right after this one, the last step that
	// does: MS-SMB2 2.2.1.2's "subsequent 8-byte aligned SMB2 header", one
	// below it pointing inside the header it ends.
	if h, err := DecodeHeader(room(64, 64), 0); h == nil || err != nil {
		t.Errorf("NextCommand 64, the next header right after this one: DecodeHeader = %v, %v; want the header", h, err)
	}
}

// No field of the reference server's response headers is the server's own
// choice: each is its request's header with MS-SMB2 3.3.4.4's changes, as
// AppendReply writes it, and the request's flags are kept.
func TestAppendReplyGivesTheCapturesResponses(t *testing.T) {
	for _, c := range []struct {
		name      string
		req, resp string
		status    uint32
		errorBody bool
	}{
		// Frame 6 is STATUS_SUCCESS with a grant of 1 credit; its NEGOTIATE
		// body, not this package's, is left out of the comparison.
		{"frame 4, NEGOTIATE, and frame 6", "negotiate_request", "negotiate_response", 0, false},
		// Frame 127 is the whole message: STATUS_ACCESS_DENIED with a grant of
		// 1 credit, the request's priority kept in Flags, and the error body.
		{"frame 126, CREATE, and frame 127", "create_request", "create_response", 0xC0000022, true},
	} {
		req, resp := readGolden(t, c.req), readGolden(t, c.resp)
		h, err := DecodeHeader(req[FrameHeaderLen:], 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := AppendReply(nil, h, c.status, 1)
		want := resp[FrameHeaderLen : FrameHeaderLen+HeaderLen]
		if c.errorBody {
			got, want = AppendError(got), resp[FrameHeaderLen:]
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the reply =\n% x\nwant the capture's\n% x", c.name, got, want)
		}
		frame, ferr := AppendFrame(nil, len(resp)-FrameHeaderLen)
		if ferr != nil {
			t.Fatalf("%s: the capture's response does not frame: %v", c.name, ferr)
		}
		if want := resp[:FrameHeaderLen]; !bytes.Equal(frame, want) {
			t.Errorf("%s: AppendFrame for the response = % x; want its Direct TCP header % x", c.name, frame, want)
		}
	}
}

func TestAppendReplyKeepsTheSyncForm(t *testing.T) {
	sync := readGolden(t, "sync_request")
	want := readGolden(t, "sync_reply")
	for _, c := range []struct {
		name string
		req  []byte
	}{
		{"a sync request with a non-zero Reserved", sync},
		{"the same request with StructureSize 65", patched(sync, 4, 0x41, 0x00)},
	} {
		h, _ := DecodeHeader(c.req, 0)
		if h == nil {
			t.Fatalf("%s: no header", c.name)
		}
		if got := AppendReply(nil, h, StatusNotSupported, 1); !bytes.Equal(got, want) {
			t.Errorf("%s: AppendReply =\n% x\nwant\n% x", c.name, got, want)
		}
	}
}

// Of a request's flags a reply echoes only the bits it carries - the
// RELATED, priority, DFS and REPLAY bits - and no others (MS-SMB2 2.2.1.1's
// table, 3.3.4.4's echoes): not SIGNED, and not ASYNC_COMMAND, whose reply
// form is the hold stage's with the AsyncId it chose; the async request's
// reply is pinned byte for byte by its golden pair instead. The 0xF000007F
// request sets every bit a reply echoes and the bits it drops: of its low
// 0x7F, RELATED, the priority mask and the request's own SERVER_TO_REDIR
// come back, ASYNC_COMMAND and SIGNED go, and the two high bits no flag
// names go with them.
func TestAReplyEchoesOnlyTheBitsItCarries(t *testing.T) {
	sync, err := DecodeHeader(readGolden(t, "sync_request"), 0)
	if err != nil {
		t.Fatal(err)
	}
	patched := *sync
	binary.LittleEndian.PutUint32(patched[offFlags:], 0xF000007F)
	reply := AppendReply(nil, &patched, 0, 1)
	if r, err := DecodeHeader(reply, 0); err != nil {
		t.Fatal(err)
	} else if got, want := r.Flags(), uint32(0x30000075); got != want {
		t.Errorf("the 0xF000007F request's reply carries %#x, want %#x (SERVER_TO_REDIR, RELATED, priority, DFS and REPLAY)", got, want)
	}
}

// A reply takes no request flag but the ones it echoes: of the CANCEL's
// ASYNC_COMMAND nothing is written, since a reply in the async form is the
// hold stage's, with the AsyncId it chose (MS-SMB2 3.3.4.4) - and the
// CANCEL is never answered anyway (3.3.5.16). The request's bytes at the
// async ID's offset ride through as the copy's, not as the reply's ID.
func TestAppendReplyTakesNoAsyncForm(t *testing.T) {
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

// A related member stays as the client signed it - all-ones IDs, or an
// async form, where its AsyncId sits where a tree would and is no tree -
// and its reply carries the member's own IDs: the pair it runs under is
// what the answering stage carries from the member before it and writes
// into the reply it signs, never into the request, the bytes the client
// signed (RFC 20 section 4.7, MS-SMB2 3.2.5.1.3, 3.3.5.2.7.2).
func TestARelatedMemberStaysAsSignedAndItsReplyCarriesItsOwnIDs(t *testing.T) {
	chain := readGolden(t, "chain_request")
	// A third member for the chain, the golden's second copied after it: the
	// second's NextCommand now steps over its padded body to the copy, which
	// ends the chain with the next MessageId.
	third := append(append(bytes.Clone(chain), make([]byte, 12)...), bytes.Clone(chain[80:])...)
	binary.LittleEndian.PutUint32(third[80+offNextCommand:], 80)
	binary.LittleEndian.PutUint64(third[160+offMessageID:], 12)
	signed := bytes.Clone(third)

	second, err := DecodeHeader(third, 80)
	if err != nil {
		t.Fatal(err)
	}
	last, err := DecodeHeader(third, 160)
	if err != nil {
		t.Fatal(err)
	}
	if last.SessionID() != ^uint64(0) || last.TreeID() != 0xffffffff || last.Flags()&FlagRelatedOperations == 0 {
		t.Fatalf("the chain's third member carries %x/%x, want a related member with the all-ones IDs", last.SessionID(), last.TreeID())
	}

	// An async middle member decodes all the same: patched into the async
	// form, session all-ones and AsyncId 0x9_00000001, so the offset a tree
	// sits at in the sync form carries the AsyncId's high half, which is no
	// tree the codec resolves.
	midFlags := binary.LittleEndian.Uint32(third[80+offFlags:]) | FlagAsyncCommand
	asyncMid := patched(patched(patched(patched(third,
		80+offFlags, byte(midFlags), byte(midFlags>>8), byte(midFlags>>16), byte(midFlags>>24)),
		80+offSessionID, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
		80+32, 1, 0, 0, 0), // the AsyncId's low half; its high half sits at the tree offset
		80+offTreeID, 9, 0, 0, 0)
	am, err := DecodeHeader(asyncMid, 80)
	if err != nil {
		t.Fatal(err)
	}
	if am.Flags()&FlagAsyncCommand == 0 || am.SessionID() != ^uint64(0) || am.TreeID() != 9 {
		t.Fatalf("the patched middle member is async with the session sentinel and AsyncId high half 9, not %x/%x", am.SessionID(), am.TreeID())
	}
	if !bytes.Equal(third, signed) {
		t.Fatalf("resolving the members' IDs wrote into the signed request:\n% x\nwant\n% x", third, signed)
	}

	// The reply the runner builds from the unmodified member carries the
	// all-ones IDs: the resolved ones are written into the reply the stage
	// signs, never back into the request.
	reply := AppendReply(nil, second, 0x00000022, 1)
	r, err := DecodeHeader(reply, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID() != ^uint64(0) || r.TreeID() != 0xffffffff {
		t.Errorf("the member's own reply carries %x/%x, want the all-ones the request carried", r.SessionID(), r.TreeID())
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
	// padded to 80. The chain's last member is padded with more false, so its
	// NextCommand stays 0 and every member still decodes (MS-SMB2 2.2.1.2).
	out := AppendError(AppendReply(nil, first, StatusNotSupported, 1))
	if out, err = Chain(out, 0, true); err != nil {
		t.Fatal(err)
	}
	out = AppendError(AppendReply(out, second, StatusNotSupported, 1))
	if out, err = Chain(out, 80, false); err != nil {
		t.Fatal(err)
	}

	if len(out) != 160 {
		t.Fatalf("two padded 73-byte replies take %d bytes, want 160", len(out))
	}
	nextCommand := func(off int) uint32 { return binary.LittleEndian.Uint32(out[off+20:]) } // MS-SMB2 2.2.1.2
	if nextCommand(0) != 80 || nextCommand(80) != 0 {
		t.Errorf("the replies' NextCommand are %d and %d, want 80 and the last 0", nextCommand(0), nextCommand(80))
	}
	for _, at := range []int{0, 80} {
		if _, err := DecodeHeader(out, at); err != nil {
			t.Fatalf("member %d does not decode: %v", at, err)
		}
	}
	if pad := out[73:80]; !bytes.Equal(pad, make([]byte, 7)) {
		t.Errorf("the padding after the first reply is % x, want zeros", pad)
	}
	if flags := binary.LittleEndian.Uint32(out[80+16:]); flags != 0x00000005 {
		t.Errorf("the related member's reply has Flags %#x, want SERVER_TO_REDIR and RELATED_OPERATIONS", flags)
	}

	if _, err := Chain(out, len(out)-HeaderLen+1, true); !errors.Is(err, errChain) {
		t.Errorf("Chain of a reply with 63 bytes in the buffer = %v, want %v", err, errChain)
	}
}

// AppendFrame states no length the reader refuses: a body past the cap, or
// negative, is refused with the same error FrameLen answers it with, not
// written - and not a panic either, since a flooded record's answers reach
// it (reply amplification: see AppendFrame's doc).
func TestAppendFramePastTheCapIsRefused(t *testing.T) {
	for _, n := range []int{-1, MaxRecord + 1} {
		if got, gerr := AppendFrame(nil, n); gerr == nil || !errors.Is(gerr, errFrameTooLong) {
			t.Errorf("AppendFrame(nil, %d) = %d, %v; want the refusal the reader answers it with", n, got, gerr)
		}
	}
}

// A record of bare 64-byte headers, the flood that answers the most per byte
// of request, still frames: its replies come to past the cap, and the writer
// refuses to state them rather than panics.
func TestAFloodedRecordWritesNoPastCapFrame(t *testing.T) {
	member := bytes.Clone(readGolden(t, "sync_request"))
	binary.LittleEndian.PutUint32(member[offNextCommand:], 64)
	rec := bytes.Repeat(member, MaxRecord/64)
	binary.LittleEndian.PutUint32(rec[len(rec)-HeaderLen+offNextCommand:], 0) // the last member ends the chain
	chain := make([]byte, 0, 80)
	for off := 0; off < len(rec); off += 64 {
		h, err := DecodeHeader(rec, off)
		if err != nil {
			t.Fatalf("the flood's member at %d does not decode: %v", off, err)
		}
		at := len(chain)
		chain = AppendError(AppendReply(chain, h, StatusNotSupported, 1))
		var cerr error
		chain, cerr = Chain(chain, at, true)
		if cerr != nil {
			t.Fatalf("the flood's chain does not step: %v", cerr)
		}
	}
	if len(chain) <= MaxRecord {
		t.Fatalf("the flood's answers come to %d bytes, want past the cap %d: the fixture stopped being the flood", len(chain), MaxRecord)
	}
	if _, err := AppendFrame(nil, len(chain)); !errors.Is(err, errFrameTooLong) {
		t.Errorf("AppendFrame of the flood's %d-byte answer = %v; want errFrameTooLong, not a panic", len(chain), err)
	}
}

// The accepted path allocates nothing: a header decoded out of the message
// bytes, the reply appended into the caller's buffer, its error body and
// step, and the frame header appended for it - the exercise writes the
// header after the record only to count the append; on the wire it precedes -
// hand out views or take the room they were given.
func TestTheAcceptedPathAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	msg := readGolden(t, "sync_request")
	out := make([]byte, 0, 256)
	body := make([]byte, 9) // error bodies suffice: 64 plus 9 rounds to a whole step
	if n := testing.AllocsPerRun(100, func() {
		h, _ := DecodeHeader(msg, 0)
		rec := AppendReply(out[:0], h, 0, 1)
		rec = append(rec, body...)
		rec, _ = Chain(rec, 0, false)
		_, _ = AppendFrame(rec, len(rec))
	}); n != 0 {
		t.Errorf("decoding and replying allocated %v, want nothing", n)
	}
}

// The per-message hot path has a baseline: one member decoded, answered with
// the error body, its reply padded for the record - the run the SMB stage
// repeats for every member of a chain.
func BenchmarkMember(b *testing.B) {
	rec := readGolden(b, "chain_request")
	out := make([]byte, 0, 80)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		h, _ := DecodeHeader(rec, 0)
		out = AppendError(AppendReply(out[:0], h, StatusNotSupported, 1))
		out, _ = Chain(out, 0, false)
		_ = out
	}
}

// Every refusal allocates nothing: a buffer allocated before its length is
// checked would be megabytes, or gigabytes, and a refusal formatted per call
// would allocate on every hostile header. All seven are pinned here, each to
// its own sentinel. The hostile messages are built once, outside the counted
// call: what is counted is the refusal itself.
func TestRefusalsAllocateNothing(t *testing.T) {
	syncReq := readGolden(t, "sync_request")
	framePastCap := []byte{0x00, 0x80, 0x10, 0x01}
	// Every fixture is built once, outside the counted call: what is counted
	// is the refusal itself, not the message it reads.
	short, badProtoID, badCommand, madStep, badSize := syncReq[:HeaderLen-1],
		patched(syncReq, 0, 0xfd, 0x53, 0x4d, 0x42),
		patched(syncReq, 12, 0x13, 0x00),
		patched(syncReq, 20, 0xf8, 0xff, 0xff, 0xff),
		patched(syncReq, 4, 0x3f, 0x00)
	for _, c := range []struct {
		name string
		want error
		refu func() error
	}{
		{"a frame one byte past the cap", errFrameTooLong, func() error {
			_, err := FrameLen(framePastCap)
			return err
		}},
		{"a header shorter than 64 bytes", errShort, func() error {
			_, err := DecodeHeader(short, 0)
			return err
		}},
		{"a protocol ID other than SMB2's", errProtocolID, func() error {
			_, err := DecodeHeader(badProtoID, 0)
			return err
		}},
		{"a command code nothing carries", errCommand, func() error {
			_, err := DecodeHeader(badCommand, 0)
			return err
		}},
		{"a NextCommand of 2^32 - 8", ErrNextCommand, func() error {
			_, err := DecodeHeader(madStep, 0)
			return err
		}},
		{"a StructureSize other than 64", errStructureSize, func() error {
			_, err := DecodeHeader(badSize, 0)
			return err
		}},
		{"a chain member outside the buffer", errChain, func() error {
			_, err := Chain(nil, 0, true)
			return err
		}},
	} {
		err := c.refu()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: refused with %v, want it wrapped in %v", c.name, err, c.want)
		}
		if prof.RaceEnabled {
			// Under -race the allocation count below reads memory the
			// detector allocates itself (sync primitives); the
			// adapter-allocs cell runs this without the detector.
			continue
		}
		if n := testing.AllocsPerRun(100, func() { _ = c.refu() }); n != 0 {
			t.Errorf("%s: refusing it allocated %v, want nothing (a buffer would be megabytes, or gigabytes, before its length was checked)", c.name, n)
		}
	}
}
