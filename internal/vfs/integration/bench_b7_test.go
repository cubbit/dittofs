//go:build engine_conformance && engine_bench

package integration

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// B7's storm: b7Rate chmods a second, each on a file drawn at random from all
// b7Files files of the share's journal, the first benchWriters of which the
// writers overwrite.
const (
	b7Files = 100000
	b7Rate  = 1000
	b7Log2G = 6 // log₂ of benchMaxFiles

	// b7MinK and b7MaxK are the fewest and the most conflicting files a burst
	// may have and still be evaluated: below 2 the bound is under the batches a
	// burst starts with no conflict at all, and above 10 1 + k × 6 passes the 65
	// transactions of a batch committed file by file.
	b7MinK = 2
	b7MaxK = 10

	// b7WantK is the expected k the storm's pool is sized for, before each
	// burst, from the length of the burst before it.
	b7WantK = 4

	// b7HeapPerFile is the live heap one file of the pool holds: prof.LiveHeap
	// read 90 745 912 bytes before the 100 000 files were made and 111 444 976
	// after, 207 a file rounded up. b7HeapSlack is the growth the profiler
	// allows B7: its default plus the pool's.
	b7HeapPerFile = 207
	b7HeapSlack   = 16<<20 + b7Files*b7HeapPerFile

	// b7ExistenceAge is long enough that no group commit comes due by its
	// timer during a run, so each burst's transactions are its fsyncs'
	// stability points alone.
	b7ExistenceAge = 10 * time.Minute
)

// BenchmarkB7 is RFC 8 §15.4's B7, the group commit under a chmod storm: B5's
// overwriting run with 10³ chmods a second on random files of the same
// journal. The engine's transactions per stability point, the storm's own
// taken out, are at most 1 + conflicting files × log₂ G (RFC 8 §5.2), and a
// burst that breaks the bound fails the run; the fsync p99 is reported, to
// compare with B5's overwriting run.
//
// decision: B7 runs B5's overwriting run, not its appending one: an append's
// fsync commits no existence, so with the long existence age no transaction
// of a burst could meet the storm and the bound was never exercised. The
// engine's transactions are counted exactly, by the keys each committed
// transaction wrote (txnRecord): one that wrote an existence key is the
// engine's, one that wrote both kinds counts once for the engine and is
// reported as mixed, and the storm's are left out. The conflicting files of a
// burst, k, are the writers' files a storm chmod ran on during it, from the
// storm's own record, and the burst is bounded at 1 + k × 6.
//
// Only bursts with 2 ≤ k ≤ 10 are evaluated. Below 2 the bound is under what
// the engine does with no conflict at all: the stability points of one burst
// start several batches (each the files due when the last one returned), 3 to
// 7 of them, and a burst with k = 1 measured 7 transactions against its bound
// of 7. The lower limit goes back to 1, and 0 is evaluated at 1, once the
// group commit gathers a burst into one batch. Above 10 the bound passes the
// 65 transactions of a batch committed file by file, so it cannot fail. B7
// fails when fewer than half of its bursts are evaluated.
//
// The storm draws from the first N of the pool's 100 000 files, the writers'
// 64 among them, all files of the same journal. N is set before each burst so
// that the expected k is 4: a burst of length D meets about b7Rate × D chmods,
// each on a writer's file with chance 64 / N, so N = 64 × b7Rate × D / 4,
// within [64, 100 000], with D the last burst's length and a warm-up burst,
// not evaluated, to measure the first. Overturned if the measured k drifts far
// from 4 while D is steady (chmods outlasting a burst would push it up), or
// when the key layout changes (the classifier must follow it).
//
// The profiler's heap slack for B7 is its default plus the pool's own
// footprint (b7HeapSlack), since the pool is held until the composition
// closes: growth above that is a leak, not the pool.
func BenchmarkB7(b *testing.B) {
	b.Run("overwrite/writers=64/chmod=1000ps", benchB7)
}

// stormChmod is one chmod of the storm: which file, when its call started and
// when it returned, zero while it runs.
type stormChmod struct {
	file       int
	start, end time.Time
}

// storm chmods random files of c at b7Rate until stopped, and records each.
// It is open-loop: each tick starts its chmod at once, whatever the earlier
// ones are still waiting on, so a slow chmod does not lower the rate.
type storm struct {
	pool     atomic.Int64 // the storm draws from the first pool files
	mu       sync.Mutex
	chmods   []stormChmod
	failed   error
	stop     chan struct{}
	ended    chan struct{}
	inFlight sync.WaitGroup
	stopOnce sync.Once
}

func startStorm(ctx context.Context, c *bench) *storm {
	s := &storm{
		stop:  make(chan struct{}),
		ended: make(chan struct{}),
	}
	s.pool.Store(int64(len(c.files)))

	r := rand.New(rand.NewPCG(7, 7))

	go func() {
		defer close(s.ended)
		defer s.inFlight.Wait()

		tick := time.NewTicker(time.Second / b7Rate)
		defer tick.Stop()

		mode := uint32(0o600)

		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}

			i := r.IntN(int(s.pool.Load()))
			mode ^= 0o040
			ch := vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(mode)}

			s.mu.Lock()
			n := len(s.chmods)
			s.chmods = append(s.chmods, stormChmod{
				file:  i,
				start: time.Now(),
			})
			s.mu.Unlock()

			s.inFlight.Go(func() {
				_, err := c.fs.SetAttr(ctx, c.admin, vfs.Target{H: c.files[i]}, ch)

				s.mu.Lock()
				defer s.mu.Unlock()

				if err != nil && s.failed == nil {
					s.failed = err
				}

				s.chmods[n].end = time.Now()
			})
		}
	}()

	return s
}

