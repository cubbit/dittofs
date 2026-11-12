package reclaim_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// carryPayload is the one write the layout plants in the source segment.
const carryPayload = "the write the delete counts on"

// carryLayout is the shape the durable-unlink gate guards, planted over one
// copy fixture: a sealed source segment holding the file's only write, and a
// settled delete of the file in a later segment — a removal record whose only
// reason to live is the source segment's write, which it may still outrank.
// The retirer is the real segment.Retirer over the fixture's ledger and seam,
// wired the way the journal's executor is: the retired-segment hook is the
// tracker's SegmentRetired, the durable signal its RemovalDurable — the
// call that runs once the unlink's directory sync returned nil.
type carryLayout struct {
	f    *copyFixture
	file core.FileID
	// sa is the sealed source segment, the one the tests retire.
	sa core.SegmentID
	// del is the delete record's Rec, registered live with the tracker.
	del reclaim.Rec
	tr  *reclaim.Tracker
	r   *segment.Retirer
}

// plantCarryLayout plants the layout and wires the retirer over it. The
// write registers with the tracker beside the delete, so the retirement's
// empty file list still names the file from the tracker's own registry.
func plantCarryLayout(t *testing.T) carryLayout {
	t.Helper()
	f := newCopyFixture(t)
	c := carryLayout{f: f, file: copyFile(9), tr: f.tracker}

	recW, seqW, vW := f.plant(t, copyTag, c.file, []byte(carryPayload), 0)
	c.sa = recW.Segment
	f.sealSource(t, c.file)
	f.waitSpares(t)

	recD, seqD, vD := f.plantDelete(t, copyTag, c.file)
	if recD.Segment == c.sa {
		t.Fatalf("the delete landed in the sealed source segment %d, want a segment of its own", c.sa)
	}
	// The delete's plant drew a spare, and the pool's refill makes a spare
	// behind it — a making that syncs the directory on its own — so the
	// pool settles here, before any test holds or faults a sync_dir.
	f.waitSpares(t)
	// The write's own registration keeps the source segment's file list, so
	// the nil file list the retirement hands the tracker still names the
	// file: without it, no per-file relay could ever reach the write's
	// drop ranges.
	c.tr.Register(reclaim.Rec{
		Kind: format.KindWrite, File: c.file, Seg: c.sa,
		Ext:  core.Extent{Len: int64(len(carryPayload))},
		Pair: index.Numbers{Version: vW, Seq: seqW}, Seq: seqW, Tag: copyTag,
	})
	c.del = reclaim.Rec{
		Kind: format.KindDelete, File: c.file, Seg: recD.Segment,
		Pair: index.Numbers{Version: vD, Seq: seqD}, Seq: seqD, Tag: copyTag,
	}
	c.tr.Register(c.del)
	// The delete's marker settles above its own version, exactly the way
	// metadata's settle does; the re-test the settle carries keeps the
	// record live, outranked by the source segment's write.
	c.tr.MarkerSettled(c.file, vD)
	if c.tr.Droppable(c.del) {
		t.Fatalf("Droppable of the settled delete beside the write it counts on = true, want false: the source segment's record outranks it")
	}

	m := segment.NewMaker(f.seam.FS(), f.set, math.MaxInt64, 0, nil)
	cache := segment.NewDescriptors(f.seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	c.r = segment.NewRetirer(segment.RetirerConfig{
		Set:   f.set,
		Cache: cache,
		Maker: m,
		FS:    f.seam.FS(),
		Retired: func(id core.SegmentID) {
			c.tr.SegmentRetired(id, nil)
		},
		Durable: func(ret segment.Retirement) {
			c.tr.RemovalDurable(ret.ID)
		},
	})
	return c
}

// plantDelete appends one delete record of file through the stream and
// publishes it through the index the way the writer's own removal path does:
// the file's append domain held, a version and a sequence number drawn, the
// header-only record appended, the removal applied with its marker left over
// the whole file, the held delta accounted.
func (f *copyFixture) plantDelete(t *testing.T, tag core.ShareTag, file core.FileID) (index.RecordAt, core.Seq, core.Version) {
	t.Helper()
	unlock := f.core.Domains().Lock(file)
	defer unlock()
	v, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	pl, err := f.core.Streams().For(file).Append(format.RecordHeader{
		Kind: format.KindDelete, File: file, Extent: core.Extent{}, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{})
	if err != nil {
		t.Fatalf("Append of the delete: %v", err)
	}
	at := index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}
	hd, err := f.ix.ApplyRemoval(tag, file, core.ChangeDelete, core.Extent{Off: 0, Len: math.MaxInt64}, v, seq, at, nil)
	if err != nil {
		t.Fatalf("ApplyRemoval of the delete: %v", err)
	}
	f.core.Account(hd)
	return at, seq, v
}

// retireAsync runs one retirement of c.sa in a goroutine, handing its
// result through done and its refusal through failed: the retirement blocks
// at its directory sync — the call the caller holds — so the caller reads
// the tracker's answers under the hold and joins on the channels after.
func (c carryLayout) retireAsync() (chan segment.Retirement, chan error) {
	done := make(chan segment.Retirement, 1)
	failed := make(chan error, 1)
	go func() {
		ret, err := c.r.Retire(c.sa, segment.NotHeld)
		if err != nil {
			failed <- err
			return
		}
		done <- ret
	}()
	return done, failed
}

// TestCarryRemovalOutlivesItsSegment: a removal record whose retention
// counted a retired segment answers not droppable while the unlink of that
// segment is still undurable — the directory sync has not returned — and
// drops only once the unlink is durable, the durable signal's relay: retire
// runs, the tracker ends the segment's state and its registrations, but the
// write's drop ranges stay in the index, so the delete that would drop at
// the next pass stays carried; release the sync, the durable signal fires,
// the ranges leave, and the delete answers droppable at last.
func TestCarryRemovalOutlivesItsSegment(t *testing.T) {
	c := plantCarryLayout(t)

	hold := c.f.seam.Hold(t, kit.OpSyncDir, "*")
	done, failed := c.retireAsync()
	select {
	case <-hold.Reached():
	case ret := <-done:
		t.Fatalf("the retirement finished as %+v before its directory sync was held, want it held mid-flight", ret)
	case err := <-failed:
		t.Fatalf("the retirement failed: %v", err)
	case <-time.After(copyWait):
		t.Fatal("the retirement never reached its directory sync")
	}

	// Inside the hold: the retired-segment hook has run — the segment left
	// the set, the tracker ended its liveness — but the unlink is not
	// durable, so the delete is what a pass would carry, not drop.
	if c.tr.Droppable(c.del) {
		t.Fatal("Droppable of the delete while the retirement's directory sync is held = true, want false: the unlink is not durable, so the source segment's record still outranks it")
	}

	hold.Release()
	select {
	case ret := <-done:
		if !ret.Durable {
			t.Fatalf("Durable of the retirement after a clean sync = %v, want true", ret.Durable)
		}
	case err := <-failed:
		t.Fatalf("the retirement failed after the sync was released: %v", err)
	case <-time.After(copyWait):
		t.Fatal("the retirement did not finish after its directory sync was released")
	}
	if !c.tr.Droppable(c.del) {
		t.Fatal("Droppable of the delete after the unlink is durable = false, want true: the write it counted on left with the source segment")
	}
}

// TestCarryRemovalStaysWhenADirectorySyncFails: a retirement whose directory
// sync fails completes — the unlink succeeded — but reports itself not
// durable, and the durable signal never fires: the delete stays carried,
// undroppable for the rest of this process, because the unlink may not be
// on disk and a crash could bring the source segment back beside it.
func TestCarryRemovalStaysWhenADirectorySyncFails(t *testing.T) {
	c := plantCarryLayout(t)

	hold := c.f.seam.Hold(t, kit.OpSyncDir, "*")
	boom := errors.New("directory sync failed at the seam")
	if err := c.f.seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 1, boom)); err != nil {
		t.Fatalf("Schedule of the failing sync: %v", err)
	}
	done, failed := c.retireAsync()
	select {
	case <-hold.Reached():
	case ret := <-done:
		t.Fatalf("the retirement finished as %+v before its directory sync was held, want it held mid-flight", ret)
	case err := <-failed:
		t.Fatalf("the retirement failed: %v", err)
	case <-time.After(copyWait):
		t.Fatal("the retirement never reached its directory sync")
	}
	hold.Release()
	select {
	case ret := <-done:
		if ret.Durable {
			t.Fatalf("Durable of the retirement after a failed directory sync = %v, want false: the removal may not be on disk", ret.Durable)
		}
	case err := <-failed:
		t.Fatalf("the retirement failed after a failed directory sync, want nil: the unlink itself succeeded: %v", err)
	case <-time.After(copyWait):
		t.Fatal("the retirement did not finish after its directory sync failed")
	}
	if c.tr.Droppable(c.del) {
		t.Fatal("Droppable of the delete after a failed directory sync = true, want false: the unlink is not durable, so the removal stays carried until this process ends")
	}
}
