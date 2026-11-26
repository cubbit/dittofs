package reclaim

import (
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// segCap is the cap the fixture's segments are accounted at.
const segCap = int64(64 << 10)

// tag is the share tag the seeded records claim their files under.
const tag = core.ShareTag(1)

func ver(n uint64) core.Version { return core.Version{Counter: n} }

func fid(n byte) core.FileID { return core.NewFileID([16]byte{15: n}) }

// fixture is a real index, a real segment ledger, and one real segment on a
// recorded storage seam: the segment exists on disk, holding nothing.
type fixture struct {
	ix   *index.Index
	set  *segment.Set
	seam *kit.Seam
	seg  core.SegmentID
	// last is the largest segment id a Maker drew over the seam, so a
	// later spare draws above it instead of over its name.
	last core.SegmentID
}

// newFixture makes one spare over the seam and takes it into the set as the
// segment the tests' records live in.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(segCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	return &fixture{ix: index.New(1 << 20), set: set, seam: seam, seg: seg, last: seg}
}

// write seeds one held extent of id inside the fixture's segment, at offset
// at in the segment, content version v and sequence number s. Disjoint
// extents supersede nothing, so nothing is retained.
func (f *fixture) write(t *testing.T, id core.FileID, off, n int64, at uint32, v, s uint64) {
	t.Helper()
	p := index.Placed{
		Extent:  core.Extent{Off: off, Len: n},
		Record:  index.RecordAt{Segment: f.seg, Offset: at},
		Version: ver(v),
	}
	if _, err := f.ix.WriteRetain(tag, id, p, core.Seq(s)); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
}

// remove seeds one removal of id over one seeded extent, at a version above
// every write's: it leaves a marker and stops the segment's bytes holding the
// extent, exactly as a settled truncate leaves it.
func (f *fixture) remove(t *testing.T, id core.FileID, off, n int64, v uint64) {
	t.Helper()
	if _, _, err := f.ix.Lose(id, core.Extent{Off: off, Len: n}, ver(v)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
}

// TestHeld_ZeroBytesNoRecords_NotHeld_NoReads: a segment whose three
// removals settled — the extents they cut are gone and no marker unsettled
// below them remains — reports not held from counters alone, with no
// registered record beside the bytes. The no-reads guarantee is structural:
// the tracker holds no handle to any segment's storage and reads only the
// index's counters and its own memory, so the seam below, the one the
// segment's records were made over, shows nothing while Held answers; the
// same call is made over a segment whose backing file is gone from the
// directory, which any read of the segment would fail on rather than
// answer.
func TestHeld_ZeroBytesNoRecords_NotHeld_NoReads(t *testing.T) {
	f := newFixture(t)
	// The seed reached the storage: the call log the no-reads check reads
	// is one that has counted calls, not one that asserts nothing.
	if got := f.seam.Calls(kit.OpAllocate); got == 0 {
		t.Fatalf("the seed made %d allocate calls, want some: the call log asserts nothing", got)
	}

	id := fid(1)
	tr := New(f.ix, nil)

	// Three records held bytes of the segment.
	for i := 0; i < 3; i++ {
		f.write(t, id, int64(i)*4096, 4096, uint32(i), uint64(i)+1, uint64(i)+1)
	}
	if b := f.ix.SegmentBytes(f.seg); b.Held != 3*4096 || b.Retained != 0 {
		t.Fatalf("the segment's bytes after three writes = held %d retained %d, want held %d retained 0", b.Held, b.Retained, 3*4096)
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held while the segment backs bytes = %v, want true", got)
	}

	// Three removals above them settle the records: nothing below their
	// markers waits for a settle, and the segment backs nothing.
	for i := 0; i < 3; i++ {
		f.remove(t, id, int64(i)*4096, 4096, 10+uint64(i))
	}
	if b := f.ix.SegmentBytes(f.seg); b.Held != 0 || b.Retained != 0 {
		t.Fatalf("the segment's bytes after three removals = held %d retained %d, want 0 and 0", b.Held, b.Retained)
	}
	if got := f.ix.MarkerSettled(id, ver(9)); !got {
		t.Fatalf("MarkerSettled below the removals = %v, want true: every marker sits above 9", got)
	}

	reads := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of a segment of settled removals, bytes gone, records none = %v, want false", got)
	}
	if got := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect) - reads; got != 0 {
		t.Fatalf("Held made %d storage reads, want 0: the answer comes from counters alone", got)
	}

	// The same answer over a segment whose storage is gone: the backing
	// file is unlinked from the directory, so the answer still comes from
	// the counters and any read of the segment would have failed to make
	// it. The file was made through the same seam this test reads the
	// call log of.
	if err := f.seam.Remove(format.SegmentName(f.seg)); err != nil {
		t.Fatalf("Remove of the segment's file: %v", err)
	}
	reads = f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of a segment of settled removals, file gone = %v, want false", got)
	}
	if got := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect) - reads; got != 0 {
		t.Fatalf("Held over a segment with no file made %d storage reads, want 0", got)
	}
}

