package reclaim_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
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
		Loc: segment.Location{Segment: recD.Segment, Off: int64(recD.Offset)},
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
	hd, _, err := f.ix.ApplyRemoval(tag, file, core.ChangeDelete, core.Extent{Off: 0, Len: math.MaxInt64}, v, seq, at, nil)
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

// TestCarryDropsDecidedAfterCopies pins, at the journal level, the rule
// that a pass decides its drops once its own copies are appended and
// counted: a settled truncate whose removal covers content the same pass
// carries forward as retained must be carried beside it, because the copy
// the pass just placed is on disk below the truncate's pair and a re-test
// that counts it answers the truncate not droppable. The marker settles
// BEFORE the pass, so the settle's own re-test spends the truncate's
// registration as dead weight — the copies that would keep it do not
// exist yet — and the carry's prologue must re-learn the spent record once
// the pass's copies are noted: a prologue that does not walks the
// registrations alone, finds nothing to carry, and its segment retires
// over the released removal — a crash would then resurrect the copied
// bytes as held where the removal's zeros belong.
//
// The retained content is planted the only way this fixture can reach it:
// nowhere can it take a snapshot hold, which is the journal's channel for
// content that outlives a removal through a seal, so the old write's
// covered half is instead retained by a superseding write that is published
// but never synced — content a removal alone would leave dead and the pass
// still owes. The superseder rides the file's own stream inside the source
// segment, and the source is filled to the brim rather than sealed, since
// sealing syncs a stream and a sync lifts the retention; full is what
// makes the pass's own copy roll onto a reserve segment, so the drop
// ranges the note below counts sit outside the source.
func TestCarryDropsDecidedAfterCopies(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(6)
	// The old write covers [40, 64840): 64800 bytes fill the source so that
	// the truncate still places after it while the pass's own copy — the
	// twenty live bytes of [40, 60) — cannot, and rolls. Payload and fills
	// below are picked from the format's arithmetic: a record costs its
	// aligned 96-byte header besides its payload, and a segment keeps room
	// for the seal marker, one catalog entry per record, and the trailer.
	const bigLen = 64800
	// x is the ten bytes below the truncation size the write holds, plus
	// the ten covered ones the superseder keeps retained: the merged copy
	// the pass places for them.
	x := core.Extent{Off: 40, Len: 20}

	// W (v0), the old write, the content the pass carries forward.
	vW, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the old write's version: %v", err)
	}
	recW, seqW := f.plantAt(t, copyTag, file, core.Extent{Off: 40, Len: bigLen}, make([]byte, bigLen), 0, vW)

	// A superseding write published over W's [50, 60): its record is
	// unsynced, so W's covered bytes there live on as retained content, and
	// the superseder's own extent sits inside the removal below, so its
	// held piece dies with it and the pass copies W alone.
	vX, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the superseding write's version: %v", err)
	}
	f.plantAt(t, copyTag, file, core.Extent{Off: 50, Len: 10}, make([]byte, 10), 0, vX)
	if p, ok := f.ix.Lookup(file, 55); !ok || p.Version != vX {
		t.Fatalf("Lookup at 55 over the superseded ten = %+v %v, want the superseder's piece at %v", p, ok, vX)
	}
	var coveredRetained bool
	for _, rt := range f.ix.Retained(file) {
		if rt.Record == recW && rt.Extent == (core.Extent{Off: 50, Len: 10}) {
			coveredRetained = true
		}
	}
	if !coveredRetained {
		t.Fatalf("Retained of the file names no piece of the old write over [50,60), want the covered ten: the content the pass owes")
	}

	// The truncate at size 50, its record appended in the source segment
	// after the two writes, cutting the superseder's piece and W's held
	// extent above it, and leaving its marker over [50, end). Its marker
	// settles at once, BEFORE the pass: the settle's re-test answers the
	// truncate droppable — every record that could keep it sits in its own
	// segment, which the outranking test never counts — and spends its
	// registration, the settled removal a later pass's own copies must
	// answer for.
	trec, _, seqT, _ := f.plantRemoval(t, copyTag, file, format.KindTruncate, core.ChangeTruncate,
		core.Extent{Off: 50}, core.Extent{Off: 50, Len: math.MaxInt64 - 50}, true)
	if trec.Seg != recW.Segment {
		t.Fatalf("the truncate landed in segment %d, want the source segment %d that holds the old write", trec.Seg, recW.Segment)
	}
	if !f.tracker.Droppable(trec) {
		t.Fatal("Droppable of the settled truncate before the pass exists = false, want true: nothing below its pair sits in another segment, so the settle's re-test spent its registration")
	}

	c := f.copier()
	res, err := c.Copy(context.Background(), recW.Segment)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies (%+v), want the old write's one: its held ten below the size and its retained ten beside them merge", len(res.Copies), res.Copies)
	}
	if cp := res.Copies[0]; cp.Version != vW || cp.Seq <= seqW || cp.Extent != x || cp.Loc.Segment == recW.Segment {
		t.Fatalf("the copy = %+v, want the old write's version %v under a fresh number above %d over %v in a segment beside the source", cp, vW, seqW, x)
	}
	f.waitSpares(t)

	carry, err := c.Carry(context.Background(), recW.Segment, res)
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the settled truncate: the settlement's re-test spent its registration, and the pass's own copy — on disk below the truncate's pair — re-learns it before the walk", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	if cr.Rec.Kind != format.KindTruncate || cr.Rec.File != file || cr.Rec.Seq != seqT || cr.Rec.Pair != trec.Pair {
		t.Fatalf("the carried record = %+v, want the truncate of the file at its own pair %+v (seq %d)", cr.Rec, trec.Pair, seqT)
	}
	if cr.Rec.Seg != cr.To.Segment || cr.To.Segment == recW.Segment {
		t.Fatalf("the carried truncate's Rec %+v and placement %v disagree or name the source segment %d", cr.Rec, cr.To, recW.Segment)
	}

	// The decision the pass made stays made: the copy below the truncate's
	// pair keeps it answering not droppable, and the pass publishes its
	// copies nowhere — the removed extents read missing, the ten below the
	// truncation still held.
	if f.tracker.Droppable(trec) {
		t.Fatal("Droppable of the truncate after the pass counted its own copies = true, want false: the copy below its pair keeps it")
	}
	for _, off := range []int64{55, 60, 1 << 16} {
		if p, ok := f.ix.Lookup(file, off); ok {
			t.Fatalf("Lookup at %d over the truncated extent = %+v, want missing", off, p)
		}
	}
	if p, ok := f.ix.Lookup(file, 45); !ok || p.Version != vW {
		t.Fatalf("Lookup at 45 below the truncation = %+v %v, want the old write's held bytes at %v", p, ok, vW)
	}

	// A reopen over the whole directory reads what the pass actually placed:
	// the truncated extent still reads missing through the rebuilt index —
	// the original removal and its carried copy both cut the old content
	// below the truncate's pair — and the ten retained bytes read held,
	// resolving to the copy the pass appended wherever the numbers place it.
	f.shutdown(t)
	_, ix2, _ := f.reopen(t)
	for _, off := range []int64{55, 60, 1 << 16} {
		if p, ok := ix2.Lookup(file, off); ok {
			t.Fatalf("Lookup at %d over the truncated extent after the reopen = %+v, want missing: the rebuilt index reads the removal the pass carried", off, p)
		}
	}
	if p, ok := ix2.Lookup(file, 45); !ok || p.Version != vW || p.Record != res.Copies[0].To {
		t.Fatalf("Lookup at 45 below the truncation after the reopen = %+v %v, want the pass's copy at %v carrying the old write's version %v", p, ok, res.Copies[0].To, vW)
	}
}

