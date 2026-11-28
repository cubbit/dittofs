package provider

import (
	"bytes"
	"testing"
)

// FuzzKMIPWrap holds the key-service wrap bytes' reader, which reads what the
// metadata store gives back: it never panics, every field it returns is the
// input's own bytes in order, and an input it accepts re-encodes to itself,
// since the join has no padding or reserved bytes.
func FuzzKMIPWrap(f *testing.F) {
	s, err := parseStoredWrap(mustHex(f, kmipVectorStored))
	if err != nil {
		f.Fatal(err)
	}
	for n := 0; n <= len(s.wrap); n += 7 {
		f.Add(s.wrap[:n])
	}
	f.Add(s.wrap)
	f.Add([]byte{1, 0xff, 0xff, 1, 2, 3})

	f.Fuzz(func(t *testing.T, b []byte) {
		iv, tag, data, err := parseKMIPWrap(b)
		if err != nil {
			return
		}
		if 3+len(iv)+len(tag)+len(data) != len(b) ||
			!bytes.Equal(iv, b[3:3+len(iv)]) || !bytes.Equal(tag, b[3+len(iv):3+len(iv)+len(tag)]) || !bytes.Equal(data, b[3+len(iv)+len(tag):]) {
			t.Fatalf("parseKMIPWrap(%x) = IV %x, tag %x, data %x, not the input's bytes in order", b, iv, tag, data)
		}
		again, err := encodeKMIPWrap(iv, tag, data)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("re-encoding parseKMIPWrap(%x) = %x, %v; want the input", b, again, err)
		}
	})
}
