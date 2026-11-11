package reclaim

import (
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// takeIn makes one more spare over the fixture's seam and takes it into the
// fixture's set, a real second segment holding nothing, so the set's own
// seal observer is the seal path the tracker reaches.
func (f *fixture) takeIn(t *testing.T) core.SegmentID {
	t.Helper()
	maker := segment.NewMaker(f.seam.FS(), f.set, 1<<62, f.last, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := f.set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	f.last = seg
	return seg
}

// TestSetDeferred_UntilTheDeferralEnds: a segment holding only a dropped
// record whose loss is deferred stays held — no byte of it backs anything,
// no record counts live — until the deferral ends: the events of the file
// its records named release nothing, and the call that ends the deferral is
// the one release, firing the candidate exactly once for the
// held-to-not-held crossing it makes.
func TestSetDeferred_UntilTheDeferralEnds(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(7)

	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the segment before the deferral = %v, want false", got)
	}
	// The segment holds nothing else: the flag alone is what holds it.
	tr.SetDeferred(f.seg, true)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held while the loss is deferred = %v, want true: the flag alone keeps a segment of dropped records", got)
	}

	// Events of the file the dropped record named release nothing: a
	// deferred loss ends only at the call that says so, however settled
	// its file is.
	tr.MarkerSettled(id, ver(4))
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after a settle of the file = %v, want true: the deferral is not the marker's to release", got)
	}
	tr.Retest(id)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after a copies-done of the file = %v, want true: the deferral is not the copies' to release", got)
	}
	tr.Retest(id)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after a re-test of the file = %v, want true: the deferral is not the re-test's to release", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the deferral held the segment, want 0", got)
	}

	// The deferral ends: nothing else holds the segment, the crossing
	// fires the candidate once, and a repeated end changes nothing.
	tr.SetDeferred(f.seg, false)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the deferral ended = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: ending the deferral is the one release", fired, f.seg)
	}
	tr.SetDeferred(f.seg, false)
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times after a repeated end, want 1: the second clear crossed nothing", got)
	}
}

// TestSetDeferred_BesideARecord: the flag holds beside a live record —
// either alone keeps the segment — so neither the flag's end nor the
// record's release is the crossing while the other stays, and whichever
// leaves last fires the candidate once.
func TestSetDeferred_BesideARecord(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })

	rec := Rec{
		Kind: format.KindTruncate, File: fid(9), Seg: f.seg,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(8), Seq: 9}, Seq: 9,
	}
	tr.Register(rec)
	tr.SetDeferred(f.seg, true)
	tr.SetDeferred(f.seg, false)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with the record still live = %v, want true: the deferral was not the only hold", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the record held the segment, want 0", got)
	}

	// The record's release, with the deferral set again, is no crossing
	// either: the flag keeps the segment.
	tr.SetDeferred(f.seg, true)
	tr.Unregister(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the record's release with the deferral on = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the deferral held, want 0", got)
	}
	tr.SetDeferred(f.seg, false)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the deferral ended = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]", fired, f.seg)
	}
}

