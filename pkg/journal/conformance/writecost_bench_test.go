//go:build journal_conformance && journal_bench

package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// The write path's own cost, with the platform taken out: the journal runs
// over kit.NullFS, whose segment writes, syncs and allocations return at once
// (a load, not a double — nothing asserts on its answers), so ns/op is the
// journal's Go code alone.

// wcOpen opens a journal of streams append streams in dir over null, the null
// storage over that same directory.
func wcOpen(b *testing.B, dir string, streams int, null kit.NullFS) *journal.Handle {
	b.Helper()
	opts := []journal.Option{
		journal.WithClock(jbClock{origin: time.Now()}),
		journal.WithSettings(journal.Settings{Streams: streams, MaxBytes: 1 << 50}),
		journal.WithStorage(null),
	}
	ctx := b.Context()
	id, err := journal.Init(ctx, dir, jbInstall, opts...)
	if err != nil {
		b.Fatalf("init: %v", err)
	}
	j, err := journal.Open(ctx, dir, journal.Version{}, id, nil, opts...)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = j.Close() })
	if err := j.SetVersionLimit(journal.Version{Counter: 1 << 62}); err != nil {
		b.Fatal(err)
	}
	// Open returns while the pool still makes its spares: poll the real
	// condition, so no first roll is refused inside the timed region.
	deadline := time.Now().Add(time.Minute)
	for j.Stats().Spares < streams+1 {
		if time.Now().After(deadline) {
			b.Fatalf("bench: %d spares after a minute, want %d", j.Stats().Spares, streams+1)
		}
		time.Sleep(time.Millisecond)
	}
	h, err := j.Share(journaltest.Tag(0), 0, journal.Version{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = h.Close() })
	return h
}

// BenchmarkWriteCostNull: b.N sequential writes split over w writers, each
// on its own file, over s streams. ns/op is wall time per write across all
// writers, so w16 against w1 at equal ns/op means no parallel speed-up.
func BenchmarkWriteCostNull(b *testing.B) {
	for _, blk := range []struct {
		name string
		n    int
	}{{"4k", 4 << 10}, {"64k", 64 << 10}} {
		for _, c := range []struct{ w, s int }{{1, 1}, {16, 1}, {16, 16}} {
			b.Run(fmt.Sprintf("%s/w%d-s%d", blk.name, c.w, c.s), func(b *testing.B) {
				// Under the test's own temporary directory: no record
				// reaches a device, so the row needs none.
				dir := filepath.Join(b.TempDir(), "journal")
				if err := os.Mkdir(dir, 0o700); err != nil {
					b.Fatalf("bench: journal directory: %v", err)
				}
				null := kit.NewNullFS(dir)
				h := wcOpen(b, dir, c.s, null)
				// b.N split exactly: the first b.N%w writers take one more,
				// so the run makes b.N writes and ns/op divides by them.
				per := make([]int, c.w)
				took := make([][]time.Duration, c.w)
				bufs := make([][]byte, c.w)
				ids := make([]journal.FileID, c.w)
				for w := range c.w {
					per[w] = b.N / c.w
					if w < b.N%c.w {
						per[w]++
					}
					ids[w] = journaltest.File(w)
					took[w] = make([]time.Duration, 0, per[w])
					bufs[w] = make([]byte, blk.n)
				}
				b.SetBytes(int64(blk.n))
				b.ReportAllocs()
				var wg sync.WaitGroup
				errs := make([]error, c.w)
				b.ResetTimer()
				for w := range c.w {
					wg.Go(func() {
						for i := range per[w] {
							start := time.Now()
							err := journaltest.RetrySpare(func() error {
								_, err := h.WriteAt(b.Context(), ids[w], int64(i*blk.n), bufs[w], 0)
								return err
							})
							took[w] = append(took[w], time.Since(start))
							if err != nil {
								errs[w] = err
								return
							}
						}
					})
				}
				wg.Wait()
				b.StopTimer()
				for w, err := range errs {
					if err != nil {
						b.Fatalf("writer %d: %v", w, err)
					}
				}
				// The probe must fire: a write that reached the platform measured
				// the platform.
				if got, want := null.Nulled(), int64(b.N*blk.n); got < want {
					b.Fatalf("nulled %d bytes, want at least %d: writes bypassed the null file", got, want)
				}
				flat := slices.Concat(took...)
				slices.Sort(flat)
				q := func(p int) float64 { return float64(flat[(p*len(flat)+99)/100-1]) / 1e3 }
				b.ReportMetric(q(50), "p50-µs")
				b.ReportMetric(q(99), "p99-µs")
			})
		}
	}
}
