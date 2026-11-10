// Package platformtest holds the platform check's inputs as injected
// values, so a test can feed the real verdict a mount its host cannot name:
// a suitable one for every journal a test stands up, and a mount without
// cache flushes for the tests of the refusal. The check is the real one
// either way, on gathered inputs.
package platformtest

import (
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// extMagic is the statfs type of ext2, ext3 and ext4, which share one
// magic, the first filesystem RFC 28 Appendix A lists.
const extMagic uint32 = 0xef53

// SuitableMount returns the probe of an ext4 mount at the root of the
// filesystem that holds dir: a real-shaped mount table of the one mount —
// a namespace's root, its own parent, covering every directory — with
// plain rw,relatime options, cache flushes on, and the ext magic's statfs
// answer, with its mount ID unknown.
func SuitableMount(dir string) storage.Probe {
	return mountTable(dir, "rw,relatime")
}

// NoBarrierMount returns the probe of an ext4 mount at the root of the
// filesystem that holds dir with cache flushes turned off: the one input
// change the barrier rule refuses. It is SuitableMount with the mount's
// superblock option nobarrier, as Linux 6.12's ext4 writes it.
func NoBarrierMount(dir string) storage.Probe {
	return mountTable(dir, "rw,nobarrier,relatime")
}

// SuitableGather is the gather that answers SuitableMount's probe for every
// directory — the mount sits at the root, so one probe covers them all: the
// suitable mount a journal's own check reads wherever the directory it sits
// in cannot name one — a host whose own mount table refuses a test's
// temporary directory, or a platform whose probe cannot produce the
// refusal the check is tested against.
func SuitableGather(dir string) storage.Gather {
	p := SuitableMount(dir)
	return gatherOf(p)
}

// gatherOf is the gather that answers the probe p for every directory.
func gatherOf(p storage.Probe) storage.Gather {
	return func(string) (storage.Probe, error) {
		return p, nil
	}
}

// mountTable returns the probe of an ext4 mount at the root of the
// filesystem holding dir, with the superblock options options: a mount
// table of one line, an ext4 mount of the root device at the root, whose
// parent is itself as a namespace's root writes it, in the layout of
// show_mountinfo (fs/proc_namespace.c).
func mountTable(dir, options string) storage.Probe {
	return storage.Probe{
		Dir:       dir,
		Mountinfo: fmt.Sprintf("33 33 259:3 / / rw,noatime - ext4 /dev/nvme1n1p1 %s\n", options),
		FSType:    extMagic,
	}
}
