package recovery

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// noPiece names no piece: the planner sees no record on disk naming a drop.
func noPiece(core.Seq, core.Extent) bool { return false }

// failedFixture plants one sealed segment 5 holding a survivor and two
// records the directory's failed list names, and answers the deps that carry
// the list.
type failedFixture struct {
	r        *rig
	d        Deps
	file     core.FileID
	survSeq  core.Seq
	survOff  int64
	failed   []core.Seq
	failOffs []int64
}

func newFailedFixture(t *testing.T) *failedFixture {
	t.Helper()
	r := newRig(t)
	r.writeFormat()
	f := &failedFixture{r: r, file: tailFile(), survSeq: 10, survOff: 0x4100,
		failed: []core.Seq{20, 21}, failOffs: []int64{0x100, 0x200}}
	b, _ := buildSealed(5,
		collectWrite(7, f.file, 10, 10, f.survOff, 32),
		collectWrite(7, f.file, 20, 20, f.failOffs[0], 32),
		collectWrite(7, f.file, 21, 21, f.failOffs[1], 32))
	r.plantSeg(5, b)
	f.d = r.runDeps()
	f.d.Failed = failedWords(f.failed)
	return f
}

// open runs the recovery over the directory as it stands and plans the open's
// losses over a reserve of none, so every failed record is past it, exactly
// as the loss pass answers.
func (f *failedFixture) open(t *testing.T) (*State, *emptyingHarness) {
	t.Helper()
	f.d.FS, f.d.Dir = f.r.seam.FS(), f.r.dir
	st, err := Run(context.Background(), f.d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, past := planOpenLosses(st, noPiece, noPiece, 0)
	// The failed records are found while the source stands; one a durable
	// unlink already took leaves nothing for a reopen to find.
	want := 0
	if slices.Contains(f.r.listing(), format.SegmentName(5)) {
		want = len(f.failed)
	}
	if len(past) != want {
		t.Fatalf("the planner left %d failed records past a reserve of none, want %d", len(past), want)
	}
	st.Kinds.PastReserve = past
	return &st, newEmptyingHarness(t, f.r, &st, segment.Targets{Reserve: 2})
}

// settled asserts what every open over the fixture must leave: the survivor
// verifies in exactly one durable placement, no failed record is durable
// anywhere, the source is gone, and the index serves the survivor and none
// of the failed extents.
func (f *failedFixture) settled(t *testing.T, st *State) {
	t.Helper()
	copies := 0
	for _, name := range f.r.listing() {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		bs, err := f.r.seam.Durable(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
		if werr != nil {
			continue // a crashed target the next open re-reads
		}
		for _, rec := range walk.Records {
			switch {
			case rec.Header.Seq == f.survSeq:
				copies++
			case slices.Contains(f.failed, rec.Header.Seq):
				t.Errorf("the failed record of seq %d is durable in %s, want it left out of the copy and its source unlinked", rec.Header.Seq, name)
			}
		}
	}
	if copies != 1 {
		t.Errorf("the survivor verifies %d times on disk, want the one copy", copies)
	}
	if slices.Contains(f.r.listing(), format.SegmentName(5)) {
		t.Errorf("the source segment is still on disk, want it unlinked")
	}
	placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff)
	if !ok || placed.Seq != f.survSeq || placed.Record.Segment == 5 {
		t.Errorf("the survivor serves %+v (%v), want seq %d from the copy", placed, ok, f.survSeq)
	}
	for _, off := range f.failOffs {
		if p, ok := st.Rebuilt.Index.Lookup(f.file, off); ok {
			t.Errorf("the failed extent at %d serves seq %d, want nothing: a failed record is never served", off, p.Seq)
		}
	}
}

// TestSealedHolderOfAFailedRecordIsEmptied holds the emptying at its own
// caller: a sealed segment holding failed records past the reserve is copied
// out without them and unlinked, the survivor repointed to the copy.
func TestSealedHolderOfAFailedRecordIsEmptied(t *testing.T) {
	f := newFailedFixture(t)
	st, h := f.open(t)
	if err := Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	f.settled(t, st)
}

// TestCrashAtEveryFailedHolderEmptyingStep crashes the emptying of a sealed
// holder at each write step with writes reordered, and holds that the unlink
// of the source waits for the copy: at the unlink the copy is already
// durable with the survivor, so a power loss in between leaves the survivor
// somewhere. Each crash is followed by a reopen, and by a second power loss
// and reopen, each settling to the same state.
func TestCrashAtEveryFailedHolderEmptyingStep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		op          string
		nth         int
		afterRemove bool
	}{
		{name: "target_append", op: "write", nth: 1},
		{name: "target_seal_first_sync", op: "sync", nth: 1},
		{name: "target_seal_second_sync", op: "sync", nth: 2},
		{name: "source_unlink", op: "remove", nth: 1},
		{name: "directory_sync_after_unlink", op: "remove", nth: 1, afterRemove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailedFixture(t)
			st, h := f.open(t)
			if err := h.pool.WaitSpares(context.Background()); err != nil {
				t.Fatalf("the pool's prewarm: %v", err)
			}
			spare := format.SegmentName(st.Attached.MaxID + 1)
			key := spare
			if tc.op == "remove" {
				key = format.SegmentName(5)
			}
			done := make(chan error, 1)
			reach := func(hold *kit.Hold, what string) {
				t.Helper()
				select {
				case <-hold.Reached():
				case err := <-done:
					t.Fatalf("the emptying returned (%v) before the held step %s", err, what)
				}
			}
			// The directory syncs are held from before the emptying starts, with
			// the prewarmed pool idle: a hold made once the unlink parked would
			// miss a refill sync already past the seam's hold check, which then
			// commits the removal before the cut.
			var dirSyncs *kit.Hold
			if tc.afterRemove {
				dirSyncs = f.r.seam.Hold(t, kit.OpSyncDir, "*")
			}
			hold := f.r.seam.HoldFirst(t, kitOp(tc.op), key)
			go func() { done <- Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil) }()
			reach(hold, tc.op+" on "+key)
			switch {
			case tc.nth == 2:
				next := f.r.seam.HoldFirst(t, kitOp(tc.op), key)
				hold.Release()
				reach(next, "the second "+tc.op+" on "+key)
				hold = next
			case tc.afterRemove:
				hold.Release()
				reach(dirSyncs, "the directory sync after the unlink")
				hold = dirSyncs
				// Another caller's directory sync may reach the hold before the
				// released unlink applies, so the cut waits on the unlink itself;
				// every directory sync stays parked meanwhile.
				deadline := time.Now().Add(30 * time.Second)
				for slices.Contains(f.r.listing(), format.SegmentName(5)) {
					if time.Now().After(deadline) {
						t.Fatalf("the source is still listed with the directory syncs held, want the unlink applied before the cut")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if tc.op == "remove" {
				// The source's unlink is reached: the copy must already be
				// durable with the survivor, or the power loss below takes
				// the only copy of it.
				bs, err := f.r.seam.Durable(spare)
				if err != nil {
					t.Fatalf("the copy %s is not durable at the source's unlink: %v", spare, err)
				}
				walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, st.Attached.MaxID+1)
				if werr != nil || !slices.ContainsFunc(walk.Records, func(r format.ScannedRecord) bool { return r.Header.Seq == f.survSeq }) {
					t.Fatalf("at the source's unlink the copy holds no durable survivor (scan err %v): the unlink ran ahead of the copy", werr)
				}
			}
			if err := f.r.seam.Crash(); err != nil {
				t.Fatalf("the power cut at the held step: %v", err)
			}
			if tc.afterRemove && !slices.Contains(f.r.listing(), format.SegmentName(5)) {
				t.Errorf("the source segment is gone after a power cut before the directory sync, want the unsynced unlink resurrected")
			}
			hold.Release()
			select {
			case err := <-done:
				_ = err // the crashed run's error is the crash's own
			case <-time.After(30 * time.Second):
				t.Fatalf("the crashed emptying never returned: a hold at the %s step wedged the phase", tc.name)
			}

			for pass := 0; pass < 2; pass++ {
				st2, h2 := f.open(t)
				if err := Emptying(context.Background(), h2.deps, st2, journalID, core.Version{}, nil); err != nil {
					t.Fatalf("the reopen's Emptying (pass %d): %v", pass, err)
				}
				f.settled(t, st2)
				if err := f.r.seam.Crash(); err != nil {
					t.Fatalf("the power loss after reopen %d: %v", pass, err)
				}
			}
		})
	}
}

