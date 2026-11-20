// Package benchdir decides whether a directory may hold a benchmark's files:
// a RAM-backed filesystem makes fsync free, so a figure measured on one
// measures everything but what bounds a write.
package benchdir

import (
	"fmt"
	"strings"
)

// RAMBacked reports whether the filesystem named name keeps its files in
// memory.
//
// decision: the journal's own platform probe judges a mount by its cache
// flushes, never by what backs it, so it cannot tell a tmpfs from a disk and
// no predicate of it is reused; this table by filesystem name is the whole
// rule. A RAM disk formatted with a disk filesystem (a macOS hdiutil volume,
// a Linux zram or brd device) carries that filesystem's name and passes; the
// operator names a real device in the run's record. Withdraw it if the probe
// ever reads what backs a mount.
func RAMBacked(name string) bool {
	switch strings.ToLower(name) {
	case "tmpfs", "ramfs":
		return true
	}
	return false
}

// Refusal returns an error naming why dir may not hold a benchmark's files,
// and nil for a directory on a filesystem that is not RAM-backed.
func Refusal(dir string) error {
	name, err := FSType(dir)
	if err != nil {
		return fmt.Errorf("filesystem of %s: %w", dir, err)
	}
	if RAMBacked(name) {
		return fmt.Errorf("%s is on a %s filesystem, which is RAM-backed: fsync is free there and the figures measure no device; name a directory on a real one", dir, name)
	}
	return nil
}
