package testproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/resources"
)

// The vectors are written by hand from the package doc's format, byte by
// byte, so they check the encoder and the decoder against the format and not
// against each other.

// echoVector is one Echo under no session: version 1, message 7, one credit,
// role 1 (context), payload "hi".
var echoVector = []byte{
	0x00, 0x00, 0x00, 0x2b, // the body's length: 43
	0x00, 0x00, 0x00, 0x00, // next: the last
	0x00,       // flags
	0x01,       // version
	0x00, 0x01, // credits
	0x00, 0x00, 0x00, 0x06, // operation: Echo
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, // message ID
	0x00, 0x00, 0x00, 0x00, // session
	0x00, 0x00, 0x00, 0x00, // tree
	0x00, 0x00, 0x00, 0x00, // sequence
	0x00, 0x00, 0x00, 0x00, // status
	0x00, 0x00, 0x00, 0x00, // checksum: none under session 0
	0x01,       // role
	0x68, 0x69, // "hi"
}

var echoMsgs = []Msg{{Op: OpEcho, Version: 1, ID: 7, Credits: 1, Role: core.RoleContext, Payload: []byte("hi")}}

// chainVector is two messages under session 5, whose key is 05 00 07 df: a
// Create of "a" in tree 9, charging 2 credits at sequence 3, then a related,
// pending Echo that inherits the session.
var chainVector = []byte{
	0x00, 0x00, 0x00, 0x52, // the body's length: 82
	0x00, 0x00, 0x00, 0x29, // next: 41
	0x00,       // flags
	0x02,       // version
	0x00, 0x02, // credits
	0x00, 0x00, 0x00, 0x07, // operation: Create
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // message ID
	0x00, 0x00, 0x00, 0x05, // session
	0x00, 0x00, 0x00, 0x09, // tree
	0x00, 0x00, 0x00, 0x03, // sequence
	0x00, 0x00, 0x00, 0x00, // status
	0x05, 0x00, 0x08, 0xac, // checksum: 0xcd of bytes, plus 5 times 0x01000193
	0x64, // "a", 0x61, XORed with the key's 0x05

	0x00, 0x00, 0x00, 0x00, // next: the last
	0x03,       // flags: related, pend
	0x02,       // version
	0x00, 0x00, // credits
	0x00, 0x00, 0x00, 0x06, // operation: Echo
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x09, // message ID
	0xff, 0xff, 0xff, 0xff, // session: inherited
	0x00, 0x00, 0x00, 0x09, // tree
	0x00, 0x00, 0x00, 0x00, // sequence
	0x00, 0x00, 0x00, 0x00, // status
	0x05, 0x00, 0x0c, 0x19, // checksum: 0x43a of bytes, plus 5 times 0x01000193
	0x05, // role 0, XORed with the key's 0x05
}

// nameVector is one Create of "name" under session 5: four bytes of
// arguments, so each byte of the key 05 00 07 df seals one.
var nameVector = []byte{
	0x00, 0x00, 0x00, 0x2c, // the body's length: 44
	0x00, 0x00, 0x00, 0x00, // next: the last
	0x00,       // flags
	0x02,       // version
	0x00, 0x01, // credits
	0x00, 0x00, 0x00, 0x07, // operation: Create
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x09, // message ID
	0x00, 0x00, 0x00, 0x05, // session
	0x00, 0x00, 0x00, 0x01, // tree
	0x00, 0x00, 0x00, 0x00, // sequence
	0x00, 0x00, 0x00, 0x00, // status
	0x05, 0x00, 0x09, 0xe8, // checksum: 0x209 of bytes, plus 5 times 0x01000193
	0x6b, 0x61, 0x6a, 0xba, // "name", 6e 61 6d 65, XORed with 05 00 07 df
}

var nameMsgs = []Msg{{Op: OpCreate, Version: 2, ID: 9, Session: 5, Tree: 1, Credits: 1, Name: "name"}}

var chainMsgs = []Msg{
	{Op: OpCreate, Version: 2, ID: 0x0102030405060708, Session: 5, Tree: 9, Credits: 2, Seq: 3, Name: "a"},
	{Op: OpEcho, Version: 2, ID: 0x0102030405060709, Session: inherited, Tree: 9, Related: true, Pend: true},
}