// TestRegisterForget_LiveUntilEverySnapshotSegmentLeaves: a forget record is
// registered with its snapshot — the segments holding records of its share
// tag when the forget was appended — and keeps its own segment held while
// any of them remains in the set; the tracker learns of each departure from
// SegmentRetired, and the retirement of the last one is the release, firing
// the candidate once.
func TestRegisterForget_LiveUntilEverySnapshotSegmentLeaves(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	one, two := f.seg+1, f.seg+2

	forget := Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: f.seg,
		Pair: index.Numbers{Version: ver(9), Seq: 20}, Seq: 20,
	}
	tr.RegisterForget(forget, []core.SegmentID{one, two})
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with the forget record's snapshot segments alive = %v, want true", got)
	}
	if got := tr.Droppable(forget); got {
		t.Fatalf("Droppable of a forget record whose snapshot segments live = %v, want false", got)
	}

	// A re-test of the zero file — the file every forget record is
	// registered under — drops nothing while a snapshot segment lives.
	tr.Retest(core.FileID{})
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after a re-test with a snapshot segment alive = %v, want true: the snapshot is the forget record's rule", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the forget record was live, want 0", got)
	}

	// One snapshot segment leaving is not the end: another still holds
	// the tag's records.
	tr.SegmentRetired(one, nil)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after one snapshot segment retired = %v, want true: another still holds the tag's records", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the forget record was live, want 0", got)
	}

	// The last snapshot segment's retirement releases the forget record,
	// exactly as any release would: the crossing fires the candidate once.
	tr.SegmentRetired(two, nil)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the last snapshot segment retired = %v, want false", got)
	}
	if got := tr.Droppable(forget); !got {
		t.Fatalf("Droppable of a forget record whose snapshot is empty = %v, want true", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the last snapshot segment's retirement is the forget record's release", fired, f.seg)
	}

	// A forget record registered with an empty snapshot names a barrier
	// that released everything below it: the record is droppable the
	// moment it is registered, and because the zero file it registers
	// under is one no later event of the journal reaches, the
	// registration itself carries the re-test that drops it — the
	// segment holding nothing else crosses at once.
	solo := Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: f.seg,
		Pair: index.Numbers{Version: ver(19), Seq: 40}, Seq: 40,
	}
	tr.RegisterForget(solo, nil)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after registering an empty-snapshot forget record = %v, want false: the registration's own re-test drops it", got)
	}
	if len(fired) != 2 {
		t.Fatalf("the candidate fired %d times with %v, want two: one firing per release", len(fired), fired)
	}
	// A later re-test of the zero file answers over an empty registry and
	// releases nothing more.
	tr.Retest(core.FileID{})
	if got := len(fired); got != 2 {
		t.Fatalf("the candidate fired %d times after the late re-test, want two still: an empty registry releases nothing", got)
	}
}

// TestHoldEnded_ReleasesBySealOrMarkPastLast: an ended segment's hold is
// registered with the copies' segment and the last copy's position. It is
// not the window's resolution, the caller's releases or the copies' sync
// that releases it: the hold answers held with nothing else held, and it
// ends at the seal of the copies' segment — through the set's own observer —
// or at a recorded mark past the last copy; a mark reaching only the last
// copy, or short of it, releases nothing.
func TestHoldEnded_ReleasesBySealOrMarkPastLast(t *testing.T) {
	// Case 1: the seal of the copies' segment is the release. The ended
	// segment holds nothing else — its window resolved and its extents
	// left the index — and the hold stands through a caller's release of
	// a record of its own and through the mark its copies reached, until
	// the seal.
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	copies := f.takeIn(t)
	f.set.OnSeal(tr.SealObserved)

	tr.HoldEnded(f.seg, copies, 100)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held while the hold on the ended segment stands = %v, want true", got)
	}

	// A caller's release of a record of the segment is no proof of the
	// copies: the hold is not a count of the records it stands.
	tr.Unregister(Rec{Kind: format.KindWrite, File: fid(5), Seg: f.seg})
	// The copies' sync reaching the last copy is not a release.
	tr.MarksRecorded(copies, 100)
	// A mark short of the last copy is not a release either.
	tr.MarksRecorded(copies, 99)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the copies reached the last copy = %v, want true: the sync and marks short of the last copy release nothing", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the hold stood, want 0", got)
	}

	// The copies' segment is sealed through the set the tracker observes:
	// the release is the tracker's own, so the candidate fires once, and
	// a later mark past the last copy finds no hold left.
	if err := f.set.Seal(copies, true); err != nil {
		t.Fatalf("Seal of the copies' segment: %v", err)
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the seal of the copies' segment = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the seal is the release", fired, f.seg)
	}
	tr.MarksRecorded(copies, 200)
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times after the hold was released, want 1: a released hold is not released again", got)
	}

	// Case 2: a recorded mark past the last copy is the release, no seal at
	// all. The mark reaching the last copy is not, and a mark short of it
	// is not: only one past it ends the hold.
	f2 := newFixture(t)
	var fired2 []core.SegmentID
	tr2 := New(f2.ix, func(seg core.SegmentID) { fired2 = append(fired2, seg) })
	copies2 := f2.takeIn(t)
	tr2.HoldEnded(f2.seg, copies2, 50)
	tr2.MarksRecorded(copies2, 50)
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held after the copies' sync alone = %v, want true: the hold is not the sync's to release", got)
	}
	tr2.MarksRecorded(copies2, 49)
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held after a mark short of the last copy = %v, want true", got)
	}
	if got := len(fired2); got != 0 {
		t.Fatalf("the candidate fired %d times while the hold stood, want 0", got)
	}
	tr2.MarksRecorded(copies2, 51)
	if got := tr2.Held(f2.seg); got {
		t.Fatalf("Held after a mark past the last copy = %v, want false: the mark proves the copies", got)
	}
	if len(fired2) != 1 || fired2[0] != f2.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the mark past the last copy is the release", fired2, f2.seg)
	}

	// Case 3: an ended segment no re-append registered holds nothing on
	// this account — Held answers false and the candidate stays quiet.
	f3 := newFixture(t)
	var fired3 []core.SegmentID
	tr3 := New(f3.ix, func(seg core.SegmentID) { fired3 = append(fired3, seg) })
	if got := tr3.Held(f3.seg); got {
		t.Fatalf("Held of an ended segment no hold registered = %v, want false: nothing on this account holds it", got)
	}
	if got := len(fired3); got != 0 {
		t.Fatalf("the candidate fired %d times with no hold registered, want 0", got)
	}

	// Case 4: a seal of a segment no hold names releases nothing — the
	// observer answers only holds that cite the sealed segment as their
	// copies, so the hold naming another segment stands through it.
	f4 := newFixture(t)
	var fired4 []core.SegmentID
	tr4 := New(f4.ix, func(seg core.SegmentID) { fired4 = append(fired4, seg) })
	ca := f4.takeIn(t)
	stranger := f4.takeIn(t)
	f4.set.OnSeal(tr4.SealObserved)
	tr4.HoldEnded(f4.seg, ca, 10)
	if err := f4.set.Seal(stranger, true); err != nil {
		t.Fatalf("Seal of a segment no hold names: %v", err)
	}
	if got := tr4.Held(f4.seg); !got {
		t.Fatalf("Held after the seal of a segment the hold does not name = %v, want true", got)
	}
	if got := len(fired4); got != 0 {
		t.Fatalf("the candidate fired %d times for a seal nothing cites, want 0", got)
	}
}

