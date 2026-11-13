//go:build journal_conformance

package conformance

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The allocations per operation of RFC 1 §9.7's counted row: one warm WriteAt, one ReadAt of held
// bytes and one Stats, counted per call against a pinned bound, so an
// operation that grows an allocation fails the check. None of it times
// anything: allocations are the one cost a check can count without a
// baseline.

const (
	// Exactly what one warm WriteAt spent when TestCounted_AllocationsPerCall
	// measured it here; the proof test fails anything added above this.
	maxWriteAllocs = 11.0
	// Exactly what one ReadAt of held bytes spent when
	// TestCounted_AllocationsPerCall measured it here.
	maxReadAllocs = 7.0
	// Exactly what one Stats spent when TestCounted_AllocationsPerCall
	// measured it here.
	maxStatsAllocs = 13.0
)

// allocRig is the fixture the allocation checks measure against, with the
// three warm operations: two files, one written through each share, the
// write overwriting the first share's and the read returning the second's
// held bytes into a buffer every call reuses.
type allocRig struct {
	f                  *journaltest.Fixture
	write, read, stats func()
}

// newAllocRig writes the two files and returns the rig with its three
// measured calls, the write and read each checked once before they are
// measured, so a call that errored never measures as cheap.
func newAllocRig(t *testing.T) *allocRig {
	t.Helper()

	f := journaltest.New(t)
	ctx := t.Context()
	a, b := f.Share(0), f.Share(1)
	ida, idb := journaltest.File(0), journaltest.File(1)
	wbuf := pattern("overwrite", countedWrite)
	rbuf := make([]byte, countedWrite)

	write(t, a, ida, 0, pattern("held-share0", countedWrite))
	write(t, b, idb, 0, pattern("held-share1", countedWrite))

	if _, err := a.WriteAt(ctx, ida, 0, wbuf, 0); err != nil {
		stub(t, "WriteAt", err)
		t.Fatalf("one warm overwrite: %v", err)
	}
	if _, _, _, _, err := b.ReadAt(ctx, idb, 0, rbuf); err != nil {
		stub(t, "ReadAt", err)
		t.Fatalf("one read of held bytes: %v", err)
	}

	return &allocRig{
		f:     f,
		write: func() { _, _ = a.WriteAt(ctx, ida, 0, wbuf, 0) },
		read:  func() { _, _, _, _, _ = b.ReadAt(ctx, idb, 0, rbuf) },
		stats: func() { f.J.Stats() },
	}
}

// allocsPerCall waits for the journal's background work, such as making
// spares, to go quiet, then returns the least of five testing.AllocsPerRun
// samples of one call, labelled op.
//
// decision: the least of several samples hides an allocation a pool sometimes
// reuses; the bounds are pinned exactly and the proof test pins that they
// hold no such slack — one allocation added to a measured op fails the
// bound, however the samples fall.
func allocsPerCall(t *testing.T, r *allocRig, op string, call func()) float64 {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for prev := -1; ; time.Sleep(10 * time.Millisecond) {
		if n := r.f.Seam.Calls(kit.OpWrite); n == prev || time.Now().After(deadline) {
			break
		} else {
			prev = n
		}
	}
	least := testing.AllocsPerRun(10, call)
	for range 4 {
		least = min(least, testing.AllocsPerRun(10, call))
	}
	return least
}

// allocLeak receives the proof's injected allocation, so no compiler can
// remove the make it is made of.
var allocLeak []byte

// TestCounted_AllocationsPerCall checks that the three measured operations
// spend no more allocations per call than the bounds pin.
func TestCounted_AllocationsPerCall(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	r := newAllocRig(t)

	w := allocsPerCall(t, r, "WriteAt", r.write)
	rd := allocsPerCall(t, r, "ReadAt", r.read)
	s := allocsPerCall(t, r, "Stats", r.stats)
	t.Logf("allocations per call: WriteAt %.0f, ReadAt %.0f, Stats %.0f", w, rd, s)

	if w > maxWriteAllocs {
		t.Fatalf("WriteAt spends %.0f allocations per call, above the pinned %.0f", w, maxWriteAllocs)
	}
	if rd > maxReadAllocs {
		t.Fatalf("ReadAt spends %.0f allocations per call, above the pinned %.0f", rd, maxReadAllocs)
	}
	if s > maxStatsAllocs {
		t.Fatalf("Stats spends %.0f allocations per call, above the pinned %.0f", s, maxStatsAllocs)
	}
}

// TestCounted_AllocationsPerCallFailsWithOneAdded proves the bounds are
// exact: each op is calibrated at or below its bound, and the same op with
// one deliberate allocation added inside the measured closure must trip it.
// Any allocation added to the real path fails the check the same way.
func TestCounted_AllocationsPerCallFailsWithOneAdded(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	r := newAllocRig(t)

	for _, tc := range []struct {
		op    string
		bound float64
		call  func()
	}{
		{"WriteAt", maxWriteAllocs, r.write},
		{"ReadAt", maxReadAllocs, r.read},
		{"Stats", maxStatsAllocs, r.stats},
	} {
		if got := allocsPerCall(t, r, tc.op, tc.call); got > tc.bound {
			t.Fatalf("%s spends %.0f allocations per call, already above its pinned %.0f: the calibration fails", tc.op, got, tc.bound)
		}
		if got := allocsPerCall(t, r, tc.op+" with one allocation added", func() {
			tc.call()
			allocLeak = make([]byte, 8)
		}); got <= tc.bound {
			t.Fatalf("%s with one deliberate allocation added spends %.0f per call, at or below its pinned %.0f: the bound carries slack", tc.op, got, tc.bound)
		}
	}
}