// TestCarryDropsSettledDeleteAfterDurableRetire is the delete-alone-selected
// half of removal-outlives-its-segment: once the segment holding the write a
// settled delete counted on is retired through the real retirer and its
// unlink is durable, the delete answers droppable at the carry's own
// re-test, which releases its registration — nothing is carried for it, and
// a second carry appends nothing. The file reads absent through the index,
// so a rebuild of the deleted write's extents finds no piece to hold.
func TestCarryDropsSettledDeleteAfterDurableRetire(t *testing.T) {
	c := plantCarryLayout(t)

	ret, err := c.r.Retire(c.sa, segment.NotHeld)
	if err != nil {
		t.Fatalf("Retire of the source segment: %v", err)
	}
	if !ret.Durable {
		t.Fatalf("Durable of the retirement after a clean sync = %v, want true", ret.Durable)
	}
	if !c.tr.Droppable(c.del) {
		t.Fatal("Droppable of the settled delete after the unlink of its older records is durable = false, want true: the write it counted on left with the source segment")
	}
	// The durable signal's re-test released the registration the layout made;
	// the pass holds the delete registered the way an open finds its record
	// on disk, which is the state the carry's own decision must answer.
	c.tr.Register(c.del)
	if !c.tr.Held(c.del.Seg) {
		t.Fatal("Held of the delete's segment after its registration = false, want the delete counted live")
	}

	// The delete's segment is sealed the way a pass claims only sealed
	// segments, before the unit runs.
	c.f.sealSource(t, c.file)
	c.f.waitSpares(t)

	cx := c.f.copier()
	res, err := cx.Copy(context.Background(), c.del.Seg)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(res.Copies) != 0 {
		t.Fatalf("Copy appended %d copies (%+v), want none: the settled delete backs no content", len(res.Copies), res.Copies)
	}

	carry, err := cx.Carry(context.Background(), c.del.Seg, res)
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if len(carry.Carried) != 0 {
		t.Fatalf("Carry carried %d records (%+v), want none: the re-test released the settled delete before the walk", len(carry.Carried), carry.Carried)
	}
	if c.tr.Held(c.del.Seg) {
		t.Fatal("Held of the delete's segment after the carry = true, want false: the re-test released its registration")
	}
	again, err := cx.Carry(context.Background(), c.del.Seg, res)
	if err != nil {
		t.Fatalf("the second Carry: %v", err)
	}
	if len(again.Carried) != 0 {
		t.Fatalf("the second Carry carried %d records (%+v), want none: nothing registered stays in the segment", len(again.Carried), again.Carried)
	}
	for off := int64(0); off < int64(len(carryPayload)); off += 7 {
		if p, ok := c.f.ix.Lookup(c.file, off); ok {
			t.Fatalf("Lookup at %d over the deleted write's extent = %+v, want missing", off, p)
		}
	}
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

// TestCarryKeepsBothNumbers carries a settled-below truncate beside the
// content the same unit copied and checks the copy the carry places: a fresh
// record at another segment carrying the source's own sequence number and
// version — the pair is the record's identity, and a copy reissued under a
// fresh number would outrank the fills the truncate once released — its bytes
// decoding back to the same header and body, its registration raising the
// target segment's count, and the truncate's marker still answering from the
// index.
func TestCarryKeepsBothNumbers(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(4)
	payload := []byte("the write the truncate leaves below it")

	recW, _, vW := f.plant(t, copyTag, file, payload, 12)
	size := core.Extent{Off: int64(len(payload))}
	trec, atT, seqT, vT := f.plantRemoval(t, copyTag, file, format.KindTruncate, core.ChangeTruncate,
		size, core.Extent{Off: size.Off, Len: math.MaxInt64 - size.Off}, false)
	if trec.Seg != recW.Segment {
		t.Fatalf("the truncate landed in segment %d, want the source segment %d that holds the write", trec.Seg, recW.Segment)
	}
	// Metadata has recorded every removal of the file below the truncate's
	// pair; its own marker is what keeps the record from answering droppable.
	f.tracker.MarkerSettled(file, vW)
	if f.tracker.Droppable(trec) {
		t.Fatal("Droppable of the truncate = true, want false: its own marker is unsettled at its own version")
	}
	f.sealSource(t, file)
	f.waitSpares(t)

	c := f.copier()
	res, err := c.Copy(context.Background(), recW.Segment)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies, want the one write's", len(res.Copies))
	}
	carry, err := c.Carry(context.Background(), recW.Segment, res)
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the truncate alone", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	// The seq equality is the point: a copy under a fresh number would
	// outrank the records the truncate released.
	if cr.Rec.Seq != seqT {
		t.Fatalf("carried truncate Seq = %d, want the source's own %d: the copy must keep the sequence number it was written under", cr.Rec.Seq, seqT)
	}
	if cr.Rec.Pair != trec.Pair {
		t.Fatalf("carried truncate Pair = %+v, want the source's %+v: both numbers are the record's identity", cr.Rec.Pair, trec.Pair)
	}
	if cr.Rec.Kind != format.KindTruncate || cr.Rec.File != file || cr.Rec.Ext != size || cr.Rec.Tag != copyTag {
		t.Fatalf("carried record = %+v, want the source's kind, file, extent and tag unchanged", cr.Rec)
	}
	if cr.To.Segment == recW.Segment {
		t.Fatalf("the carried truncate landed in its own source segment %d, want another", recW.Segment)
	}
	if cr.Rec.Seg != cr.To.Segment || cr.Rec.Loc.Segment != cr.To.Segment || cr.Rec.Loc.Off != int64(cr.To.Offset) {
		t.Fatalf("carried Rec = %+v names no the placement %v: Seg and Loc must name the copy", cr.Rec, cr.To)
	}

	// Both records decoded back through the seam's own bytes: the copy holds
	// the same header and body the source does.
	hS, bS := f.readBack(t, atT)
	hC, bC := f.readBack(t, cr.To)
	if hS.Kind != hC.Kind || hS.File != hC.File || hS.Extent != hC.Extent ||
		hS.Seq != hC.Seq || hS.Version != hC.Version || hS.Tag != hC.Tag {
		t.Fatalf("the copy's header (%+v) and the source's (%+v) differ on kind, file, extent, seq, version or tag", hC, hS)
	}
	if hC.Seq != seqT {
		t.Fatalf("the copy's record Seq = %d, want the source's own %d", hC.Seq, seqT)
	}
	if bS.MTime != 0 || bC.MTime != 0 || bS.Payload != nil || bC.Payload != nil ||
		bS.Loss != (format.Loss{}) || bC.Loss != (format.Loss{}) {
		t.Fatalf("a truncate's body is empty; got source %+v, copy %+v", bS, bC)
	}

	// The copy's registration raises the target's count: nothing the index
	// holds names the target, so its liveness is the copy's alone.
	if !f.tracker.Held(cr.To.Segment) {
		t.Fatalf("Held of segment %d after the carry = false, want the copy registered live there", cr.To.Segment)
	}
	// The truncate's change still answers from the index.
	var found bool
	for _, ch := range f.ix.Since(file, vW) {
		if ch.Kind == core.ChangeTruncate && ch.Version == vT && ch.Extent.Off == size.Off {
			found = true
		}
	}
	if !found {
		t.Fatalf("Since above the write's version %v yields no truncate's change, want the marker at %v", vW, vT)
	}

	// A reopen over the whole directory — the consumer of what the carry
	// placed, not the live index a carry cannot break: the rebuild reads the
	// original and the copy both off the disk. The truncate reads as ONE
	// record: the copy keeps both numbers the source carried, and the
	// rebuild collapses records that share a pair wherever they sit, so the
	// rebuilt file yields exactly the one truncate change over the extent,
	// never one per copy.
	f.shutdown(t)
	_, ix2, _ := f.reopen(t)
	var truncs int
	for _, ch := range ix2.Since(file, core.Version{}) {
		if ch.Kind == core.ChangeTruncate && ch.Version == vT && ch.Extent.Off == size.Off {
			truncs++
		}
	}
	if truncs != 1 {
		t.Fatalf("Since of the rebuilt file yields %d truncate changes at version %v over [%d,end), want 1: the original and its carried copy share both numbers, so the rebuild reads them as one record", truncs, vT, size.Off)
	}
}