// TestSetDeferred_CountedPerLoss: two deferred losses on one segment keep
// it held through the first end — the count still stands at one — and the
// last end is the crossing that fires the candidate once; a repeated end
// lowers nothing, counting no underflow, and the segment answers not held.
func TestSetDeferred_CountedPerLoss(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })

	tr.SetDeferred(f.seg, true)
	tr.SetDeferred(f.seg, true)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with two deferred losses = %v, want true", got)
	}
	// The first end leaves one standing.
	tr.SetDeferred(f.seg, false)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after one deferral of two ended = %v, want true: the second loss keeps the segment", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times with a deferral standing, want 0", got)
	}
	// The last end is the crossing; a stray third end lowers nothing and
	// fires nothing, rather than dipping a count below zero.
	tr.SetDeferred(f.seg, false)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after both deferrals ended = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the last end is the crossing", fired, f.seg)
	}
	tr.SetDeferred(f.seg, false)
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times after a stray end, want 1: the count clamps at zero", got)
	}
	if got := tr.stateOf(f.seg).deferred.Load(); got != 0 {
		t.Fatalf("the deferred count after a stray end = %d, want 0: no underflow", got)
	}
}

// TestRegisterForget_RetiredBetweenSnapshotAndRegister: a snapshot segment
// retired after the caller took its snapshot but before the registration
// reaches the tracker is pruned by the registration's own re-check, not left
// in the snapshot forever: the forget record is not live past the
// retirement the tracker never heard of, and once the remaining snapshot
// segments leave, the record is released. The member check is the tracker's
// one question of the set, answered by State.
func TestRegisterForget_RetiredBetweenSnapshotAndRegister(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	tr.SetMember(func(seg core.SegmentID) bool {
		_, err := f.set.State(seg)
		return err == nil
	})
	one, two := f.takeIn(t), f.takeIn(t)
	three := f.takeIn(t)

	// The retirement the tracker never hears of, between the caller's
	// snapshot and its registration.
	if err := f.set.Seal(two, true); err != nil {
		t.Fatalf("Seal of a snapshot segment: %v", err)
	}
	if _, err := f.set.Retire(two); err != nil {
		t.Fatalf("Retire of a snapshot segment: %v", err)
	}

	forget := Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: f.seg,
		Pair: index.Numbers{Version: ver(9), Seq: 20}, Seq: 20,
	}
	tr.RegisterForget(forget, []core.SegmentID{one, two, three})
	// The already-retired segment never held the record: the re-check
	// pruned it, so the record lives on the two that remain.
	if got := len(tr.forgets[forget]); got != 2 {
		t.Fatalf("the snapshot kept %d segments after the re-check, want 2: the retired one was pruned at registration", got)
	}
	if got := tr.Droppable(forget); got {
		t.Fatalf("Droppable with two snapshot segments alive = %v, want false", got)
	}

	// The remaining two leave, and the record is released — not held
	// forever by a snapshot naming a segment long gone.
	for _, s := range []core.SegmentID{one, three} {
		if err := f.set.Seal(s, true); err != nil {
			t.Fatalf("Seal of a snapshot segment: %v", err)
		}
		if _, err := f.set.Retire(s); err != nil {
			t.Fatalf("Retire of a snapshot segment: %v", err)
		}
		tr.SegmentRetired(s, nil)
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after every snapshot segment left = %v, want false: a segment retired before the registration held the record forever otherwise", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]", fired, f.seg)
	}
}

