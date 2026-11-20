//go:build journal_conformance && journal_bench

package conformance

import (
	"os"
	"syscall"
)

// jbFlush makes f's data durable the way the journal's segments are
// flushed on Linux, with fdatasync.
func jbFlush(f *os.File) error { return syscall.Fdatasync(int(f.Fd())) }
