//go:build journal_conformance && journal_bench

package conformance

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/benchdir"
)

// The benchmarks of the journal's write and read paths, run on a real
// filesystem in real time. Every J1 row has a raw twin that writes the same
// bytes through os and Sync with the same block size, writers and sync
// pattern, since a row means something only as a fraction of its twin. Each
// iteration is one burst, so -benchtime=1x -count=10 hands benchstat its
// samples.

// benchDirEnv names a directory on the device under test. Without it the
// benchmarks skip, and the suite's own guard fails the cell.
const benchDirEnv = "DITTOFS_BENCH_DIR"

// The burst sizes, in total bytes over all writers. A burst with a sync after
// each write is smaller, since its cost is the sync's, not the bytes'.
//
// ponytail: fixed here, so a nightly cannot scale them. Upgrade to one
// environment knob read by jbBurstBytes when a nightly needs smaller or
// larger bursts from the same bodies.
const (
	// ponytail: the streamed burst is half a segment, so a first iteration
	// on the one stream never rolls and no write waits on a spare; a run of
	// more than one iteration (-benchtime=Nx) rolls and the wait's spin
	// shows in allocs/op. Upgrade to a wait for the pool before each roll
	// when a run needs more than one iteration per journal.
	jbBurstStreamed = 128 << 20
	jbBurstSynced   = 16 << 20
	jbWriters       = 16
	// jbMixedStream is the streaming file of the mixed case: half a segment,
	// so the one stream never rolls beside the small writer's records.
	jbMixedStream = 128 << 20
	// jbLookups is how many point lookups one iteration of J3 times.
	jbLookups = 100_000
	// jbWarmBytes is the file J3's warm read reads, a block at a time.
	jbWarmBytes = 64 << 20
)

var jbInstall = journal.InstallationID{'j', 'b', 'e', 'n', 'c', 'h'}

