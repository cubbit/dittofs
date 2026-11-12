package reclaim

import (
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// plant builds one file's on-disk state through the recovery rebuild, the
// way the journal's open plants it: the records are fed in any order and
// Finish applies them with their marker and drop-range state built whole,
// so the planted state is the state recovery would rebuild from the same
// disk. Tags claim the files for the seed's share; segments are handed in
// by the caller, one per placement the planted records sit at.
type plant struct {
	ix  *index.Index
	ctr *index.Counters
	b   *index.Rebuilder
}

// newPlant makes a builder over the fixture's index.
func newPlant(f *fixture) *plant {
	p := &plant{ix: f.ix}
	p.ctr = &index.Counters{}
	p.b = f.ix.Rebuilder(p.ctr)
	return p
}

// write feeds one write record of id in seg over e, at record offset at and
// pair (v, s).
func (p *plant) write(t *testing.T, id core.FileID, seg core.SegmentID, e core.Extent, at uint32, v, s uint64) {
	t.Helper()
	err := p.b.Feed(index.Record{
		Kind: index.RecWrite, File: id, Tag: tag, Segment: seg, At: at,
		Off: e.Off, Len: e.Len, Numbers: index.Numbers{Version: ver(v), Seq: core.Seq(s)},
	})
	if err != nil {
		t.Fatalf("feed write: %v", err)
	}
}

// release feeds one release record of id in seg over e, at record offset at
// and pair (v, s).
func (p *plant) release(t *testing.T, id core.FileID, seg core.SegmentID, e core.Extent, at uint32, v, s uint64) {
	t.Helper()
	err := p.b.Feed(index.Record{
		Kind: index.RecRelease, File: id, Tag: tag, Segment: seg, At: at,
		Off: e.Off, Len: e.Len, Numbers: index.Numbers{Version: ver(v), Seq: core.Seq(s)},
	})
	if err != nil {
		t.Fatalf("feed release: %v", err)
	}
}

// truncate feeds one truncate record of id in seg, truncating to size off,
// at record offset at and pair (v, s).
func (p *plant) truncate(t *testing.T, id core.FileID, seg core.SegmentID, off int64, at uint32, v, s uint64) {
	t.Helper()
	err := p.b.Feed(index.Record{
		Kind: index.RecTruncate, File: id, Tag: tag, Segment: seg, At: at,
		Off: off, Len: 0, Numbers: index.Numbers{Version: ver(v), Seq: core.Seq(s)},
	})
	if err != nil {
		t.Fatalf("feed truncate: %v", err)
	}
}

// retain feeds one retained piece the rebuild's caller computed: content of
// id in seg over e, superseded at version By, with the piece's own pair
// (v, s).
func (p *plant) retain(t *testing.T, id core.FileID, seg core.SegmentID, e core.Extent, at uint32, v, s uint64, by uint64) {
	t.Helper()
	err := p.b.Retain(id, index.Retained{
		Piece: index.Piece{
			Extent:  e,
			Record:  index.RecordAt{Segment: seg, Offset: at},
			Version: ver(v),
			Seq:     core.Seq(s),
		},
		By: ver(by),
	})
	if err != nil {
		t.Fatalf("feed retained: %v", err)
	}
}

// finish applies everything fed.
func (p *plant) finish(t *testing.T) {
	t.Helper()
	if err := p.b.Finish(core.Version{}); err != nil {
		t.Fatalf("finish the rebuild: %v", err)
	}
}

// TestDroppable_SettledTruncate: a truncate record whose marker Settle has
// covered and whose file holds no record it can still outrank answers
// droppable, and the re-test the settle carries drops its registration, so
// a segment holding nothing but that truncation answers not held; the same
// truncate beside a record of its file in another segment that overlaps
// what it covers at a lower pair answers live until that segment retires.
func TestDroppable_SettledTruncate(t *testing.T) {
	// Held in another segment, live: a record of the file over [0, 4KiB) at
	// a pair below the truncate's, overlapping what the truncate covers —
	// [8KiB, end) — is nowhere it reaches, so the truncate answers
	// droppable once settled.
	f := newFixture(t)
	id := fid(3)
	p := newPlant(f)
	other := f.seg + 1
	p.write(t, id, other, core.Extent{Off: 0, Len: 4096}, 0, 3, 1)
	p.truncate(t, id, f.seg, 8192, 0, 5, 6)
	p.finish(t)
	tr := New(f.ix, nil)

	rec := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 8192},
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6,
	}
	if got := tr.Droppable(rec); got {
		t.Fatalf("Droppable of a truncate whose marker is unsettled = %v, want false: its marker is rebuilt from it", got)
	}

	tr.Register(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with the truncate registered live = %v, want true", got)
	}

	// Settle above the truncate's version: its marker leaves, it outranks
	// nothing — the other segment's record sits over [0, 4KiB) below its
	// size — and the re-test the settle carries drops it.
	tr.MarkerSettled(id, ver(9))
	if got := tr.Droppable(rec); !got {
		t.Fatalf("Droppable of the settled truncate = %v, want true: nothing of its file overlaps what it covers at a lower pair", got)
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the settle's re-test dropped the truncate = %v, want false", got)
	}

	// The same shape, but the other segment's record lies over the
	// truncate's size, inside what it covers at the lower pair: the
	// truncate stays live until that record's segment is retired.
	f2 := newFixture(t)
	id2 := fid(4)
	p2 := newPlant(f2)
	over := f2.seg + 1
	p2.write(t, id2, over, core.Extent{Off: 8192, Len: 4096}, 0, 3, 1)
	p2.truncate(t, id2, f2.seg, 8192, 0, 5, 6)
	p2.finish(t)
	tr2 := New(f2.ix, nil)
	rec2 := rec
	rec2.File = id2
	rec2.Seg = f2.seg
	tr2.Register(rec2)
	tr2.MarkerSettled(id2, ver(9))
	if got := tr2.Droppable(rec2); got {
		t.Fatalf("Droppable beside a lower-pair record inside what the truncate covers = %v, want false: it may still outrank the record", got)
	}
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held while the truncate is kept = %v, want true", got)
	}

	// Retire the record's segment: the ranges of the retired segment answer
	// nothing any more, so the re-test the retirement carries drops the
	// truncate.
	tr2.SegmentRetired(over, []core.FileID{id2})
	if got := tr2.Droppable(rec2); !got {
		t.Fatalf("Droppable after the lower record's segment retired = %v, want true: nothing it can outrank remains", got)
	}
	if got := tr2.Held(f2.seg); got {
		t.Fatalf("Held after the retirement re-tested the truncate = %v, want false", got)
	}
}