// TestRegisterForget_RetireRacesRegister drives the two sequential orders of
// a retirement and a registration, each once: a retirement running after the
// registration prunes the snapshot itself; one that ran before it is answered
// by the member check. The concurrent race between a registration and a
// re-test is TestRegisterForget_ConcurrentRetestKeepsLiveSnapshot's.
func TestRegisterForget_RetireRacesRegister(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	tr.SetMember(func(seg core.SegmentID) bool {
		_, err := f.set.State(seg)
		return err == nil
	})
	one, two := f.takeIn(t), f.takeIn(t)
	forget := Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: f.seg,
		Pair: index.Numbers{Version: ver(9), Seq: 20}, Seq: 20,
	}

	// The whole registration before the retirement: the prune catches it.
	if err := f.set.Seal(one, true); err != nil {
		t.Fatalf("Seal of a snapshot segment: %v", err)
	}
	if _, err := f.set.Retire(one); err != nil {
		t.Fatalf("Retire of a snapshot segment: %v", err)
	}
	tr.RegisterForget(forget, []core.SegmentID{one, two})
	tr.SegmentRetired(two, nil)
	if _, ok := tr.forgets[forget]; ok {
		t.Fatalf("the forget record is still live after every snapshot segment left, want it released")
	}

	// The retirement before the whole registration: the member check
	// catches it. Fresh segments — an id is never drawn again once left.
	three, four := f.takeIn(t), f.takeIn(t)
	if err := f.set.Seal(three, true); err != nil {
		t.Fatalf("Seal of a snapshot segment: %v", err)
	}
	if _, err := f.set.Retire(three); err != nil {
		t.Fatalf("Retire of a snapshot segment: %v", err)
	}
	second := forget
	second.Seq = 21
	second.Pair.Seq = 21
	tr.RegisterForget(second, []core.SegmentID{three, four})
	if got := len(tr.forgets[second]); got != 1 {
		t.Fatalf("the snapshot kept %d segments, want 1: the retired segment was pruned by the re-check", got)
	}
}

