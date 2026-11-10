package durability

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// registerCopy puts a copy of the record numbered seq at loc, written with the
// payload checksum checksum, in the copy table directly, as registerOriginal
// would once a re-append's new place completed, without asking the set
// whether loc's segment is still there.
func (d *Durability) registerCopy(seq core.Seq, loc segment.Location, checksum uint32) {
	d.copyMu.Lock()
	defer d.copyMu.Unlock()
	if d.copies == nil {
		d.copies = map[core.Seq]copyEntry{}
	}
	d.copies[seq] = copyEntry{seq: seq, loc: loc, checksum: checksum}
}

// reappended is a write whose record was appended a second time with the same
// sequence number and version, the index moved to the second copy as a
// re-append moves it: orig is the copy the table can name, again the one the
// index names and the Corruption reports.
type reappended struct {
	orig, again segment.Location
	c           Corruption
	// kept is the payload checksum the ledger kept for orig when it was
	// written.
	kept uint32
}

// reappend publishes a write of f, seals its segment, appends the write again
// under the same number and version, so the second copy lands in another
// segment, and repoints the index to the second copy, under f's domain. The
// first copy is synced by the seal; the second is not synced.
func (cr *corruptRig) reappend(t *testing.T, f core.FileID, off int64, payload string) reappended {
	t.Helper()
	orig, v := cr.publish(t, f, off, payload)
	seq := core.Seq(cr.lastSeq.Load())
	kept := cr.keptChecksum(t, orig)
	x := core.Extent{Off: off, Len: int64(len(payload))}
	// The append after the seal rolls into a spare, which an earlier roll of
	// the same test may have taken and the pool not yet made again.
	cr.settlePool(t)
	if _, err := cr.streams.For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The worker completes the sealed record's entry on its own pass.
	eventually(t, "the sealed record's entry to complete", func() bool { return !cr.inLedger(orig) })

	unlock := cr.domains.Lock(f)
	p, err := cr.streams.For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: x, Seq: seq, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		unlock()
		t.Fatalf("Append of the second copy: %v", err)
	}
	from := index.RecordAt{Segment: orig.Segment, Offset: uint32(orig.Off)}
	to := index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}
	moved, _, err := cr.ix.RepointRecord(f, from, to)
	unlock()
	if err != nil || moved == 0 {
		t.Fatalf("RepointRecord = %d, %v, want the record moved", moved, err)
	}
	if p.Loc.Segment == orig.Segment {
		t.Fatalf("the second copy landed in the first copy's segment %d", orig.Segment)
	}
	return reappended{orig: orig, again: p.Loc, kept: kept, c: Corruption{
		Share: 1, File: f, Record: to, Seq: seq, Version: v, Extent: x,
	}}
}

// keptChecksum returns the payload checksum the ledger holds for the record
// at loc.
func (r *rig) keptChecksum(t *testing.T, loc segment.Location) uint32 {
	t.Helper()
	st := r.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.ledger {
		if e.w.Loc == loc {
			return e.w.PayloadCRC
		}
	}
	t.Fatalf("no ledger entry for %+v", loc)
	return 0
}

// placedAt returns the record the index serves offset off of f from.
func (r *rig) placedAt(t *testing.T, f core.FileID, off int64) index.RecordAt {
	t.Helper()
	p, ok := r.ix.Lookup(f, off)
	if !ok {
		t.Fatalf("offset %d of the file is not in the index", off)
	}
	return p.Record
}

// queuedCorrupt returns how many corrupt losses wait for the worker.
func (cr *corruptRig) queuedCorrupt() int {
	st := cr.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.corrupt)
}

// tableLen returns how many entries the copy table holds.
func (cr *corruptRig) tableLen() int {
	cr.d.copyMu.Lock()
	defer cr.d.copyMu.Unlock()
	return len(cr.d.copies)
}

