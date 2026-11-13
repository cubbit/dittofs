//go:build engine_conformance && engine_bench

package integration

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// B7's storm: b7Rate chmods a second, each on a file drawn at random from all
// b7Files files of the share's journal, the first benchWriters of which the
// writers append to. b7Calibrate is how long the window with no fsync lasts.
const (
	b7Files     = 1024
	b7Rate      = 1000
	b7Calibrate = 500 * time.Millisecond
	b7Log2G     = 6 // log₂ of benchMaxFiles

	// decision: B7's existence age is long enough that no group commit comes
	// due by its timer during a run's bursts, so every burst's transactions are
	// its fsyncs' stability points and the bound is checked on each, not only on
	// the bursts a timer commit missed. B5's appending run keeps the short age,
	// since it needs timer commits during the run. Overturned if a run's bursts
	// ever last longer than this age, which the evaluated-bursts floor shows.
	b7ExistenceAge = 10 * time.Minute
)

// BenchmarkB7 is RFC 8 §15.4's B7, the group commit under a chmod storm: B5's
// appending run with 10³ chmods a second on random files of the same journal.
// The engine's transactions per stability point, the storm's own taken out,
// are at most 1 + conflicting files × log₂ G (RFC 8 §5.2), and a run that
// breaks the bound fails; the fsync p99 is reported, to compare with B5's.
//
// decision: the storm draws from every file of the journal, the writers'
// 64 among 1024, as the RFC's "random files of the same journal" reads. The
// conflicting files of a burst are bounded by the writers' files the storm
// touched during it, from the storm's own record, so a burst that touched c of
// them may cost 1 + c × 6; with about 6 of 64 touched per 100 ms burst, a group
// commit that retried every file of a batch (1 + 64) breaks it. The storm's own
// transactions are taken out at the rate measured in a window with no fsync
// (store Updates per chmod there, times the chmods made during the burst,
// rounded up), which is exact while a chmod costs a fixed number of Updates,
// so no slack is added. Overturned if a chmod's Updates vary, which the
// calibration's ratio shows as a fraction, or if bursts grow long enough that
// c × 6 passes 64 and the bound stops telling a retry of every file apart.
func BenchmarkB7(b *testing.B) {
	b.Run("append/writers=64/chmod=1000ps", benchB7)
}

// stormChmod is one chmod of the storm: when it ended and which file.
type stormChmod struct {
	at   time.Time
	file int
}

// storm chmods random files of c at b7Rate until stopped, and records each.
type storm struct {
	mu     sync.Mutex
	done   []stormChmod
	failed error
	stop   chan struct{}
	ended  chan struct{}
}

func startStorm(ctx context.Context, c *bench) *storm {
	s := &storm{
		stop:  make(chan struct{}),
		ended: make(chan struct{}),
	}
	r := rand.New(rand.NewPCG(7, 7))

	go func() {
		defer close(s.ended)

		tick := time.NewTicker(time.Second / b7Rate)
		defer tick.Stop()

		mode := uint32(0o600)

		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}

			i := r.IntN(len(c.files))
			mode ^= 0o040
			ch := vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(mode)}
			_, err := c.fs.SetAttr(ctx, c.admin, vfs.Target{H: c.files[i]}, ch)

			s.mu.Lock()
			if err != nil && s.failed == nil {
				s.failed = err
			}

			s.done = append(s.done, stormChmod{at: time.Now(), file: i})
			s.mu.Unlock()
		}
	}()

	return s
}

// end stops the storm and waits for its last chmod, at most
// benchBurstDeadline.
func (s *storm) end() error {
	close(s.stop)

	select {
	case <-s.ended:
	case <-time.After(benchBurstDeadline):
		return fmt.Errorf("the storm's last chmod still running after %v", benchBurstDeadline)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failed
}

// mark is how many chmods the storm has made so far.
func (s *storm) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.done)
}

// since is the chmods made after mark from, and the distinct writers' files
// among them.
func (s *storm) since(from int) (chmods, writersTouched int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var touched []int
	for _, d := range s.done[from:] {
		if d.file < benchWriters && !slices.Contains(touched, d.file) {
			touched = append(touched, d.file)
		}
	}

	return len(s.done) - from, len(touched)
}

func benchB7(b *testing.B) {
	c := composeBench(b, b7Files, b7ExistenceAge)
	ctx := b.Context()
	data := make([]byte, mib)

	var fsyncs []time.Duration

	most, evaluated := 0, 0

	b.ResetTimer()

	for i := range b.N {
		s := startStorm(ctx, c)

		// The storm's own cost, in a window with no fsync: a bounded wait is
		// the measurement here, as the storm runs on the real clock.
		c.counts.TakeUpdates()

		from := s.mark()

		<-time.After(b7Calibrate)

		alone := c.counts.TakeUpdates().Updates

		chmods, _ := s.since(from)
		if chmods == 0 {
			b.Fatalf("run %d: the storm made no chmod in %v", i+1, b7Calibrate)
		}

		perChmod := float64(alone) / float64(chmods)

		for k := range b5Bursts {
			off := int64(i*b5Bursts+k) * mib
			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				ref := vfs.Anonymous(c.files[w])
				_, err := c.fs.Write(ctx, c.admin, ref, off, data, vfs.Unstable)

				return err
			}); err != nil {
				b.Fatalf("burst %d of run %d: a write refused: %v", k+1, i+1, err)
			}

			before := c.committedSizes(b)
			c.counts.TakeUpdates()

			from := s.mark()

			took := make([]time.Duration, benchWriters)
			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				start := time.Now()
				_, err := c.fs.Commit(ctx, c.admin, vfs.Anonymous(c.files[w]), 0, 0)
				took[w] = time.Since(start)

				return err
			}); err != nil {
				b.Fatalf("burst %d of run %d: an fsync failed: %v", k+1, i+1, err)
			}

			updates := c.counts.TakeUpdates().Updates
			chmods, touched := s.since(from)

			fsyncs = append(fsyncs, took...)

			// A group commit of earlier bursts that came due during this one,
			// off the fsyncs' path, may split on the storm's conflicts as well:
			// such a burst is not evaluated, as the bound counts one group commit.
			if !slices.Equal(before, c.committedSizes(b)) {
				continue
			}

			engine := updates - int(math.Ceil(perChmod*float64(chmods)))

			bound := 1 + touched*b7Log2G
			if engine > bound {
				b.Fatalf(
					"burst %d of run %d: %d store transactions for %d fsyncs once the storm's "+
						"%d chmods "+
						"(%.2f transactions each) are taken out, "+
						"want at most 1 + %d conflicting files × %d = %d",
					k+1,
					i+1,
					engine,
					benchWriters,
					chmods,
					perChmod,
					touched,
					b7Log2G,
					bound,
				)
			}

			most, evaluated = max(most, engine), evaluated+1
		}

		if err := s.end(); err != nil {
			b.Fatalf("run %d: a chmod of the storm failed: %v", i+1, err)
		}
	}

	b.StopTimer()
	b.ReportMetric(microseconds(percentile(fsyncs, 99)), "fsync-p99-µs")
	b.ReportMetric(float64(most), "txn/stability-point")
	b.ReportMetric(float64(evaluated), "evaluated-bursts")

	if bursts := b.N * b5Bursts; 2*evaluated < bursts {
		b.Fatalf(
			"%d of %d bursts evaluated, want at least half: a group commit came due by its timer "+
				"during the rest, so the bound went unchecked",
			evaluated,
			bursts,
		)
	}
}
