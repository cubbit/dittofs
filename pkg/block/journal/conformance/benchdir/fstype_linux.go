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
	return nameOfMagic(uint32(st.Type)), nil
}

// nameOfMagic is the name of the filesystem whose statfs type is magic, or
// the magic in hex when the suite does not tell it apart.
func nameOfMagic(magic uint32) string {
	switch magic {
	case unix.TMPFS_MAGIC:
		return "tmpfs"
	case unix.RAMFS_MAGIC:
		return "ramfs"
	case unix.EXT4_SUPER_MAGIC:
		return "ext4"
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs"
	}
	return fmt.Sprintf("0x%x", magic)
}