// replyVector is two replies to message 7 in one record: an interim granting
// 3 credits, then the final one, status 13, granting 1, with the body "ok".
var replyVector = []byte{
	0x00, 0x00, 0x00, 0x52, // the body's length: 82
	0x00, 0x00, 0x00, 0x28, // next: 40
	0x0c,       // flags: reply, interim
	0x02,       // version
	0x00, 0x03, // credits
	0x00, 0x00, 0x00, 0x06, // operation: Echo
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, // message ID
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // session, tree
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // sequence, status
	0x00, 0x00, 0x00, 0x00, // checksum

	0x00, 0x00, 0x00, 0x00, // next: the last
	0x04,       // flags: reply
	0x02,       // version
	0x00, 0x01, // credits
	0x00, 0x00, 0x00, 0x06, // operation: Echo
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, // message ID
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // session, tree
	0x00, 0x00, 0x00, 0x00, // sequence
	0x00, 0x00, 0x00, 0x0d, // status: 13
	0x00, 0x00, 0x00, 0x00, // checksum
	0x6f, 0x6b, // "ok"
}

// fields is a header's every field, for comparing a decode with what a
// vector was written from.
type fields struct {
	next     int
	flags    uint8
	version  core.Version
	credits  uint16
	op       uint32
	id       uint64
	session  uint32
	tree     uint32
	seq      uint32
	status   uint32
	checksum uint32
	args     string
}

// decodeAll decodes every message of a record's body through the codec.
func decodeAll(t *testing.T, body []byte) []fields {
	t.Helper()
	rec := &resources.Buf{B: body}
	var out []fields
	for off := 0; ; {
		ch, err := headerCodec{}.Decode(rec, off)
		if err != nil {
			t.Fatalf("Decode at %d: %v", off, err)
		}
		h := ch.(*header)
		if h.Related() != (h.flags()&flagRelated != 0) {
			t.Errorf("Related at %d disagrees with the flags %#x", off, h.flags())
		}
		out = append(out, fields{
			ch.Next(), h.flags(), ch.Version(), h.credits(), ch.Op(), h.id(), h.session(), h.tree(),
			h.seq(), h.status(), h.checksum(), string(arguments(body, off, h)),
		})
		if off = ch.Next(); off == 0 {
			return out
		}
	}
}