// holds records every copy the layer handed to holdCopy and to unholdCopy.
type holds struct {
	mu       sync.Mutex
	got, let []ownedCopy
}

func (h *holds) record(c ownedCopy) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, c)
}

func (h *holds) release(c ownedCopy) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.let = append(h.let, c)
}

func (h *holds) all() []ownedCopy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ownedCopy(nil), h.got...)
}

func (h *holds) released() []ownedCopy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ownedCopy(nil), h.let...)
}

// watch installs h on the layer's hold hooks.
func (h *holds) watch(d *Durability) {
	d.onHoldCopy, d.onUnholdCopy = h.record, h.release
}

// wantNoLoss stops the layer, which drains every queued corrupt loss, and
// fails the test on any loss event or durable loss record.
func (cr *corruptRig) wantNoLoss(t *testing.T) {
	t.Helper()
	cr.d.Stop()
	if evs := cr.events(); len(evs) != 0 {
		t.Fatalf("loss events = %+v, want none", evs)
	}
	if recs := cr.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Fatalf("loss records = %+v, want none", recs)
	}
}

// TestCorruptUsesVerifyingCopy covers a record with another copy that
// verifies: the index is repointed to the copy, the read is told to serve it,
// nothing is appended and no loss is recorded, and the corrupt copy's segment
// is counted damaged only when that copy was synced.
func TestCorruptUsesVerifyingCopy(t *testing.T) {
	for _, synced := range []bool{false, true} {
		name := "unsynced"
		if synced {
			name = "synced"
		}
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			h := &holds{}
			cr := newCorruptRig(t, func(cr *corruptRig) { h.watch(cr.d) })
			f := file(1)
			r := cr.reappend(t, f, 0, "aaaa")
			cr.d.registerCopy(r.c.Seq, r.orig, r.kept)
			if synced {
				if err := cr.d.Sync(context.Background(), f); err != nil {
					t.Fatalf("Sync: %v", err)
				}
				// The worker completes the synced entries on its own pass.
				eventually(t, "the synced entries to complete", func() bool { return cr.ledgerLen(0) == 0 })
			}
			cr.settlePool(t)
			writes := cr.seam.Calls(kit.OpWrite)
			ledger := cr.ledgerLen(0)

			out, err := cr.d.Corrupt(context.Background(), r.c)
			if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
				t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
			}
			if got := cr.seam.Calls(kit.OpWrite); got != writes {
				t.Fatalf("the read made %d writes, want none", got-writes)
			}
			if got := cr.ledgerLen(0); got != ledger {
				t.Fatalf("the ledger holds %d entries after the read, want %d", got, ledger)
			}
			want := index.RecordAt{Segment: r.orig.Segment, Offset: uint32(r.orig.Off)}
			for off := range int64(4) {
				if got := cr.placedAt(t, f, off); got != want {
					t.Fatalf("offset %d is served from %+v, want the copy %+v", off, got, want)
				}
			}
			if n := cr.queuedCorrupt(); n != 0 {
				t.Fatalf("%d corrupt losses queued, want none", n)
			}
			// Only the corrupt copy's segment can be counted, never the
			// segment of the copy that is served.
			p := cr.d.Stats()
			if synced && (p.DamagedSegments != 1 || len(p.Segments) != 1 || p.Segments[0].Segment != r.c.Record.Segment) {
				t.Fatalf("damaged = %d, rows %+v, want one row for the corrupt copy's segment %d", p.DamagedSegments, p.Segments, r.c.Record.Segment)
			}
			if !synced && (p.DamagedSegments != 0 || len(p.Segments) != 0) {
				t.Fatalf("damaged = %d, rows %+v, want none for an unsynced record", p.DamagedSegments, p.Segments)
			}
			if got := h.all(); len(got) != 1 || got[0].Seq != r.c.Seq || got[0].Copy != r.orig || got[0].Record != r.c.Record || got[0].File != f {
				t.Fatalf("holdCopy was handed %+v, want the one record and its copy", got)
			}
			if got := h.released(); len(got) != 0 {
				t.Fatalf("unholdCopy was handed %+v after a repoint that moved the record, want nothing", got)
			}
			cr.wantNoLoss(t)
		})
	}
}