// TestHoldEnded_CopiesRetiredNeverSealed: two ended segments cite one
// copies segment, and the copies segment is retired without ever being
// sealed — no mark will pass any last copy now. The retirement releases
// both holds exactly once: each ended segment's count lowers by one, no
// candidate fires on a segment another hold or a record still keeps, and
// none of the three segments' counts goes negative.
func TestHoldEnded_CopiesRetiredNeverSealed(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	copies := f.takeIn(t)
	ended1 := f.takeIn(t)
	ended2 := f.takeIn(t)

	tr.HoldEnded(ended1, copies, 10)
	tr.HoldEnded(ended2, copies, 20)
	if got := tr.Held(ended1); !got {
		t.Fatalf("Held of the first ended segment = %v, want true", got)
	}
	if got := tr.Held(ended2); !got {
		t.Fatalf("Held of the second ended segment = %v, want true", got)
	}

	tr.SegmentRetired(copies, nil)
	if got := tr.Held(ended1); got {
		t.Fatalf("Held of the first ended segment after its copies retired = %v, want false", got)
	}
	if got := tr.Held(ended2); got {
		t.Fatalf("Held of the second ended segment after its copies retired = %v, want false", got)
	}
	// One firing each, in whichever order the releases ran.
	if len(fired) != 2 {
		t.Fatalf("the candidate fired %d times, want 2: once per ended segment's release", len(fired))
	}
	if got := tr.stateOf(ended1).holds.Load(); got != 0 {
		t.Fatalf("the first ended segment's hold count after the release = %d, want 0: released once, never negative", got)
	}
	if got := tr.stateOf(ended2).holds.Load(); got != 0 {
		t.Fatalf("the second ended segment's hold count after the release = %d, want 0: released once, never negative", got)
	}

	// The registry entry left with the copies segment: a mark past every
	// last copy now finds nothing to release, and no count dips below
	// zero.
	tr.MarksRecorded(copies, 1000)
	if got := tr.stateOf(ended1).holds.Load(); got != 0 {
		t.Fatalf("the first ended segment's hold count after both releases = %d, want 0", got)
	}
	if got := tr.stateOf(ended2).holds.Load(); got != 0 {
		t.Fatalf("the second ended segment's hold count after both releases = %d, want 0", got)
	}
	if got := len(fired); got != 2 {
		t.Fatalf("the candidate fired %d times, want 2: no second release for either hold", got)
	}

	// The ended segment's own retirement clears the holds it was the
	// keeping segment of, so a later mark or seal of a copies segment it
	// named lowers nothing — the releaseHold the registry would hand it
	// finds no state.
	third := f.takeIn(t)
	fourth := f.takeIn(t)
	tr.HoldEnded(third, fourth, 5)
	tr.SegmentRetired(third, nil)
	if st := tr.stateOf(third); st != nil {
		t.Fatalf("the ended segment's state survived its retirement, want it gone")
	}
	tr.SegmentRetired(fourth, nil)
	if got := len(fired); got != 2 {
		t.Fatalf("the candidate fired %d times after the ended segment retired, want 2: a released hold's registry entry lowers nothing", got)
	}
}

// TestSegmentRetired_ForgetRecordTakenWhateverTheFilesList: a forget record
// registered live in the retiring segment under the zero file — which a
// caller's files list never names — must still have its registration taken,
// with its snapshot, or the zero file's registry entry leaks and a later
// release of the record lowers a state already gone to the segment's cost.
func TestSegmentRetired_ForgetRecordTakenWhateverTheFilesList(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(3)
	forget := Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: f.seg,
		Pair: index.Numbers{Version: ver(9), Seq: 20}, Seq: 20,
	}
	tr.RegisterForget(forget, []core.SegmentID{f.seg + 1})

	// Retire the segment carrying the record, with a files list that names
	// a real file and never the zero file.
	tr.SegmentRetired(f.seg, []core.FileID{id})
	if _, ok := tr.recs[core.FileID{}][forget]; ok {
		t.Fatalf("the forget record is still registered under the zero file, want the retirement to take it whatever the files list named")
	}
	if _, ok := tr.forgets[forget]; ok {
		t.Fatalf("the forget record's snapshot survived its own segment's retirement, want it taken with the registration")
	}
	if st := tr.stateOf(f.seg); st != nil {
		t.Fatalf("the retired segment's state survived, want it gone")
	}
}

