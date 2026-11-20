package datapath

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// A journal whose storage answers EROFS turns read-only at the first seam
// call that meets it, and the admission then refuses every later write at
// once — RFC 1 §5.4: while read-only every call that would append fails with
// ErrJournalReadOnly, reserves nothing and takes no spare. These tests cover
// the seam wrap, Durability.Observe, that makes the EROFS of any storage call
// the journal's own read-only cause, and the Admit wiring that carries the
// refusal to the admission before a byte is read of any limit.

// TestCore_EROFSTurnsJournalReadOnly covers the seam wrap: a spare the pool
// tries to make fails with EROFS, the journal enters read-only through
// Observe, and the next WriteAt is refused with ErrJournalReadOnly before any
// seam call and without taking a spare.
func TestCore_EROFSTurnsJournalReadOnly(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	// A warm-up write gives the two streams each an active segment, so a
	// later write demanding a spare needs one a rolled piece cannot spare.
	if _, err := c.WriteAt(ctx, s, testFile(1), 0, []byte("w"), 1); err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}

	// Every later segment create fails with EROFS: the pool's refill meets it
	// on the spare it makes for the write below, whose hold the pool could
	// not serve. The pool is at its target first, so the fault meets its
	// first create only once the refused write below has kicked a refill.
	awaitSettledSpares(t, c, 2*testStreams)
	removes0 := seam.Calls(kit.OpRemove)
	if err := seam.Schedule(kit.Failing(kit.OpCreate, "*", 0, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	// The write that hits it: its pieces need more spares than ready, so its
	// hold is refused naming the pool and the refill the refusal kicked
	// meets the EROFS on its create.
	p := make([]byte, 4*testSegmentCap+roomAfter(0))
	g := testFile(2)
	if streamOf(c, g) == streamOf(c, testFile(1)) {
		g = testFile(3)
	}
	_, err := c.WriteAt(ctx, s, g, 0, p, 1)
	_ = refused(t, err, core.LimitSpare)

	// The EROFS the refill met turns the journal read-only: Enter ran inside
	// the seam wrap, so the refusal is visible once the call that carried it
	// has returned.
	waitIn(t, 30e9, "the journal to turn read-only", func() bool {
		return c.Durability().Admit() != nil
	})
	// The failed make has one more seam call to make: its cleanup removes the
	// spare's temporary name, whose create failed at its first step. Enter
	// runs inside the wrapped Create, before Make has returned, so that
	// Remove lands after the read-only state is already visible — waiting on
	// Enter alone can return inside that window — and the baseline below must
	// be taken once the whole failed pass is done. A create-step failure
	// names only the spare's temporary name, and read-only stops every later
	// pass at its own check before any seam call, so this is exactly one more
	// Remove and then none.
	waitIn(t, 30e9, "the failed make to clean up after itself", func() bool {
		return seam.Calls(kit.OpRemove) >= removes0+1
	})

	// The refused write consulted no storage of its own and took no spare,
	// and the read-only one after it neither: the pool's counters and the
	// seam's calls stand exactly where the read-only refusal left them, for
	// the pool makes no spare while read-only and the admission refuses
	// before any limit is read.
	pool := c.pool.Counters()
	census := seamCensus(seam)
	if _, err := c.WriteAt(ctx, s, testFile(4), 0, []byte("w"), 1); !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("the write after the journal turned read-only = %v, want ErrJournalReadOnly", err)
	}
	if got := c.pool.Counters(); got != pool {
		t.Fatalf("the refused write moved the pool from %+v to %+v", pool, got)
	}
	if got := seamCensus(seam); got != census {
		t.Fatalf("the refused write made %d seam calls, want none", got-census)
	}
}