// TestCorruptCopyVerifyingInReadOnly covers the copy in a read-only journal:
// it is still served and the index repointed, with no loss and no hand-off.
func TestCorruptCopyVerifyingInReadOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})
	h := &holds{}
	cr := newCorruptRig(t, func(cr *corruptRig) { cr.d.onHoldCopy = h.record })
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.registerCopy(r.c.Seq, r.orig, r.kept)
	// Registering the copy is in memory only; what matters is that the record
	// it serves for is synced before the journal enters read-only, because
	// the read-only pass fails every unsynced record and leaves a synced one.
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	eventually(t, "the synced entry to complete", func() bool { return cr.ledgerLen(0) == 0 })
	cr.settlePool(t)
	cr.d.Enter(errors.New("the journal is read-only"))
	writes := cr.seam.Calls(kit.OpWrite)

	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
	}
	if got := cr.seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("the read made %d writes in a read-only journal, want none", got-writes)
	}
	want := index.RecordAt{Segment: r.orig.Segment, Offset: uint32(r.orig.Off)}
	if got := cr.placedAt(t, f, 0); got != want {
		t.Fatalf("the extent is served from %+v, want the copy %+v", got, want)
	}
	// The record was synced, so the corrupt copy's segment is counted
	// damaged, never the segment of the copy that is served.
	if p := cr.d.Stats(); p.DamagedSegments != 1 || len(p.Segments) != 1 || p.Segments[0].Segment != r.c.Record.Segment {
		t.Fatalf("damaged = %d, rows %+v, want one row for the corrupt copy's segment %d", p.DamagedSegments, p.Segments, r.c.Record.Segment)
	}
	if got := h.all(); len(got) != 0 {
		t.Fatalf("holdCopy was handed %+v in a read-only journal, want nothing", got)
	}
	cr.wantNoLoss(t)
}

// TestCorruptCopyChecksumKeptNotReadBack covers the copy's verification: a
// copy whose bytes match the checksum read back with them, but not the one
// kept from the write, is not used, and the record is dropped.
func TestCorruptCopyChecksumKeptNotReadBack(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.registerCopy(r.c.Seq, r.orig, ^r.kept)

	cr.corrupt(t, r.c)
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent is served from %+v, want it dropped", p)
	}
	cr.awaitRecord(t, r.c.Seq)
}

// TestCorruptCopyGoneIsLoss covers a copy whose segment is gone: it does not
// verify, and the record is dropped as a loss rather than the read erroring.
func TestCorruptCopyGoneIsLoss(t *testing.T) {
	cases := map[string]func(cr *corruptRig, r reappended) segment.Location{
		// The descriptor cache finds no file and answers ErrSegmentGone.
		"segment gone": func(_ *corruptRig, r reappended) segment.Location {
			return segment.Location{Segment: 999, Off: r.orig.Off, Len: r.orig.Len}
		},
		// The storage answers ENOENT to the read itself.
		"ENOENT": func(cr *corruptRig, r reappended) segment.Location {
			if err := cr.seam.Schedule(kit.Failing(kit.OpRead, format.SegmentName(r.orig.Segment), 0, syscall.ENOENT)); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
			return r.orig
		},
	}
	for name, at := range cases {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			cr := newCorruptRig(t, nil)
			f := file(1)
			r := cr.reappend(t, f, 0, "aaaa")
			cr.d.registerCopy(r.c.Seq, at(cr, r), r.kept)

			cr.corrupt(t, r.c)
			if p, ok := cr.ix.Lookup(f, 0); ok {
				t.Fatalf("the extent is served from %+v, want it dropped", p)
			}
			cr.awaitRecord(t, r.c.Seq)
		})
	}
}