// TestDroppable_TruncateUnsettled: a truncate whose marker waits for a
// settle — its file holds another marker below the truncate's version — is
// never droppable, whatever it outranks, and drops only at the settle that
// covers its own marker.
func TestDroppable_TruncateUnsettled(t *testing.T) {
	f := newFixture(t)
	id := fid(11)
	p := newPlant(f)
	p.truncate(t, id, f.seg, 0, 0, 4, 5)
	p.truncate(t, id, f.seg, 0, 4096, 7, 8)
	p.finish(t)
	tr := New(f.ix, nil)

	low := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	high := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(7), Seq: 8}, Seq: 8,
	}
	tr.Register(low)
	tr.Register(high)
	if got := tr.Droppable(high); got {
		t.Fatalf("Droppable of a truncate whose marker is unsettled = %v, want false", got)
	}

	// Settle below the second truncate's version: the first marker goes —
	// its record's answer, nothing unsettled at or below its version and
	// nothing it covers in another segment, crosses to droppable — so the
	// settle's re-test drops the first record's registration. The second's
	// own marker stays: a removal that arrived after the last settle keeps
	// its answer false and its registration live.
	tr.MarkerSettled(id, ver(4))
	if got := tr.Droppable(high); got {
		t.Fatalf("Droppable after a settle below the truncate's own marker = %v, want false", got)
	}
	if got := tr.Droppable(low); !got {
		t.Fatalf("Droppable of the first truncate after a settle at its own version = %v, want true: nothing unsettled and nothing to outrank remains", got)
	}
	if _, ok := tr.recs[id][low]; ok {
		t.Fatalf("the first truncate is still registered live, want its registration released by the settle's re-test")
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the first truncate's registration dropped, with the second's marker still keeping it = %v, want true", got)
	}
	if _, ok := tr.recs[id][high]; !ok {
		t.Fatalf("the second truncate is not registered live, want its unsettled marker to keep it")
	}
}

