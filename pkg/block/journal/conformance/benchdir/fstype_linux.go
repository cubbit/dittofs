package benchdir

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FSType is the name of the filesystem holding dir, from statfs's magic.
func FSType(dir string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", err
	}
	switch uint32(st.Type) {
	case unix.TMPFS_MAGIC:
		return "tmpfs", nil
	case unix.RAMFS_MAGIC:
		return "ramfs", nil
	case unix.EXT4_SUPER_MAGIC:
		return "ext4", nil
	case unix.XFS_SUPER_MAGIC:
		return "xfs", nil
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs", nil
	}
	return fmt.Sprintf("0x%x", uint32(st.Type)), nil
}
