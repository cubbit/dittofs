package bootstrap

import (
	"bytes"
	"testing"
)

// FuzzNodeID checks the node-id parser on any bytes: it never panics, and
// what it accepts re-encodes to the same bytes, so no input parses that the
// writer would not have written. The golden vector, written by hand, catches
// a layout bug the writer and the parser share.
func FuzzNodeID(f *testing.F) {
	flipped := bytes.Clone(golden)
	flipped[22] ^= 0x01

	magic := bytes.Clone(golden)
	magic[3] = '2'

	for _, seed := range [][]byte{
		golden,
		nil,
		golden[:20],
		golden[:23],
		append(bytes.Clone(golden), 0),
		magic,
		flipped,
		zeroIdentity(),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		id, err := parseNodeID(b)
		if err != nil {
			return
		}

		if got := encodeNodeID(id); !bytes.Equal(got, b) {
			t.Fatalf("parse(%x) = %x, which encodes to %x", b, id, got)
		}
	})
}
