package sid

import (
	"bytes"
	"testing"
)

// FuzzSID decodes SIDs as they arrive off the wire. What decodes never
// claims more bytes than it was given, allocates at most what those bytes
// hold, and re-encodes to exactly the bytes it consumed, which decode to the
// same SID.
func FuzzSID(f *testing.F) {
	for _, s := range []string{"S-1-1-0", "S-1-5-7", "S-1-5-32-544", "S-1-22-1-0", "S-1-5-21-1-2-3-1107", "S-1-0-0"} {
		var b bytes.Buffer
		EncodeSID(&b, ParseSIDMust(s))
		f.Add(b.Bytes())
		f.Add(append(b.Bytes(), 0xff)) // trailing bytes past the SID
	}
	f.Add([]byte{1, 255, 0, 0, 0, 0, 0, 5}) // claims 255 sub-authorities, holds none
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d, n, err := DecodeSID(data)
		if err != nil {
			return
		}
		if n > len(data) || n != SIDSize(d) {
			t.Fatalf("DecodeSID(% x) consumed %d of %d bytes, size %d", data, n, len(data), SIDSize(d))
		}
		if len(d.SubAuthorities) != int(d.SubAuthorityCount) || 4*cap(d.SubAuthorities) > len(data) {
			t.Fatalf("DecodeSID(% x): %d sub-authorities of cap %d for count %d from %d bytes", data, len(d.SubAuthorities), cap(d.SubAuthorities), d.SubAuthorityCount, len(data))
		}
		var b bytes.Buffer
		EncodeSID(&b, d)
		if !bytes.Equal(b.Bytes(), data[:n]) {
			t.Fatalf("EncodeSID(DecodeSID(% x)) = % x, want the consumed bytes", data[:n], b.Bytes())
		}
		again, _, err := DecodeSID(b.Bytes())
		if err != nil || !again.Equal(d) {
			t.Fatalf("decoding the re-encoded %s = %v, %v", FormatSID(d), again, err)
		}
	})
}
