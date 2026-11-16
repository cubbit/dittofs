//go:build !windows

package share

import "runtime"

// defaultMode is the SMB mount's file and directory mode: 0777 on macOS, which
// cannot set the mount's owner, and 0755 elsewhere, where uid and gid do.
func defaultMode() string {
	if runtime.GOOS == "darwin" {
		return "0777"
	}
	return "0755"
}
