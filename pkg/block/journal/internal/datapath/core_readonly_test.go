package datapath

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