// jbBase is a fresh directory under the device's, removed when b ends. It
// fails the benchmark on a RAM-backed filesystem and skips without the
// variable.
func jbBase(b *testing.B) string {
	b.Helper()

	root := os.Getenv(benchDirEnv)
	if root == "" {
		b.Skipf("set %s to a directory on the device under test", benchDirEnv)
	}
	if err := benchdir.Refusal(root); err != nil {
		b.Fatalf("bench: %s: %v", benchDirEnv, err)
	}
	jbPrintBox(b, root)

	dir, err := os.MkdirTemp(root, "journal-bench-")
	if err != nil {
		b.Fatalf("bench: directory under %s: %v", benchDirEnv, err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

var jbBoxOnce sync.Once

// jbPrintBox prints, before the first result line, what each result is
// recorded with, as key: value lines benchstat reads as configuration.
func jbPrintBox(b *testing.B, dir string) {
	b.Helper()
	jbBoxOnce.Do(func() {
		box, err := benchdir.Describe(dir)
		if err != nil {
			b.Fatalf("bench: %v", err)
		}
		box.Print(os.Stdout)
		fmt.Printf("bench-dir: %s\n", dir)
		fmt.Printf("sync-bound: %v\n", journal.SyncBound)
		fmt.Printf("segment-cap-bytes: %d\n", journaltest.SegmentCap)
		fmt.Printf("writers: %d\n", jbWriters)
	})
}

// jbClock is the wall clock the journal runs on.
type jbClock struct{ origin time.Time }

func (jbClock) Now() time.Time           { return time.Now() }
func (c jbClock) Elapsed() time.Duration { return time.Since(c.origin) }
func (jbClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

// jbOpen builds a journal with streams append streams in a fresh directory,
// waits for its first spares and returns the share's handle.
func jbOpen(b *testing.B, streams int) *journal.Handle {
	b.Helper()

	dir := filepath.Join(jbBase(b), "journal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		b.Fatalf("bench: journal directory: %v", err)
	}
	opts := []journal.Option{
		journal.WithClock(jbClock{origin: time.Now()}),
		journal.WithSettings(journal.Settings{Streams: streams}),
	}
	ctx := b.Context()
	id, err := journal.Init(ctx, dir, jbInstall, opts...)
	if err != nil {
		b.Fatalf("bench: journal init: %v", err)
	}
	j, err := journal.Open(ctx, dir, journal.Version{}, id, nil, opts...)
	if err != nil {
		b.Fatalf("bench: journal open: %v", err)
	}
	b.Cleanup(func() { _ = j.Close() })

	// The journal issues no version before its first limit.
	if err := j.SetVersionLimit(journal.Version{Counter: 1 << 62}); err != nil {
		b.Fatalf("bench: version limit: %v", err)
	}

	// Open returns while the pool is still making its spares, and a write
	// that rolls first is refused: poll the real condition, the content
	// spares and the headroom and repack reserve at their full size, so no
	// pool work competes with the timed region.
	deadline := time.Now().Add(30 * time.Minute)
	for {
		st := j.Stats()
		if st.Spares >= streams && st.HeadroomBytes >= journaltest.SegmentCap && st.RepackReserveBytes >= journaltest.SegmentCap {
			break
		}
		if time.Now().After(deadline) {
			b.Fatalf("bench: the pool made %d of %d spares, headroom %d and reserve %d of %d bytes",
				st.Spares, streams, st.HeadroomBytes, st.RepackReserveBytes, int64(journaltest.SegmentCap))
		}
		time.Sleep(time.Millisecond)
	}

	h, err := j.Share(journaltest.Tag(0), 0, journal.Version{})
	if err != nil {
		b.Fatalf("bench: share: %v", err)
	}
	b.Cleanup(func() { _ = h.Close() })
	return h
}

// jbSink is where a burst's writes go: the journal, or a file of the raw twin.
type jbSink interface {
	// write puts p at off of writer w's file and returns how long the call took.
	write(w int, off int64, p []byte) (time.Duration, error)
	// sync makes writer w's file durable.
	sync(w int) error
}

type jbJournal struct {
	b *testing.B
	h *journal.Handle
}

func (s jbJournal) write(w int, off int64, p []byte) (took time.Duration, err error) {
	// Only the last attempt is timed: a refusal for want of a spare is
	// retried, and its wait is the pool's, not the call's.
	err = journaltest.RetrySpare(func() error {
		start := time.Now()
		_, err := s.h.WriteAt(s.b.Context(), journaltest.File(w), off, p, 0)
		took = time.Since(start)
		return err
	})
	return took, err
}

func (s jbJournal) sync(w int) error {
	_, err := s.h.Sync(s.b.Context(), journaltest.File(w))
	return err
}

// jbRaw writes straight through os, one file per writer.
type jbRaw struct{ files []*os.File }

func jbOpenRaw(b *testing.B, writers int) jbRaw {
	b.Helper()

	dir := jbBase(b)
	r := jbRaw{}
	for w := range writers {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("raw-%d", w)))
		if err != nil {
			b.Fatalf("bench: raw file %d: %v", w, err)
		}
		b.Cleanup(func() { _ = f.Close() })
		r.files = append(r.files, f)
	}
	return r
}

func (r jbRaw) write(w int, off int64, p []byte) (time.Duration, error) {
	start := time.Now()
	_, err := r.files[w].WriteAt(p, off)
	return time.Since(start), err
}

func (r jbRaw) sync(w int) error { return jbFlush(r.files[w]) }

// prepare gives every file its full span, zero-filled and synced, as the
// journal's segments are preallocated, so a timed write overwrites blocks the
// file already owns instead of extending it.
func (r jbRaw) prepare(span int64) error {
	zero := make([]byte, 1<<20)
	for _, f := range r.files {
		for off := int64(0); off < span; off += int64(len(zero)) {
			n := min(int64(len(zero)), span-off)
			if _, err := f.WriteAt(zero[:n], off); err != nil {
				return err
			}
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// jbSpec is one burst: writers each write span bytes in blocks of block, in
// order or at random aligned offsets within the span, syncing after every
// write, or once at the end of the writer's run.
type jbSpec struct {
	writers  int
	block    int
	span     int64 // bytes each writer writes
	random   bool
	syncEach bool
	syncEnd  bool
}

// jbParallel runs fn on n goroutines and fails b on the first error.
func jbParallel(b *testing.B, n int, fn func(w int) error) {
	b.Helper()

	errs := make([]error, n)
	var wg sync.WaitGroup
	for w := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[w] = fn(w)
		}()
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			b.Fatalf("bench: writer %d: %v", w, err)
		}
	}
}

// jbBurst runs b.N bursts of spec through sink and reports the bytes written,
// and the p99 of one write call, sync and the timer's own work excluded: the
// durations are collected into slices made before the timer starts and
// sorted after it stops.
func jbBurst(b *testing.B, sink jbSink, spec jbSpec) {
	b.Helper()
	b.ReportAllocs()

	ops := int(spec.span) / spec.block
	took := make([][]time.Duration, spec.writers)
	for w := range took {
		took[w] = make([]time.Duration, 0, ops*b.N)
	}
	b.SetBytes(spec.span * int64(spec.writers))

	if p, ok := sink.(interface{ prepare(span int64) error }); ok {
		if err := p.prepare(spec.span * int64(b.N)); err != nil {
			b.Fatalf("bench: prepare the raw files: %v", err)
		}
	}

	b.ResetTimer()
	for iter := range b.N {
		base := int64(iter) * spec.span
		jbParallel(b, spec.writers, func(w int) error {
			p := make([]byte, spec.block)
			rng := rand.New(rand.NewPCG(uint64(iter), uint64(w)))
			for i := range ops {
				off := base + int64(i*spec.block)
				if spec.random {
					off = base + int64(rng.IntN(ops)*spec.block)
				}
				d, err := sink.write(w, off, p)
				if err != nil {
					return err
				}
				took[w] = append(took[w], d)
				if spec.syncEach {
					if err := sink.sync(w); err != nil {
						return err
					}
				}
			}
			if spec.syncEnd {
				return sink.sync(w)
			}
			return nil
		})
	}
	b.StopTimer()

	jbReportP99(b, took, "p99-write-µs")
}

// jbReportP99 reports the 99th percentile of every duration of all as unit.
func jbReportP99(b *testing.B, all [][]time.Duration, unit string) {
	b.Helper()

	flat := slices.Concat(all...)
	if len(flat) == 0 {
		return
	}
	slices.Sort(flat)
	rank := (99*len(flat) + 99) / 100
	b.ReportMetric(float64(flat[max(rank, 1)-1])/float64(time.Microsecond), unit)
}

var jbBlocks = []struct {
	name  string
	bytes int
}{{"4k", 4 << 10}, {"64k", 64 << 10}, {"256k", 256 << 10}, {"1m", 1 << 20}}

// jbForEachMode runs body once per block size and sync pattern, as a
// sub-benchmark of b, the patterns being the rows J1's setup names.
func jbForEachMode(b *testing.B, sizes []string, patterns []string, body func(b *testing.B, spec jbSpec)) {
	for _, blk := range jbBlocks {
		if !slices.Contains(sizes, blk.name) {
			continue
		}
		for _, pat := range patterns {
			b.Run(blk.name+"-"+pat, func(b *testing.B) {
				spec := jbSpec{writers: jbWriters, block: blk.bytes, span: jbBurstStreamed / jbWriters}
				if pat == "sync" {
					spec.syncEach = true
					spec.span = jbBurstSynced / jbWriters
				}
				body(b, spec)
			})
		}
	}
}

var (
	jbAllSizes = []string{"4k", "64k", "256k", "1m"}
	jbPatterns = []string{"nosync", "sync"}
)

// J1: the write path, 16 writers on the one append stream.
func BenchmarkJ1_WriteSequential(b *testing.B) {
	jbForEachMode(b, jbAllSizes, jbPatterns, func(b *testing.B, spec jbSpec) {
		jbBurst(b, jbJournal{b, jbOpen(b, 1)}, spec)
	})
}

func BenchmarkJ1_WriteRandom(b *testing.B) {
	jbForEachMode(b, jbAllSizes, jbPatterns, func(b *testing.B, spec jbSpec) {
		spec.random = true
		jbBurst(b, jbJournal{b, jbOpen(b, 1)}, spec)
	})
}

// J1's mixed case: one writer syncing every 4 KiB beside one stream writing a
// 128 MiB file in 1 MiB blocks, synced at its end, on the one stream. The
// metric is the p99 of the small writer's write and sync together.
func BenchmarkJ1_MixedDurability(b *testing.B) {
	b.ReportAllocs()

	sink := jbJournal{b, jbOpen(b, 1)}
	b.SetBytes(jbMixedStream)

	var small []time.Duration
	b.ResetTimer()
	for iter := range b.N {
		var done atomic.Bool
		var mine []time.Duration
		jbParallel(b, 2, func(w int) error {
			if w == 0 {
				defer done.Store(true)
				p := make([]byte, 1<<20)
				for off := int64(0); off < jbMixedStream; off += int64(len(p)) {
					if _, err := sink.write(0, int64(iter)*jbMixedStream+off, p); err != nil {
						return err
					}
				}
				return sink.sync(0)
			}
			p := make([]byte, 4<<10)
			for off := int64(0); !done.Load(); off += int64(len(p)) {
				start := time.Now()
				if _, err := sink.write(1, int64(iter)<<40+off, p); err != nil {
					return err
				}
				if err := sink.sync(1); err != nil {
					return err
				}
				mine = append(mine, time.Since(start))
			}
			return nil
		})
		small = append(small, mine...)
	}
	b.StopTimer()

	jbReportP99(b, [][]time.Duration{small}, "p99-sync-write-µs")
}

// J2: write scaling, the same bytes spread over n files written concurrently,
// each synced at its end, on the journal's default streams.
func jbScaling(b *testing.B, n int) {
	jbBurst(b, jbJournal{b, jbOpen(b, 16)}, jbSpec{
		writers: n, block: 1 << 20, span: jbBurstStreamed / int64(n), syncEnd: true,
	})
}

func BenchmarkJ2_WriteScaling1(b *testing.B)  { jbScaling(b, 1) }
func BenchmarkJ2_WriteScaling4(b *testing.B)  { jbScaling(b, 4) }
func BenchmarkJ2_WriteScaling16(b *testing.B) { jbScaling(b, 16) }
func BenchmarkJ2_WriteScaling64(b *testing.B) { jbScaling(b, 64) }

// jbPointExtent is the size of one extent of the point-lookup index, and the
// stride between two, so no two extents merge.
const (
	jbPointExtent = 64
	jbPointStride = 128
)

// J3: a ReadAt of one 64-byte extent over an index of n extents, timed as a
// block of lookups since one is below the timer's resolution. It is an upper
// bound of the index's own lookup: it includes the pread of the extent's
// bytes from the segment and the copy into the caller's buffer, and the
// public API gives no way to time the index alone, so the index lookup's
// target is not judged from it.
func jbPointLookup(b *testing.B, n int) {
	b.ReportAllocs()

	h := jbOpen(b, 16)
	id := journaltest.File(0)
	sink := jbJournal{b, h}
	p := make([]byte, jbPointExtent)
	for i := range n {
		if _, err := sink.write(0, int64(i)*jbPointStride, p); err != nil {
			b.Fatalf("bench: build the index, extent %d: %v", i, err)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))

	b.ResetTimer()
	var spent time.Duration
	for range b.N {
		start := time.Now()
		for range jbLookups {
			off := int64(rng.IntN(n)) * jbPointStride
			if _, missing, _, _, err := h.ReadAt(b.Context(), id, off, p); err != nil || len(missing) != 0 {
				b.Fatalf("bench: lookup at %d: missing %v, err %v", off, missing, err)
			}
		}
		spent += time.Since(start)
	}
	b.StopTimer()

	b.ReportMetric(float64(spent)/float64(b.N*jbLookups), "ns/lookup")
}

func BenchmarkJ3_PointLookup(b *testing.B) {
	b.Run("extents=1e3", func(b *testing.B) { jbPointLookup(b, 1_000) })
	b.Run("extents=1e6", func(b *testing.B) { jbPointLookup(b, 1_000_000) })
}

// J3's warm read: one stream reading a held file 1 MiB at a time, after one
// read that warms what the operating system caches.
func BenchmarkJ3_WarmRead1m(b *testing.B) {
	b.ReportAllocs()

	h := jbOpen(b, 16)
	id := journaltest.File(0)
	sink := jbJournal{b, h}
	p := make([]byte, 1<<20)
	for off := int64(0); off < jbWarmBytes; off += int64(len(p)) {
		if _, err := sink.write(0, off, p); err != nil {
			b.Fatalf("bench: fill the file at %d: %v", off, err)
		}
	}
	readAll := func() {
		for off := int64(0); off < jbWarmBytes; off += int64(len(p)) {
			if _, missing, _, _, err := h.ReadAt(b.Context(), id, off, p); err != nil || len(missing) != 0 {
				b.Fatalf("bench: read at %d: missing %v, err %v", off, missing, err)
			}
		}
	}
	readAll()
	b.SetBytes(jbWarmBytes)

	b.ResetTimer()
	for range b.N {
		readAll()
	}
}

// The raw twins, on the same filesystem with J1's writers, blocks and sync
// patterns: each J1 row is a fraction of the twin of its own name.

// BenchmarkRawSequential is the twin of J1's sequential rows, with the same
// block sizes and sync patterns.
func BenchmarkRawSequential(b *testing.B) {
	jbForEachMode(b, jbAllSizes, jbPatterns, func(b *testing.B, spec jbSpec) {
		jbBurst(b, jbOpenRaw(b, spec.writers), spec)
	})
}

// BenchmarkRawRandom is the twin of J1's random rows, with the same block
// sizes and sync patterns: its 4 KiB row with a sync per write is the device's
// own fsync rate.
func BenchmarkRawRandom(b *testing.B) {
	jbForEachMode(b, jbAllSizes, jbPatterns, func(b *testing.B, spec jbSpec) {
		spec.random = true
		jbBurst(b, jbOpenRaw(b, spec.writers), spec)
	})
}

// BenchmarkRawScaling is the twin of J2: the same bytes over n files of 1 MiB
// blocks, each synced at its end, as a sub-benchmark per file count.
func BenchmarkRawScaling(b *testing.B) {
	for _, n := range []int{1, 4, 16, 64} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			jbBurst(b, jbOpenRaw(b, n), jbSpec{
				writers: n, block: 1 << 20, span: jbBurstStreamed / int64(n), syncEnd: true,
			})
		})
	}
}
