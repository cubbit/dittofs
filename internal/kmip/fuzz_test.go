package kmip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzReadMessage holds the frame read to an independent reading of the
// header: an input holding a whole frame whose declared length is within the
// cap reads as exactly that frame, a prefix of the input; any other input is
// refused. Nothing panics, and no read runs past the frame.
func FuzzReadMessage(f *testing.F) {
	f.Add(frame(16, 0))
	f.Add(frame(3, 5))
	f.Add([]byte{0x42, 0x00, 0x7B, 0x01})
	over := make([]byte, headerLen)
	binary.BigEndian.PutUint32(over[4:8], maxMessageSize+1)
	f.Add(over)
	f.Fuzz(func(t *testing.T, in []byte) {
		r := bytes.NewReader(in)
		got, err := readMessage(r)

		want := -1
		if len(in) >= headerLen {
			if n := uint64(binary.BigEndian.Uint32(in[4:8])); n <= maxMessageSize {
				if total := headerLen + n + (8-n%8)%8; total <= uint64(len(in)) {
					want = int(total)
				}
			}
		}
		if want < 0 {
			if err == nil {
				t.Fatalf("readMessage accepted %d bytes of %x, want a refusal", len(got), in)
			}
			return
		}
		if err != nil {
			t.Fatalf("readMessage refused a whole %d-byte frame: %v", want, err)
		}
		if !bytes.Equal(got, in[:want]) {
			t.Fatalf("readMessage returned %x, want the frame %x", []byte(got), in[:want])
		}
		if r.Len() != len(in)-want {
			t.Fatalf("readMessage left %d bytes, want %d after the frame", r.Len(), len(in)-want)
		}
	})
}
