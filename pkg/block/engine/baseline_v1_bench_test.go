package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger"
)

// The baseline workloads. Names, shapes and metric units match the engine
// that replaces this one, so benchstat pairs the two result files: B5 is 64
// writers each fsyncing every 1 MiB, B9 is 64 writers rewriting 4 KiB in
// place over 1 MiB of committed content, stable against unstable with one
// flush per second.
const (
	baselineDirEnv  = "DITTOFS_BENCH_DIR"
	baselineShare   = "/baseline"
	baselineWriters = 64
	baselineMiB     = 1 << 20
	b9Size          = baselineMiB
	b9Write         = 4 << 10
	b9RunFor        = 3 * time.Second
	b9Flush         = time.Second
)

// baselineBursts is B5's bursts per run. Zero runs B5's setup and close
// with no burst, the floor the fsync proof subtracts from a real run's count.
var baselineBursts = flag.Int(
	"baseline.bursts",
	8,
	"B5's bursts per run; 0 runs only its setup and close",
)

// baselineMarks prints, for each B5 burst, when its COMMIT phase started and
// ended, in Unix nanoseconds, so a trace of the run can show a journal sync
// inside each one.
var baselineMarks = flag.Bool("baseline.marks", false, "print each B5 COMMIT phase's start and end")

// BenchmarkB5 drives what an NFSv3 WRITE and COMMIT call for 64 writers
// each writing 1 MiB and then committing, appending, then overwriting
// committed content. It reports the commit latency's p99 and the bytes
// written per second of the timed part.
func BenchmarkB5(b *testing.B) {
	b.Run("append/writers=64", func(b *testing.B) {
		benchB5(b, false)
	})
	b.Run("overwrite/writers=64", func(b *testing.B) {
		benchB5(b, true)
	})
}

// BenchmarkB9 drives 64 writers rewriting committed content in place, every
// write FILE_SYNC, against the same run with unstable writes and one COMMIT
// of every file per second. It reports the write latency's p99 and mean and
// the bytes written per second of the timed part.
func BenchmarkB9(b *testing.B) {
	b.Run("stable/writers=64", func(b *testing.B) {
		benchB9(b, true)
	})
	b.Run("unstable/writers=64", func(b *testing.B) {
		benchB9(b, false)
	})
}

func benchB5(b *testing.B, overwrite bool) {
	c := newBaseline(b)
	ctx := b.Context()

	if overwrite {
		for k := range *baselineBursts {
			c.burst(b, ctx, uint64(k)*baselineMiB, baselineMiB, false)
		}

		c.commitAll(b, ctx)
	}

	var fsyncs []time.Duration

	c.wrote.Store(0)
	c.metaFailed.Store(0)
	c.metaFlushes.Store(0)
	b.ResetTimer()

	start := time.Now()

	for i := range b.N {
		for k := range *baselineBursts {
			off := uint64(k) * baselineMiB
			if !overwrite {
				off = uint64(i*(*baselineBursts)+k) * baselineMiB
			}

			c.burst(b, ctx, off, baselineMiB, false)

			committing := time.Now()

			fsyncs = append(fsyncs, c.commitAll(b, ctx)...)

			if *baselineMarks {
				fmt.Printf(
					"baseline-mark commit %d %d\n",
					committing.UnixNano(),
					time.Now().UnixNano(),
				)
			}
		}
	}

	elapsed := time.Since(start)

	b.StopTimer()

	b.ReportMetric(0, "ns/op") // the waits are in the wall time: keep benchstat off it
	b.ReportMetric(microseconds(percentileOf(fsyncs, 99)), "fsync-p99-µs")
	b.ReportMetric(float64(c.wrote.Load())/baselineMiB/elapsed.Seconds(), "MB/s")
	c.reportMetaFails(b)

	// The bytes behind MB/s are the workload's: every writer's 1 MiB per burst.
	want := int64(b.N) * int64(*baselineBursts) * baselineMiB * baselineWriters
	if got := c.wrote.Load(); got != want {
		b.Fatalf("the timed part wrote %d bytes, the workload is %d", got, want)
	}

	size := uint64(*baselineBursts) * baselineMiB
	if !overwrite {
		size *= uint64(b.N)
	}

	c.settle(b, ctx)
	c.reopenAndCheck(b, size)
}