// TestDroppable_PerExtentPerFile: a removal of id over [1GiB, 2GiB) answers
// droppable beside an older record of the same file over [0, 1MiB) — the
// drop query stays inside the removal's extent — and beside an older record
// of another file even over the same extent; the same removal over the
// file's own older record's extent is kept.
func TestDroppable_PerExtentPerFile(t *testing.T) {
	f := newFixture(t)
	a, b, id := fid(1), fid(2), fid(3)
	p := newPlant(f)
	// A has an older record over [0, 1MiB), the removal's file its own at
	// the same spot; B an older release record over the removal's own
	// extent — releases carry no payload, so a [1GiB, 2GiB) one fits a
	// segment the way a write of those bytes could not.
	p.release(t, a, f.seg, core.Extent{Off: 0, Len: 1 << 20}, 0, 3, 1)
	p.release(t, id, f.seg, core.Extent{Off: 0, Len: 1 << 20}, 4096, 3, 2)
	p.release(t, b, f.seg, core.Extent{Off: 1 << 30, Len: 1 << 30}, 8192, 3, 3)
	p.finish(t)
	tr := New(f.ix, nil)

	// A removal (a dealloc's shape carries a real extent) of id over
	// [1GiB, 2GiB): the assertion that answers per extent needs the marker
	// term settled too, so an index built with a settled settle is run here
	// first — the removal's marker is unplanted, but MarkerSettled names
	// every marker at or below the version it is given, and there is none.
	tr.MarkerSettled(id, ver(9))
	removal := Rec{
		Kind: format.KindDeallocate, File: id, Seg: f.seg + 1,
		Ext:  core.Extent{Off: 1 << 30, Len: 1 << 30},
		Pair: index.Numbers{Version: ver(5), Seq: 9}, Seq: 9,
	}
	if got := tr.Droppable(removal); !got {
		t.Fatalf("Droppable over [1GiB,2GiB) beside the file's older [0,1MiB) record = %v, want true: the query stays per extent", got)
	}

	// The same removal over [0, 1MiB), where the file's own older record
	// sits: kept.
	overlap := removal
	overlap.Ext = core.Extent{Off: 0, Len: 1 << 20}
	if got := tr.Droppable(overlap); got {
		t.Fatalf("Droppable over the file's own older record's extent = %v, want false: the record may still be outranked", got)
	}
}

// TestDroppable_PlantedSection72: the planted case, a retained copy above
// the truncate's sequence number but below its version: the pair comparison
// keeps the truncate live (the truncated content would come back as held
// after a crash otherwise), where a sequence-only comparison would drop it —
// the copy's sequence outranks the truncate's, so the false assertion holds
// only while the comparison reads the version first.
func TestDroppable_PlantedSection72(t *testing.T) {
	f := newFixture(t)
	id := fid(7)
	p := newPlant(f)
	// A version-3 record over [4KiB, 8KiB) is superseded and removed by the
	// version-7 truncate; a repack copy of the retained content lives in
	// another segment keeping the old version under the new sequence
	// number 100, above the truncate's 6. The copy's record is fed beside
	// the retained piece, exactly as recovery finds both on disk: the copy
	// notes its range, and the truncate's answer must keep it.
	p.write(t, id, f.seg, core.Extent{Off: 4096, Len: 4096}, 0, 3, 3)
	p.truncate(t, id, f.seg, 4096, 4096, 7, 6)
	p.write(t, id, f.seg+1, core.Extent{Off: 4096, Len: 4096}, 0, 3, 100)
	p.retain(t, id, f.seg+1, core.Extent{Off: 4096, Len: 4096}, 0, 3, 100, 7)
	p.finish(t)
	tr := New(f.ix, nil)
	tr.MarkerSettled(id, ver(9))

	trunc := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(7), Seq: 6}, Seq: 6,
	}
	if got := tr.Droppable(trunc); got {
		t.Fatalf("Droppable over a copy of higher sequence but lower version = %v, want false: the pair comparison keeps the truncate over the copy", got)
	}

	// The copy's segment retiring ends it: nothing the truncate can outrank
	// remains, so it drops — the state a sequence-only comparison would
	// have declared before the retirement, while the copy's bytes were
	// still on disk.
	tr.Register(trunc)
	tr.SegmentRetired(f.seg+1, []core.FileID{id})
	if got := tr.Droppable(trunc); !got {
		t.Fatalf("Droppable after the copy's segment retired = %v, want true: nothing it can outrank remains", got)
	}
}