// TestAnAsideDuplicateInASealedHolderThisRunEmptiesStillSurvives holds the
// standing-copy tests against a sealed row the same run unlinks: an ended
// segment holds a live loss record whose duplicate sits in a sealed holder of
// a failed record, which the emptying also empties. Each copy is registered
// at its own segment, so whichever segment is emptied last finds no standing
// duplicate left and carries the loss, while one emptied after a copy this
// run already carried skips it; the row holds exactly one durable copy for
// both id orders, failing if both skip on each other or both carry.
func TestAnAsideDuplicateInASealedHolderThisRunEmptiesStillSurvives(t *testing.T) {
	for _, tc := range []struct {
		name          string
		holder, ended core.SegmentID
	}{
		{name: "holder_before_ended", holder: 3, ended: 5},
		{name: "ended_before_holder", holder: 5, ended: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const tag = core.ShareTag(7)
			file := tailFile()
			r := newRig(t)
			r.writeFormat()
			loss := collectLoss(tag, file, 40, 41, 941, 0x200, 64, 1<<8)
			// The record the loss names, in a sealed segment, keeps it live.
			older, _ := buildSealed(6, collectWrite(tag, file, 40, 40, 0x200, 64))
			r.plantSeg(6, older)
			// The sealed holder: the loss's duplicate and a failed write.
			holder, _ := buildSealed(tc.holder, loss, collectWrite(tag, file, 20, 20, 0x100, 32))
			r.plantSeg(tc.holder, holder)
			r.plantSeg(tc.ended, damageHeader(buildSeg(tc.ended, loss)))

			d := r.runDeps()
			d.Failed = failedWords([]core.Seq{20})
			st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			_, past := planOpenLosses(st, noPiece, noPiece, 0)
			if len(past) != 1 {
				t.Fatalf("the planner left %d failed records past the reserve, want 1: the fixture no longer plants the case", len(past))
			}
			st.Kinds.PastReserve = past
			if got, err := st.Assembled.Set.State(tc.holder); err != nil || got != segment.Sealed {
				t.Fatalf("the holder's row got %v (%v), want Sealed", got, err)
			}
			h := newEmptyingHarness(t, r, &st, segment.Targets{Reserve: 2})
			if err := Emptying(context.Background(), h.deps, &st, journalID, core.Version{}, nil); err != nil {
				t.Fatalf("Emptying: %v", err)
			}

			survivors := 0
			for _, name := range r.listing() {
				kind, id := format.ClassifyName(name)
				if kind != format.NameSegment {
					continue
				}
				if id == tc.holder || id == tc.ended {
					t.Errorf("segment %d is still on disk, want it emptied", id)
				}
				bs, err := r.seam.Durable(name)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				walk, werr := format.ScanSegmentHeaders(bytes.NewReader(bs), int64(len(bs)), journalID, id)
				if werr != nil {
					continue
				}
				for _, rec := range walk.Records {
					if rec.Header.Kind == format.KindLoss && rec.Header.Seq == 41 {
						survivors++
					}
				}
			}
			if survivors != 1 {
				t.Errorf("%d durable copies of the loss record of seq 41 remain, want exactly one: none lost with its segment, none carried twice", survivors)
			}
		})
	}
}