// TestHeld_OneLiveRecord: one registered record keeps a segment that backs
// no bytes held, its file is kept under its segment, and the candidate fires
// exactly once when the segment's held state leaves through the tracker's
// own release — never while bytes or another record still hold it, and never
// for a release nothing counted.
func TestHeld_OneLiveRecord(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(7)

	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the empty segment = %v, want false", got)
	}

	rec := Rec{
		Kind: format.KindTruncate,
		File: id,
		Seg:  f.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(4), Seq: 9},
		Seq:  9,
	}
	tr.Register(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with one live record = %v, want true", got)
	}
	if _, ok := tr.stateOf(f.seg).files[id]; !ok {
		t.Fatalf("the record's file was not kept under its segment")
	}

	// Bytes beside the record: releasing the record leaves the segment
	// held by what it still backs, and the candidate stays quiet.
	f.write(t, fid(8), 0, 4096, 100, 20, 20)
	tr.Unregister(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the record's release, bytes still backed = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the segment held bytes, want 0", got)
	}

	// The bytes leaving through the index moves the answer to false, but a
	// release the tracker did not make fires nothing.
	f.remove(t, fid(8), 0, 4096, 30)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the bytes left = %v, want false", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times for an index-driven release, want 0", got)
	}

	// The transition itself: two records, one release leaving the other
	// live, then the last release firing the candidate once.
	second := rec
	second.File = fid(9)
	second.Ext = core.Extent{Off: 4096, Len: 4096}
	tr.Register(rec)
	tr.Register(second)
	tr.Unregister(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with the second record still live = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while a record was still live, want 0", got)
	}
	tr.Unregister(second)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the last record's release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: one held-to-not-held transition", fired, f.seg)
	}

	// A release of what nothing counted changes nothing.
	tr.Unregister(second)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after a release of nothing = %v, want false", got)
	}
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times after a release of nothing, want 1", got)
	}
}

