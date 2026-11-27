package benchdir

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// platformBox reads what statfs and sysctl give cheaply: the device and
// filesystem of dir, the memory and the kernel. The mount options are left
// empty.
func platformBox(dir string) Box {
	var b Box

	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		b.Device = cString(st.Mntfromname[:])
		b.Filesystem = cString(st.Fstypename[:])
	}

	// The standard library reads a sysctl as text only, which a 64-bit
	// number is not, so the memory comes from the sysctl tool.
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if mem, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			b.Memory = fmt.Sprintf("%d kB", mem/1024)
		}
	}

	if rel, err := syscall.Sysctl("kern.osrelease"); err == nil {
		b.Kernel = "darwin " + rel
	}

	return b
}