// TestCarryUnsettledTruncateAlways carries a truncate whose marker was never
// settled: never droppable, so always carried, and its marker keeps
// answering from the index after the copy is placed.
func TestCarryUnsettledTruncateAlways(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(5)
	trec, _, seqT, vT := f.plantRemoval(t, copyTag, file, format.KindTruncate, core.ChangeTruncate,
		core.Extent{Off: 8}, core.Extent{Off: 8, Len: math.MaxInt64 - 8}, false)
	if f.tracker.Droppable(trec) {
		t.Fatal("Droppable of the unsettled truncate = true, want false: its marker never settled")
	}
	f.sealSource(t, file)
	f.waitSpares(t)

	carry, err := f.copier().Carry(context.Background(), trec.Seg, reclaim.Result{})
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the unsettled truncate: it is never droppable", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	if cr.Rec.Seq != seqT || cr.Rec.Pair != trec.Pair {
		t.Fatalf("carried truncate Seq %d Pair %+v, want the source's own %d %+v", cr.Rec.Seq, cr.Rec.Pair, seqT, trec.Pair)
	}
	if f.tracker.Droppable(trec) {
		t.Fatal("Droppable of the unsettled truncate after its carry = true, want false: the carry settles nothing")
	}
	var found bool
	for _, ch := range f.ix.Since(file, core.Version{}) {
		if ch.Kind == core.ChangeTruncate && ch.Version == vT {
			found = true
		}
	}
	if !found {
		t.Fatalf("Since from the bottom yields no truncate's change, want the marker at %v still answering", vT)
	}

	// The marker still answers after a reopen too: neither record leaves a
	// settle behind, so the rebuild makes the marker again from whichever
	// record the scan finds — the original and its carried copy, one record
	// at the pair they share — and the rebuilt file yields the unsettled
	// truncate exactly once.
	f.shutdown(t)
	_, ix2, _ := f.reopen(t)
	var truncs int
	for _, ch := range ix2.Since(file, core.Version{}) {
		if ch.Kind == core.ChangeTruncate && ch.Version == vT {
			truncs++
		}
	}
	if truncs != 1 {
		t.Fatalf("Since of the rebuilt file yields %d truncate changes at version %v, want the one marker the unsettled removal still leaves after a reopen", truncs, vT)
	}
}