// TestTrackerIdentityClassHoldRules walks the identity-class hold table over
// two placements of one header-only record: the original in one segment (S1)
// and a carried copy of the same record — every identifying field unchanged,
// only the segment and the location moving — in another (S2). The class holds
// the live count on one placement's segment only while that placement is the
// class's last: a second placement registers and both segments' holds end, a
// placement lost re-holds the survivor, and the original's own retirement —
// the fix's own path — re-holds the copy exactly as the copy's loss would
// re-hold the original.
func TestTrackerIdentityClassHoldRules(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(7)
	// A lower-pair write of the removal's file in a third segment, planted
	// through the rebuild: it overlaps what the delete covers, so the
	// identity stays undroppable throughout the walk and no re-test the
	// transitions carry can spend the registrations the table moves.
	over := f.takeIn(t)
	p := newPlant(f)
	p.write(t, id, over, core.Extent{Off: 0, Len: 4096}, 0, 3, 1)
	p.finish(t)
	if got := tr.Droppable(Rec{Kind: format.KindDelete, File: id, Pair: index.Numbers{Version: ver(5), Seq: 6}}); got {
		t.Fatalf("Droppable of the removal over the lower-pair write = %v, want false: the fixture's outranker keeps the class undroppable", got)
	}

	s1, s2 := f.seg, f.takeIn(t)
	base := Rec{
		Kind: format.KindDelete, File: id, Ext: core.Extent{},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
	}
	orig := base
	orig.Seg, orig.Loc = s1, at64(s1)
	cp := base
	cp.Seg, cp.Loc = s2, at64(s2)

	// 1. The original alone: its placement is the class's last, so it holds
	// its segment, and the candidate stays quiet — a registration makes no
	// segment less held.
	tr.Register(orig)
	if got := tr.Held(s1); !got {
		t.Fatalf("Held of the original's segment, the sole placement registered = %v, want true: the class's one placement holds", got)
	}
	if got := tr.stateOf(s1).live.Load(); got != 1 {
		t.Fatalf("the original's segment's live count with its sole placement = %d, want 1", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times for a first placement's registration, want 0", got)
	}

	// 2. The copy registers: the identity now stands in two places, so
	// neither placement holds — the wedge's rule, the moment that unwinds
	// it — and the original's segment crossing to not held is a candidacy.
	tr.Register(cp)
	if got := tr.Held(s1); got {
		t.Fatalf("Held of the original's segment once the copy registered = %v, want false: a record in two places holds neither", got)
	}
	if got := tr.Held(s2); got {
		t.Fatalf("Held of the copy's segment, second placement of one class = %v, want false: a record in two places holds neither", got)
	}
	if got, want := tr.recs[id][orig], 1; got != want {
		t.Fatalf("the original placement's registration count after the copy registered = %d, want %d", got, want)
	}
	if got, want := tr.recs[id][cp], 1; got != want {
		t.Fatalf("the copy placement's registration count after it registered = %d, want %d", got, want)
	}
	if len(fired) != 1 || fired[0] != s1 {
		t.Fatalf("the candidate fired %d times with %v, want one firing with %d: the class's doubling is the original's held-to-not-held crossing", len(fired), fired, s1)
	}

	// 3. The copy is lost unsettled — its registration released with the
	// record still live elsewhere: the class's last placement is the
	// original again, so the original re-holds. A re-hold raises a count, so
	// it fires no candidacy.
	tr.Unregister(cp)
	if got := tr.Held(s1); !got {
		t.Fatalf("Held of the original's segment after the copy's release = %v, want true: the survivor re-holds", got)
	}
	if got := tr.Held(s2); got {
		t.Fatalf("Held of the copy's segment after its release = %v, want false", got)
	}
	if _, ok := tr.recs[id][cp]; ok {
		t.Fatalf("the copy's registration survived its release, want it spent")
	}
	if got := tr.stateOf(s1).live.Load(); got != 1 {
		t.Fatalf("the original's segment's live count after the survivor re-held = %d, want 1", got)
	}
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times over the survivor's re-hold, want 1: a re-hold is not a held-to-not-held crossing", got)
	}

	// 4. The copy registers again, doubling the class — neither holds, the
	// candidacy for the original's crossing fires again...
	tr.Register(cp)
	if got := tr.Held(s1); got {
		t.Fatalf("Held of the original's segment after the re-doubling = %v, want false", got)
	}
	if got := tr.Held(s2); got {
		t.Fatalf("Held of the copy's segment after the re-doubling = %v, want false", got)
	}
	if len(fired) != 2 {
		t.Fatalf("the candidate fired %d times after the re-doubling, want 2: two crossings of the original's segment", len(fired))
	}
	// ...and the original's own segment then retires with the class still
	// undroppable — the fix's own path: the retirement's sweep takes the
	// original's registration, the class drops to the copy alone, and the
	// copy re-holds its segment.
	tr.SegmentRetired(s1, []core.FileID{id})
	if got := tr.Held(s2); !got {
		t.Fatalf("Held of the copy's segment after the original's segment retired = %v, want true: the class's last placement re-held it", got)
	}
	if got, want := tr.recs[id][cp], 1; got != want {
		t.Fatalf("the copy's registration count after the original's retirement = %d, want %d: the retirement took only the original's", got, want)
	}
	if _, ok := tr.recs[id][orig]; ok {
		t.Fatalf("the original's registration survived its segment's retirement, want the sweep to take it")
	}
	if got := len(fired); got != 2 {
		t.Fatalf("the candidate fired %d times over the original's retirement, want 2: the swept registration held nothing to release and no crossing ran", got)
	}
}