// TestDroppable_SyncedLoss: a synced loss record lives while its marker is
// unsettled — nothing to outrank beside it, the marker alone keeps it — and
// while a record it can still outrank remains after the settle; a superseded
// synced loss — dropped because metadata holds newer content under it —
// leaves no marker and answers by outranking alone, with no settle run.
func TestDroppable_SyncedLoss(t *testing.T) {
	// Case 1, the marker alone: a synced loss of a file whose every record
	// sits in the loss's own segment — nothing to outrank anywhere — is
	// live while its marker is unsettled and drops at the settle that
	// covers it. Deleting the MarkerSettled term turns the first assertion
	// green and the second red, so the term is what the case holds on.
	f := newFixture(t)
	id := fid(19)
	p := newPlant(f)
	p.write(t, id, f.seg, core.Extent{Off: 0, Len: 4096}, 0, 3, 2)
	p.finish(t)
	// The loss's own marker over its extent, planted the way the journal
	// leaves one: a synced loss the index applied.
	if _, _, err := f.ix.Lose(id, core.Extent{Off: 0, Len: 4096}, ver(6)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	tr := New(f.ix, nil)
	sole := Rec{
		Kind: format.KindLoss, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(6), Seq: 9}, Seq: 9, Synced: true,
	}
	if got := tr.Droppable(sole); got {
		t.Fatalf("Droppable of a synced loss whose marker is unsettled, nothing to outrank = %v, want false", got)
	}
	tr.MarkerSettled(id, ver(6))
	if got := tr.Droppable(sole); !got {
		t.Fatalf("Droppable of the same settled synced loss, its own segment holding its records = %v, want true: the marker was its only reason to live", got)
	}

	// Case 2, the outranking term beside the settled marker: a lower-pair
	// record of the file in another segment keeps the loss live after the
	// settle, and retiring that segment is the last thing the answer waits
	// on.
	f2 := newFixture(t)
	id2 := fid(20)
	p2 := newPlant(f2)
	p2.write(t, id2, f2.seg, core.Extent{Off: 0, Len: 4096}, 0, 3, 2)
	p2.finish(t)
	if _, _, err := f2.ix.Lose(id2, core.Extent{Off: 0, Len: 4096}, ver(6)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	tr2 := New(f2.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id2, Seg: f2.seg + 1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(6), Seq: 9}, Seq: 9, Synced: true,
	}

	// The marker is unsettled, so the loss is live whatever the records
	// around it say.
	if got := tr2.Droppable(loss); got {
		t.Fatalf("Droppable of a synced loss whose marker is unsettled = %v, want false", got)
	}
	// Settle at the loss's own version: the marker goes, and the lower-pair
	// record over the loss's extent still keeps it live — this assertion
	// holds on the outranking term alone.
	tr2.MarkerSettled(id2, ver(6))
	if got := tr2.Droppable(loss); got {
		t.Fatalf("Droppable of a settled synced loss over a lower-pair record = %v, want false: the record may still be outranked", got)
	}
	// Retiring the record's segment is the last thing the loss's answer
	// waits on: nothing it can outrank remains, so it drops.
	tr2.SegmentRetired(f2.seg, []core.FileID{id2})
	if got := tr2.Droppable(loss); !got {
		t.Fatalf("Droppable after the last record it could outrank left = %v, want true", got)
	}

	// A superseded loss: the journal dropped the record because newer
	// content replaced it, so it leaves no marker — the outranking rule
	// alone. It also needs no settle: a lower-pair record keeps it, its
	// segment's retirement drops it, and no settle is run in this case.
	f3 := newFixture(t)
	id3 := fid(21)
	p3 := newPlant(f3)
	p3.write(t, id3, f3.seg, core.Extent{Off: 0, Len: 4096}, 0, 3, 2)
	p3.finish(t)
	tr3 := New(f3.ix, nil)
	superseded := Rec{
		Kind: format.KindLoss, File: id3, Seg: f3.seg + 1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(6), Seq: 9}, Seq: 9,
		Synced: true, Superseded: true,
	}
	if got := tr3.Droppable(superseded); got {
		t.Fatalf("Droppable of a superseded loss over a lower-pair record, before any settle = %v, want false: the record may still be outranked, the marker is not its reason to live", got)
	}
	tr3.SegmentRetired(f3.seg, []core.FileID{id3})
	if got := tr3.Droppable(superseded); !got {
		t.Fatalf("Droppable of a superseded loss with nothing to outrank, no settle run = %v, want true", got)
	}
}