func TestVectorsDecodeToTheirFields(t *testing.T) {
	for _, c := range []struct {
		name   string
		vector []byte
		want   []fields
	}{
		{"echo", echoVector, []fields{
			{0, 0, 1, 1, OpEcho, 7, 0, 0, 0, 0, 0, "\x01hi"},
		}},
		{"chain", chainVector, []fields{
			{41, 0, 2, 2, OpCreate, 0x0102030405060708, 5, 9, 3, 0, 0x050008ac, "d"},
			{0, flagRelated | flagPend, 2, 0, OpEcho, 0x0102030405060709, inherited, 9, 0, 0, 0x05000c19, "\x05"},
		}},
		{"replies", replyVector, []fields{
			{40, flagReply | flagInterim, 2, 3, OpEcho, 7, 0, 0, 0, 0, 0, ""},
			{0, flagReply, 2, 1, OpEcho, 7, 0, 0, 0, 13, 0, "ok"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, err := framer{}.BodyLen(c.vector[:lengthLen])
			if err != nil || n != len(c.vector)-lengthLen {
				t.Fatalf("BodyLen = %d, %v; want %d", n, err, len(c.vector)-lengthLen)
			}
			got := decodeAll(t, c.vector[lengthLen:])
			if len(got) != len(c.want) {
				t.Fatalf("decoded %d messages, want %d", len(got), len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("message %d:\n got %+v\nwant %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestAppendEncodesToTheVectors(t *testing.T) {
	for _, c := range []struct {
		name   string
		msgs   []Msg
		vector []byte
	}{
		{"echo", echoMsgs, echoVector},
		{"chain", chainMsgs, chainVector},
		{"name", nameMsgs, nameVector},
	} {
		if got := Append(nil, c.msgs...); !bytes.Equal(got, c.vector) {
			t.Errorf("%s:\n got % x\nwant % x", c.name, got, c.vector)
		}
	}
	if got := Append([]byte{0xee}, echoMsgs...); !bytes.Equal(got[1:], echoVector) || got[0] != 0xee {
		t.Errorf("Append after a byte already there:\n got % x", got)
	}
}

func TestBodyLenRefusesPastTheCap(t *testing.T) {
	if n, err := (framer{}).BodyLen([]byte{0x00, 0x10, 0x00, 0x00}); err != nil || n != 1<<20 {
		t.Errorf("BodyLen at the cap = %d, %v; want 1 MiB", n, err)
	}
	if _, err := (framer{}).BodyLen([]byte{0x00, 0x10, 0x00, 0x01}); !errors.Is(err, errTooLong) {
		t.Errorf("BodyLen one byte past the cap = %v, want a refusal", err)
	}
	if _, err := (framer{}).BodyLen([]byte{0x00, 0x10}); err == nil {
		t.Error("BodyLen of a 2-byte prefix was accepted")
	}
}

func TestDecodeRefusesAHeaderOrNextOutsideTheRecord(t *testing.T) {
	body := func(next uint32, n int) *resources.Buf {
		b := make([]byte, n)
		binary.BigEndian.PutUint32(b, next)
		return &resources.Buf{B: b}
	}
	for _, c := range []struct {
		name string
		rec  *resources.Buf
		off  int
		want error
	}{
		{"a body shorter than a header", body(0, headerLen-1), 0, errShort},
		{"an offset whose header runs past the end", body(0, 2*headerLen), headerLen + 1, errShort},
		{"a negative offset", body(0, headerLen), -1, errShort},
		{"a next inside its own header", body(headerLen-1, 3*headerLen), 0, errNext},
		{"a next that leaves no room for a header", body(headerLen+1, 2*headerLen), 0, errNext},
		{"a next past the end", body(1<<31, 2*headerLen), 0, errNext},
	} {
		if _, err := (headerCodec{}).Decode(c.rec, c.off); !errors.Is(err, c.want) {
			t.Errorf("%s: Decode = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := (headerCodec{}).Decode(body(headerLen, 2*headerLen), 0); err != nil {
		t.Errorf("a next right after its header, with a whole header left: %v", err)
	}
}

// A reply's header carries its request's operation, version, IDs, sequence
// and credits, the reply flag and the status, and Chain points it at the next.
func TestEncodeAndChainAReply(t *testing.T) {
	req, err := headerCodec{}.Decode(&resources.Buf{B: chainVector[lengthLen:]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := &resources.Buf{B: make([]byte, 0, 4*headerLen)}
	if err := (headerCodec{}).Encode(req, 13, out); err != nil {
		t.Fatal(err)
	}
	if err := (headerCodec{}).Chain(out, 0); err != nil {
		t.Fatal(err)
	}
	if err := (headerCodec{}).Encode(req, 0, out); err != nil {
		t.Fatal(err)
	}
	got := decodeAll(t, out.B)
	want := []fields{
		{40, flagReply, 2, 2, OpCreate, 0x0102030405060708, 5, 9, 3, 13, 0, ""},
		{0, flagReply, 2, 2, OpCreate, 0x0102030405060708, 5, 9, 3, 0, 0, ""},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the two replies:\n got %+v\nwant %+v", got, want)
	}
	// A reply to a related, pending request carries the reply flag alone.
	related, err := headerCodec{}.Decode(&resources.Buf{B: chainVector[lengthLen:]}, 41)
	if err != nil {
		t.Fatal(err)
	}
	out2 := &resources.Buf{}
	if err := (headerCodec{}).Encode(related, 0, out2); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, out2.B)[0]; got.flags != flagReply || got.credits != 1 {
		t.Errorf("the reply to a related, pending request that charged nothing has flags %#x and grants %d; want the reply flag alone and 1", got.flags, got.credits)
	}
	if err := (headerCodec{}).Chain(out, len(out.B)); !errors.Is(err, errShort) {
		t.Errorf("Chain at an offset with no header = %v, want a refusal", err)
	}
}

// loopback returns the two ends of a TCP connection on 127.0.0.1.
func loopback(t *testing.T) (client, server net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	client, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	deadline := time.Now().Add(10 * time.Second) // a backstop: a peer that never answers fails the read
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)

	return client, server
}

// The client writes exactly the bytes Append gives, and its balance is the
// credits granted, an interim's counted once, less the credits charged.
func TestClientWritesTheRecordAndKeepsTheBalance(t *testing.T) {
	conn, peer := loopback(t)
	c := NewClient(conn)
	if got := c.Balance(); got != 1 {
		t.Fatalf("a new client's balance is %d, want 1", got)
	}

	if err := c.Send(chainMsgs...); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(chainVector))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, chainVector) {
		t.Errorf("the client wrote:\n got % x\nwant % x", got, chainVector)
	}
	if got := c.Balance(); got != -1 {
		t.Errorf("after charging 2 credits the balance is %d, want -1", got)
	}

	if _, err := peer.Write(replyVector); err != nil {
		t.Fatal(err)
	}
	replies, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	want := []Reply{
		{ID: 7, Interim: true, Credits: 3, Body: []byte{}},
		{ID: 7, Status: 13, Credits: 1, Body: []byte("ok")},
	}
	if len(replies) != 2 {
		t.Fatalf("Recv returned %d replies, want 2", len(replies))
	}
	for i, r := range replies {
		w := want[i]
		if r.ID != w.ID || r.Status != w.Status || r.Interim != w.Interim || r.Credits != w.Credits || !bytes.Equal(r.Body, w.Body) {
			t.Errorf("reply %d = %+v, want %+v", i, r, w)
		}
	}
	if got := c.Balance(); got != 3 {
		t.Errorf("after grants of 3 and 1 the balance is %d, want 3", got)
	}

	// Every message of a record is charged, not the first alone.
	if err := c.Send(Msg{Op: OpEcho, Version: 2, Credits: 1}, Msg{Op: OpEcho, Version: 2, Credits: 2}); err != nil {
		t.Fatal(err)
	}
	if got := c.Balance(); got != 0 {
		t.Errorf("after charging 1 and 2 in one record the balance is %d, want 0", got)
	}

	if _, err := peer.Write([]byte{0x00, 0x10, 0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recv(); !errors.Is(err, errTooLong) {
		t.Errorf("Recv of a record past the cap = %v, want a refusal", err)
	}
}

// Header decode and encode allocate nothing: the header is read in place and
// the reply's is appended into a buffer with room.
func TestHeaderDecodeAndEncodeAllocateNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	rec := &resources.Buf{B: chainVector[lengthLen:]}
	out := &resources.Buf{B: make([]byte, 0, headerLen)}
	var codec headerCodec
	allocs := testing.AllocsPerRun(100, func() {
		h, err := codec.Decode(rec, 0)
		if err != nil {
			t.Fatal(err)
		}
		out.B = out.B[:0]
		if err := codec.Encode(h, 1, out); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("a header decode and encode allocate %v times, want 0", allocs)
	}
}

// FuzzFrame: no input panics the framer or the header decoder, a length past
// the cap is refused, every message decoded lies inside the body, and a
// header that decodes re-encodes, as its reply, to bytes that decode to the
// same fields.
func FuzzFrame(f *testing.F) {
	for _, v := range [][]byte{echoVector, chainVector, replyVector, {0x00, 0x10, 0x00, 0x01}, {}} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, record []byte) {
		if len(record) < lengthLen {
			if _, err := (framer{}).BodyLen(record); err == nil {
				t.Fatal("a prefix shorter than 4 bytes was accepted")
			}
			return
		}
		n, err := framer{}.BodyLen(record[:lengthLen])
		if err != nil {
			if binary.BigEndian.Uint32(record) <= maxBody {
				t.Fatalf("a length within the cap was refused: %v", err)
			}
			return
		}
		if n > maxBody {
			t.Fatalf("a length of %d past the cap was accepted", n)
		}
		body := record[lengthLen:]
		if n < len(body) {
			body = body[:n]
		}
		rec := &resources.Buf{B: body}
		out := &resources.Buf{}
		for off, steps := 0, 0; ; steps++ {
			if steps > len(body)/headerLen {
				t.Fatalf("the chain has more messages than the body has room for")
			}
			ch, err := headerCodec{}.Decode(rec, off)
			if err != nil {
				return
			}
			h := ch.(*header)
			if off+headerLen > len(body) {
				t.Fatalf("a header at %d was decoded past a body of %d bytes", off, len(body))
			}
			out.B = out.B[:0]
			if err := (headerCodec{}).Encode(ch, core.Status(h.status()), out); err != nil {
				t.Fatal(err)
			}
			rh, err := headerCodec{}.Decode(out, 0)
			if err != nil {
				t.Fatalf("the reply's header does not decode: %v", err)
			}
			r := rh.(*header)
			if r.Op() != h.Op() || r.Version() != h.Version() || r.id() != h.id() || r.session() != h.session() ||
				r.tree() != h.tree() || r.seq() != h.seq() || r.credits() != max(h.credits(), 1) || r.status() != h.status() {
				t.Fatalf("a header re-encoded to different fields:\n got % x\nfrom % x", r[:], h[:])
			}
			next := ch.Next()
			if next == 0 {
				return
			}
			if next <= off {
				t.Fatalf("a next of %d at %d does not move forwards", next, off)
			}
			off = next
		}
	})
}

// A Cancel names its target by carrying the target's message ID as its own,
// and has no arguments.
func TestACancelCarriesItsTargetsID(t *testing.T) {
	got := decodeAll(t, Append(nil, Msg{Op: OpCancel, Version: 2, ID: 1, Target: 77})[lengthLen:])
	if len(got) != 1 || got[0].id != 77 || got[0].op != OpCancel || got[0].args != "" {
		t.Errorf("a Cancel of message 77 encodes to %+v", got)
	}
}