// TestTrackerIdentityClassOpenBothPlacementsHoldNothing: the crash shape. A
// persisted journal can hold one header-only record in two segments — the
// copy reached the disk before the crash — and the open's rebuild registers
// both: two placements of one identity, so neither segment is held by the
// record, whichever of the two the rebuild reads first.
func TestTrackerIdentityClassOpenBothPlacementsHoldNothing(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(17)
	s3, s4 := f.seg, f.takeIn(t)
	at := Rec{
		Kind: format.KindTruncate, File: id, Seg: s3,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
	}
	wait := at
	wait.Seg = s4 // the rebuild hands plain records no location
	tr.RegisterAtOpen([]Rec{at, wait}, nil, nil)
	if got := tr.Held(s3); got {
		t.Fatalf("Held of the one placement the open registered first = %v, want false: one identity in two places holds neither", got)
	}
	if got := tr.Held(s4); got {
		t.Fatalf("Held of the placement the open registered second = %v, want false: one identity in two places holds neither", got)
	}
}

// TestTrackerIdentityClassOnePlacementTwice: two registrations of the same
// Rec — one placement registered twice — are one placement by identity, so
// the class's weight is the placement's registration count and the segment
// stays held: the class rule counts placements, not registrations.
func TestTrackerIdentityClassOnePlacementTwice(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(27)
	rec := Rec{
		Kind: format.KindDelete, File: id, Seg: f.seg,
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
		Loc: at64(f.seg),
	}
	tr.Register(rec)
	tr.Register(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with one placement registered twice = %v, want true: the class's sole placement holds, by its registration count", got)
	}
	if got := tr.stateOf(f.seg).live.Load(); got != 2 {
		t.Fatalf("the segment's live count for two registrations of one placement = %d, want 2", got)
	}
}

// TestTrackerIdentityClassTwoPlacementsOneSegmentReHoldsIt: two copies of one
// record at two offsets of the SAME segment are two placements of one identity
// — a carry that lands beside its own earlier copy, a unit abandoned between
// the two — but the class spans the SEGMENTS its placements sit in: the class
// two segments wide holds nothing while the original stands, and the
// original's release collapses it onto the one surviving segment and
// re-holds that one by the SUM of its placements' counts. A re-hold keyed by
// placement instead — one that raises nothing at three placements — leaves
// the segment reading not held and it retires with the record's only copies
// in it, undroppable.
func TestTrackerIdentityClassTwoPlacementsOneSegmentReHoldsIt(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(37)
	base := Rec{
		Kind: format.KindTruncate, File: id,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
	}
	orig := base
	orig.Seg, orig.Loc = f.seg, at64(f.seg)
	two := f.takeIn(t)
	cp1 := base
	cp1.Seg, cp1.Loc = two, at64(two)
	cp2 := cp1
	cp2.Loc = segment.Location{Segment: two, Off: 640, Len: 16}

	// Original in one segment and two copies of it at two offsets of
	// another: the class spans two distinct segments and holds neither.
	tr.Register(orig)
	tr.Register(cp1)
	tr.Register(cp2)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the original's segment, its class two segments wide = %v, want false", got)
	}
	if got := tr.Held(two); got {
		t.Fatalf("Held of the copies' segment, its class two segments wide = %v, want false", got)
	}

	// The original's release — the retirement sweep of its segment takes
	// exactly this — collapses the class onto the copies' one segment and
	// re-holds it by both copies' counts, or the copies' segment retires
	// holding the record's only two placements.
	tr.Unregister(orig)
	if got := tr.Held(two); !got {
		t.Fatalf("Held of the copies' segment after the original's release = %v, want true: the class's one surviving segment re-holds, by the sum of its placements' counts", got)
	}
	if got := tr.stateOf(two).live.Load(); got != 2 {
		t.Fatalf("the copies' segment's live count after the re-hold = %d, want 2: one unit per copy of the class's surviving segment", got)
	}
}

