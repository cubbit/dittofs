package benchdir

import (
	"os"
	"syscall"
)

// flush makes f's data durable as the journal's segments are flushed on
// Linux, with fdatasync.
func flush(f *os.File) error { return syscall.Fdatasync(int(f.Fd())) }