// end stops the storm and waits for its last chmod, at most
// benchBurstDeadline.
func (s *storm) end() error {
	s.stopOnce.Do(func() {
		close(s.stop)
	})

	select {
	case <-s.ended:
	case <-time.After(benchBurstDeadline):
		return fmt.Errorf("the storm's last chmod still running after %v", benchBurstDeadline)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failed
}

// made is how many chmods the storm has started so far.
func (s *storm) made() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.chmods)
}

// touched is the distinct writers' files a chmod ran on at some time between
// from and to: one that started before to and had not returned by from.
func (s *storm) touched(from, to time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var files []int

	for _, c := range s.chmods {
		running := c.end.IsZero() || c.end.After(from)
		if c.file < benchWriters && c.start.Before(to) && running &&
			!slices.Contains(files, c.file) {
			files = append(files, c.file)
		}
	}

	return len(files)
}

func benchB7(b *testing.B) {
	c := composeBench(b, b7Files, b7ExistenceAge, b7HeapSlack)
	ctx := b.Context()
	data := make([]byte, mib)
	c.fill(b, b5Bursts*mib)

	var fsyncs []time.Duration

	most, evaluated, touchedAll, mixedAll, stormTotal := 0, 0, 0, 0, 0

	var stormed time.Duration

	b.ResetTimer()

	for i := range b.N {
		s := startStorm(ctx, c)

		b.Cleanup(func() {
			_ = s.end()
		})

		stormFrom := time.Now()

		var last time.Duration

		// Burst -1 is the warm-up: it measures the first burst's length, and
		// is not evaluated.
		for k := -1; k < b5Bursts; k++ {
			if k >= 0 {
				s.pool.Store(stormPool(last))
			}

			off := int64(max(k, 0)) * mib
			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				ref := vfs.Anonymous(c.files[w])
				_, err := c.fs.Write(ctx, c.admin, ref, off, data, vfs.Unstable)

				return err
			}); err != nil {
				b.Fatalf("burst %d of run %d: a write refused: %v", k+1, i+1, err)
			}

			c.txns.take()

			from := time.Now()

			took := make([]time.Duration, benchWriters)
			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				start := time.Now()
				_, err := c.fs.Commit(ctx, c.admin, vfs.Anonymous(c.files[w]), 0, 0)
				took[w] = time.Since(start)

				return err
			}); err != nil {
				b.Fatalf("burst %d of run %d: an fsync failed: %v", k+1, i+1, err)
			}

			engine, mixed := c.txns.take()
			last = time.Since(from)
			touched := s.touched(from, time.Now())

			if k >= 0 {
				fsyncs = append(fsyncs, took...)
				mixedAll += mixed
			}

			b.Logf(
				"burst %d of run %d: N %d, k %d, engine transactions %d, mixed %d",
				k+1,
				i+1,
				s.pool.Load(),
				touched,
				engine,
				mixed,
			)

			if k < 0 || touched < b7MinK || touched > b7MaxK {
				continue
			}

			bound := 1 + touched*b7Log2G
			if engine > bound {
				b.Fatalf(
					"burst %d of run %d: %d existence transactions for %d fsyncs, "+
						"want at most 1 + %d conflicting files × %d = %d",
					k+1,
					i+1,
					engine,
					benchWriters,
					touched,
					b7Log2G,
					bound,
				)
			}

			most, evaluated = max(most, engine), evaluated+1
			touchedAll += touched
		}

		stormed += time.Since(stormFrom)
		stormTotal += s.made()

		if err := s.end(); err != nil {
			b.Fatalf("run %d: a chmod of the storm failed: %v", i+1, err)
		}
	}

	b.StopTimer()
	b.ReportMetric(microseconds(percentile(fsyncs, 99)), "fsync-p99-µs")
	b.ReportMetric(float64(most), "txn/stability-point")
	b.ReportMetric(float64(evaluated), "evaluated-bursts")
	b.ReportMetric(float64(touchedAll)/float64(max(evaluated, 1)), "k/evaluated-burst")
	b.ReportMetric(float64(mixedAll), "mixed-txns")
	b.ReportMetric(float64(stormTotal)/stormed.Seconds(), "chmods/s")

	if bursts := b.N * b5Bursts; 2*evaluated < bursts {
		b.Fatalf(
			"%d of %d bursts evaluated, want at least half: the storm touched fewer than %d or "+
				"more than %d of the writers' files during the rest, so the bound went unchecked",
			evaluated,
			bursts,
			b7MinK,
			b7MaxK,
		)
	}
}

// stormPool is how many of the pool's files the storm draws from for a burst
// expected to last as long as last did: enough that about b7WantK of the
// writers' files are touched.
func stormPool(last time.Duration) int64 {
	n := int64(float64(benchWriters*b7Rate) * last.Seconds() / b7WantK)

	return min(max(n, benchWriters), b7Files)
}
