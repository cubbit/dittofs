package benchdir

import "testing"

// The magics are the kernel's, written out so a changed constant fails here.
func TestNameOfMagic(t *testing.T) {
	for _, tc := range []struct {
		magic uint32
		want  string
	}{
		{0x01021994, "tmpfs"},
		{0x858458f6, "ramfs"},
		{0xef53, "ext4"},
		{0x58465342, "xfs"},
		{0x9123683e, "btrfs"},
		{0x1234, "0x1234"},
	} {
		if got := nameOfMagic(tc.magic); got != tc.want {
			t.Errorf("nameOfMagic(%#x) = %q, want %q", tc.magic, got, tc.want)
		}
	}
}