// TestAHolderNamedRecordWithASealedDuplicateRepointsWithoutACarry holds the
// standing copy for a record the index serves from a sealed holder this run
// empties: a sealed segment below it holds a verifying duplicate and a
// record of its own, so the holder's survivor is not copied, its served
// piece moves to the duplicate before the holder is unlinked, and the
// holder carries nothing at all. It fails if the duplicate is not found,
// for the survivor is then carried into a spare, and if the repoint of a
// run that carries nothing is dropped, for the extent then serves from the
// unlinked holder.
func TestAHolderNamedRecordWithASealedDuplicateRepointsWithoutACarry(t *testing.T) {
	f := newFailedFixture(t)
	dup, _ := buildSealed(3,
		collectWrite(7, f.file, uint64(f.survSeq), uint64(f.survSeq), f.survOff, 32),
		collectWrite(7, f.file, 12, 12, 0x8100, 32))
	f.r.plantSeg(3, dup)
	st, h := f.open(t)
	if placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff); !ok || placed.Record.Segment != 5 {
		t.Fatalf("the survivor serves %+v (%v) at the open, want the holder 5's copy, the greater one", placed, ok)
	}
	if err := Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	f.settled(t, st)
	if placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff); !ok || placed.Record.Segment != 3 {
		t.Errorf("the survivor serves %+v (%v), want the standing duplicate in 3, nothing carried", placed, ok)
	}
}