// TestTrackerSegmentUnlinkedReHoldsTheSurvivorBeforeTheDurableSignal: the
// retirer's retired hook fires on every completed retirement, durable or
// not, and the ones whose directory sync failed deliver no durable signal
// at all — no SegmentRetired sweep ever runs for them. A class left
// two segments wide by such an unlink would hold nothing while the survivor
// reads not held, and the two hand-over paths that consult no in-flight
// gate — a pass retiring its own source, the open's emptying — could retire
// the survivor with the record's only live placements in it. The unlinked
// report re-holds the survivor on the spot, errs held — and the durable
// sweep that later runs for a successful sync finds the class already
// collapsed and neither the registrations nor the re-hold doubled.
func TestTrackerSegmentUnlinkedReHoldsTheSurvivorBeforeTheDurableSignal(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(47)
	base := Rec{
		Kind: format.KindTruncate, File: id,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
	}
	src, dst := f.seg, f.takeIn(t)
	// A lower-pair write of the file inside what the truncate covers, in a
	// third segment: it keeps every placement of the class the test moves
	// undroppable, so nothing the unlinked report or the sweep carries can
	// spend the registrations the test reads.
	p := newPlant(f)
	p.write(t, id, f.takeIn(t), core.Extent{Off: 4096, Len: 4096}, 0, 3, 1)
	p.finish(t)
	if tr.Droppable(base) {
		t.Fatalf("Droppable of the truncate over the planted outranker = true, want false: the test's classes must stay undroppable")
	}
	orig := base
	orig.Seg, orig.Loc = src, at64(src)
	cp := base
	cp.Seg, cp.Loc = dst, at64(dst)
	tr.Register(orig)
	tr.Register(cp)

	// The unlink whose directory sync failed: the retirer's retired hook
	// alone reported it, the durable signal it promised never arriving.
	tr.SegmentUnlinked(src)
	if got := tr.Held(dst); !got {
		t.Fatalf("Held of the survivor after the non-durable unlink of its mate = %v, want true: the class's surviving segment re-held on the report", got)
	}
	// The re-hold names the record's file under the survivor, so a pass's
	// carry walk reaches it there; a raise that kept no file would answer
	// held over a record no walk can enumerate, and the survivor could
	// never drain.
	if _, ok := tr.stateOf(dst).files[id]; !ok {
		t.Fatalf("the re-held record's file was not kept under the survivor after the unlinked report")
	}
	// The registrations stay — the durable signal alone releases them.
	if got, want := tr.recs[id][orig], 1; got != want {
		t.Fatalf("the gone segment's registration after the unlinked report = %d, want %d: the re-hold releases nothing", got, want)
	}

	// The durable signal's unit: the take of the gone segment's own
	// registration. Its class weight went to the survivor at the unlink,
	// so the take reports no weight and lowers none of the gone segment's
	// own count — the guard keeps the counts exact while nothing reads a
	// gone segment's count today.
	goneLive := tr.stateOf(src).live.Load()
	tr.rm.Lock()
	delta, taken := tr.take(orig)
	tr.rm.Unlock()
	if !taken {
		t.Fatalf("take of the gone segment's registration after the unlink = not taken, want taken: the durable signal's sweep spends it")
	}
	if delta != 0 {
		t.Fatalf("the take of a registration whose segment the class no longer spans = delta %d, want 0: its weight was re-attributed to the survivor at the unlink", delta)
	}
	if got := tr.stateOf(src).live.Load(); got != goneLive {
		t.Fatalf("the gone segment's live count after its own take = %d, want %d: the take of an unlinked placement spends no weight the segment carries", got, goneLive)
	}

	// The durable sweep a successful sync would have delivered: the class
	// already collapsed, the sweep releases what the take above spent
	// without raising the survivor a second time and sweeps the placements
	// whole; the survivor's count is the re-hold's alone.
	tr.SegmentRetired(src, []core.FileID{id})
	if got := tr.Held(dst); !got {
		t.Fatalf("Held of the survivor after the durable sweep of its mate = %v, want true", got)
	}
	if got := tr.stateOf(dst).live.Load(); got != 1 {
		t.Fatalf("the survivor's live count after the sweep = %d, want 1: once from the re-hold, the raise never doubled", got)
	}

	// The same two-step over a class whose two placements sit in one
	// segment beside the mate: the unlinked report re-holds that segment
	// by the sum of its placements' counts, and the durable sweep — with
	// the class's span already collapsed — takes the gone side without
	// raising again.
	id2, gone2, rest2 := fid(48), f.takeIn(t), f.takeIn(t)
	p2 := newPlant(f)
	p2.write(t, id2, f.takeIn(t), core.Extent{Off: 4096, Len: 4096}, 0, 3, 1)
	p2.finish(t)
	base2 := base
	base2.File, base2.Seq, base2.Pair = id2, 9, index.Numbers{Version: ver(5), Seq: 9}
	mate2 := base2
	mate2.Seg, mate2.Loc = gone2, at64(gone2)
	r1 := base2
	r1.Seg, r1.Loc = rest2, at64(rest2)
	r2 := r1
	r2.Loc = segment.Location{Segment: rest2, Off: 640, Len: 16}
	tr.Register(mate2)
	tr.Register(r1)
	tr.Register(r2)
	tr.SegmentUnlinked(gone2)
	if got := tr.stateOf(rest2).live.Load(); got != 2 {
		t.Fatalf("the two-placement segment's live count after the unlinked report = %d, want 2: the re-hold raises it by both placements' counts", got)
	}
	tr.SegmentRetired(gone2, []core.FileID{id2})
	if got := tr.stateOf(rest2).live.Load(); got != 2 {
		t.Fatalf("the two-placement segment's live count after the durable sweep = %d, want 2: the raise never doubled", got)
	}
}