// forgetAt draws one forget record for the tests below, at a fresh sequence
// number each call so two registrations of the same record key never collide
// with another test's.
func forgetAt(seq uint64) Rec {
	return Rec{
		Kind: format.KindForget, File: core.FileID{}, Seg: 0,
		Pair: index.Numbers{Version: ver(9), Seq: core.Seq(seq)}, Seq: core.Seq(seq),
	}
}

// TestRegisterForget_EmptySnapshotDropsItself: a forget record registered
// with an empty snapshot names a barrier that released everything below it,
// so the record answers droppable at once — and its own registration, not a
// later event of the zero file nothing reaches, must carry the re-test that
// drops it: the segment holding nothing else crosses to not held and the
// candidate fires exactly once.
func TestRegisterForget_EmptySnapshotDropsItself(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	seg := f.takeIn(t)
	forget := forgetAt(20)
	forget.Seg = seg
	tr.RegisterForget(forget, nil)
	if got := tr.Droppable(forget); !got {
		t.Fatalf("Droppable of a forget record with an empty snapshot = %v, want true: no segment holds a record of its tag below it", got)
	}
	if got := tr.Held(seg); got {
		t.Fatalf("Held of the segment holding only the empty-snapshot forget = %v, want false: the registration's own re-test must drop the record", got)
	}
	if len(fired) != 1 || fired[0] != seg {
		t.Fatalf("the candidate fired %v, want [%d]: the registration's re-test is the one crossing", fired, seg)
	}
	if _, ok := tr.recs[core.FileID{}][forget]; ok {
		t.Fatalf("the empty-snapshot forget record is still registered, want the registration's re-test to have taken it")
	}
}

// TestRegisterForget_SnapshotPrunedToEmptyDropsItself: a forget record whose
// snapshot segments were all retired before its registration — the member
// re-check prunes each — is droppable the same moment, and the registration
// itself must carry the re-test: the record's segment, holding nothing else,
// answers not held with one candidacy.
func TestRegisterForget_SnapshotPrunedToEmptyDropsItself(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	tr.SetMember(func(seg core.SegmentID) bool {
		_, err := f.set.State(seg)
		return err == nil
	})
	one, two := f.takeIn(t), f.takeIn(t)
	for _, s := range []core.SegmentID{one, two} {
		if err := f.set.Seal(s, true); err != nil {
			t.Fatalf("Seal of a snapshot segment: %v", err)
		}
		if _, err := f.set.Retire(s); err != nil {
			t.Fatalf("Retire of a snapshot segment: %v", err)
		}
		tr.SegmentRetired(s, nil)
	}
	seg := f.takeIn(t)
	forget := forgetAt(30)
	forget.Seg = seg
	tr.RegisterForget(forget, []core.SegmentID{one, two})
	if got := tr.droppableLocked(forget); !got {
		t.Fatalf("droppableLocked of a forget whose snapshot drained at registration = %v, want true", got)
	}
	if got := tr.recs[core.FileID{}][forget]; got != 0 {
		t.Fatalf("registrations of the drained forget left in the registry = %d, want 0", got)
	}
	if len(fired) != 1 || fired[0] != seg {
		t.Fatalf("the candidate fired %v, want [%d]: the re-check-drained snapshot leaves the re-test the one crossing", fired, seg)
	}
	if _, ok := tr.recs[core.FileID{}]; ok {
		t.Fatalf("the zero file's registry entry survived the drained-snapshot registration, want it removed when it empties")
	}
}