// TestCorruptCopyIOErrorDropsNothing covers a read of the copy that fails
// with an error that shows nothing about it: Corrupt returns the error, the
// extents stay in the index, and no loss, mark or queued loss follows.
func TestCorruptCopyIOErrorDropsNothing(t *testing.T) {
	type tc struct {
		arrange func(cr *corruptRig, r reappended) segment.Location
		want    error
	}
	cases := map[string]tc{
		"EIO": {
			arrange: func(cr *corruptRig, r reappended) segment.Location {
				if err := cr.seam.Schedule(kit.Failing(kit.OpRead, format.SegmentName(r.orig.Segment), 0, syscall.EIO)); err != nil {
					t.Fatalf("Schedule: %v", err)
				}
				return r.orig
			},
			want: syscall.EIO,
		},
		// The copy is named past the end of its segment's file.
		"short read": {
			arrange: func(_ *corruptRig, r reappended) segment.Location {
				return segment.Location{Segment: r.orig.Segment, Off: rigCap - 8, Len: r.orig.Len}
			},
			want: io.ErrUnexpectedEOF,
		},
		"no descriptor": {
			arrange: func(cr *corruptRig, r reappended) segment.Location {
				cr.d.readCopyAt = func(core.SegmentID, []byte, int64) (int, error) { return 0, segment.ErrNoDescriptor }
				return r.orig
			},
			want: segment.ErrNoDescriptor,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			cr := newCorruptRig(t, nil)
			f := file(1)
			r := cr.reappend(t, f, 0, "aaaa")
			cr.d.registerCopy(r.c.Seq, c.arrange(cr, r), r.kept)

			out, err := cr.d.Corrupt(context.Background(), r.c)
			if !errors.Is(err, c.want) || out != (CorruptOutcome{}) {
				t.Fatalf("Corrupt = %+v, %v, want no outcome and %v", out, err, c.want)
			}
			if got := cr.placedAt(t, f, 0); got != r.c.Record {
				t.Fatalf("the extent is served from %+v, want the record still at %+v", got, r.c.Record)
			}
			if !cr.inLedger(r.again) {
				t.Fatal("the record left the ledger")
			}
			if n := cr.queuedCorrupt(); n != 0 {
				t.Fatalf("%d corrupt losses queued, want none", n)
			}
			if got := cr.d.Stats().DamagedSegments; got != 0 {
				t.Fatalf("DamagedSegments = %d, want 0", got)
			}
			if err := cr.seam.Schedule(); err != nil {
				t.Fatalf("clearing the schedule: %v", err)
			}
			cr.wantNoLoss(t)
		})
	}
}

// TestRetiredDropsTableEntries covers the table's bound: a retired segment's
// copies leave it, and a record whose only other copy sat there is then a
// loss, not an error.
func TestRetiredDropsTableEntries(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.registerCopy(r.c.Seq, r.orig, r.kept)
	other := segment.Location{Segment: r.orig.Segment + 100, Off: r.orig.Off, Len: r.orig.Len}
	cr.d.registerCopy(r.c.Seq+1000, other, r.kept)

	cr.d.Retired(r.orig.Segment)
	if n := cr.tableLen(); n != 1 {
		t.Fatalf("the table holds %d entries after the copy's segment retired, want the other segment's 1", n)
	}
	cr.d.Retired(other.Segment)
	if n := cr.tableLen(); n != 0 {
		t.Fatalf("the table holds %d entries after both segments retired, want 0", n)
	}

	cr.corrupt(t, r.c)
	if p, ok := cr.ix.Lookup(f, 0); ok {
		t.Fatalf("the extent is served from %+v, want it dropped", p)
	}
	cr.awaitRecord(t, r.c.Seq)
}