func benchB9(b *testing.B, stable bool) {
	c := newBaseline(b)
	ctx := b.Context()

	c.burst(b, ctx, 0, b9Size, false)
	c.commitAll(b, ctx)

	var (
		writes []time.Duration
		total  time.Duration
	)

	c.wrote.Store(0)
	c.metaFailed.Store(0)
	c.metaFlushes.Store(0)
	b.ResetTimer()

	start := time.Now()

	for range b.N {
		end := time.Now().Add(b9RunFor)
		flushed := time.Now()

		for off := uint64(0); time.Now().Before(end); off = (off + b9Write) % b9Size {
			took := c.burst(b, ctx, off, b9Write, stable)

			writes = append(writes, took...)
			for _, d := range took {
				total += d
			}

			if !stable && time.Since(flushed) >= b9Flush {
				c.commitAll(b, ctx)

				flushed = time.Now()
			}
		}
	}

	elapsed := time.Since(start)

	b.StopTimer()

	b.ReportMetric(0, "ns/op") // the fixed loop is not a latency: keep benchstat off it
	b.ReportMetric(microseconds(percentileOf(writes, 99)), "write-p99-µs")
	b.ReportMetric(microseconds(total/time.Duration(max(len(writes), 1))), "write-mean-µs")
	b.ReportMetric(float64(c.wrote.Load())/baselineMiB/elapsed.Seconds(), "MB/s")
	c.reportMetaFails(b)

	// The last unstable writes have no COMMIT yet; a client would send one.
	c.commitAll(b, ctx)
	c.settle(b, ctx)
	c.reopenAndCheck(b, b9Size)
}

// baseline is v1's write path as a share serves it: the metadata service over
// an on-disk badger store, and the block store over the journal on the same
// device, with an in-memory remote whose offload never runs, so a figure
// measures the local disk path alone.
type baseline struct {
	dir   string
	meta  *badger.BadgerMetadataStore
	svc   *metadata.Service
	bs    *Store
	files []metadata.FileHandle
	ids   []metadata.PayloadID

	// last is, per writer, the offset and the bytes of its last write, read
	// back after a reopen.
	last []lastWrite
	bufs [][]byte
	seq  []uint64

	// metaFailed counts the metadata flushes that failed, a badger
	// transaction conflict between concurrent flushes among them.
	metaFailed atomic.Int64

	// wrote counts the bytes the block store took, for MB/s.
	wrote       atomic.Int64
	metaFlushes atomic.Int64
}

type lastWrite struct {
	off  uint64
	data []byte
}

func newBaseline(b *testing.B) *baseline {
	b.Helper()
	silenceLoggerForBench(b)

	root := os.Getenv(baselineDirEnv)
	if root == "" {
		b.Skipf("set %s to a directory on the device under test", baselineDirEnv)
	}

	dir, err := os.MkdirTemp(root, "baseline-v1-")
	if err != nil {
		b.Fatalf("the run's directory: %v", err)
	}

	b.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	c := &baseline{
		dir:  dir,
		last: make([]lastWrite, baselineWriters),
		bufs: make([][]byte, baselineWriters),
		seq:  make([]uint64, baselineWriters),
	}
	c.open(b)
	b.Cleanup(c.close)

	ctx := context.Background()

	rootFile, err := c.meta.CreateRootDirectory(ctx, baselineShare, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	})
	if err != nil {
		b.Fatalf("the share's root: %v", err)
	}

	rootHandle, err := metadata.EncodeShareHandle(baselineShare, rootFile.ID)
	if err != nil {
		b.Fatalf("the root's handle: %v", err)
	}

	for w := range baselineWriters {
		f, _, err := c.svc.CreateFile(
			c.auth(ctx),
			rootHandle,
			fmt.Sprintf("f%d", w),
			&metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0o644,
			},
		)
		if err != nil {
			b.Fatalf("file %d: %v", w, err)
		}

		h, err := metadata.EncodeShareHandle(baselineShare, f.ID)
		if err != nil {
			b.Fatalf("file %d's handle: %v", w, err)
		}

		c.files = append(c.files, h)
		c.ids = append(c.ids, f.PayloadID)
		c.bufs[w] = make([]byte, baselineMiB)
	}

	return c
}

