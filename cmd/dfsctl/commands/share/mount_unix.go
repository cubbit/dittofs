//go:build !windows

package share

import "runtime"

const (
	// Default file/dir modes: 0777 on macOS (can't set owner), 0755 on Linux (uid/gid works)
	defaultModeDarwin = "0777"
	defaultModeLinux  = "0755"
)

func getDefaultModeForPlatform() (string, string) {
	if runtime.GOOS == "darwin" {
		return defaultModeDarwin, "File permissions for SMB mount (octal, default 0777 on macOS since uid/gid not supported)"
	}
	return defaultModeLinux, "File permissions for SMB mount (octal)"
}