// TestDroppable_UnsyncedLoss: an unsynced loss record reaches only the
// record it names, so it is droppable when no live registration of the file
// carries the named sequence number and nothing it can outrank remains:
// live while the named record's copy is registered, live too while a
// lower-pair record overlaps its extent — the guard that bounds a registry
// incomplete — and droppable once the last segment holding the named record
// is retired.
func TestDroppable_UnsyncedLoss(t *testing.T) {
	// Case 1: the registry is empty and nothing the loss can outrank
	// remains on disk, so the named record is gone and the loss drops.
	f := newFixture(t)
	id := fid(30)
	tr := New(f.ix, nil)
	loss := Rec{
		Kind: format.KindLoss, File: id, Seg: f.seg + 1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 9}, Seq: 9, Named: 3,
	}
	if got := tr.Droppable(loss); !got {
		t.Fatalf("Droppable of an unsynced loss, the named record gone and nothing to outrank = %v, want true", got)
	}

	// Case 2: the registry holds the named record — the loss is kept by
	// it; a release that takes the last registration moves the answer with
	// it; and the retirement path reaches the same answer when the copy's
	// segment goes.
	f2 := newFixture(t)
	id2 := fid(31)
	p2 := newPlant(f2)
	// The dropped record's copy over [0, 4KiB) at sequence 3 — the number
	// the loss names — still on disk in another segment.
	p2.write(t, id2, f2.seg, core.Extent{Off: 0, Len: 4096}, 0, 3, 3)
	p2.finish(t)
	tr2 := New(f2.ix, nil)
	loss2 := Rec{
		Kind: format.KindLoss, File: id2, Seg: f2.seg + 1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 9}, Seq: 9, Named: 3,
	}
	lost := Rec{
		Kind: format.KindWrite, File: id2, Seg: f2.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(3), Seq: 3}, Seq: 3,
	}
	tr2.Register(lost)
	if got := tr2.Droppable(loss2); got {
		t.Fatalf("Droppable over the named record's live registration = %v, want false: the record the loss names is still held", got)
	}
	// A release that takes the last registration of the named record is
	// unsynced-loss coverage no event needs.
	tr2.Unregister(lost)
	if got := tr2.Droppable(loss2); got {
		t.Fatalf("Droppable after the named record's release, nothing to outrank left = %v, want false: a lower-pair write still overlaps the loss's extent in another segment, so the guard keeps it", got)
	}
	// The guard's own end: retire the overlapping record's segment and the
	// loss drops at last.
	tr2.SegmentRetired(f2.seg, []core.FileID{id2})
	if got := tr2.Droppable(loss2); !got {
		t.Fatalf("Droppable after the last segment holding the named record and its overlapping record retired = %v, want true", got)
	}

	// Case 3: the named record was never registered — a registry gap — but
	// a lower-pair record of the file overlaps the loss's extent in another
	// segment: the loss stays live anyway, which is the guard the
	// incomplete-registry decision names.
	f3 := newFixture(t)
	id3 := fid(32)
	p3 := newPlant(f3)
	p3.write(t, id3, f3.seg, core.Extent{Off: 0, Len: 4096}, 0, 2, 2)
	p3.finish(t)
	tr3 := New(f3.ix, nil)
	loss3 := Rec{
		Kind: format.KindLoss, File: id3, Seg: f3.seg + 1,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(5), Seq: 9}, Seq: 9, Named: 3,
	}
	if got := tr3.Droppable(loss3); got {
		t.Fatalf("Droppable of an unsynced loss whose named record is unregistered but a lower-pair record overlaps = %v, want false: the outranking guard bounds a registry gap", got)
	}
}

// TestSegmentRetired_StateGone: retirement removes the segment's state, so
// a scan of the tracker's table finds no entry for it, Held of the retired
// segment answers from the index's counters alone, and the candidate shows
// nothing for it any more.
func TestSegmentRetired_StateGone(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(40)
	p := newPlant(f)
	p.truncate(t, id, f.seg, 0, 0, 4, 5)
	p.finish(t)

	rec := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	tr.Register(rec)
	tr.Register(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with two registered records = %v, want true", got)
	}

	tr.SegmentRetired(f.seg, nil)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the retired segment = %v, want false: the state left with the segment", got)
	}
	if st := tr.stateOf(f.seg); st != nil {
		t.Fatalf("the retired segment's state is %v, want nil: retirement removed it", st)
	}
	// A file of the retired segment is still nameable from the caller's
	// list: the re-test ran with it and dropped nothing — the truncate's
	// marker is still unsettled — so the registry of the file survives the
	// segment's retirement, ready for the records other segments hold.
	if got := len(fired); got != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %d times with %v, want one firing with %d: the retirement ended the segment's liveness", got, fired, f.seg)
	}
}