// open opens the metadata store, the service and the block store in c.dir.
func (c *baseline) open(b *testing.B) {
	b.Helper()

	ctx := context.Background()

	meta, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, c.dir+"/metadata")
	if err != nil {
		b.Fatalf("the metadata store: %v", err)
	}

	svc := metadata.New()
	if err := svc.RegisterStoreForShare(baselineShare, meta); err != nil {
		b.Fatalf("the share's store: %v", err)
	}

	local, err := journal.Open(c.dir+"/journal", journal.Config{})
	if err != nil {
		b.Fatalf("the journal: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ManualSync = true // no offload: the figure is the local disk path

	rem := remotememory.New()

	bs, err := New(BlockStoreConfig{
		Local:           local,
		Remote:          rem,
		RemoteSync:      NewRemoteSync(local, rem, meta, cfg),
		FileChunkStore:  meta,
		SyncedHashStore: meta,
	})
	if err != nil {
		b.Fatalf("the block store: %v", err)
	}

	if err := bs.Start(ctx); err != nil {
		b.Fatalf("the block store's start: %v", err)
	}

	// A Flush that waits on the remote would put the remote in the figures.
	if !bs.LocalDurable() || bs.RequireDurableCommit() {
		b.Fatalf(
			"the block store's Flush waits on the remote: local durable %v, "+
				"durable commit required %v",
			bs.LocalDurable(),
			bs.RequireDurableCommit(),
		)
	}

	c.meta, c.svc, c.bs = meta, svc, bs
}

func (c *baseline) close() {
	if c.bs != nil {
		_ = c.bs.Close()
	}

	if c.meta != nil {
		_ = c.meta.Close()
	}

	c.bs, c.meta = nil, nil
}

func (c *baseline) auth(ctx context.Context) *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		ClientAddr: "127.0.0.1",
	}
}

// write is one NFSv3 WRITE of p at off to writer w's file, in the calls the
// handler makes: PrepareWrite, the block store's WriteAt, CommitWrite, and for
// FILE_SYNC the block store's Flush and the durable metadata flush.
func (c *baseline) write(ctx context.Context, w int, off uint64, p []byte, stable bool) error {
	auth := c.auth(ctx)

	op, err := c.svc.PrepareWrite(auth, c.files[w], off+uint64(len(p)))
	if err != nil {
		return fmt.Errorf("PrepareWrite: %w", err)
	}

	if _, err := c.bs.WriteAt(ctx, string(op.PayloadID), nil, p, off); err != nil {
		return fmt.Errorf("WriteAt: %w", err)
	}

	c.wrote.Add(int64(len(p)))

	if _, err := c.svc.CommitWrite(auth, op); err != nil {
		return fmt.Errorf("CommitWrite: %w", err)
	}

	if stable {
		_, err := c.commit(ctx, w, true)

		return err
	}

	return nil
}

// commit is the handler's flush of writer w's file: the block store's Flush,
// then the metadata flush, durable for FILE_SYNC and relaxed for COMMIT. It
// answers whether the metadata flush succeeded. A failed one is counted and
// is no error, as the handlers answer it: COMMIT logs it and succeeds, and a
// FILE_SYNC write is answered UNSTABLE.
func (c *baseline) commit(ctx context.Context, w int, durable bool) (bool, error) {
	if _, err := c.bs.Flush(ctx, string(c.ids[w])); err != nil {
		return false, fmt.Errorf("Flush: %w", err)
	}

	c.metaFlushes.Add(1)

	if _, err := c.svc.FlushPendingWriteForFile(c.auth(ctx), c.files[w], durable); err != nil {
		c.metaFailed.Add(1)

		return false, nil
	}

	return true, nil
}

// reportMetaFails reports the timed part's failed metadata flushes, as a
// count and as a share of its flushes, and fails a run where more than half
// failed: its figures would measure the conflicts, not the write path.
func (c *baseline) reportMetaFails(b *testing.B) {
	b.Helper()

	failed, flushes := c.metaFailed.Load(), c.metaFlushes.Load()
	b.ReportMetric(float64(failed), "meta-flush-fails")
	b.ReportMetric(100*float64(failed)/float64(max(flushes, 1)), "meta-flush-fail-%")

	if 2*failed > flushes {
		b.Fatalf("%d of %d metadata flushes failed, more than half", failed, flushes)
	}
}

