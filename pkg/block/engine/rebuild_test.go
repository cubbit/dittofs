package engine_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// runLen is the length of each of the source's two journal-held runs in the
// rebuild tests; a hole of the same length lies between them.
const runLen = 64 << 10

// markerCrash is a rig whose journal a crash left with a clone's marker
// above the destination's applied version: the source holding two runs and a
// hole between them, and the destination holding the first copies of
// them, as many as the crash let the clone make.
type markerCrash struct {
	*rig
	holds    *recHolds
	src, dst entity.FileID
	want     []byte // the source extent at the clone's asOf
}

func newMarkerCrash(t *testing.T, copies int) *markerCrash {
	t.Helper()
	holds := &recHolds{}
	r := newRig(t, withStartDeps(func(d *engine.Deps) { d.Holds = holds }))
	_, src := r.newFile(t, "src")
	_, dst := r.newFile(t, "dst")
	ctx := t.Context()

	m := &markerCrash{rig: r, holds: holds, src: src, dst: dst,
		want: slices.Concat(fill(runLen, 's'), make([]byte, runLen), fill(runLen, 't'))}
	var asOf journal.Version
	for _, off := range []int64{0, 2 * runLen} {
		v, err := r.jh.WriteAt(ctx, journal.NewFileID(src), off, m.want[off:off+runLen], writeMtime.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		asOf = v
	}
	spec := journal.CloneSpec{SrcShare: journal.ShareID(r.one.ID), Src: journal.NewFileID(src), Len: 3 * runLen, AsOf: asOf}
	if _, err := r.jh.CloneTarget(ctx, journal.NewFileID(dst), spec); err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 2 * runLen}[:copies] {
		if _, err := r.jh.WriteAt(ctx, journal.NewFileID(dst), off, m.want[off:off+runLen], writeMtime.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// writeErr is a one-byte write at off of f in share s.
func (r *rig) writeErr(t *testing.T, s entity.ShareID, f entity.FileID, off int64) error {
	t.Helper()
	_, err := r.e.Write(t.Context(), s, f, off, []byte("x"), engine.WriteOpts{Done: func() {}})
	return err
}

// until advances the rig's clock past the loop's backoff until cond holds.
func (r *rig) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		r.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// cloneDone waits until neither file holds a registration of the clone.
func (r *rig) cloneDone(t *testing.T, src, dst entity.FileID) {
	t.Helper()
	r.until(t, "the rebuilt clone done", func() bool {
		s, _ := r.e.Registrations(src)
		_, d := r.e.Registrations(dst)
		return s == 0 && d == 0
	})
}

// readIn reads n bytes at off of f in share s.
func (r *rig) readIn(t *testing.T, s entity.ShareID, f entity.FileID, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := r.e.Read(t.Context(), s, f, off, p); err != nil {
		t.Fatalf("read [%d, %d): %v", off, off+int64(n), err)
	}
	return p
}

// A clone a crash cut short before its phase 1 is rebuilt from its journal
// marker: both extents are registered before Start serves a file, so a write
// into the source extent and a read of the destination's answer ErrDelay;
// the copies the journal holds are kept, only the others made again through
// a reservation of their size, each counted toward the version limit as a
// live copy is; and the clone finishes with the destination reading the
// source at its asOf, never zeros where it held bytes, as a marker applied
// as a plain removal would leave it.
func TestACloneIsRebuiltFromItsMarkerAtEachCrashPoint(t *testing.T) {
	for _, c := range []struct {
		name   string
		copies int
	}{
		{"after the journal step", 0},
		{"after one copy", 1},
		{"after the copies", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMarkerCrash(t, c.copies)
			var mu sync.Mutex
			var issued []journal.Version
			m.e.OnIssued(func(v journal.Version) { mu.Lock(); issued = append(issued, v); mu.Unlock() })
			m.begin(t)

			if err := m.writeErr(t, m.one.ID, m.src, runLen/2); !isDelay(err, vfserr.DelayClone) {
				t.Errorf("a write into the source extent before the clone is done: %v, want DelayClone", err)
			}
			if _, err := m.e.Read(t.Context(), m.one.ID, m.dst, 0, make([]byte, 8)); !isDelay(err, vfserr.DelayClone) {
				t.Errorf("a read of the destination extent before the clone is done: %v, want DelayClone", err)
			}
			m.run()
			m.cloneDone(t, m.src, m.dst)

			if got := m.readIn(t, m.one.ID, m.dst, 0, 3*runLen); !bytes.Equal(got, m.want) {
				t.Error("the rebuilt clone's destination does not read the source at its asOf")
			}
			remade := 2 - c.copies
			res := m.j.Reserves()
			switch {
			case remade == 0 && len(res) != 0:
				t.Errorf("reserved %+v with every copy kept, want nothing", res)
			case remade > 0 && (len(res) != 1 || res[0].N != int64(remade)*runLen || res[0].Writes != remade):
				t.Errorf("reserved %+v, want one of %d bytes and %d writes", res, remade*runLen, remade)
			}
			copies := m.j.ReservedWrites()
			if len(copies) != remade {
				t.Fatalf("%d copies made again, want %d", len(copies), remade)
			}
			mu.Lock()
			counted := slices.Clone(issued)
			mu.Unlock()
			if !slices.Equal(counted, copies) {
				t.Errorf("versions counted toward the version limit %v, want the copies made again %v", counted, copies)
			}
			if err := m.writeErr(t, m.one.ID, m.src, runLen/2); err != nil {
				t.Errorf("a write into the source extent once the clone is done: %v", err)
			}
		})
	}
}

// A rebuilt clone whose copies the destination's journal has no space for
// waits, uncounted, the calls behind it answering DelayCapacity, and does not
// hold up Start or any other file; it finishes once the journal reserves.
func TestARebuiltCloneWaitsForSpace(t *testing.T) {
	m := newMarkerCrash(t, 0)
	m.j.RefuseReserve(&journal.NoSpaceError{Limit: journal.LimitShare, Missing: runLen, Unit: journal.UnitBytes})
	m.begin(t)
	m.run()
	_, other := m.newFile(t, "other")
	if err := m.writeErr(t, m.one.ID, other, 0); err != nil {
		t.Errorf("a write to another file while the clone waits for space: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for err := m.writeErr(t, m.one.ID, m.dst, 0); !isDelay(err, vfserr.DelayCapacity); err = m.writeErr(t, m.one.ID, m.dst, 0) {
		if time.Now().After(deadline) {
			t.Fatalf("a write into the destination extent of a clone waiting for space: %v, want DelayCapacity", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.writeErr(t, m.one.ID, m.src, 0); !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a write into the source extent of a clone waiting for space: %v, want DelayCapacity", err)
	}
	m.cloneDone(t, m.src, m.dst)
	if got := m.readIn(t, m.one.ID, m.dst, 0, 3*runLen); !bytes.Equal(got, m.want) {
		t.Error("the clone that waited for space does not read the source")
	}
}

// A clone of a destination already holding every copy, on a share whose
// journal has no space left, asks no reservation and finishes.
func TestARebuiltCloneHoldingEveryCopyReservesNothing(t *testing.T) {
	m := newMarkerCrash(t, 2)
	m.j.RefuseReserve(&journal.NoSpaceError{Limit: journal.LimitShare, Missing: 1, Unit: journal.UnitBytes})
	m.begin(t)
	m.run()
	m.cloneDone(t, m.src, m.dst)
	if r := m.j.Reserves(); len(r) != 0 {
		t.Errorf("reserved %+v at a full limit, every copy held, want nothing", r)
	}
	if got := m.readIn(t, m.one.ID, m.dst, 0, 3*runLen); !bytes.Equal(got, m.want) {
		t.Error("the destination does not read the source")
	}
}

// removalCrash is a rig whose clone's phase 1 committed before a crash cut
// its phase 2 short, restarted with the engine of the next process built and
// not started.
//
// Both files are in one share: the store gives every share a namespace of
// its own, so its phase 1 refuses a clone between two of the rig's shares.
type removalCrash struct {
	*rig
	holds    *servedGate
	src, dst entity.FileID
	want     []byte
}

// servedGate is recHolds whose Served returns only once open is closed.
type servedGate struct {
	*recHolds
	open chan struct{}
}

func (h *servedGate) Served(ctx context.Context, s entity.ShardID) error {
	select {
	case <-h.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newRemovalCrash(t *testing.T, served bool) *removalCrash {
	t.Helper()
	holds := &servedGate{recHolds: &recHolds{}, open: make(chan struct{})}
	if served {
		close(holds.open)
	}
	r := newRig(t, withStartDeps(func(d *engine.Deps) { d.Holds = holds }))
	r.begin(t)
	r.run()
	r.e.FakeCloneDone()
	m := &removalCrash{rig: r, holds: holds, src: r.newFile2(t, "src"), dst: r.newFile2(t, "dst"),
		want: fill(runLen, 's')}
	ctx := t.Context()
	if _, err := r.e.Write(ctx, r.one.ID, m.src, 0, m.want, engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	r.content.freeze(true) // phase 2 never runs: the crash comes between the phases
	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- r.e.Clone(cctx, r.one.ID, m.src, r.one.ID, m.dst, vfserr.Extent{Len: runLen}) }()
	// The hold on the source is taken once phase 1 committed, before phase 2.
	r.until(t, "the clone's phase 1 committed", func() bool {
		select {
		case err := <-errc:
			t.Fatalf("the clone answered %v before its phase 2", err)
		default:
		}
		n, _ := holds.counts()
		return n > 0
	})
	cancel()
	if err := <-errc; !isDelay(err, vfserr.DelayClone) {
		t.Fatalf("the clone cut short between its phases answered %v, want DelayClone", err)
	}
	r.restart(t)
	r.content.freeze(false)
	return m
}

// newFile2 is newFile, answering the file's ID alone.
func (r *rig) newFile2(t *testing.T, name string) entity.FileID {
	t.Helper()
	_, f := r.newFile(t, name)
	return f
}

// A clone a crash cut short between its phases is rebuilt from its removal record: a write into the source
// extent answers ErrDelay as the first call after Start, Start takes no hold
// on the source, and once open state serves the source's shard the clone's
// phase 2 runs, taking the hold, and the destination reads the source.
func TestACloneIsRebuiltFromItsRemoval(t *testing.T) {
	m := newRemovalCrash(t, true)
	holds, _ := m.holds.counts()
	m.begin(t)
	if err := m.writeErr(t, m.one.ID, m.src, 0); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a write into the source extent as the first call after Start: %v, want DelayClone", err)
	}
	if n, _ := m.holds.counts(); n != holds {
		t.Errorf("Start took %d holds on the source, want none", n-holds)
	}
	m.run()
	m.cloneDone(t, m.src, m.dst)
	if n, _ := m.holds.counts(); n != holds+1 {
		t.Errorf("the rebuilt clone took %d holds on the source, want one", n-holds)
	}
	if got := m.readIn(t, m.one.ID, m.dst, 0, runLen); !bytes.Equal(got, m.want) {
		t.Error("the rebuilt clone's destination does not read the source")
	}
	if err := m.writeErr(t, m.one.ID, m.src, 0); err != nil {
		t.Errorf("a write into the source extent once the clone is done: %v", err)
	}
}

// A clone rebuilt from its removal runs no batch until open state serves its
// source's shard: it is removal_stalled after five backoffs of waiting,
// named by its destination, and cleared once the shard is served, after
// which it finishes.
func TestARebuiltCloneWaitsForItsSourcesShard(t *testing.T) {
	m := newRemovalCrash(t, false)
	resumed := m.content.resumed(m.dst)
	m.begin(t)
	m.run()
	m.until(t, "removal_stalled for the destination", func() bool { return m.e.RemovalStalled(m.dst) })
	if n := m.content.resumed(m.dst); n != resumed {
		t.Errorf("the clone's phase 2 ran %d times before its source's shard was served", n-resumed)
	}
	if !logged(m.logs, "removal_stalled", m.dst) {
		t.Error("no removal_stalled line names the destination")
	}
	if err := m.writeErr(t, m.one.ID, m.src, 0); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a write into the source extent while the clone waits: %v, want DelayClone", err)
	}
	close(m.holds.open)
	m.cloneDone(t, m.src, m.dst)
	if m.e.RemovalStalled(m.dst) || !logged(m.logs, "removal_stalled cleared", m.dst) {
		t.Error("removal_stalled not cleared once the source's shard was served")
	}
	if got := m.readIn(t, m.one.ID, m.dst, 0, runLen); !bytes.Equal(got, m.want) {
		t.Error("the rebuilt clone's destination does not read the source")
	}
}

// A Start that fails after it rebuilt a clone takes back what the rebuild put
// on the files, the destination's guard and both registrations, so Start runs
// again as on a new engine: it rebuilds the clone once more, without waiting
// on a guard the failed Start left, and the clone finishes with one
// registration on each file, not a stale pair that would answer DelayClone
// for good.
func TestAStartThatFailsAfterARebuildRunsAgain(t *testing.T) {
	m := newMarkerCrash(t, 0)
	m.content.listErr = errors.New("the listing failed")
	m.atRestart()
	if err := m.start(t); err == nil {
		t.Fatal("Start with a failing listing succeeded")
	}
	if s, _ := m.e.Registrations(m.src); s != 0 {
		t.Errorf("after the failed Start the source holds %d registrations, want 0", s)
	}
	if _, d := m.e.Registrations(m.dst); d != 0 {
		t.Errorf("after the failed Start the destination holds %d registrations, want 0", d)
	}

	m.content.listErr = nil
	m.j.Reopen()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.e.Start(ctx, append([]engine.Share{m.one, m.two}, m.more...)); err != nil {
		t.Fatalf("Start again after a failed one: %v", err)
	}
	s, _ := m.e.Registrations(m.src)
	_, d := m.e.Registrations(m.dst)
	if s != 1 || d != 1 {
		t.Errorf("the rebuilt clone holds %d source and %d destination registrations, want 1 and 1", s, d)
	}
	m.run()
	m.cloneDone(t, m.src, m.dst)
	if got := m.readIn(t, m.one.ID, m.dst, 0, 3*runLen); !bytes.Equal(got, m.want) {
		t.Error("the clone rebuilt by the second Start does not read the source at its asOf")
	}
}