// TestTrackerIdentityClassReTestSpendsDroppableBoth: a re-test over a
// droppable identity spends each registration the registry holds, the
// original's and the copy's alike — the copy's re-hold while the original
// spent is raised and then spent with it — so neither segment holds and the
// registry entries are gone.
func TestTrackerIdentityClassReTestSpendsDroppableBoth(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(37)
	// A droppable identity: a release over an extent the file's one planted
	// note — at a disjoint spot, in a third segment — never reaches.
	p := newPlant(f)
	p.write(t, id, f.takeIn(t), core.Extent{Off: 0, Len: 4096}, 0, 3, 1)
	p.finish(t)
	s1, s2 := f.seg, f.takeIn(t)
	base := Rec{
		Kind: format.KindRelease, File: id,
		Ext:  core.Extent{Off: 1 << 30, Len: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6, Tag: tag,
	}
	orig := base
	orig.Seg, orig.Loc = s1, at64(s1)
	cp := base
	cp.Seg, cp.Loc = s2, at64(s2)
	tr.Register(orig)
	tr.Register(cp)
	if got := tr.Droppable(orig); !got {
		t.Fatalf("Droppable of the release identity before the re-test = %v, want true: nothing it covers sits below its pair anywhere", got)
	}

	tr.Retest(id)
	if _, ok := tr.recs[id][orig]; ok {
		t.Fatalf("the original placement's registration survived the re-test, want it spent: the identity was droppable")
	}
	if _, ok := tr.recs[id][cp]; ok {
		t.Fatalf("the copy placement's registration survived the re-test, want it spent: the identity was droppable")
	}
	if got := tr.Held(s1); got {
		t.Fatalf("Held of the original's segment after the re-test spent both placements = %v, want false", got)
	}
	if got := tr.Held(s2); got {
		t.Fatalf("Held of the copy's segment after the re-test spent both placements = %v, want false", got)
	}
	if got := len(fired); got != 2 {
		t.Fatalf("the candidate fired %d times over the doubling and the re-test, want 2: one crossing per segment the class came to hold", got)
	}
	for _, seg := range fired {
		if seg != s1 && seg != s2 {
			t.Fatalf("the candidate fired for segment %d, want only the two placements' segments %d and %d", seg, s1, s2)
		}
	}
}

// TestTrackerIdentityClassContentCopyIsNewIdentity: the class rule reaches
// only records whose carried copies keep their numbers. A content record's
// copy takes a fresh sequence number, so the copy is a different identity:
// registering it does not end the original's hold, and the copy holds its
// own segment like any record of one placement.
func TestTrackerIdentityClassContentCopyIsNewIdentity(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(47)
	s1, s2 := f.seg, f.takeIn(t)
	orig := Rec{
		Kind: format.KindWrite, File: id, Seg: s1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(3), Seq: 6}, Seq: 6, Tag: tag,
		Loc: at64(s1),
	}
	cp := orig
	cp.Pair.Seq, cp.Seq = 100, 100 // a repack copy's fresh number
	cp.Seg, cp.Loc = s2, at64(s2)
	tr.Register(orig)
	if got := tr.Held(s1); !got {
		t.Fatalf("Held of the content record's original before its copy = %v, want true", got)
	}
	tr.Register(cp)
	if got := tr.Held(s1); !got {
		t.Fatalf("Held of the content record's original once its numbered copy registered = %v, want true: a fresh number is a different identity, the class rule does not reach it", got)
	}
	if got := tr.Held(s2); !got {
		t.Fatalf("Held of the content copy's own segment = %v, want true: its identity's sole placement holds", got)
	}
}