// settle flushes every file's metadata until it succeeds, outside the timed
// part, so the reopen reads what a client's last COMMIT asked for.
func (c *baseline) settle(b *testing.B, ctx context.Context) {
	b.Helper()

	for w := range c.files {
		for attempt := 1; ; attempt++ {
			ok, err := c.commit(ctx, w, true)
			if err != nil {
				b.Fatalf("file %d's last flush: %v", w, err)
			}

			if ok {
				break
			}

			if attempt == 20 {
				b.Fatalf("file %d's metadata flush failed %d times", w, attempt)
			}
		}
	}
}

// burst has every writer write n bytes at off at once, each write stamped
// with the writer and its sequence so a read after a reopen tells it apart,
// and answers how long each write took.
func (c *baseline) burst(
	b *testing.B,
	ctx context.Context,
	off uint64,
	n int,
	stable bool,
) []time.Duration {
	b.Helper()

	// A rewrite past the content would grow the file B9 keeps at its size.
	if n == b9Write && off+uint64(n) > b9Size {
		b.Fatalf(
			"a B9 rewrite at %d of %d bytes reaches past the %d bytes it rewrites",
			off,
			n,
			b9Size,
		)
	}

	took := make([]time.Duration, baselineWriters)

	err := together(func(w int) error {
		c.seq[w]++
		p := c.bufs[w][:n]
		binary.LittleEndian.PutUint64(p, uint64(w)<<32|c.seq[w])

		start := time.Now()
		err := c.write(ctx, w, off, p, stable)
		took[w] = time.Since(start)

		c.last[w] = lastWrite{off: off, data: slices.Clone(p[:8])}

		return err
	})
	if err != nil {
		b.Fatalf("a write at %d: %v", off, err)
	}

	return took
}

// commitAll sends every file a COMMIT at once and answers how long each took.
func (c *baseline) commitAll(b *testing.B, ctx context.Context) []time.Duration {
	b.Helper()

	took := make([]time.Duration, baselineWriters)

	err := together(func(w int) error {
		start := time.Now()
		_, err := c.commit(ctx, w, false)
		took[w] = time.Since(start)

		return err
	})
	if err != nil {
		b.Fatalf("a commit: %v", err)
	}

	return took
}

// reopenAndCheck closes the stores, opens them again from the device, and
// requires each file's committed size to be size and its last write's stamp
// to read back: a run whose metadata or bytes stayed in memory fails here.
func (c *baseline) reopenAndCheck(b *testing.B, size uint64) {
	b.Helper()

	c.close()
	c.open(b)

	ctx := context.Background()
	got := make([]byte, 8)

	for w, h := range c.files {
		f, err := c.svc.GetFile(ctx, h)
		if err != nil {
			b.Fatalf("file %d after the reopen: %v", w, err)
		}

		if f.Size != size {
			b.Fatalf("file %d after the reopen: committed size %d, want %d", w, f.Size, size)
		}

		if c.last[w].data == nil {
			continue // no write to read back: a run with no burst
		}

		if _, err := c.bs.ReadAt(ctx, string(c.ids[w]), got, c.last[w].off); err != nil {
			b.Fatalf("file %d after the reopen: read at %d: %v", w, c.last[w].off, err)
		}

		if !bytes.Equal(got, c.last[w].data) {
			b.Fatalf(
				"file %d after the reopen: at %d read %x, want %x",
				w,
				c.last[w].off,
				got,
				c.last[w].data,
			)
		}
	}
}

// together runs fn for every writer at once and answers the first error.
func together(fn func(w int) error) error {
	errs := make([]error, baselineWriters)

	var wg sync.WaitGroup
	for w := range baselineWriters {
		wg.Go(func() {
			errs[w] = fn(w)
		})
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// percentileOf is the p-th percentile of ds, nearest rank.
func percentileOf(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}

	s := slices.Clone(ds)
	slices.Sort(s)

	return s[min(len(s)-1, (len(s)*p+99)/100-1)]
}

func microseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Microsecond)
}
