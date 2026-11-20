package benchdir

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestNameOfMagic(t *testing.T) {
	for _, tc := range []struct {
		magic uint32
		want  string
	}{
		{unix.TMPFS_MAGIC, "tmpfs"},
		{unix.RAMFS_MAGIC, "ramfs"},
		{unix.EXT4_SUPER_MAGIC, "ext4"},
		{unix.XFS_SUPER_MAGIC, "xfs"},
		{unix.BTRFS_SUPER_MAGIC, "btrfs"},
		{0x1234, "0x1234"},
	} {
		if got := nameOfMagic(tc.magic); got != tc.want {
			t.Errorf("nameOfMagic(%#x) = %q, want %q", tc.magic, got, tc.want)
		}
	}
}