// TestSegmentRetired_DoubledForgetSnapshotEmptied: a forget record registered
// twice whose snapshot empties at a retirement must release BOTH
// registrations — the records walk below takes per-registration, and the
// forget branch above it must too — or the segment keeps a count and answers
// held after everything that named it is gone.
func TestSegmentRetired_DoubledForgetSnapshotEmptied(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	seg := f.takeIn(t)
	one := f.takeIn(t)
	forget := forgetAt(40)
	forget.Seg = seg
	tr.RegisterForget(forget, []core.SegmentID{one})
	tr.RegisterForget(forget, []core.SegmentID{one})
	if got := tr.recs[core.FileID{}][forget]; got != 2 {
		t.Fatalf("the doubled forget registered %d, want 2", got)
	}
	tr.SegmentRetired(one, nil)
	if got, want := tr.recs[core.FileID{}][forget], 0; got != want {
		t.Fatalf("after the snapshot emptied at the retirement, the doubled forget holds %d registrations, want %d: the forget branch releases each", got, want)
	}
	seg2 := f.takeIn(t)
	_ = seg2
	if st := tr.stateOf(seg); st != nil && st.live.Load() != 0 {
		t.Fatalf("the double-registered forget's segment still counts %d live, want 0", st.live.Load())
	}
	// Holding nothing else, the segment crossed once the second
	// registration released: exactly one candidacy, whichever order the two
	// releases crossed the line.
	if len(fired) != 1 || fired[0] != seg {
		t.Fatalf("the candidate fired %v, want [%d]: one crossing for the two registrations", fired, seg)
	}
}

// TestRegisterForget_ConcurrentRetestKeepsLiveSnapshot: a forget record
// registered with a live snapshot is never dropped by a re-test of the zero
// file racing its registration. The registration and the snapshot enter
// under one hold of rm, so a re-test sees both or neither; with the two
// split, the re-test can see the record with no snapshot, answer it
// droppable and release it, leaving the snapshot behind with nothing
// registered — Held of the record's segment false while a snapshot segment
// still holds the tag's records. Three racing re-tests each run: another
// empty-snapshot registration, the open's close, and the retirement of a
// segment holding another forget record. Each runs against the registration
// released at one barrier; the race is a window of a few instructions, so
// several racers share each round.
func TestRegisterForget_ConcurrentRetestKeepsLiveSnapshot(t *testing.T) {
	const rounds, racers = 1500, 4
	f := newFixture(t)
	snapSeg, other, third := f.takeIn(t), f.takeIn(t), f.takeIn(t)
	a := forgetAt(20)
	a.Seg = f.seg
	b := forgetAt(21)
	b.Seg = f.seg
	c := forgetAt(22)
	c.Seg = other

	races := map[string]func(tr *Tracker, i int){
		"RegisterForget": func(tr *Tracker, i int) {
			r := b
			r.Seq = core.Seq(100 + i)
			r.Pair.Seq = r.Seq
			tr.RegisterForget(r, nil)
		},
		"OpenDone": func(tr *Tracker, _ int) { tr.OpenDone() },
		"SegmentRetired": func(tr *Tracker, _ int) {
			tr.SegmentRetired(other, nil)
		},
	}
	for name, race := range races {
		t.Run(name, func(t *testing.T) {
			for round := range rounds {
				tr := New(f.ix, nil)
				if name == "SegmentRetired" {
					// The forget record c lives in other, its snapshot a
					// segment that stays: retiring other drops c and
					// re-tests the zero file its record was registered under.
					tr.RegisterForget(c, []core.SegmentID{third})
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(1 + racers)
				go func() {
					defer wg.Done()
					<-start
					tr.RegisterForget(a, []core.SegmentID{snapSeg})
				}()
				for i := range racers {
					go func() {
						defer wg.Done()
						<-start
						race(tr, i)
					}()
				}
				close(start)
				wg.Wait()

				if got := tr.recs[core.FileID{}][a]; got != 1 {
					t.Fatalf("round %d: registrations of the forget record with a live snapshot = %d, want 1: a racing re-test dropped it", round, got)
				}
				if got := len(tr.forgets[a]); got != 1 {
					t.Fatalf("round %d: snapshot segments kept for the forget record = %d, want 1", round, got)
				}
				if got := tr.Held(f.seg); !got {
					t.Fatalf("round %d: Held of the forget record's segment = %v, want true while its snapshot segment lives", round, got)
				}
				// The snapshot segment retiring is the record's release.
				tr.SegmentRetired(snapSeg, nil)
				if got := tr.recs[core.FileID{}][a]; got != 0 {
					t.Fatalf("round %d: registrations left after the snapshot segment retired = %d, want 0", round, got)
				}
				if got := tr.Held(f.seg); got {
					t.Fatalf("round %d: Held after the snapshot segment retired = %v, want false", round, got)
				}
			}
		})
	}
}
