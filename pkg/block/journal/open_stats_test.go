package journal

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// The open's statistics and span: the marks of what the open recovered, the
// damaged segment it keeps counted, and the counts its own span children
// carry. Every check reads its answers through the journal's public surface —
// Stats, Inspect and the seam's own images of the files — so a recovery that
// computes a mark the served journal never reports fails at the caller that
// reads it.

// openOver opens the fixture's directory through the production Open, handing
// the marks the caller recorded durably, and closes what it answered when the
// test ends. It fails the row only on the open's own refusal; an open that
// reached a lost-tail refusal the row did not plant answers here.
func (f *assembledJournal) openOver(t *testing.T, marks []SyncMark) *Journal {
	t.Helper()
	j, err := Open(context.Background(), f.dir, Version{}, f.id, marks, f.opts(nil)...)
	if err != nil {
		t.Fatalf("the public Open over %s: %v", f.dir, err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the opened journal: %v", err)
		}
	})
	return j
}

// viewRecord reads the record header at off of segment seg from the seam's
// live view, the bytes the process that placed it still saw: its sequence
// number and the size of its whole on-disk record.
func (f *assembledJournal) viewRecord(t *testing.T, seg SegmentID, off int64) (seq, size uint64) {
	t.Helper()
	file, err := f.seam.Open(format.SegmentName(core.SegmentID(seg)))
	if err != nil {
		t.Fatalf("open %s to read: %v", format.SegmentName(core.SegmentID(seg)), err)
	}
	b := make([]byte, format.RecordHeaderSize)
	if _, err := file.ReadAt(b, off); err != nil {
		t.Fatalf("read the record header at %d of segment %d: %v", off, seg, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close segment %d: %v", seg, err)
	}
	h, err := format.DecodeRecordHeader(b, core.JournalID(f.id), core.SegmentID(seg), off)
	if err != nil {
		t.Fatalf("decode the record header at %d of segment %d: %v", off, seg, err)
	}
	return uint64(h.Seq), uint64(h.Size())
}

// heldLocation answers where the journal's index places the held piece
// covering byte off of file id.
func heldLocation(t *testing.T, j *Journal, id FileID, off int64) RecordRef {
	t.Helper()
	entries, err := j.Inspect().File(id)
	if err != nil {
		t.Fatalf("read the index entries of file %v: %v", id, err)
	}
	for _, e := range entries {
		if e.Kind == EntryHeld && e.Extent.Off <= off && off < e.Extent.Off+e.Extent.Len {
			return e.Location
		}
	}
	t.Fatalf("no held index entry of file %v covers byte %d", id, off)
	return RecordRef{}
}

// killJournal ends the process the journal ran in with no power loss: the
// seam keeps what the process held — the files' bytes included — and the
// caller opens the next process over them.
func (f *assembledJournal) killJournal(t *testing.T, j *Journal) {
	t.Helper()
	f.seam.Abandon()
	closeDead(t, j)
}

// droppedSyncs arms a rule that loses every sync of the segment file name:
// each reports success and puts nothing on the device.
func (f *assembledJournal) droppedSyncs(name string) {
	if err := f.seam.Schedule(kit.Dropping(kit.OpSync, name, 0)); err != nil {
		panic(err)
	}
}

// TestStatsSyncMarksAfterOpen checks R34's acceptance: an open that
// re-appended the record its stream lost reports, in that stream's entry of
// Stats.SyncMarks, the re-appended record's own numbers — the sealed target
// the open copied it into, with the copy's end and the record's sequence —
// so the mark of the old segment, whose tail the device lost and whose file
// the emptying unlinked, is not what the entry names for the losing stream.
// The entry also belongs to the caller alone: a change to a returned map
// reaches no later Stats.
func TestStatsSyncMarksAfterOpen(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	id := fileOf(9)

	// The half of the stream whose sync the device holds: recorded by its
	// own mark, as the group commit would.
	writeReady(t, h, id, "the synced half of the stream")
	marks, err := h.Sync(context.Background(), id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("Sync of the durable half: marks %+v, err %v, want its stream's one mark", marks, err)
	}
	recorded := marks[0]
	seg := heldLocation(t, j, id, 0).Segment

	// The stream's tail: a second record — an overwrite of the file — the
	// device takes only where the seam's view serves it, and every sync of
	// its segment answers success while putting nothing on the device, so
	// the open's own tail proof finds it at its first read and not on the
	// device.
	f.droppedSyncs(format.SegmentName(core.SegmentID(seg)))
	writeReady(t, h, id, "the overwritten tail")
	tail := heldLocation(t, j, id, 0)
	if tail.Segment != seg {
		t.Fatalf("the tail's record landed in segment %d, want the file's one and only segment %d", tail.Segment, seg)
	}
	tailSeq, tailSize := f.viewRecord(t, tail.Segment, tail.Offset)

	// The process dies with no power loss: the device keeps none of the
	// tail's bytes, while the next process still reads them at its first
	// read — the first copy the open hands its re-append.
	f.killJournal(t, j)
	j2 := f.openOver(t, []SyncMark{recorded})

	if got := j2.Stats().Counters.SyncReappendedBytes; got == 0 {
		t.Fatalf("SyncReappendedBytes = %d after an open whose device lost the segment's tail, want the re-append's bytes counted", got)
	}
	moved := heldLocation(t, j2, id, 0)
	if moved.Segment == seg {
		t.Fatalf("the lost tail's record is still served from segment %d, want it served from the re-append's sealed copy", seg)
	}

	m := j2.Stats().SyncMarks
	entry, ok := m[uint32(recorded.Stream)]
	if !ok {
		t.Fatalf("SyncMarks after the open = %+v, want an entry for stream %d", m, recorded.Stream)
	}
	if entry.Sequence != tailSeq {
		t.Errorf("the entry for stream %d names sequence %d, want the re-appended record's %d", recorded.Stream, entry.Sequence, tailSeq)
	}
	if entry.Segment != uint64(moved.Segment) {
		t.Errorf("the entry for stream %d names segment %d, want the sealed copy's target %d", recorded.Stream, entry.Segment, moved.Segment)
	}
	// A recovered entry's Through and Offset both name the end of the
	// stream's last verifying record, the re-appended copy included.
	if want := moved.Offset + int64(tailSize); entry.Offset != want || entry.Through != want {
		t.Errorf("the entry for stream %d names through %d and offset %d, want both at the re-appended copy's end %d", recorded.Stream, entry.Through, entry.Offset, want)
	}
	for stream, e := range m {
		if e.Segment == uint64(seg) {
			t.Errorf("the entry for stream %d names the old segment %d the emptying unlinked: %+v", stream, seg, e)
		}
	}

	// The returned map is the caller's: changing and deleting in it reaches
	// no later Stats.
	stream := uint32(recorded.Stream)
	m[stream] = SyncMark{}
	if got := j2.Stats().SyncMarks[stream]; got.Sequence != tailSeq {
		t.Fatalf("a changed SyncMarks entry reached the next Stats: got %+v, want the stream's entry with sequence %d", got, tailSeq)
	}
	delete(m, stream)
	if got, ok := j2.Stats().SyncMarks[stream]; !ok || got.Sequence != tailSeq {
		t.Fatalf("a deleted SyncMarks entry reached the next Stats: got %+v (present %v), want the stream's entry with sequence %d", got, ok, tailSeq)
	}
}

// TestStatsSyncMarksLoweredAfterDeviceLoss checks R34's acceptance over a
// device-lost tail the caller's recorded mark covers: the open re-appends
// the acknowledged record the device lost, and its stream's entry names the
// recovering record — its copy's own end, below the end the recorded mark
// claimed — so the group commit's next key is written lower. The next open,
// handed the recovered entry the commit recorded, answers the same numbers
// again with no re-append, no torn tail and no loss: an idle stream does not
// report the same loss at every open.
func TestStatsSyncMarksLoweredAfterDeviceLoss(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	id := fileOf(4)

	// The stream's first record, whose sync the device holds whole: the tail
	// the device then loses sits above it, so the recovering record's copy
	// lands lower than the placement the recorded mark named.
	writeReady(t, h, id, strings.Repeat("the durable half ", 256))
	if _, err := h.Sync(context.Background(), id); err != nil {
		t.Fatalf("Sync of the stream's first record: %v", err)
	}
	first := heldLocation(t, j, id, 0)

	// The record the device acknowledges and then loses: its storage call,
	// and every later sync of its segment, reports success and puts
	// nothing on the device, so the mark the Sync returns names a
	// placement the device holds nowhere.
	f.droppedSyncs(format.SegmentName(core.SegmentID(first.Segment)))
	writeReady(t, h, id, strings.Repeat("acknowledged and lost ", 200))
	held := heldLocation(t, j, id, 0)
	if held.Offset <= first.Offset {
		t.Fatalf("the tail's record landed at %d, want above the stream's first record at %d", held.Offset, first.Offset)
	}
	marks, err := h.Sync(context.Background(), id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("the acknowledged sync: marks %+v, err %v, want its stream's one mark", marks, err)
	}
	recorded := marks[0]
	tailSeq, tailSize := f.viewRecord(t, held.Segment, held.Offset)

	f.killJournal(t, j)
	j2 := f.openOver(t, []SyncMark{recorded})

	m := j2.Stats().SyncMarks
	entry, ok := m[uint32(recorded.Stream)]
	if !ok {
		t.Fatalf("SyncMarks after the open = %+v, want an entry for stream %d", m, recorded.Stream)
	}
	if entry.Sequence != tailSeq {
		t.Errorf("the recovered entry names sequence %d, want the lost-and-re-appended record's %d", entry.Sequence, tailSeq)
	}
	moved := heldLocation(t, j2, id, 0)
	if entry.Segment != uint64(moved.Segment) || entry.Segment == uint64(held.Segment) {
		t.Errorf("the recovered entry names segment %d, want the sealed copy's segment %d the open recovered the record into", entry.Segment, moved.Segment)
	}
	if want := moved.Offset + int64(tailSize); entry.Offset != want || entry.Through != want {
		t.Errorf("the recovered entry names through %d and offset %d, want both at the recovering record's end %d", entry.Through, entry.Offset, want)
	}
	if entry.Offset >= recorded.Offset {
		t.Errorf("the recovered entry reads at offset %d, want below the recorded mark's %d: the group commit's next key goes lower, at the recovering record", entry.Offset, recorded.Offset)
	}

	// The idle stream: the recovered entry is what the group commit records
	// and the next open hands back, and the open over it reports the same
	// numbers and no loss.
	if err := j2.Close(); err != nil {
		t.Fatalf("close the reopened journal: %v", err)
	}
	j3 := f.openOver(t, []SyncMark{entry})
	st := j3.Stats()
	if got := st.Counters.SyncReappendedBytes; got != 0 {
		t.Errorf("SyncReappendedBytes = %d at the reopen over the recovered entry, want 0: an open whose marks name what verifies re-appends nothing", got)
	}
	if got := st.Counters.TornTails; got != 0 {
		t.Errorf("TornTails = %d at the reopen over the recovered entry, want 0: the recovered record verifies where its copy sealed it", got)
	}
	again, ok := st.SyncMarks[uint32(recorded.Stream)]
	if !ok {
		t.Fatalf("SyncMarks at the reopen = %+v, want the stream's entry again", st.SyncMarks)
	}
	if again != entry {
		t.Errorf("the stream's entry at the reopen = %+v, want the entry recovered before it: %+v: an idle stream does not re-report its loss", again, entry)
	}
}

// TestStatsSyncMarksAfterAReopenedCopy checks that a copy the open sealed,
// whose header names no stream because its records come from several, is
// never read back as stream 0's segment: the reopen over it, with no mark
// naming the copy, gives the re-appended record's stream no entry from it and
// stream 0 none either, rather than handing stream 0 another stream's
// record.
func TestStatsSyncMarksAfterAReopenedCopy(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	id := fileOf(4)

	writeReady(t, h, id, strings.Repeat("the durable half ", 256))
	if _, err := h.Sync(context.Background(), id); err != nil {
		t.Fatalf("Sync of the stream's first record: %v", err)
	}
	first := heldLocation(t, j, id, 0)
	f.droppedSyncs(format.SegmentName(core.SegmentID(first.Segment)))
	writeReady(t, h, id, strings.Repeat("acknowledged and lost ", 200))
	marks, err := h.Sync(context.Background(), id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("the acknowledged sync: marks %+v, err %v, want its stream's one mark", marks, err)
	}
	recorded := marks[0]
	if recorded.Stream == 0 {
		t.Fatalf("the file's stream is 0, want a stream other than 0 so a mark wrongly handed to stream 0 shows")
	}

	f.killJournal(t, j)
	j2 := f.openOver(t, []SyncMark{recorded})
	if _, ok := j2.Stats().SyncMarks[uint32(recorded.Stream)]; !ok {
		t.Fatalf("SyncMarks after the re-appending open = %+v, want an entry for stream %d", j2.Stats().SyncMarks, recorded.Stream)
	}
	if err := j2.Close(); err != nil {
		t.Fatalf("close the re-appending open: %v", err)
	}

	// The reopen: no mark names the copy the second open sealed.
	j3 := f.openOver(t, nil)
	copySeg := heldLocation(t, j3, id, 0).Segment
	for stream, e := range j3.Stats().SyncMarks {
		if e.Segment == uint64(copySeg) {
			t.Errorf("the reopen's entry for stream %d = %+v, want none naming the copy %d: its records belong to stream %d, and its header names no stream", stream, e, copySeg, recorded.Stream)
		}
	}
}

// TestEmptiedDamagedSegmentCountsUntilClose checks R45's acceptance: a
// damaged segment the open empties — its records copied out and its file
// unlinked with a synced removal — stays counted in DamagedSegments under its
// old id until the journal closes, and the close takes the count with it.
// The damage is a torn header slot a caller's mark proves synced, the one
// shape a torn header is reported as damaged in at all, and the same torn
// header is what ends the segment the emptying unlinks.
func TestEmptiedDamagedSegmentCountsUntilClose(t *testing.T) {
	f := seededJournal(t)
	seg := core.SegmentID(945)
	file := fileOf(6)

	// The segment: a header slot whose checksum does not verify — torn, not
	// zeroed — with one write record following that verifies whole, for the
	// walk attaches an unverified-header segment's records by their own
	// bytes alone.
	rec := plantRecord(1, ownerTag, file, 30, 40, 0, 32)
	rec.Identity = core.JournalID(f.id)
	rec.Segment = uint64(seg)
	b := raw.SegmentHeader{Identity: [16]byte(f.id), ID: uint64(seg)}.Bytes()
	rec.RecordOffset = uint32(len(b))
	recordEnd := int64(len(b)) + int64(len(rec.Bytes()))
	b = append(b, rec.Bytes()...)
	b[47] ^= 0xff // the header's checksum: the slot tears
	plantSealedBytes(t, f, format.SegmentName(seg), b)

	// The caller's mark proves the header slot synced — a sync covered its
	// bytes whole — and covers the segment's one record.
	mark := SyncMark{
		Stream:   0,
		Segment:  uint64(seg),
		Through:  format.SegmentHeaderSize,
		Offset:   recordEnd,
		Sequence: rec.Seq,
	}
	j := f.openOver(t, []SyncMark{mark})

	if got := j.Stats().DamagedSegments; got != 1 {
		t.Fatalf("DamagedSegments = %d after the open, want 1: the damaged segment the open reports stays counted", got)
	}
	if slices.Contains(f.segmentFileNames(t), format.SegmentName(seg)) {
		t.Fatalf("the damaged segment %d still holds its file, want the emptying to have unlinked it", seg)
	}
	// The emptying copied the segment's record out and unlinked the file, so
	// no entry may name it: an entry names a segment that stands.
	for stream, e := range j.Stats().SyncMarks {
		if e.Segment == uint64(seg) {
			t.Errorf("the entry for stream %d names segment %d, whose file the emptying unlinked: %+v", stream, seg, e)
		}
		if !slices.Contains(f.segmentFileNames(t), format.SegmentName(core.SegmentID(e.Segment))) {
			t.Errorf("the entry for stream %d names segment %d, which is not on disk: %+v", stream, e.Segment, e)
		}
	}
	// The count holds until the journal closes; the close serves the zero
	// Stats, so the row went with it.
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
	if got := j.Stats().DamagedSegments; got != 0 {
		t.Fatalf("DamagedSegments = %d after Close, want 0: the damaged row lives no longer than the journal", got)
	}
}

// recordedSpan is one span a recordingTracer opened.
type recordedSpan struct {
	op    SpanOp
	attrs []slog.Attr
}

// recordingTracer records every span the journal opens, with the attributes
// the emitter passed, under a mutex: the journal's background work opens
// spans beside the open's own.
type recordingTracer struct {
	mu    sync.Mutex
	spans []recordedSpan
}

func (rt *recordingTracer) tracer(ctx context.Context, op SpanOp, attrs ...slog.Attr) (context.Context, func(error)) {
	rt.mu.Lock()
	rt.spans = append(rt.spans, recordedSpan{op: op, attrs: attrs})
	rt.mu.Unlock()
	return ctx, func(error) {}
}

// the spans recorded for the op, in the order they opened.
func (rt *recordingTracer) of(op SpanOp) []recordedSpan {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []recordedSpan
	for _, s := range rt.spans {
		if s.op == op {
			out = append(out, s)
		}
	}
	return out
}

// TestTheOpenSpanCountsItsSegments checks the open span's own contract: each
// phase the open runs opens a child span under it, and the segment counts
// the open kept — how many segments it read by their catalog and how many by
// scan — ride the emptying's work span as attributes, the same counts the
// opened journal's Stats reports as its Recovery.
func TestTheOpenSpanCountsItsSegments(t *testing.T) {
	f := seededJournal(t)
	plantSegment(t, f.seam, core.SegmentID(951),
		plantRecord(1, ownerTag, fileOf(1), 10, 10, 0, 32))
	plantSegment(t, f.seam, core.SegmentID(952),
		plantRecord(1, ownerTag, fileOf(2), 11, 11, 0, 32))

	// The open runs on a clock the tracer advances when the rebuild phase
	// opens, so the duration the Stats report is the advance and nothing else.
	const took = 37 * time.Millisecond
	clock := kit.NewStopwatchClock()
	rt := &recordingTracer{}
	tracer := func(ctx context.Context, op SpanOp, attrs ...slog.Attr) (context.Context, func(error)) {
		if op == core.SpanOpenRebuild {
			clock.Advance(took)
		}
		return rt.tracer(ctx, op, attrs...)
	}
	opts := append(f.opts(nil), WithClock(clock), WithTracer(tracer))
	j := f.openTraced(t, opts)

	if got := len(rt.of(core.SpanOpen)); got != 1 {
		t.Fatalf("the open opened %d spans of its own op, want the one the recovery runs under", got)
	}
	for _, op := range []SpanOp{core.SpanOpenAttach, core.SpanOpenRebuild, core.SpanOpenMarksRaise, core.SpanOpenTailProof,
		core.SpanOpenMarksLostTail, core.SpanOpenSpares, core.SpanOpenReappend, core.SpanOpenAssembly, core.SpanOpenSeal, core.SpanOpenEmptying} {
		if got := len(rt.of(op)); got == 0 {
			t.Errorf("the open opened no child span of op %s", op)
		}
	}

	// The emptying's work span is the one that carries the counts; the
	// recovery's no-op span beside it carries none.
	var attrs map[string]int64
	for _, sp := range rt.of(core.SpanOpenEmptying) {
		m := map[string]int64{}
		for _, a := range sp.attrs {
			if a.Value.Kind() == slog.KindInt64 {
				m[a.Key] = a.Value.Int64()
			}
		}
		if _, ok := m["catalog_segments"]; ok {
			attrs = m
		}
	}
	if attrs == nil {
		t.Fatalf("no span of op %s carries catalog_segments, want the emptying's work span to", core.SpanOpenEmptying)
	}
	rec := j.Stats().Recovery
	if rec.CatalogSegments != 0 || rec.ScannedSegments != 2 {
		t.Errorf("Recovery counts = %d by catalogue and %d by scan, want 0 and 2: the two planted segments, neither sealed with a catalogue", rec.CatalogSegments, rec.ScannedSegments)
	}
	if rec.Duration != took {
		t.Errorf("Recovery.Duration = %v, want %v: the advance the test clock made during the open", rec.Duration, took)
	}
	if got, ok := attrs["catalog_segments"]; !ok || got != int64(rec.CatalogSegments) {
		t.Errorf("the emptying's work span carries catalog_segments %d (present %v), want %d as the Stats reports", got, ok, rec.CatalogSegments)
	}
	if got, ok := attrs["scanned_segments"]; !ok || got != int64(rec.ScannedSegments) {
		t.Errorf("the emptying's work span carries scanned_segments %d (present %v), want %d as the Stats reports", got, ok, rec.ScannedSegments)
	}
}

// openTraced opens the fixture's directory through the production Open under
// the options opts, and closes what it answered when the test ends.
func (f *assembledJournal) openTraced(t *testing.T, opts []Option) *Journal {
	t.Helper()
	j, err := Open(context.Background(), f.dir, Version{}, f.id, nil, opts...)
	if err != nil {
		t.Fatalf("the public Open over %s: %v", f.dir, err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the opened journal: %v", err)
		}
	})
	return j
}
