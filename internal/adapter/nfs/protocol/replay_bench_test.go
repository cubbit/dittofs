package protocol

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
)

// replayLookupGoroutines is the number of goroutines the miss is measured
// under: the connections of a busy adapter all reach the one cache.
const replayLookupGoroutines = 64

// BenchmarkALookupMiss measures a Lookup that finds nothing and marks its
// call in progress, then the Drop that clears the mark, from 64 goroutines at
// once, each on a call of its own key. The one lock every connection takes is
// what it loads; run it with -mutexprofile and -blockprofile to read the
// lock's wait.
func BenchmarkALookupMiss(b *testing.B) {
	w, _ := replayed(1 << 20)
	template := sysCall(0, 0, "host", 1000, 100, []uint32{4, 24}, nil)
	ctx := context.Background()

	var xid atomic.Uint32

	b.ReportAllocs()
	b.SetParallelism(replayLookupGoroutines)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		call := append([]byte(nil), template...)
		h := headOf(call)

		for pb.Next() {
			binary.BigEndian.PutUint32(call, xid.Add(1))

			if buf, err := w.Lookup(ctx, h); buf != nil || err != nil {
				b.Errorf("a new call's lookup answers %v with %v, want a plain miss", err, buf)

				return
			}

			// the end of a request clears its mark, as Store or Drop does
			// in production, so the map does not grow with b.N.
			w.Drop(h)
		}
	})
}
