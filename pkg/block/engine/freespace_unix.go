//go:build !windows

package engine

import "syscall"

// freeSpace is the free space, in bytes, an unprivileged writer has on the
// filesystem holding dir.
func freeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}

	return int64(st.Bavail) * int64(st.Bsize), nil
}