// TestCarrySkipsForgottenTag carries a segment holding records of two tags
// whose first is being forgotten: the forgotten tag's record is appended
// and registered nowhere, the other tag's record carries.
func TestCarrySkipsForgottenTag(t *testing.T) {
	f := newCopyFixture(t)
	f.forgetting = func(tag core.ShareTag) bool { return tag == copyTag }
	// Two files on the one stream, so their removals share one segment.
	fileA, fileB := copyFile(3), copyFile(5)
	if f.core.Streams().IndexFor(fileA) != f.core.Streams().IndexFor(fileB) {
		t.Fatalf("files 3 and 5 stream to %d and %d, want one shared stream", f.core.Streams().IndexFor(fileA), f.core.Streams().IndexFor(fileB))
	}
	const otherTag = core.ShareTag(2)
	recA, _, _, _ := f.plantRemoval(t, copyTag, fileA, format.KindDelete, core.ChangeDelete,
		core.Extent{}, core.Extent{Len: math.MaxInt64}, false)
	recB, _, seqB, _ := f.plantRemoval(t, otherTag, fileB, format.KindTruncate, core.ChangeTruncate,
		core.Extent{Off: 4}, core.Extent{Off: 4, Len: math.MaxInt64 - 4}, false)
	if recA.Seg != recB.Seg {
		t.Fatalf("the removals landed in segments %d and %d, want one shared source segment", recA.Seg, recB.Seg)
	}
	f.sealSource(t, fileA)
	f.waitSpares(t)

	carry, err := f.copier().Carry(context.Background(), recA.Seg, reclaim.Result{})
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the forgotten tag's file skipped whole and the other tag's truncate alone carried", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	if cr.Rec.File != fileB || cr.Rec.Tag != otherTag || cr.Rec.Seq != seqB {
		t.Fatalf("the carried record = %+v, want the other tag's truncate of fileB at the source's own numbers", cr.Rec)
	}
}