// TestCorruptCopyOverwrittenBeforeRepointReleasesHold covers a record
// overwritten between the copy's read and the repoint: the repoint moves
// nothing, the hold the record joined is released, and the record is reported
// dropped.
func TestCorruptCopyOverwrittenBeforeRepointReleasesHold(t *testing.T) {
	prof.Watch(t, prof.Options{})
	h := &holds{}
	cr := newCorruptRig(t, func(cr *corruptRig) { h.watch(cr.d) })
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.registerCopy(r.c.Seq, r.orig, r.kept)
	// The copy is read with no domain held, so a writer overwrites the
	// record right after the read and before Corrupt takes the domain, and
	// the overwrite syncs, so the index keeps no retained piece of it either.
	cr.d.readCopyAt = func(seg core.SegmentID, p []byte, off int64) (int, error) {
		n, err := cr.streams.For(f).ReadAt(seg, p, off)
		cr.publish(t, f, 0, "bbbb")
		if err := cr.d.Sync(context.Background(), f); err != nil {
			t.Errorf("Sync of the overwrite: %v", err)
		}
		eventually(t, "the overwritten record to leave the index", func() bool { return len(cr.ix.Retained(f)) == 0 })
		return n, err
	}

	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want Dropped and nil", out, err)
	}
	held, let := h.all(), h.released()
	if len(held) != 1 || len(let) != 1 || let[0] != held[0] {
		t.Fatalf("held %+v, released %+v, want the one hold released", held, let)
	}
	if got := cr.placedAt(t, f, 0); got.Segment == r.orig.Segment && got.Offset == uint32(r.orig.Off) {
		t.Fatalf("the overwritten extent is served from the copy %+v", got)
	}
}

// TestCorruptUsedCopyThenWindowFailsPinsKnownCeiling pins a known ceiling: a
// record served from a copy stays in its pending window, and when the window
// fails, the worker still reports a failed-window loss for it although the
// record is not lost: the index names the copy, and then the place the hold's
// re-append of the copy put it at. The test keeps this path by making the
// window's re-read fail. The success branch, where the
// re-read verifies and the record is re-appended and repointed from the copy,
// is TestACopyServedRecordInTheWindowIsRepointedFromTheCopy.
func TestCorruptUsedCopyThenWindowFailsPinsKnownCeiling(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	// The window's re-read fails, so the window fails the record rather than
	// re-appending it; the read of the copy goes through.
	var rereadFails atomic.Uint32
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.reappend = cr.d.reappendHeld
		cr.d.beforeFail = func(*window) { hook() }
		cr.d.readCopyAt = func(seg core.SegmentID, p []byte, off int64) (int, error) {
			if uint32(seg) == rereadFails.Load() {
				return 0, syscall.EIO
			}
			return cr.streams.For(file(1)).ReadAt(seg, p, off)
		}
	})
	t.Cleanup(release)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.registerCopy(r.c.Seq, r.orig, r.kept)
	rereadFails.Store(uint32(r.again.Segment))
	cr.failByTimer(t, r.again.Segment)
	<-reached

	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
	}
	release()
	if got := cr.awaitResolved(t); got != r.again.Segment {
		t.Fatalf("resolved segment %d, want %d", got, r.again.Segment)
	}
	// The hold's re-append of the copy that verified moves the record off the
	// copy, which the failed window's loss does not take back.
	eventually(t, "the hold's re-append to move the record off the copy", func() bool {
		p, ok := cr.ix.Lookup(f, 0)
		return ok && p.Record != copyAt(r) && p.Record != placeAt(r.again)
	})
	cr.d.Stop()

	evs := cr.lossesOf(f)
	if len(evs) != 1 || evs[0].Reason != core.LossFailedWindow || evs[0].Version != r.c.Version {
		t.Fatalf("events = %+v, want exactly one failed-window loss of the record", evs)
	}
	if p, ok := cr.ix.Lookup(f, 0); !ok || p.Version != r.c.Version {
		t.Fatalf("Lookup = %+v, %v, want the record's version %+v still served", p, ok, r.c.Version)
	}
}
