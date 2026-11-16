//go:build windows

package share

func getDefaultModeForPlatform() (string, string) {
	return "", "File permissions (not applicable on Windows)"
}