// TestCarryNotedCopyKeepsALaterRemoval pins the note a carried header-only
// record's placement leaves: the copy is never on disk unnoted, so a later
// removal of the same file that outranks the copy's pair stays live on the
// range the note keeps, read through the drop test the note serves. The
// carried record is a loss, whose original left a marker and no range
// anywhere — the loss path's marker holds no record of a segment — so the
// copy's note is the only range below the probe, and the probe's extent
// keeps clear of the claiming write's own range beside it.
func TestCarryNotedCopyKeepsALaterRemoval(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(11)
	// The claim and the segment's content: a small write far below the
	// loss's extent, so its own noted range never overlaps the probe's.
	recW, seqW, _ := f.plant(t, copyTag, file, []byte("the write claiming the file"), 0)
	e := core.Extent{Off: 1 << 10, Len: 512}
	lrec, _, seqL, _ := f.plantLoss(t, copyTag, file, seqW, format.LossStale, e, false)
	if lrec.Seg != recW.Segment {
		t.Fatalf("the loss landed in segment %d, want the write's own %d", lrec.Seg, recW.Segment)
	}
	if f.tracker.Droppable(lrec) {
		t.Fatal("Droppable of the loss before its carry = true, want false: its marker never settled")
	}
	f.sealSource(t, file)
	f.waitSpares(t)

	carry, err := f.copier().Carry(context.Background(), lrec.Seg, reclaim.Result{})
	if err != nil {
		t.Fatalf("Carry of the loss's segment: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the loss record alone", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	if cr.Rec.Kind != format.KindLoss || cr.Rec.File != file || cr.Rec.Seq != seqL {
		t.Fatalf("the carried record = %+v, want the loss of the file at its source's own seq %d", cr.Rec, seqL)
	}

	// The stream's active segment is the one the carried copy made of its
	// reserve spare; sealing it rolls the probe below onto a segment of its
	// own, where its own range cannot hide the note this half asks about.
	f.sealSource(t, file)
	f.waitSpares(t)
	drec, _, _, _ := f.plantRemoval(t, copyTag, file, format.KindDeallocate, core.ChangeDeallocate, e, e, true)
	if drec.Seg == cr.To.Segment {
		t.Fatalf("the deallocate landed in the carried copy's segment %d, want a segment of its own", cr.To.Segment)
	}
	if f.tracker.Droppable(drec) {
		t.Fatal("Droppable of the settled deallocate above the carried copy = true, want false: the carried loss's placement, noted over the copy's extent, is the only range below it")
	}
}

// TestCarryBadRecordAbandonsUnit corrupts a header-only record the carry is
// about to read and checks the unit's answer: an error, nothing appended and
// nothing registered for the carry, the record's registration standing, and
// the content copies the unit's first step appended still on disk.
func TestCarryBadRecordAbandonsUnit(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(8)
	payload := []byte("content the same unit already copied")
	recW, _, _ := f.plant(t, copyTag, file, payload, 3)
	lrec, atL, _, _ := f.plantLoss(t, copyTag, file, 71, format.LossCorrupt, core.Extent{Off: 8 << 10, Len: 4 << 10}, false)
	if lrec.Seg != recW.Segment {
		t.Fatalf("the loss landed in segment %d, want the source segment %d that holds the write", lrec.Seg, recW.Segment)
	}
	if f.tracker.Droppable(lrec) {
		t.Fatal("Droppable of the loss = true, want false: its marker never settled, so the carry must reach it")
	}
	f.sealSource(t, file)
	f.waitSpares(t)

	c := f.copier()
	res, err := c.Copy(context.Background(), recW.Segment)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(res.Copies) != 1 {
		t.Fatalf("Copy appended %d copies, want the one write's", len(res.Copies))
	}
	// Damage on the device under the loss record's own header.
	f.flipByte(t, atL, int64(atL.Offset))

	carry, err := c.Carry(context.Background(), recW.Segment, res)
	if err == nil {
		t.Fatal("Carry over a segment whose loss record does not read back = nil error, want the unit abandoned")
	}
	if len(carry.Carried) != 0 {
		t.Fatalf("the abandoned carry placed %d records (%+v), want none", len(carry.Carried), carry.Carried)
	}
	// The registration stands: the carry still reaches the record and fails
	// on it again, having dropped nothing.
	again, err := c.Carry(context.Background(), recW.Segment, res)
	if err == nil {
		t.Fatal("the second Carry = nil error, want the corrupt record's registration still standing in the source segment")
	}
	if len(again.Carried) != 0 {
		t.Fatalf("the second carry placed %d records (%+v), want none", len(again.Carried), again.Carried)
	}
	// The content copies the unit's first step appended remain.
	h, body := f.readBack(t, res.Copies[0].To)
	if h.Kind != format.KindWrite || !bytes.Equal(body.Payload, payload) {
		t.Fatalf("the content copy at %v holds kind %s and %d payload bytes, want the write's bytes whole", res.Copies[0].To, h.Kind, len(body.Payload))
	}
}

// plantForget appends one forget record of tag through the zero file's
// stream — the stream a record naming no file belongs to — and registers it
// live with the snapshot the caller names, the way the Forget path's own
// barrier hands the registration its snapshot.
func (f *copyFixture) plantForget(t *testing.T, tag core.ShareTag, snap []core.SegmentID) (reclaim.Rec, core.Seq) {
	t.Helper()
	zero := core.FileID{}
	unlock := f.core.Domains().Lock(zero)
	defer unlock()
	// A forget record names only its tag: the format refuses any version,
	// file or extent on one, so the record's pair is the sequence number
	// alone.
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq of the forget record: %v", err)
	}
	pl, err := f.core.Streams().For(zero).Append(format.RecordHeader{
		Kind: format.KindForget, File: zero, Seq: seq, Tag: tag,
	}, format.RecordBody{})
	if err != nil {
		t.Fatalf("Append of the forget record: %v", err)
	}
	rec := reclaim.Rec{
		Kind: format.KindForget, File: zero, Seg: pl.Loc.Segment,
		Pair: index.Numbers{Seq: seq}, Seq: seq, Tag: tag, Loc: pl.Loc,
	}
	f.tracker.RegisterForget(rec, snap)
	return rec, seq
}

// TestCarryCarriesForgetRecord carries a live forget record — the walk's
// KindForget branch — and checks the copy the branch places end to end:
// appended on the zero file's deterministic stream under the source's own
// pair — both numbers are the record's identity, and a copy reissued under
// a fresh one would outrank what the forget ends below it — its
// registration cloned from the source's snapshot so it answers not
// droppable while any snapshot segment remains, and released at the
// retirement of the last one through the dropped-at-retirement snapshot
// rule, the real retirer driving both retirements. The second half checks
// the seam the copy pass shares with the content half: a forget record
// whose tag the forgetting mark names is carried nowhere, the tag's drop
// being the forget's own, and its registration left standing.
func TestCarryCarriesForgetRecord(t *testing.T) {
	f := newCopyFixture(t)
	zero := core.FileID{}

	// Two segments holding the tag's records below the forget: the snapshot
	// that keeps the record live, seeded the way the barrier collects it.
	fileA, fileB := copyFile(3), copyFile(5)
	recA, _, _ := f.plant(t, copyTag, fileA, []byte("records of the forgotten tag, one"), 0)
	sa1 := recA.Segment
	f.sealSource(t, fileA)
	f.waitSpares(t)
	recB, _, _ := f.plant(t, copyTag, fileB, []byte("records of the forgotten tag, two"), 0)
	sa2 := recB.Segment
	if sa2 == sa1 {
		t.Fatalf("the second write landed in segment %d, want a segment beside the sealed %d", sa2, sa1)
	}
	f.sealSource(t, fileB)
	f.waitSpares(t)

	frec, seqF := f.plantForget(t, copyTag, []core.SegmentID{sa1, sa2})
	if f.tracker.Droppable(frec) {
		t.Fatal("Droppable of the forget record with two live snapshot segments = true, want false: the snapshot keeps it")
	}
	f.sealSource(t, zero)
	f.waitSpares(t)

	ctx := context.Background()
	c := f.copier()
	res, err := c.Copy(ctx, frec.Seg)
	if err != nil {
		t.Fatalf("Copy of the forget's segment: %v", err)
	}
	if len(res.Copies) != 0 {
		t.Fatalf("Copy appended %d copies (%+v), want none: a forget record backs no content", len(res.Copies), res.Copies)
	}
	carry, err := c.Carry(ctx, frec.Seg, res)
	if err != nil {
		t.Fatalf("Carry of the forget's segment: %v", err)
	}
	if len(carry.Carried) != 1 {
		t.Fatalf("Carry carried %d records (%+v), want the live forget record: the walk's forget branch appends the record's copy", len(carry.Carried), carry.Carried)
	}
	cr := carry.Carried[0]
	if cr.Rec.Kind != format.KindForget || cr.Rec.Tag != copyTag || cr.Rec.File != zero {
		t.Fatalf("the carried record = %+v, want a forget record of the zero file under the stream's tag", cr.Rec)
	}
	if cr.Rec.Seq != seqF || cr.Rec.Pair != frec.Pair {
		t.Fatalf("carried forget Seq %d Pair %+v, want the source's own %d %+v: both numbers are the record's identity", cr.Rec.Seq, cr.Rec.Pair, seqF, frec.Pair)
	}
	if cr.Rec.Seg != cr.To.Segment || cr.To.Segment == frec.Seg {
		t.Fatalf("carried forget Rec %+v and placement %v disagree or name the source segment %d, want the copy's own placement", cr.Rec, cr.To, frec.Seg)
	}
	// The copy on disk decodes back at its placement through the seam's own
	// bytes, on the zero file's stream.
	hC, bC := f.readBack(t, cr.To)
	if hC.Kind != format.KindForget || hC.Seq != seqF || hC.Tag != copyTag || hC.File != (core.FileID{}) {
		t.Fatalf("the copy's header = %+v, want the forget record's kind, zero file, tag and the source's own seq %d", hC, seqF)
	}
	if bC.MTime != 0 || bC.Payload != nil || bC.Loss != (format.Loss{}) {
		t.Fatalf("the copy's body = %+v, want empty: a forget record carries none", bC)
	}
	// The copy's registration answers not droppable: the snapshot it was
	// registered with still names a live segment.
	if f.tracker.Droppable(cr.Rec) {
		t.Fatal("Droppable of the carried copy = true, want false: its cloned snapshot keeps it live while either segment remains")
	}

	// The retirer wired the way the journal's executor is: the retired-segment
	// hook the tracker's own, the durable signal its relay.
	m := segment.NewMaker(f.seam.FS(), f.set, math.MaxInt64, 0, nil)
	cache := segment.NewDescriptors(f.seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	ret := segment.NewRetirer(segment.RetirerConfig{
		Set:   f.set,
		Cache: cache,
		Maker: m,
		FS:    f.seam.FS(),
		Retired: func(id core.SegmentID) {
			f.tracker.SegmentRetired(id, nil)
		},
		Durable: func(r segment.Retirement) {
			f.tracker.RemovalDurable(r.ID)
		},
	})
	if _, err := ret.Retire(sa1, segment.NotHeld); err != nil {
		t.Fatalf("Retire of the first snapshot segment %d: %v", sa1, err)
	}
	if f.tracker.Droppable(cr.Rec) {
		t.Fatal("Droppable of the carried copy after the first snapshot segment's retirement = true, want false: the second keeps it")
	}
	if _, err := ret.Retire(sa2, segment.NotHeld); err != nil {
		t.Fatalf("Retire of the second snapshot segment %d: %v", sa2, err)
	}
	if !f.tracker.Droppable(cr.Rec) {
		t.Fatal("Droppable of the carried copy after its snapshot emptied at the retirements = false, want true: the retirements released its registration")
	}
	if f.tracker.Held(cr.To.Segment) {
		t.Fatal("Held of the copy's segment after the release = true, want false: nothing the index counts and nothing registered keeps it")
	}

	// A forget record whose tag the forgetting mark names is carried
	// nothing: the tag's drop is the forget's own, the copy pass sharing
	// the content half's seam, and its registration left standing.
	f.forgetting = func(tag core.ShareTag) bool { return tag == copyTag }
	frec2, _ := f.plantForget(t, copyTag, []core.SegmentID{cr.To.Segment})
	if f.tracker.Droppable(frec2) {
		t.Fatal("Droppable of the second forget record = true, want false: its snapshot names a live segment")
	}
	if !f.tracker.Held(frec2.Seg) {
		t.Fatal("Held of the second forget's segment before its carry = false, want true: the record is registered live")
	}
	again, err := f.copier().Carry(ctx, frec2.Seg, reclaim.Result{})
	if err != nil {
		t.Fatalf("Carry of the marked tag's forget record: %v", err)
	}
	if len(again.Carried) != 0 {
		t.Fatalf("Carry carried %d records (%+v), want none: the forgetting seam the share registry marks carries nothing for the marked tag", len(again.Carried), again.Carried)
	}
	if !f.tracker.Held(frec2.Seg) {
		t.Fatal("Held of the second forget's segment after its carry = false, want true: the skip abandons the copy and keeps the registration standing")
	}
}

// TestCarryDeferredLossOnReserve is the journal-level form of the rule that a
// deferred loss is owed its record by the copy that retires the segment
// holding the dropped record: the segment stays held by the deferral alone
// and answers a selection like any candidate, the unit writes the record
// from the registry entry on the repack-reserve admission — the one class
// the headroom never refuses, checked over a real headroom held at its
// limit — and the deferral ends only once the target is durable, which the
// carry never does itself: the end the durable-waiting caller plays leaves
// the source crossing to not held, the hand-over the pass waits for. The
// record the unit places is never on disk unnoted: once the source's own
// ranges have left with its retirement, a later removal of the file stays
// live on the range the record's placement noted alone.
func TestCarryDeferredLossOnReserve(t *testing.T) {
	f := newCopyFixture(t)
	dur := f.core.Durability()
	file := copyFile(7)
	payload := []byte("the write the deferred loss names")
	ctx := context.Background()

	// The dropped record: the file's only write, synced and sealed in the
	// source segment, so its bytes are durable there and nothing but the
	// held piece keeps the segment.
	recW, seqW, _ := f.plant(t, copyTag, file, payload, 0)
	sa := recW.Segment
	if err := dur.Sync(ctx, file); err != nil {
		t.Fatalf("Sync of the planted write: %v", err)
	}
	f.sealSource(t, file)
	f.waitSpares(t)

	// The deferral. A deferred Lose naming no segment is a caller bug
	// refused before the piece is taken: the same loss raised again with one
	// goes through whole, which the registry below answers to.
	vL, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the loss's version: %v", err)
	}
	x := core.Extent{Len: int64(len(payload))}
	if _, err := dur.Lose(ctx, durability.Loss{
		Share: copyTag, File: file, Extent: x, Version: vL,
		Seq: seqW, Reason: core.LossStale, Synced: true, Deferred: true,
	}); err == nil {
		t.Fatal("Lose of a deferred loss naming no segment = nil error, want the refusal")
	}
	if _, err := dur.Lose(ctx, durability.Loss{
		Share: copyTag, File: file, Extent: x, Version: vL,
		Seq: seqW, Reason: core.LossStale, Synced: true, Deferred: true, Seg: sa,
	}); err != nil {
		t.Fatalf("Lose of the deferred loss: %v", err)
	}
	// The deferral appends no record and drops the piece, as a synced loss
	// does: nothing but the write's bytes kept the source, so it holds
	// nothing now.
	if n := f.countLossRecords(t); n != 0 {
		t.Fatalf("%d loss records on disk after the deferred Lose, want 0: the deferral appends no record anywhere", n)
	}
	if p, ok := f.ix.Lookup(file, 0); ok {
		t.Fatalf("Lookup at 0 over the lost write = %+v, want missing: the marker the synced loss left stopped holding it", p)
	}
	losses := dur.DeferredAgainst(sa)
	if len(losses) != 1 {
		t.Fatalf("DeferredAgainst of the source = %+v, want the one deferred loss", losses)
	}
	if losses[0].Seq != seqW || losses[0].Seg != sa {
		t.Fatalf("the registry entry = %+v, want the dropped record's seq %d against the source %d", losses[0], seqW, sa)
	}

	// Held by the deferral alone, the source is a candidate like any other:
	// the loss emptied it, and nothing but the deferred flag keeps it.
	if f.tracker.Held(sa) {
		t.Fatal("Held of the source before the deferral starts = true, want false: the loss dropped its only content")
	}
	f.tracker.SetDeferred(sa, true)
	if !f.tracker.Held(sa) {
		t.Fatal("Held of the source kept by the deferral alone = false, want true")
	}
	sel := f.selector()
	cands := sel.Select(reclaim.Scope{Segments: []core.SegmentID{sa}}, 1)
	if len(cands) != 1 || cands[0].ID != sa {
		t.Fatalf("Select over the source = %+v, want the source %d alone: a segment held by a deferral is a candidate", cands, sa)
	}
	if cands[0].Live != 0 {
		t.Fatalf("the candidate's live bytes = %d, want 0: the loss dropped the only content", cands[0].Live)
	}
	sel.Done(sa)

	// A real headroom over injected facts, at its limit and drawn past the
	// open reserve: every class but Open and the repack copy draws nothing
	// and is refused, so the record below lands only through the one
	// admission that never refuses.
	const hrRecords = 2
	hr := capacity.NewHeadroom(capacity.HeadroomConfig{
		Records:    hrRecords,
		Footprint:  func() int64 { return 1 },
		MaxBytes:   1,
		SegmentCap: copySegCap,
		Admit:      func() error { return nil },
	})
	draw, err := hr.Admit(capacity.Stamp)
	if err != nil {
		t.Fatalf("the headroom's opening draw: %v", err)
	}
	hr.Drawn(draw)

	// The wirings the pass would make: the registry mapped field by field,
	// and the headroom the record is admitted through.
	f.deferred = func(seg core.SegmentID) []reclaim.DeferredLoss {
		var out []reclaim.DeferredLoss
		for _, l := range dur.DeferredAgainst(seg) {
			out = append(out, reclaim.DeferredLoss{
				Share: l.Share, File: l.File, Extent: l.Extent, Version: l.Version,
				Seq: l.Seq, Reason: l.Reason, Offloaded: l.Offloaded, Synced: l.Synced,
			})
		}
		return out
	}
	f.admit = hr.Admit
	f.drawn = hr.Drawn

	// The unit: no content copies — the only content left the index at the
	// loss — and the deferred loss record written beside them.
	c := f.copier()
	res, err := c.Copy(ctx, sa)
	if err != nil {
		t.Fatalf("Copy of the source: %v", err)
	}
	if len(res.Copies) != 0 {
		t.Fatalf("Copy appended %d copies (%+v), want none: the loss dropped the only content", len(res.Copies), res.Copies)
	}
	carry, err := c.Carry(ctx, sa, res)
	if err != nil {
		t.Fatalf("Carry of the source: %v", err)
	}
	if len(carry.Deferred) != 1 {
		t.Fatalf("Carry wrote %d deferred loss records (%+v), want the one the segment owes", len(carry.Deferred), carry.Deferred)
	}
	dc := carry.Deferred[0]
	want := reclaim.DeferredLoss{
		Share: copyTag, File: file, Extent: x, Version: vL,
		Seq: seqW, Reason: core.LossStale, Synced: true,
	}
	if dc.Loss != want {
		t.Fatalf("the deferred copy's entry = %+v, want the registry's own %+v", dc.Loss, want)
	}
	if dc.Loc.Segment == sa {
		t.Fatalf("the loss record landed in its own source segment %d, want another", sa)
	}
	// The record landed, whole and decoded back: kind, named number, reason
	// and its own numbers.
	at := index.RecordAt{Segment: dc.Loc.Segment, Offset: uint32(dc.Loc.Off)}
	h, body := f.readBack(t, at)
	if h.Kind != format.KindLoss {
		t.Fatalf("the carried record's kind = %s, want a loss record", h.Kind)
	}
	if body.Loss.Dropped != seqW {
		t.Fatalf("the loss record's Dropped = %d, want the dropped record's seq %d", body.Loss.Dropped, seqW)
	}
	if body.Loss.Reason != format.LossStale {
		t.Fatalf("the loss record's reason = %d, want the stale one", body.Loss.Reason)
	}
	if !body.Loss.Synced || body.Loss.Offloaded {
		t.Fatalf("the loss record's flags = synced %v offloaded %v, want the entry's synced true offloaded false", body.Loss.Synced, body.Loss.Offloaded)
	}
	if h.File != file || h.Extent != x || h.Version != vL || h.Tag != copyTag {
		t.Fatalf("the loss record's header = %+v, want the entry's file, extent, version and tag", h)
	}
	if h.Seq <= seqW {
		t.Fatalf("the loss record's Seq = %d, want a fresh one above the dropped record's %d", h.Seq, seqW)
	}
	// Admitted through the wired headroom: the real one, held at its limit,
	// would have refused the record under any other class.
	if _, err := hr.Admit(capacity.Loss); err == nil {
		t.Fatal("Admit of a loss-class record over the headroom at its limit = nil error, want the refusal: only the repack copy draws")
	}
	// The record is registered live in the target, as every record the walk
	// places is, and the deferral is untouched: the registry still names
	// the entry and the source stays held.
	if !f.tracker.Held(dc.Loc.Segment) {
		t.Fatalf("Held of the target %d after the carry = false, want the loss record registered live there", dc.Loc.Segment)
	}
	if still := dur.DeferredAgainst(sa); len(still) != 1 || still[0] != losses[0] {
		t.Fatalf("DeferredAgainst of the source after the carry = %+v, want the deferral still standing: the carry ends nothing", still)
	}
	if !f.tracker.Held(sa) {
		t.Fatal("Held of the source after the carry = false, want true: the deferral ends only once the target is durable")
	}

	// The record's placement is noted through the index, and the note is the
	// range that answers a later removal: the dropped write's own range is
	// what kept a removal live until now, so the source retires first —
	// through the real retirer — and its ranges leave with the durable
	// signal. The write reached the index but never the tracker's registry,
	// so the retired hook hands the file list the write's plant never
	// registered, or no SegmentGone would name the file's ranges.
	m := segment.NewMaker(f.seam.FS(), f.set, math.MaxInt64, 0, nil)
	cache := segment.NewDescriptors(f.seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:   f.set,
		Cache: cache,
		Maker: m,
		FS:    f.seam.FS(),
		Retired: func(id core.SegmentID) {
			f.tracker.SegmentRetired(id, []core.FileID{file})
		},
		Durable: func(ret segment.Retirement) {
			f.tracker.RemovalDurable(ret.ID)
		},
	})
	if _, err := r.Retire(sa, segment.NotHeld); err != nil {
		t.Fatalf("Retire of the source: %v", err)
	}
	// A removal of the file whose pair outranks the deferred copy's own,
	// settled at once: it stays live on the copy's note alone, carrying
	// nothing else below it — the file's only older range left with the
	// source. The stream's active segment is the one the deferred record
	// made of its reserve spare, so it is sealed here: the probe rolls onto
	// a segment of its own, where its own range cannot be the exclusion that
	// hides the note this half asks about.
	f.sealSource(t, file)
	f.waitSpares(t)
	drec, _, _, _ := f.plantRemoval(t, copyTag, file, format.KindDelete, core.ChangeDelete,
		core.Extent{}, core.Extent{Len: math.MaxInt64}, true)
	if drec.Seg == dc.Loc.Segment {
		t.Fatalf("the delete landed in the deferred record's segment %d, want a segment of its own", dc.Loc.Segment)
	}
	if f.tracker.Droppable(drec) {
		t.Fatal("Droppable of the delete after the deferred copy is placed = true, want false: the loss record's placement, noted in the copy's segment, is the only range below the delete now the source retired")
	}

	// The end the durable-waiting caller plays once the target is synced:
	// the registry gives its entry up, idempotently, and the source crosses
	// to not held — the hand-over condition the pass waits for before it
	// retires the source.
	if !dur.EndDeferral(sa, losses[0]) {
		t.Fatal("EndDeferral of the entry the registry still holds = false, want true: the deferral ends")
	}
	if still := dur.DeferredAgainst(sa); len(still) != 0 {
		t.Fatalf("DeferredAgainst of the source after the end = %+v, want empty", still)
	}
	if dur.EndDeferral(sa, losses[0]) {
		t.Fatal("the second EndDeferral = true, want false: the end is idempotent")
	}
	f.tracker.SetDeferred(sa, false)
	if f.tracker.Held(sa) {
		t.Fatal("Held of the source after the deferral ended = true, want false: the crossing the durable-waiting caller's hand-over turns on")
	}
}