// TestRegister_CountsLiveUnconditionally: a record registered beside a
// droppable answer still counts its segment held until an event or a
// retirement drops it — no register is its own re-test.
func TestRegister_CountsLiveUnconditionally(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	id := fid(50)
	p := newPlant(f)
	p.write(t, id, f.seg, core.Extent{Off: 0, Len: 1 << 20}, 0, 3, 2)
	p.finish(t)

	// One release, droppable the moment it is registered: the file holds
	// no other record it can outrank. It counts live anyway.
	rec := Rec{
		Kind: format.KindRelease, File: id, Seg: f.seg + 1,
		Ext:  core.Extent{Off: 1 << 30, Len: 4096},
		Pair: index.Numbers{Version: ver(6), Seq: 9}, Seq: 9,
	}
	if got := tr.Droppable(rec); !got {
		t.Fatalf("Droppable of the release before its registration = %v, want true: the file's only record is disjoint from it", got)
	}
	tr.Register(rec)
	if got := tr.Held(f.seg + 1); !got {
		t.Fatalf("Held after registering a droppable record = %v, want true: registers do not re-test", got)
	}
	// One event of its file drops it.
	tr.Retest(id)
	if got := tr.Held(f.seg + 1); got {
		t.Fatalf("Held after the file's re-test dropped the record = %v, want false", got)
	}
}

// TestRetest_DroppedRecordReleasedOnce: a record a re-test drops answers a
// later paired Unregister with nothing — the registry, not the caller's
// bookkeeping, is the authority a release answers by — so a second release
// cannot take another record's count. Two records of one segment; a
// re-test drops the settled one; the caller's own release of it arrives
// after; the segment stays held by the record still live, the candidate
// fires only for the second record's own release, and no count of the live
// record went missing.
func TestRetest_DroppedRecordReleasedOnce(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(60)
	p := newPlant(f)
	// A settled truncate — nothing unsettled below its version — beside
	// one whose marker waits, both in a file whose only other record sits
	// nowhere near what either covers.
	p.truncate(t, id, f.seg, 0, 0, 4, 5)
	p.truncate(t, id, f.seg, 0, 0, 8, 9)
	p.finish(t)

	settled := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	waiting := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(8), Seq: 9}, Seq: 9,
	}
	tr.Register(settled)
	tr.Register(waiting)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with both records live = %v, want true", got)
	}

	// The settle's re-test drops the settled truncate only.
	tr.MarkerSettled(id, ver(4))
	if _, ok := tr.recs[id][settled]; ok {
		t.Fatalf("the settled truncate is still registered, want the settle's re-test to have dropped it")
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the settled record dropped, the other still live = %v, want true", got)
	}

	// The caller's paired release of the record the re-test already took:
	// it answers nothing, counts nothing, fires nothing.
	tr.Unregister(settled)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after a second release of the dropped record = %v, want true: the live record's count is not the dropped one's to take", got)
	}
	if _, ok := tr.recs[id][waiting]; !ok {
		t.Fatalf("the waiting truncate's registration was taken by the dropped record's second release, want it live")
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the segment held a live record, want 0", got)
	}

	// The live record's own release is the transition: held ends, the
	// candidate fires once.
	tr.MarkerSettled(id, ver(8))
	if _, ok := tr.recs[id][waiting]; ok {
		t.Fatalf("the waiting truncate is still registered after its settle, want the re-test to have dropped it")
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after both records left = %v, want false", got)
	}
	if got := len(fired); got != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %d times with %v, want one firing with %d", got, fired, f.seg)
	}
}

