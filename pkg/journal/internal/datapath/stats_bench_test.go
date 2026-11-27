package datapath

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
)

// benchFile returns the id of the i'th benchmark file.
func benchFile(i int) core.FileID {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:], uint64(i)+1)
	return core.NewFileID(b)
}

// dirtyDelta is the delta a write of a new file makes, stamped with the
// injected clock's second i past base. The dirty list keys its oldest write by
// the whole second, so files written over distinct seconds make as many keys
// as files: ten thousand writes in a tight loop would make a handful and the
// benchmarks below could not tell a scan of the keys from a read of one value.
func dirtyDelta(base time.Time, i int) index.HeldDelta {
	return index.HeldDelta{
		Tag: 1, File: benchFile(i), HeldBytes: 1, DirtyBytes: 1,
		Dirty: index.BecameDirty, OldestDirty: base.Add(time.Duration(i) * time.Second),
	}
}

func cleanDelta(i int) index.HeldDelta {
	return index.HeldDelta{Tag: 1, File: benchFile(i), HeldBytes: -1, DirtyBytes: -1, Dirty: index.BecameClean}
}

// dirtySizes are the dirty-file counts both benchmarks run at.
var dirtySizes = []int{1e3, 1e5}

// benchCore returns a core whose dirty list holds n files over n distinct
// seconds of the injected clock.
func benchCore(b *testing.B, n int) (*Core, time.Time) {
	b.Helper()
	c, _ := newTestCore(b)
	base := c.clock.Now()
	for i := range n {
		c.Account(dirtyDelta(base, i))
	}
	return c, base
}

// Stats costs the same however many files are dirty, over as many seconds.
func BenchmarkStatsDirty(b *testing.B) {
	for _, n := range dirtySizes {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			c, base := benchCore(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				s := c.Stats()
				if want := time.Unix(base.Unix(), 0); !s.OldestDirty.Equal(want) {
					b.Fatalf("oldest dirty = %v, want %v, the first write's second", s.OldestDirty, want)
				}
			}
		})
	}
}

// Account's cost does not grow with the dirty files either. Each iteration
// cleans the oldest file and dirties a new newest one, so the list keeps its
// size and every iteration moves the oldest: a run that only dirtied would
// drain nothing and one that only cleaned would empty the set once b.N passed
// its size, and both would read flat for the wrong reason.
func BenchmarkAccountDirty(b *testing.B) {
	for _, n := range dirtySizes {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			c, base := benchCore(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				c.Account(cleanDelta(i))
				c.Account(dirtyDelta(base, n+i))
			}
			b.StopTimer()
			if got, want := c.oldestDirty(), time.Unix(base.Add(time.Duration(b.N)*time.Second).Unix(), 0); !got.Equal(want) {
				b.Fatalf("after %d rounds oldest dirty = %v, want %v", b.N, got, want)
			}
		})
	}
}
