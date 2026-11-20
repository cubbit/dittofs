package benchdir

import (
	"fmt"
	"syscall"
)

// The statfs types of the filesystems the suite tells apart, from the kernel's
// linux/magic.h.
const (
	magicTmpfs uint32 = 0x01021994
	magicRamfs uint32 = 0x858458f6
	magicExt   uint32 = 0xef53
	magicXFS   uint32 = 0x58465342
	magicBtrfs uint32 = 0x9123683e
)

// FSType is the name of the filesystem holding dir, from statfs's magic.
func FSType(dir string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}
	return nameOfMagic(uint32(st.Type)), nil
}

// nameOfMagic is the name of the filesystem whose statfs type is magic, or
// the magic in hex when the suite does not tell it apart.
func nameOfMagic(magic uint32) string {
	switch magic {
	case magicTmpfs:
		return "tmpfs"
	case magicRamfs:
		return "ramfs"
	case magicExt:
		return "ext4"
	case magicXFS:
		return "xfs"
	case magicBtrfs:
		return "btrfs"
	}
	return fmt.Sprintf("0x%x", magic)
}
