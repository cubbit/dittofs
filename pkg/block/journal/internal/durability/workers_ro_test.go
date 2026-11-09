package durability_test

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// writeOps are the seam calls that change storage. A read-only journal makes
// none of them; open, read, read_direct, list and close change nothing and are
// left out.
var writeOps = []string{
	journaltest.OpCreate, journaltest.OpAllocate, journaltest.OpWrite,
	journaltest.OpSync, journaltest.OpTruncate, journaltest.OpRename,
	journaltest.OpRemove, journaltest.OpSyncDir,
}

// workerChecks are the read-only checks each background worker is handed; a
// test that swaps one for nil proves the assertion names that worker's call.
type workerChecks struct{ pool, idle, retire func() error }

// runWorkers wires the real pool, idle sealer and retirer to d.Admit over a
// seam, leaves each of them one piece of work (a pool below its target, an
// active segment idle past the seal age, a sealed segment awaiting
// retirement), optionally enters read-only, then drives all three and returns
// how many times each write op reached the seam since the entry point, with
// what one Make of a spare costs in write ops. The layer's probe makes its
// spare through the same maker, which consults no read-only check, as the
// probe's spare is the one write a read-only journal makes.
func runWorkers(t *testing.T, enter bool, mutate func(*workerChecks)) (delta, makeCost map[string]int, d *durability.Durability) {
	t.Helper()
	prof.Watch(t, prof.Options{})

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	clock := journaltest.NewStopwatchClock()
	var maker *segment.Maker
	d, err = durability.New(durability.Config{
		Clock:    clock,
		Domains:  noDomains{},
		Index:    index.New(1 << 20),
		Dir:      t.TempDir(),
		Counters: &index.Counters{},
		MarkLoss: func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error) { return index.HeldDelta{}, nil },
		Check:    func() error { return nil },
		MakeSpare: func() error {
			_, err := maker.Make()
			return err
		},
	})
	if err != nil {
		t.Fatalf("durability.New: %v", err)
	}
	checks := workerChecks{pool: d.Admit, idle: d.Admit, retire: d.Admit}
	if mutate != nil {
		mutate(&checks)
	}

	fsys := d.Observe(seam.FS())
	set := segment.NewSet(harnessCap, nil)
	maker = segment.NewMaker(fsys, set, 1<<62, 0, nil)
	pool := segment.NewPool(maker, segment.Targets{Content: 2}, checks.pool, nil)
	t.Cleanup(pool.Stop)
	pool.Start()
	awaitSpares(t, pool, segment.Targets{Content: 2})
	cache := segment.NewDescriptors(fsys, format.SegmentName, core.NewDescriptorBudget(64))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	streams := segment.NewStreams(1, harnessIdentity, set, pool, cache, d.Sink)
	t.Cleanup(func() { _ = streams.Close() })
	t.Cleanup(d.Stop)
	d.Start(streams)

	// The idle sealer's threshold is below the record the stream holds, and
	// the record is synced, so nothing is unsynced when the journal turns
	// read-only.
	idle := segment.NewIdleSealer(streams, clock, 8, checks.idle, nil)
	t.Cleanup(idle.Stop)
	idle.Start()
	s := streams.Stream(0)
	h := format.RecordHeader{Kind: format.KindWrite, File: file(1), Extent: core.Extent{Len: 64}, Version: core.Version{Counter: 1}, Tag: 1}
	if _, err := s.Append(h, format.RecordBody{Payload: make([]byte, 64), MTime: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	awaitSpares(t, pool, segment.Targets{Content: 2})

	// A sealed, not full segment: its retirement is an unlink. Its Make is
	// the measure of what the probe's spare costs.
	counts := func() map[string]int {
		m := map[string]int{}
		for _, op := range writeOps {
			m[op] = seam.Calls(op)
		}
		return m
	}
	beforeMake := counts()
	retired, err := maker.Make()
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	makeCost = counts()
	for op, n := range beforeMake {
		makeCost[op] -= n
	}
	if err := set.Take(retired); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := set.Seal(retired, false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	retirer := segment.NewRetirer(segment.RetirerConfig{
		Set: set, Cache: cache, Pool: pool, Maker: maker, FS: fsys, ReadOnly: checks.retire,
	})
	t.Cleanup(retirer.Stop)
	retirer.Start()
	awaitClockArmed(t, clock)

	if enter {
		armed := clock.Pending()
		d.Enter(syscall.EROFS)
		eventually(t, "the probe to arm its wait", func() bool { return clock.Pending() > armed })
	}
	before := counts()

	// The pool falls below its target, the clock passes the idle seal age and
	// the probe interval, and the retirement is queued and run directly.
	for range 2 {
		if _, err := pool.Take(segment.Content); err != nil {
			t.Fatalf("Take: %v", err)
		}
	}
	pool.Kick()
	clock.Advance(2*core.IdleSealAge + 2*core.ProbeInterval)
	retirer.Enqueue(retired, segment.NotHeld)
	_, retireErr := retirer.Retire(retired, segment.NotHeld)
	if enter {
		if !errors.Is(retireErr, core.ErrJournalReadOnly) || !errors.Is(retireErr, syscall.EROFS) {
			t.Errorf("Retire in a read-only journal = %v, want core.ErrJournalReadOnly wrapping EROFS", retireErr)
		}
		eventually(t, "the probe's spare", func() bool { return d.ReadOnlyState().SpareMade })
		// A refusal leaves nothing to wait for, so give the workers a bounded
		// window to prove they stay still.
		time.Sleep(100 * time.Millisecond)
	} else {
		if retireErr != nil && !errors.Is(retireErr, segment.ErrNotInSet) && !errors.Is(retireErr, segment.ErrClaimed) {
			t.Errorf("Retire in a writable journal = %v", retireErr)
		}
		// The queue worker may have taken the retirement first.
		eventually(t, "the unlink", func() bool { return seam.Calls(journaltest.OpRemove) > before[journaltest.OpRemove] })
		eventually(t, "the idle seal", func() bool { return seam.Calls(journaltest.OpTruncate) > before[journaltest.OpTruncate] })
		eventually(t, "the pool's refill", func() bool { return pool.Counters().Spares >= 2 })
	}

	delta = counts()
	for op, n := range before {
		delta[op] -= n
	}
	return delta, makeCost, d
}

func awaitClockArmed(t *testing.T, c journaltest.DrivenClock) {
	t.Helper()
	eventually(t, "the idle sealer to arm its wait", func() bool { return c.Pending() >= 1 })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestWorkersCreateSealAndUnlinkBeforeReadOnly is the control: with the
// journal writable the same wiring refills the pool (create, allocate,
// rename), seals the idle segment (write, sync, and a truncate of the early
// seal) and unlinks the retired one (remove).
func TestWorkersCreateSealAndUnlinkBeforeReadOnly(t *testing.T) {
	delta, _, _ := runWorkers(t, false, nil)
	for _, op := range []string{journaltest.OpCreate, journaltest.OpAllocate, journaltest.OpRename, journaltest.OpWrite, journaltest.OpSync, journaltest.OpTruncate, journaltest.OpRemove} {
		if delta[op] == 0 {
			t.Errorf("a writable journal's workers made no %s call, so the read-only check below would prove nothing: %v", op, delta)
		}
	}
}

// TestWorkersMakeNoSpareAndWriteNothingOnceReadOnly asserts the consumer: the
// storage seam's call log gains, after entry, exactly the write ops of one
// spare, the probe's, and its one create is the only file made; and Admit
// refuses at once.
func TestWorkersMakeNoSpareAndWriteNothingOnceReadOnly(t *testing.T) {
	assertQuiet(t, nil)
}

func assertQuiet(t *testing.T, mutate func(*workerChecks)) {
	t.Helper()
	delta, makeCost, d := runWorkers(t, true, mutate)
	if makeCost[journaltest.OpCreate] != 1 {
		t.Fatalf("one Make cost %v, want exactly one create", makeCost)
	}
	if delta[journaltest.OpCreate] != 1 {
		t.Errorf("the seam saw %d creates after the journal went read-only, want the probe's one: %v", delta[journaltest.OpCreate], delta)
	}
	for _, op := range writeOps {
		if delta[op] != makeCost[op] {
			t.Errorf("the seam saw %d %s call(s) after the journal went read-only, want the probe's spare's %d: %v", delta[op], op, makeCost[op], delta)
		}
	}
	if !d.ReadOnlyState().SpareMade {
		t.Error("the probe made no spare")
	}
	if err := d.Admit(); !errors.Is(err, core.ErrJournalReadOnly) {
		t.Errorf("Admit = %v, want core.ErrJournalReadOnly", err)
	}
}
