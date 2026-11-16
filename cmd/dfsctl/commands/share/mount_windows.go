//go:build windows

package share

// defaultMode is empty: a Windows mount takes no file or directory mode.
func defaultMode() string { return "" }
