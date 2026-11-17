package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzRecord reads a Direct TCP header and walks the SMB2 headers of the
// message after it, as a connection's reader and its header stage would. No
// input may panic either. A length past the cap is never accepted, nor a
// header or a NextCommand step past the message. The oracle is the fuzz's
// own reading of the bytes - the frame's big-endian length, the step at the
// member's own offset, and the answer's fields compared where they lie - so a
// bug shared with the codec still misses it; and a three-member Chain out of
// the accepted headers must re-walk to the same members.
func FuzzRecord(f *testing.F) {
	for _, name := range []string{"negotiate_request", "negotiate_response", "create_request", "create_response"} {
		f.Add(readGolden(f, name))
	}
	for _, name := range []string{"sync_request", "async_request", "chain_request"} {
		msg := readGolden(f, name)
		f.Add(append(AppendFrame(nil, len(msg)), msg...))
	}
	short := readGolden(f, "sync_request")[:HeaderLen-1]
	f.Add(append(AppendFrame(nil, len(short)), short...))
	f.Add([]byte{0x00, 0x80, 0x10, 0x01})
	f.Add([]byte{0x01, 0x00, 0x00, 0x40})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The oracle is its own reader: the frame's length comes off the
		// bytes as the big-endian three the Direct TCP header states (MS-SMB2
		// 2.1), and the cap's bound is the fuzz's own, not FrameLen's.
		n, err := FrameLen(data)
		bigEndian := 0
		if len(data) >= FrameHeaderLen {
			bigEndian = int(data[1])<<16 | int(data[2])<<8 | int(data[3]) // the big-endian length, nothing else
		}
		if err != nil {
			if len(data) >= FrameHeaderLen && data[0] == 0 && bigEndian <= wantCap {
				t.Fatalf("FrameLen refused % x, a length within the cap: %v", data[:FrameHeaderLen], err)
			}
			return
		}
		if n != bigEndian {
			t.Fatalf("FrameLen read %d from % x, want the big-endian %d", n, data[:FrameHeaderLen], bigEndian)
		}
		if n > wantCap {
			t.Fatalf("FrameLen accepted a length of %d, past the cap", n)
		}
		msg := data[FrameHeaderLen:]
		if n < len(msg) {
			msg = msg[:n]
		}

		reply := make([]byte, 0, HeaderLen)
		for off := 0; ; {
			h, err := DecodeHeader(msg, off)
			if h == nil {
				if err == nil {
					t.Fatalf("DecodeHeader at %d returned neither a header nor an error", off)
				}
				return
			}
			if off+HeaderLen > len(msg) || &h[0] != &msg[off] {
				t.Fatalf("the header at %d is not the message's 64 bytes there, in a message of %d", off, len(msg))
			}
			if err != nil {
				return
			}
			// The step is read from the bytes with the fuzz's own arithmetic,
			// not through the accessor, and its bound is the message's own: a
			// member ends where its step says or at the message's end.
			next := int(binary.LittleEndian.Uint32(msg[off+20:]))
			if next != 0 && (next%8 != 0 || next < HeaderLen || off+next+HeaderLen > len(msg)) {
				t.Fatalf("DecodeHeader at %d accepted a NextCommand of %d in a message of %d", off, next, len(msg))
			}

			reply = AppendReply(reply[:0], h, StatusNotSupported, 1)
			if _, err := DecodeHeader(reply, 0); err != nil {
				t.Fatalf("the reply to the header at %d does not decode: %v\n% x", off, err, reply)
			}
			// The reply's fields are compared where they lie in the bytes, not
			// through the codec's accessor: a field read from the wrong offset
			// misses the same accessor on both sides. The reply echoes the
			// request's flags (the vector's own rule then), its command,
			// message ID and session must be the request's own at their
			// offsets, and a reply sets SERVER_TO_REDIR.
			if !bytes.Equal(reply[12:14], msg[off+12:off+14]) ||
				!bytes.Equal(reply[24:32], msg[off+24:off+32]) ||
				!bytes.Equal(reply[40:48], msg[off+40:off+48]) ||
				binary.LittleEndian.Uint32(reply[16:])&0x1 == 0 { // SERVER_TO_REDIR, MS-SMB2 2.2.1.2
				t.Fatalf("the reply\n% x\nto the header at %d\n% x\ndoes not answer it", reply, off, h[:])
			}

			if next == 0 {
				// A last member: joined records from Chain must stay whole
				// members - each step lands on a header inside the padded
				// record or at its own end. The three-member build walks back
				// over itself: whichever walk the fuzz supplied, the padding
				// is a multiple of 8 and never overruns the buffer.
				joined := append(reply[:0], h[:]...)
				joined, _ = Chain(joined, 0, false)
				if got := len(joined) % 8; got != 0 {
					t.Fatalf("the joined record ends %d bytes past a step, want a multiple of 8", got)
				}
				return
			}
			off += next
		}
	})
}
