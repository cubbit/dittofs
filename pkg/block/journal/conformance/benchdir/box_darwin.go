package benchdir

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// platformBox reads what statfs and sysctl give cheaply: the device and
// filesystem of dir, the memory and the kernel. The mount options are left
// empty.
func platformBox(dir string) Box {
	var b Box

	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err == nil {
		b.Device = unix.ByteSliceToString(st.Mntfromname[:])
		b.Filesystem = unix.ByteSliceToString(st.Fstypename[:])
	}

	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		b.Memory = fmt.Sprintf("%d kB", mem/1024)
	}

	if rel, err := unix.Sysctl("kern.osrelease"); err == nil {
		b.Kernel = "darwin " + rel
	}

	return b
}