// TestRetest_ConcurrentReleaseOnce: two re-tests of one file racing each
// other and a paired release racing them both. The re-tests — a settle's
// and a copies-done — ask the same question over the same registry while
// the settle's marker is covered, and the caller's paired release of the
// record they can both drop arrives whenever it arrives: the registry
// hands the record's one registration to exactly one taker, so the count
// leaves once, and the record registered beside it — one the drop test
// keeps live — keeps the segment held and its own count intact. The
// candidate stays quiet through the whole race, because the segment never
// stops being held.
func TestRetest_ConcurrentReleaseOnce(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) {
		mu.Lock()
		fired = append(fired, seg)
		mu.Unlock()
	})
	id := fid(70)
	p := newPlant(f)
	// Two truncates of the file, planted unsettled: the first's marker is
	// settled straight through the index on the main goroutine — no re-test
	// runs with it — so its record is still registered when the racers
	// start, and the second's marker never settles, so its record is one the
	// drop test keeps live whatever the re-test asks. The race is over the
	// registry and the counts alone: two collectors and the caller's paired
	// release, all reaching for one registration.
	p.truncate(t, id, f.seg, 0, 0, 4, 5)
	p.truncate(t, id, f.seg, 0, 4096, 8, 9)
	p.finish(t)

	settled := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	kept := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(8), Seq: 9}, Seq: 9,
	}
	tr.Register(settled)
	tr.Register(kept)
	// The settle runs through the index alone — the tracker's own
	// MarkerSettled event would run its re-test here, before the racers
	// start, and there would be nothing left to race over.
	f.ix.Settle(id, ver(4))

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		tr.Retest(id)
	}()
	go func() {
		defer wg.Done()
		<-start
		tr.Retest(id)
	}()
	go func() {
		defer wg.Done()
		<-start
		// The caller's paired release of the settled record, racing the
		// re-tests that may drop it first: whoever arrives second — release
		// or re-test — must find the registration already gone and release
		// nothing.
		tr.Unregister(settled)
	}()
	close(start)
	wg.Wait()

	// The settled truncate's one registration was taken once, by whichever
	// re-test reached it first — the other found nothing to take.
	if _, ok := tr.recs[id][settled]; ok {
		t.Fatalf("the settled truncate is still registered after two racing re-tests, want its one registration taken")
	}
	// The kept record is untouched: its registration and its count both
	// live, and the segment they hold shows held with the candidate quiet.
	if _, ok := tr.recs[id][kept]; !ok {
		t.Fatalf("the kept truncate is not registered, want the racing re-tests to leave it alone")
	}
	if got := tr.recs[id][kept]; got != 1 {
		t.Fatalf("the kept truncate's registration count = %d, want 1: a racing re-test took a count that is not the dropped record's", got)
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the racing re-tests, the kept record still live = %v, want true", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the kept record held the segment, want 0", got)
	}
}

// TestRetest_DuplicatedFileListReleasedOnce: a file named twice by one
// event's list — a copies-done with the file in it twice, or a caller's
// retirement list with a duplicate — collects a droppable record twice but
// must release it once: the registry hands each registration to one taker,
// so a count-1 record's second collection takes nothing and no count of the
// record beside it is spent. The segment stays held, the candidate stays
// quiet.
func TestRetest_DuplicatedFileListReleasedOnce(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(80)
	p := newPlant(f)
	// A truncate that settles and drops, beside one whose marker never
	// settles, both in the one segment.
	p.truncate(t, id, f.seg, 0, 0, 4, 5)
	p.truncate(t, id, f.seg, 0, 4096, 8, 9)
	p.finish(t)
	tr.MarkerSettled(id, ver(4)) // the marker leaves; the record is still registered

	settled := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	kept := Rec{
		Kind: format.KindTruncate, File: id, Seg: f.seg,
		Ext:  core.Extent{Off: 4096},
		Pair: index.Numbers{Version: ver(8), Seq: 9}, Seq: 9,
	}
	tr.Register(settled)
	tr.Register(kept)

	// The same file, twice in one event's list.
	tr.Retest(id, id)
	if _, ok := tr.recs[id][settled]; ok {
		t.Fatalf("the settled truncate is still registered, want the duplicated-list re-test to have dropped it once")
	}
	if _, ok := tr.recs[id][kept]; !ok {
		t.Fatalf("the kept truncate is not registered, want the duplicated-list re-test to leave it alone")
	}
	if got := tr.recs[id][kept]; got != 1 {
		t.Fatalf("the kept truncate's registration count = %d, want 1: the duplicated collection spent a count that is not the dropped record's", got)
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the duplicated-list re-test, the kept record still live = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the kept record held the segment, want 0", got)
	}

	// The same duplicated list reaching retest through SegmentRetired: the
	// settled record re-registered — the single registration its segment's
	// retirement will take — and retired with the file named twice. The
	// retirement's own take loop owns the registry entries here, so the
	// re-test the retirement carries finds nothing of the retired segment
	// left to collect; it re-tests the file once either way.
	tr.Register(settled)
	tr.SegmentRetired(f.seg, []core.FileID{id, id})
	if _, ok := tr.recs[id][settled]; ok {
		t.Fatalf("the re-registered settled truncate is still registered after the retirement, want the retirement to take it")
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the retired segment = %v, want false", got)
	}
	if got := len(fired); got != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %d times with %v, want one firing with %d", got, fired, f.seg)
	}
}

