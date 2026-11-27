package kvstore

import "testing"

// Every v1 prefix starts with a lowercase letter, above this format's kind
// bytes (0x00 to 0x4C, and 0xFF), so a prefix match never names one of this
// format's records.
func TestV1PrefixesMissThisFormatsKinds(t *testing.T) {
	for _, p := range v1Prefixes {
		if p[0] < 'a' || p[0] > 'z' {
			t.Errorf("v1 prefix %q starts with 0x%02x, want a lowercase letter, which no kind of this format is", p, p[0])
		}
	}
}
