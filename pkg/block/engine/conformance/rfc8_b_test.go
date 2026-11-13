//go:build engine_conformance

package conformance

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// secondFile creates a file of the fixture's second share.
func secondFile(t *testing.T, f *enginetest.Fixture, name string) entity.FileID {
	t.Helper()

	_, file := f.FS.Create(t, f.FS.Admin, f.SecondRoot, name, kvstore.Regular(0o644))

	return file.ID
}

// TestRFC8B_SharedJournal: two shares on one device journal, one filled to
// its journal limit, and the other's writes are not refused.
func TestRFC8B_SharedJournal(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	second := f.Second
	second.Limit = 64 << 10

	if err := f.E.Bind(ctx, second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	full := secondFile(t, f, "full")
	p := make([]byte, 16<<10)
	filled := false

	for i := range 16 {
		_, err := f.E.Write(ctx, second.ID, full, int64(i*len(p)), p, engine.WriteOpts{Done: func() {}})
		if err == nil {
			continue
		}

		var d *vfserr.Delay

		if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
			t.Fatalf("the filling write: %v, want ErrDelay(DelayCapacity) at the limit", err)
		}

		filled = true

		break
	}

	if !filled {
		t.Fatal("the second share never reached its journal limit")
	}

	_, id := f.File("other")
	write(t, f, id, 0, []byte("the other share is served"))
}

// p99 is the 99th percentile of ds.
func p99(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)

	return s[len(s)*99/100]
}

// writeLatencies times n writes of 4 KiB to the fixture's share, and returns
// them with the file_state lock waits the engine recorded meanwhile.
func writeLatencies(t *testing.T, f *enginetest.Fixture, name string, n int) ([]time.Duration, engine.Series) {
	t.Helper()

	_, id := f.File(name)
	p := make([]byte, 4<<10)
	ds := make([]time.Duration, 0, n)
	before := f.E.Stats().FileStateLockWaits

	for i := range n {
		start := time.Now()
		write(t, f, id, int64(i*len(p)), p)
		ds = append(ds, time.Since(start))
	}

	after := f.E.Stats().FileStateLockWaits

	return ds, engine.Series{Count: after.Count - before.Count, Total: after.Total - before.Total}
}

// TestRFC8B_NoCrossShareSlowdown, the write half: one share saturated with
// writes adds no wait on an engine lock to the other share's writes. The
// engine_lock_wait_seconds series, file_state its one area so far, records
// no more contended takes while the other share is loaded than in the
// unloaded baseline.
//
// decision: the row asserts the lock-wait half of RFC 8 §15's check and only
// logs the p99 write latency. Both shares append to one device journal, whose
// stream and sync §1.1 lets every file share, so the same load on the
// measured share itself fails a wall-clock bound as often as the other
// share's does, and a load that only burns CPU fails it too: the bound
// measured the host and the shared stream, not a cross-share point. The
// ceiling is the areas the engine instruments; a new lock outside them is not
// seen here. Assert latency again when the row can run each share on its own
// journal and measure without competing for the host's CPU.
//
// The journal's pool keeps a spare for each of the row's concurrent writers,
// the loaders and the measured one, and one for each of its two streams'
// first roll, so no write is refused for want of a spare whatever the pool's
// refill timing: that refusal is a capacity answer, not the slowdown the row
// measures.
func TestRFC8B_NoCrossShareSlowdown(t *testing.T) {
	const loaders = 4

	f := enginetest.New(t, enginetest.WithSpares(loaders+1+2))
	ctx := t.Context()

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	base, baseWaits := writeLatencies(t, f, "baseline", 200)
	loads := make([]entity.FileID, loaders)

	for w := range loads {
		loads[w] = secondFile(t, f, "load"+string(rune('a'+w)))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(loads))

	for _, id := range loads {
		go func() {
			defer wg.Done()

			p := make([]byte, 4<<10)

			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}

				_, err := f.E.Write(ctx, f.Second.ID, id, int64(i%256*len(p)), p, engine.WriteOpts{Done: func() {}})
				if err != nil {
					t.Errorf("a loading write: %v", err)

					return
				}
			}
		}()
	}

	loaded, loadedWaits := writeLatencies(t, f, "loaded", 200)
	close(stop)
	wg.Wait()

	if loadedWaits.Count > baseWaits.Count {
		t.Errorf("file_state lock waits under the other share's load: %d contended takes, want at most the baseline's %d",
			loadedWaits.Count, baseWaits.Count)
	}

	t.Logf("p99 write latency %v under the other share's load, baseline %v", p99(loaded), p99(base))
}
