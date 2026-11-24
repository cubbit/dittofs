//go:build !linux

package benchdir

import "os"

// flush makes f's data durable with the platform's own sync, F_FULLFSYNC on
// macOS.
func flush(f *os.File) error { return f.Sync() }