// TestCarryDeferredLossSkipsAForgottenTag: a deferred entry whose share the
// forgetting mark names is owed no record — the walk's own skip, taken under
// the entry's file domain — while an unmarked second share's entry is written
// the one record its deferral promised; the registry keeps both entries
// standing, the forget's own path clearing what it clears.
func TestCarryDeferredLossSkipsAForgottenTag(t *testing.T) {
	f := newCopyFixture(t)
	dur := f.core.Durability()
	ctx := context.Background()
	const otherTag = core.ShareTag(2)

	// Two shares' writes in the one source segment, so their deferred losses
	// ride one carry.
	fileA := copyFile(1)
	fileB := sameStreamFile(t, f, fileA)
	payA, payB := []byte("the forgotten share's write"), []byte("the other share's write")
	recA, seqA, _ := f.plant(t, copyTag, fileA, payA, 0)
	recB, seqB, _ := f.plant(t, otherTag, fileB, payB, 0)
	if recA.Segment != recB.Segment {
		t.Fatalf("the writes landed in segments %d and %d, want one shared source segment", recA.Segment, recB.Segment)
	}
	sa := recA.Segment
	if err := dur.Sync(ctx, fileA); err != nil {
		t.Fatalf("Sync of the first write: %v", err)
	}
	if err := dur.Sync(ctx, fileB); err != nil {
		t.Fatalf("Sync of the second write: %v", err)
	}
	f.sealSource(t, fileA)
	f.waitSpares(t)

	// The deferrals of both shares against the one source.
	xA, xB := core.Extent{Len: int64(len(payA))}, core.Extent{Len: int64(len(payB))}
	vA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the first loss's version: %v", err)
	}
	if _, err := dur.Lose(ctx, durability.Loss{
		Share: copyTag, File: fileA, Extent: xA, Version: vA,
		Seq: seqA, Reason: core.LossStale, Synced: true, Deferred: true, Seg: sa,
	}); err != nil {
		t.Fatalf("Lose of the first deferred loss: %v", err)
	}
	vB, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the second loss's version: %v", err)
	}
	if _, err := dur.Lose(ctx, durability.Loss{
		Share: otherTag, File: fileB, Extent: xB, Version: vB,
		Seq: seqB, Reason: core.LossStale, Synced: true, Deferred: true, Seg: sa,
	}); err != nil {
		t.Fatalf("Lose of the second deferred loss: %v", err)
	}
	if got := dur.DeferredAgainst(sa); len(got) != 2 {
		t.Fatalf("DeferredAgainst of the source = %+v, want the two shares' entries", got)
	}

	// The wirings the pass would make, the mark among them: the first
	// share's tag answers forgotten before the unit runs.
	f.deferred = func(seg core.SegmentID) []reclaim.DeferredLoss {
		var out []reclaim.DeferredLoss
		for _, l := range dur.DeferredAgainst(seg) {
			out = append(out, reclaim.DeferredLoss{
				Share: l.Share, File: l.File, Extent: l.Extent, Version: l.Version,
				Seq: l.Seq, Reason: l.Reason, Offloaded: l.Offloaded, Synced: l.Synced,
			})
		}
		return out
	}
	hr := capacity.NewHeadroom(capacity.HeadroomConfig{
		Records:    copyHeadroomRecords,
		Footprint:  func() int64 { return 0 },
		MaxBytes:   math.MaxInt64,
		SegmentCap: copySegCap,
		Admit:      func() error { return nil },
	})
	f.admit = hr.Admit
	f.drawn = hr.Drawn
	f.forgetting = func(tag core.ShareTag) bool { return tag == copyTag }

	carry, err := f.copier().Carry(ctx, sa, reclaim.Result{})
	if err != nil {
		t.Fatalf("Carry of the source: %v", err)
	}
	if len(carry.Deferred) != 1 {
		t.Fatalf("Carry wrote %d deferred loss records (%+v), want the unmarked share's one alone: the forgotten tag's entry is owed no record under a fresh number above the forget record's", len(carry.Deferred), carry.Deferred)
	}
	dc := carry.Deferred[0]
	if dc.Loss.Share != otherTag || dc.Loss.File != fileB || dc.Loss.Extent != xB {
		t.Fatalf("the deferred copy's entry = %+v, want the unmarked share's loss of the second file over %v", dc.Loss, xB)
	}
	if still := dur.DeferredAgainst(sa); len(still) != 2 {
		t.Fatalf("DeferredAgainst of the source after the carry = %+v, want both entries standing: the skip records nothing and ends nothing, the forget's own path clearing the marked share's", still)
	}
}
