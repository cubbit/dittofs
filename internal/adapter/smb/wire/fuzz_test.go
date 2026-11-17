package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// framed is a golden vector with the Direct TCP header the writer states for
// it in front.
func framed(f *testing.F, name string) []byte {
	msg := readGolden(f, name)
	fr, err := AppendFrame(nil, len(msg))
	if err != nil {
		f.Fatalf("the golden %s does not frame: %v", name, err)
	}
	return append(fr, msg...)
}

// framedShort is a frame around a header the message cut short.
func framedShort(f *testing.F) []byte {
	short := readGolden(f, "sync_request")[:HeaderLen-1]
	fr, err := AppendFrame(nil, len(short))
	if err != nil {
		f.Fatalf("a cut record does not frame: %v", err)
	}
	return append(fr, short...)
}

// FuzzRecord reads a Direct TCP header and walks the SMB2 headers of the
// message after it, as a connection's reader and its header stage would. No
// input may panic either. A length past the cap is never accepted, nor a
// header or a NextCommand step past the message. The oracle is the fuzz's
// own reading of the bytes - the frame's big-endian length, the gates an
// accepted header passed, the step at the member's own offset, and the
// answer's fields compared where they lie - so a bug shared with the codec
// still misses it; and the chain the runner would answer - every member's
// reply appended, padded and pointed - must re-walk to the same members.
func FuzzRecord(f *testing.F) {
	for _, name := range []string{"negotiate_request", "negotiate_response", "create_request", "create_response"} {
		f.Add(readGolden(f, name))
	}
	for _, name := range []string{"sync_request", "async_request", "chain_request"} {
		f.Add(framed(f, name))
	}
	f.Add(framedShort(f))
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

		reply := make([]byte, 0, 80)
		var chain []byte  // every member's answer, padded and pointed, as the runner builds it
		var reqOffs []int // the message's member each of those answers stands for
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
			// The three gates an ACCEPTED header passed - one returned malformed
			// but answerable is the codec refusing as it should - read off the
			// bytes with the fuzz's own comparisons, not the codec's: a gate
			// deleted in DecodeHeader still dies here (SMB2's 0xFE 'S' 'M' 'B',
			// the command bound, the 64-byte structure size).
			next := 0
			if err == nil {
				if binary.LittleEndian.Uint32(msg[off:]) != 0x424D53FE ||
					binary.LittleEndian.Uint16(msg[off+12:]) > 0x12 ||
					binary.LittleEndian.Uint16(msg[off+4:]) != 64 {
					t.Fatalf("DecodeHeader accepted the header at %d\n% x", off, msg[off:off+HeaderLen])
				}
				// The step is read from the bytes with the fuzz's own
				// arithmetic, not through the accessor, and its bound is the
				// message's own: a member ends where its step says or at the
				// message's end.
				next = int(binary.LittleEndian.Uint32(msg[off+20:]))
				if next != 0 && (next%8 != 0 || next < HeaderLen || off+next+HeaderLen > len(msg)) {
					t.Fatalf("DecodeHeader at %d accepted a NextCommand of %d in a message of %d", off, next, len(msg))
				}
			}

			reply = AppendReply(reply[:0], h, StatusNotSupported, 1)
			if _, err := DecodeHeader(reply, 0); err != nil {
				t.Fatalf("the reply to the header at %d does not decode: %v\n% x", off, err, reply)
			}
			// The reply's fields are compared where they lie in the bytes, not
			// through the codec's accessor: a field read from the wrong offset
			// misses the same accessor on both sides. The reply echoes the
			// request's command, message ID and session at their offsets, and
			// carries SERVER_TO_REDIR with exactly the bits a reply echoes
			// (MS-SMB2 2.2.1.1, 3.3.4.4): the related, priority, DFS and REPLAY
			// bits and none else - not ASYNC_COMMAND and not SIGNED.
			wantFlags := uint32(0x1) | binary.LittleEndian.Uint32(msg[off+16:])&0x30000074
			if !bytes.Equal(reply[12:14], msg[off+12:off+14]) ||
				!bytes.Equal(reply[24:32], msg[off+24:off+32]) ||
				!bytes.Equal(reply[40:48], msg[off+40:off+48]) ||
				binary.LittleEndian.Uint32(reply[16:]) != wantFlags {
				t.Fatalf("the reply\n% x\nto the header at %d\n% x\ndoes not answer it: flags are %#x, want %#x",
					reply, off, h[:], binary.LittleEndian.Uint32(reply[16:]), wantFlags)
			}

			// The runner's answer joins every member's reply into one
			// record: appended, padded to its step and pointed at the next,
			// the last padded alone. A malformed-but-answerable member ends
			// the chain, its own reply last.
			at := len(chain)
			chain = AppendReply(chain, h, StatusNotSupported, 1)
			chain = AppendError(chain)
			reqOffs = append(reqOffs, off)
			if err != nil || next == 0 {
				chain, cerr := Chain(chain, at, false)
				if cerr != nil {
					t.Fatalf("the answer chain of %d members does not close: %v", len(reqOffs), cerr)
				}
				reWalk(t, chain, msg, reqOffs)
				return
			}
			var cerr error
			chain, cerr = Chain(chain, at, true)
			if cerr != nil {
				t.Fatalf("the answer chain of %d members does not step: %v", len(reqOffs), cerr)
			}
			off += next
		}
	})
}

// reWalk reads the runner's answer record back with the codec's own reader
// and checks it against the message its members answered: the same count,
// every step a multiple of 8 past a whole member, and each reply carrying the
// command, message ID and session of the member it stands for.
func reWalk(t *testing.T, chain, msg []byte, reqOffs []int) {
	t.Helper()
	for i, at := 0, 0; ; i, at = i+1, at+80 {
		if i >= len(reqOffs) || at+HeaderLen > len(chain) {
			t.Fatalf("the answer record re-walks %d members with %d answered", i, len(reqOffs))
		}
		if _, err := DecodeHeader(chain, at); err != nil {
			t.Fatalf("the answer record's member %d does not decode: %v", i, err)
		}
		if !bytes.Equal(chain[at+12:at+14], msg[reqOffs[i]+12:reqOffs[i]+14]) ||
			!bytes.Equal(chain[at+24:at+32], msg[reqOffs[i]+24:reqOffs[i]+32]) ||
			!bytes.Equal(chain[at+40:at+48], msg[reqOffs[i]+40:reqOffs[i]+48]) {
			t.Fatalf("the answer record's reply %d\n% x\ndoes not answer the member\n% x",
				i, chain[at:at+HeaderLen], msg[reqOffs[i]:reqOffs[i]+HeaderLen])
		}
		step := int(binary.LittleEndian.Uint32(chain[at+20:]))
		if step == 0 {
			if i != len(reqOffs)-1 {
				t.Fatalf("the answer record ends at member %d with %d still unanswered", i, len(reqOffs)-1-i)
			}
			return
		}
		if step != 80 {
			t.Fatalf("the answer record's member %d steps %d, want the 80 of a padded reply", i, step)
		}
	}
}
