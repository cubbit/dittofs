//go:build engine_conformance && engine_bench

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// B9's workload: each writer rewrites b9Write bytes in place, moving through
// the b9Size bytes of committed content it starts with, for b9RunFor.
const (
	b9Size   = mib
	b9Write  = 4 << 10
	b9RunFor = 3 * time.Second
	b9Flush  = time.Second // the unstable run's flush interval
)

// BenchmarkB9 is RFC 8 §15.4's B9, stable overwrites on a continuously
// available share, through the filesystem service rather than an adapter: 64
// writers rewriting committed content in place with every write stable,
// against the same run with unstable writes and one flush per second. It
// reports the write latency and the store transactions per burst and per
// write, and never fails: RFC 8 makes a miss of its targets, counts included,
// the evidence for the write-intent upgrade of §5.1.
//
// The run with one unrelated append held in flight for 100 ms is not here: it
// needs a pass-through over the journal's storage that holds one write, and
// the storage option's type is internal to the journal's packages, so only a
// wrapper written there can hold it. It joins once such a wrapper exists. The
// run with a snapshot cut every 10 s waits for snapshots.
func BenchmarkB9(b *testing.B) {
	b.Run("stable/writers=64", func(b *testing.B) {
		benchB9(b, vfs.FileSync)
	})
	b.Run("unstable/writers=64", func(b *testing.B) {
		benchB9(b, vfs.Unstable)
	})
}

// benchB9 runs B9's workload with writes of stability s; an unstable run
// flushes every file once per b9Flush.
func benchB9(b *testing.B, s vfs.Stability) {
	c := composeBench(b, benchWriters)
	ctx := b.Context()
	data := make([]byte, b9Write)
	fill := make([]byte, b9Size)
	if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
		_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(c.files[w]), 0, fill, vfs.Unstable)
		return err
	}); err != nil {
		b.Fatalf("the content to rewrite: a write refused: %v", err)
	}
	c.awaitSizes(b, b9Size, 30*time.Second+10*benchExistenceAge)
	var writes []time.Duration
	var total time.Duration
	most, updates, bursts := 0, 0, 0
	b.ResetTimer()

	for range b.N {
		end := time.Now().Add(b9RunFor)
		flushed := time.Now()
		for off := int64(0); time.Now().Before(end); off = (off + b9Write) % b9Size {
			c.counts.TakeUpdates()
			took := make([]time.Duration, benchWriters)
			if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
				start := time.Now()
				_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(c.files[w]), off, data, s)
				took[w] = time.Since(start)
				return err
			}); err != nil {
				b.Fatalf("burst %d: a write failed: %v", bursts+1, err)
			}
			n := c.counts.TakeUpdates().Updates
			most, updates, bursts = max(most, n), updates+n, bursts+1
			writes = append(writes, took...)
			for _, d := range took {
				total += d
			}

			if s == vfs.Unstable && time.Since(flushed) >= b9Flush {
				if err := together(ctx, benchWriters, func(ctx context.Context, w int) error {
					_, err := c.fs.Commit(ctx, c.admin, vfs.Anonymous(c.files[w]), 0, 0)
					return err
				}); err != nil {
					b.Fatalf("the flush after burst %d failed: %v", bursts, err)
				}
				flushed = time.Now()
			}
		}
	}

	b.StopTimer()
	if s == vfs.FileSync && most > 1 {
		b.Logf("stable writes cost up to %d store transactions per burst, over one per journal: "+
			"the evidence for the write-intent upgrade", most)
	}
	b.ReportMetric(microseconds(percentile(writes, 99)), "write-p99-µs")
	b.ReportMetric(microseconds(total/time.Duration(max(len(writes), 1))), "write-mean-µs")
	b.ReportMetric(float64(most), "txn/burst")
	b.ReportMetric(float64(updates)/float64(max(len(writes), 1)), "txn/write")
}