// TestRegisterUnregisterAllocations checks that Register and Unregister on
// a segment whose state already exists, of a file the segment already
// knows, allocate nothing — the steady update, which is the common shape.
// The scope is that pair of calls alone: a segment's or a file's first
// record may allocate its state and its table entries, and the re-test
// events — Retest, MarkerSettled, SegmentRetired, OpenDone —
// allocate by design, the release slice and the per-walk dedupe map.
// Measured outside -race, which changes allocation counts.
func TestRegisterUnregisterAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	f := newFixture(t)
	tr := New(f.ix, nil)
	rec := Rec{
		Kind: format.KindTruncate,
		File: fid(7),
		Seg:  f.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(4), Seq: 9},
		Seq:  9,
	}

	// Warm the segment's state and its files table so the measured runs
	// find both in place.
	tr.Register(rec)
	tr.Unregister(rec)

	rg := testing.AllocsPerRun(1000, func() { tr.Register(rec) })
	// Enough live records that every measured release decrements one.
	for i := 0; i < 2000; i++ {
		tr.Register(rec)
	}
	un := testing.AllocsPerRun(1000, func() { tr.Unregister(rec) })
	if rg != 0 || un != 0 {
		t.Fatalf("allocations per Register = %v, per Unregister = %v, want 0 and 0", rg, un)
	}
}

