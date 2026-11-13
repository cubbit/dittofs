//go:build engine_conformance && engine_bench

package integration

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// b5Bursts is how many bursts one run makes: each writer writes 1 MiB and
// then fsyncs, b5Bursts times.
const b5Bursts = 8

// BenchmarkB5 is RFC 8 §15.4's B5, the group commit: 64 writers each with
// an fsync every 1 MiB, appending, then overwriting committed content. The
// counts follow from RFC 8 §5.1 and §5.2, so a run that breaks one fails; the
// latency is reported.
func BenchmarkB5(b *testing.B) {
	b.Run("append/writers=64", benchB5Append)
	b.Run("overwrite/writers=64", benchB5Overwrite)
}

// benchB5Append appends: an fsync's path holds no existence commit, only the
// marks wait's transaction, at most one per journal per burst, and the
// existence commits that follow are at most one per journal per existence
// interval.
func benchB5Append(b *testing.B) {
	c := composeBench(b, benchWriters, benchExistenceAge, 0)
	ctx := b.Context()
	data := make([]byte, mib)

	var fsyncs []time.Duration

	most := 0

	b.ResetTimer()

	for i := range b.N {
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

			fsyncs = append(fsyncs, took...)

			// decision: a group commit of earlier bursts' existence may come
			// due during this burst on its own timer, off every fsync's path,
			// and the store shows it as committed sizes that moved; it is
			// allowed as one transaction, the batch bound holding all 64 files.
			allowed := 1
			if !slices.Equal(before, c.committedSizes(b)) {
				allowed++
			}

			if updates > allowed {
				b.Fatalf(
					"burst %d of run %d: %d store transactions across %d fsyncs, want at most %d: "+
						"one per journal, never one per file",
					k+1,
					i+1,
					updates,
					benchWriters,
					allowed,
				)
			}

			most = max(most, updates)
		}

		// The window starts once the burst's last fsync has returned, so no
		// fsync's transaction is counted in it.
		start := time.Now()

		c.counts.TakeUpdates()
		c.awaitSizes(b, int64((i+1)*b5Bursts)*mib, 30*time.Second+10*benchExistenceAge)
		commits := c.counts.TakeUpdates().Updates

		intervals := int(math.Ceil(float64(time.Since(start)) / float64(benchExistenceAge)))
		if commits > intervals {
			b.Fatalf(
				"run %d: %d existence transactions in %d existence intervals with no fsync, "+
					"want at most "+
					"one per journal per interval",
				i+1,
				commits,
				intervals,
			)
		}
	}

	b.StopTimer()
	b.ReportMetric(microseconds(percentile(fsyncs, 99)), "fsync-p99-µs")
	b.ReportMetric(float64(most), "txn/burst")
}

// benchB5Overwrite overwrites committed content: each burst's fsyncs cost at
// most one transaction per journal, never one per file.
func benchB5Overwrite(b *testing.B) {
	c := composeBench(b, benchWriters, benchExistenceAge, 0)
	ctx := b.Context()
	data := make([]byte, mib)

	for k := range b5Bursts {
		if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
			ref := vfs.Anonymous(c.files[w])
			_, err := c.fs.Write(ctx, c.admin, ref, int64(k)*mib, data, vfs.Unstable)

			return err
		}); err != nil {
			b.Fatalf("the content to overwrite: a write refused: %v", err)
		}
	}

	c.awaitSizes(b, b5Bursts*mib, 30*time.Second+10*benchExistenceAge)

	var fsyncs []time.Duration

	most := 0

	b.ResetTimer()

	for i := range b.N {
		for k := range b5Bursts {
			written := time.Now()

			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				ref := vfs.Anonymous(c.files[w])
				_, err := c.fs.Write(ctx, c.admin, ref, int64(k)*mib, data, vfs.Unstable)

				return err
			}); err != nil {
				b.Fatalf("burst %d of run %d: a write refused: %v", k+1, i+1, err)
			}

			c.counts.TakeUpdates()

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

			fsyncs = append(fsyncs, took...)

			// decision: the burst's writes come due for a group commit of
			// their own an existence age after they were made; a burst that
			// outlasts it may see that commit beside the fsyncs' own, so one
			// more transaction is allowed only then.
			allowed := 1
			if time.Since(written) >= benchExistenceAge {
				allowed++
			}

			if updates > allowed {
				b.Fatalf(
					"burst %d of run %d: %d store transactions across %d fsyncs of overwrites, "+
						"want at "+
						"most %d: one per journal, never one per file",
					k+1,
					i+1,
					updates,
					benchWriters,
					allowed,
				)
			}

			most = max(most, updates)
		}
	}

	b.StopTimer()
	b.ReportMetric(microseconds(percentile(fsyncs, 99)), "fsync-p99-µs")
	b.ReportMetric(float64(most), "txn/burst")
}