// TestARottenHolderCopyIsNotLostWhileAnEndedCopyVerifies holds the read-found
// loss leg to its rule, a loss record only while no other copy verifies: the
// sealed holder 5 serves the survivor over an equal copy in the ended
// segment 6, both are held, so the holder's leg runs first, and its copy
// rots before the emptying reads it. The ended copy verifies, so it is
// carried and no loss is written, rather than the ended segment's good copy
// going with its unlink.
func TestARottenHolderCopyIsNotLostWhileAnEndedCopyVerifies(t *testing.T) {
	f := newFailedFixture(t)
	f.r.plantSeg(6, damageHeader(buildSeg(6,
		collectWrite(7, f.file, uint64(f.survSeq), uint64(f.survSeq), f.survOff, 32),
		collectWrite(7, f.file, 13, 13, 0x9100, 32))))
	st, h := f.open(t)
	if got, err := st.Assembled.Set.State(6); err != nil || got != segment.Ended {
		t.Fatalf("segment 6's row got %v (%v), want Ended by its damaged header", got, err)
	}
	if placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff); !ok || placed.Record.Segment != 5 {
		t.Fatalf("the survivor serves %+v (%v) at the open, want the holder 5's copy over the ended one", placed, ok)
	}
	if !h.deps.Liveness.Held(5) || !h.deps.Liveness.Held(6) {
		t.Fatalf("held 5 %v, held 6 %v, want both held so the holder's leg runs first", h.deps.Liveness.Held(5), h.deps.Liveness.Held(6))
	}
	rotByte(t, f.r, format.SegmentName(5), format.SegmentHeaderSize+format.RecordHeaderSize)
	if err := Emptying(context.Background(), h.deps, st, journalID, core.Version{}, nil); err != nil {
		t.Fatalf("Emptying: %v", err)
	}
	if n := verifyingCopies(t, f.r, uint64(f.survSeq)); n != 1 {
		t.Errorf("the survivor of seq %d verifies in %d placements, want 1: the ended segment's good copy carried", f.survSeq, n)
	}
	if placed, ok := st.Rebuilt.Index.Lookup(f.file, f.survOff); !ok || placed.Seq != f.survSeq {
		t.Errorf("the survivor's extent serves %+v (%v), want seq %d, no loss", placed, ok, f.survSeq)
	}
}
