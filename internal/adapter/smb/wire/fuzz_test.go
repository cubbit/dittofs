package wire

import (
	"encoding/binary"
	"testing"
)

// FuzzRecord reads a Direct TCP header and walks the SMB2 headers of the
// message after it, as a connection's reader and its header stage would. No
// input may panic either. A length past the cap is never accepted, nor a
// header or a NextCommand step past the message. An accepted header's reply,
// from AppendReply, decodes as a reply carrying its command, message ID and
// session.
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
		n, err := FrameLen(data)
		if err != nil {
			if len(data) >= FrameHeaderLen && data[0] == 0 && int(data[1])<<16|int(data[2])<<8|int(data[3]) <= wantCap {
				t.Fatalf("FrameLen refused % x, a length within the cap: %v", data[:FrameHeaderLen], err)
			}
			return
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
			next := int(h.NextCommand())
			if next != 0 && (next%8 != 0 || next < HeaderLen || off+next+HeaderLen > len(msg)) {
				t.Fatalf("DecodeHeader at %d accepted a NextCommand of %d in a message of %d", off, next, len(msg))
			}

			reply = AppendReply(reply[:0], h, StatusNotSupported, 1)
			r, err := DecodeHeader(reply, 0)
			if err != nil {
				t.Fatalf("the reply to the header at %d does not decode: %v\n% x", off, err, reply)
			}
			if r.Command() != h.Command() || r.MessageID() != h.MessageID() || r.SessionID() != h.SessionID() ||
				binary.LittleEndian.Uint32(reply[16:])&0x1 == 0 { // SERVER_TO_REDIR, MS-SMB2 2.2.1.2
				t.Fatalf("the reply\n% x\nto the header at %d\n% x\ndoes not answer it", reply, off, h[:])
			}

			if next == 0 {
				return
			}
			off += next
		}
	})
}
