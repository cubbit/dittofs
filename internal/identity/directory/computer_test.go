package directory

import "testing"

// unicodePwd quotes the password and encodes it UTF-16LE, a code unit per
// character and a surrogate pair beyond the basic plane.
func TestUnicodePwd(t *testing.T) {
	for _, tc := range []struct {
		password string
		want     string
	}{
		{"ab", "\"\x00a\x00b\x00\"\x00"},
		{"é", "\"\x00\xe9\x00\"\x00"},
		{"𝄞", "\"\x00\x34\xd8\x1e\xdd\"\x00"},
	} {
		if got := unicodePwd([]byte(tc.password)); got != tc.want {
			t.Errorf("unicodePwd(%q) = %x, want %x", tc.password, got, tc.want)
		}
	}
}
