//go:build journal_conformance && journal_bench && !linux

package conformance

import "os"

// jbFlush makes f's data durable with the platform's own sync, F_FULLFSYNC
// on macOS, as the journal's segments are flushed there.
func jbFlush(f *os.File) error { return f.Sync() }