// TestCore_SecondRefusedSpareDirSyncTurnsJournalReadOnly covers the
// refused-segment rule reached from the making of spares: an error of the
// directory sync that is not EROFS names no cause through the seam wrap, so
// a journal kept making spares once per kick however the device answered.
// The second new spare in a row whose directory sync the device refused —
// the second segment the rule counts refused — now enters the read-only state
// with the refusal that met it, so the state names the device. The counting
// extends the read-only triggers the RFC lists; the decision at the counter
// in the pool states its ceiling.
func TestCore_SecondRefusedSpareDirSyncTurnsJournalReadOnly(t *testing.T) {
	c, seam := newTestCore(t)
	awaitSettledSpares(t, c, 2*testStreams)
	if err := seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 0, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// Take every spare the pool readied, so the later refused take kicks a
	// refill whose make meets the refused directory sync. Each make ends its
	// pass at the failure, so at least one kicked make landed by the time
	// the drained pool's first refused sync below is observed.
	for range 2 * testStreams {
		_, _ = c.pool.Take(segment.Content)
	}
	dirs := seam.Calls(kit.OpSyncDir)
	waitIn(t, 30e9, "the first refused spare's directory sync", func() bool {
		return seam.Calls(kit.OpSyncDir) > dirs
	})

	// The take over the emptied pool is refused and sends the kick whose
	// make is the second refused spare in a row; the journal enters
	// read-only with the device's refusal, which the state then names, and
	// the pool's own read-only check stops every later make before any
	// storage, so the seam's calls stand.
	if _, err := c.pool.Take(segment.Content); err == nil {
		t.Fatal("a take over the emptied pool succeeded, want the spare refusal that kicks the second make")
	}
	waitIn(t, 30e9, "the journal to turn read-only on the second refused spare in a row", func() bool {
		st := c.Durability().ReadOnlyState()
		return st.ReadOnly && errors.Is(st.Err, syscall.EIO) && errors.Is(st.Err, segment.ErrDirSyncRefused)
	})
	syncs := seam.Calls(kit.OpSyncDir)
	if _, err := c.pool.Take(segment.Content); err == nil {
		t.Fatal("a take after the journal turned read-only succeeded, want the spare refusal")
	}
	// WaitSpares is parked by the pass the take's kick queued and answered
	// by that pass's settle, so its return is the event that the kicked
	// read-only pass finished — the one pass that could have touched storage.
	// The bound is the test's own: a pass that never settles must fail the
	// test, not park it forever.
	settled := make(chan error, 1)
	go func() { settled <- c.pool.WaitSpares(t.Context()) }()
	select {
	case err := <-settled:
		_ = err // the answer itself is the event: the read-only pool tells a shortage
	case <-time.After(30 * time.Second):
		t.Fatal("the kicked read-only pass never settled its pool waits")
	}
	if got := seam.Calls(kit.OpSyncDir); got != syncs {
		t.Fatalf("the read-only pool made %d more spares after its kicked take, want none", got-syncs)
	}
}

// TestCore_ReadOnlyRefusesAtOnce covers the admission's own gate: a journal
// the durability layer has entered read-only refuses the next WriteAt with
// ErrJournalReadOnly, holding no reservation.
func TestCore_ReadOnlyRefusesAtOnce(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s, testFile(1), 0, []byte("w"), 1); err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}

	c.Durability().Enter(syscall.EROFS)
	_, err := c.WriteAt(ctx, s, testFile(2), 0, []byte("w"), 1)
	if !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("the write = %v, want ErrJournalReadOnly", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the read-only refusal = %v, want the EROFS that caused it wrapped", err)
	}
	if got := c.ReservedBytes(); got != 0 {
		t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
	}
}

// TestCore_ReadOnlyProbeChecksThenMakesASpare covers the probe's wiring: a
// read-only core runs Deps.Check at each probe, a failing check lands in the
// read-only state and makes no spare file, and once it passes the probe makes
// one spare segment on disk, past the pool's read-only refusal, and the state
// reports it (RFC 1 §5.4).
func TestCore_ReadOnlyProbeChecksThenMakesASpare(t *testing.T) {
	errNoFlush := errors.New("the mount does not flush its cache")
	var failing atomic.Bool
	failing.Store(true)
	clk := kit.NewStopwatchClock()
	c, seam := newTestCore(t, func(o *coreOptions) {
		o.clock = clk
		o.check = func() error {
			if failing.Load() {
				return errNoFlush
			}
			return nil
		}
	})
	segs := func() int {
		names, err := seam.List()
		if err != nil {
			t.Fatalf("list the directory: %v", err)
		}
		n := 0
		for _, name := range names {
			if strings.HasSuffix(name, ".seg") {
				n++
			}
		}
		return n
	}
	before := segs()

	c.Durability().Enter(syscall.EROFS)
	// The probe arms its timer on its own goroutine, so each poll lets one
	// more interval pass.
	probe := func(what string, cond func() bool) {
		waitIn(t, 30e9, what, func() bool {
			clk.Advance(core.ProbeInterval)
			return cond()
		})
	}
	probe("the failed check in the read-only state", func() bool {
		return errors.Is(c.Durability().ReadOnlyState().Check, errNoFlush)
	})
	if ro := c.Durability().ReadOnlyState(); ro.SpareMade {
		t.Fatalf("ReadOnlyState = %+v under a failing check, want no spare made", ro)
	}
	if got := segs(); got != before {
		t.Fatalf("%d segment files under a failing check, want the %d before it", got, before)
	}

	failing.Store(false)
	probe("the probe's spare in the read-only state", func() bool {
		return c.Durability().ReadOnlyState().SpareMade
	})
	if ro := c.Durability().ReadOnlyState(); !ro.ReadOnly || ro.Check != nil {
		t.Fatalf("ReadOnlyState = %+v after the check passed, want read-only with the check cleared", ro)
	}
	if got := segs(); got != before+1 {
		t.Fatalf("%d segment files after the probe's spare, want %d: the one spare it keeps", got, before+1)
	}
}