// TestDroppable_DeleteCoversTheWholeFile: a delete record's header names the
// extent 0,0, but a delete covers the whole file, so a settled delete is not
// droppable while a lower-pair write of the file sits in another segment,
// wherever in the file that write lies, and is droppable once nothing of the
// file remains below it.
func TestDroppable_DeleteCoversTheWholeFile(t *testing.T) {
	f := newFixture(t)
	id := fid(8)
	lower, hi := f.seg, f.takeIn(t)
	p := newPlant(f)
	p.write(t, id, lower, core.Extent{Off: 1 << 20, Len: 1 << 20}, 0, 3, 1)
	p.finish(t)
	tr := New(f.ix, nil)

	del := Rec{
		Kind: format.KindDelete, File: id, Seg: hi,
		Pair: index.Numbers{Version: ver(5), Seq: 6}, Seq: 6,
	} // Ext is the zero extent, as a delete's header spells it
	if got := tr.Droppable(del); got {
		t.Fatalf("Droppable of a delete beside a lower-pair write at [1MiB,2MiB) of another segment = %v, want false: a delete covers the whole file, not its header's extent", got)
	}

	tr.SegmentRetired(lower, []core.FileID{id})
	if got := tr.Droppable(del); !got {
		t.Fatalf("Droppable of the delete once the lower-pair write's segment retired = %v, want true", got)
	}
}

// TestDroppable_SupersededLossNeedsNoSettle: a synced loss dropped because
// metadata holds newer content under it left no marker, so it is droppable
// without a settle, while another loss of the file at the same version that
// was not superseded waits for the file's unsettled marker, and the marker's
// own record is unaffected: the re-test releases the superseded loss alone.
func TestDroppable_SupersededLossNeedsNoSettle(t *testing.T) {
	f := newFixture(t)
	id := fid(9)
	lossSeg, truncSeg, plainSeg := f.seg, f.takeIn(t), f.takeIn(t)
	p := newPlant(f)
	p.truncate(t, id, truncSeg, 8192, 0, 4, 5)
	p.finish(t)
	tr := New(f.ix, nil)

	pair := index.Numbers{Version: ver(6), Seq: 7}
	ext := core.Extent{Off: 0, Len: 4096}
	super := Rec{Kind: format.KindLoss, File: id, Seg: lossSeg, Ext: ext, Pair: pair, Seq: 7, Named: 2, Synced: true, Superseded: true}
	plain := super
	plain.Seg, plain.Superseded = plainSeg, false
	trunc := Rec{
		Kind: format.KindTruncate, File: id, Seg: truncSeg,
		Ext:  core.Extent{Off: 8192},
		Pair: index.Numbers{Version: ver(4), Seq: 5}, Seq: 5,
	}
	for _, r := range []Rec{super, plain, trunc} {
		tr.Register(r)
	}

	if got := tr.Droppable(super); !got {
		t.Fatalf("Droppable of a superseded synced loss beside an unsettled marker = %v, want true: it left no marker", got)
	}
	if got := tr.Droppable(plain); got {
		t.Fatalf("Droppable of a synced loss, not superseded, beside an unsettled marker = %v, want false: it waits for the settle", got)
	}
	if got := tr.Droppable(trunc); got {
		t.Fatalf("Droppable of the truncate whose marker is unsettled = %v, want false", got)
	}

	tr.Retest(id)
	if got := tr.Held(lossSeg); got {
		t.Fatalf("Held of the superseded loss's segment after the re-test = %v, want false", got)
	}
	if got := tr.Held(plainSeg); !got {
		t.Fatalf("Held of the plain loss's segment after the re-test = %v, want true: its marker is unsettled", got)
	}
	if got := tr.Held(truncSeg); !got {
		t.Fatalf("Held of the truncate's segment after the re-test = %v, want true: its marker is unsettled", got)
	}
}

// TestSegmentRetired_WithdrawsTheSegmentsBytes: a retirement relays to the
// index, so a segment holding extents of a file the tracker's registry never
// heard of — no record of it was registered, so no per-file SegmentGone can
// name it — answers not held afterwards, and the index counts nothing in it.
func TestSegmentRetired_WithdrawsTheSegmentsBytes(t *testing.T) {
	f := newFixture(t)
	tr := New(f.ix, nil)
	f.write(t, fid(4), 0, 4096, 0, 3, 1)
	if got := f.ix.SegmentBytes(f.seg).Held; got != 4096 {
		t.Fatalf("held bytes of the segment before its retirement = %d, want 4096", got)
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held before the retirement = %v, want true", got)
	}

	tr.SegmentRetired(f.seg, nil)
	if got := f.ix.SegmentBytes(f.seg); got != (index.SegmentBytes{}) {
		t.Fatalf("the index's counters of the retired segment = %+v, want zero: the retirement relays to the index", got)
	}
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the retirement = %v, want false", got)
	}
}