// TestHeld_ConcurrentRelease: many goroutines register and release the same
// segment's records, and the candidate fires exactly once for the one
// held-to-not-held transition, whichever goroutine's release crosses the
// line. The second case runs in two ordered steps, so nothing in it depends
// on how the releases interleave with the index: first the same release
// storm over a segment whose bytes hold it — no release crosses the line the
// bytes keep, and the candidate fires nothing while they last — then, with
// the releases joined, one removal leaves the records the only thing held,
// and a single register-and-release pair on the main goroutine is the one
// transition the candidate answers.
func TestHeld_ConcurrentRelease(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	// Case 1: records alone. Every goroutine registers its records, waits
	// on a barrier so all are live, then releases them.
	f := newFixture(t)
	var mu sync.Mutex
	fired := make([]core.SegmentID, 0, 2)
	tr := New(f.ix, func(seg core.SegmentID) {
		mu.Lock()
		fired = append(fired, seg)
		mu.Unlock()
	})
	var ready, done sync.WaitGroup
	ready.Add(goroutines)
	done.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer done.Done()
			rec := Rec{
				Kind: format.KindTruncate,
				File: fid(byte(g + 1)),
				Seg:  f.seg,
				Ext:  core.Extent{Off: int64(g) * 4096, Len: 4096},
				Pair: index.Numbers{Version: ver(uint64(g) + 1), Seq: core.Seq(g)},
				Seq:  core.Seq(g),
			}
			for i := 0; i < perGoroutine; i++ {
				tr.Register(rec)
			}
			ready.Done()
			ready.Wait()
			for i := 0; i < perGoroutine; i++ {
				tr.Unregister(rec)
			}
		}(g)
	}
	done.Wait()
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after every record's release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: one transition however many releases raced", fired, f.seg)
	}

	// Case 2a: bytes beside the records. The same release storm, joined
	// before anything else moves: the bytes hold the segment through every
	// one of the 400 releases, so no release crosses the line and the
	// candidate fires nothing, and the segment still answers held.
	f2 := newFixture(t)
	mu.Lock()
	fired = fired[:0]
	mu.Unlock()
	tr2 := New(f2.ix, func(seg core.SegmentID) {
		mu.Lock()
		fired = append(fired, seg)
		mu.Unlock()
	})
	owner := fid(90)
	f2.write(t, owner, 0, 4096, 0, 50, 50)
	var ready2, done2 sync.WaitGroup
	ready2.Add(goroutines)
	done2.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer done2.Done()
			rec := Rec{
				Kind: format.KindTruncate,
				File: fid(byte(g + 1)),
				Seg:  f2.seg,
				Ext:  core.Extent{Off: int64(g) * 4096, Len: 4096},
				Pair: index.Numbers{Version: ver(uint64(g) + 1), Seq: core.Seq(g)},
				Seq:  core.Seq(g),
			}
			for i := 0; i < perGoroutine; i++ {
				tr2.Register(rec)
			}
			ready2.Done()
			ready2.Wait()
			for i := 0; i < perGoroutine; i++ {
				tr2.Unregister(rec)
			}
		}(g)
	}
	done2.Wait()
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the bytes held the segment, want 0: no release crosses the line it holds", got)
	}
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held after the releases, bytes still backed = %v, want true", got)
	}

	// Case 2b: the bytes leave, then one release on the main goroutine —
	// after the removal, the single register-and-release pair is the
	// tracker's own transition, and the candidate fires exactly once.
	f2.remove(t, owner, 0, 4096, 60)
	last := Rec{
		Kind: format.KindTruncate,
		File: owner,
		Seg:  f2.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(60), Seq: 60},
		Seq:  60,
	}
	tr2.Register(last)
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held with one live record after the removal = %v, want true", got)
	}
	tr2.Unregister(last)
	if got := tr2.Held(f2.seg); got {
		t.Fatalf("Held after the removal and the last release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f2.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the release after the bytes left is the one transition", fired, f2.seg)
	}
}
