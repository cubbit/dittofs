package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// seamedIdentity is the journal identity newSeamedJournal writes under.
var seamedIdentity = core.JournalID{0xd, 0xa, 0x7, 0xa}

// TestHandleSync_PublicSyncsItsOwnAndRefusesAnotherShares syncs a written
// file through its handle, and the same file through another share's handle,
// which is refused with an OtherShareError and returns no marks. The
// owner's sync returns one mark, naming the share's write exactly: the
// stream and segment the seam saw the record placed in, the record's end
// and the sequence number its placed header carries, and a synced-through
// offset that covers it.
func TestHandleSync_PublicSyncsItsOwnAndRefusesAnotherShares(t *testing.T) {
	j, seam := newSeamedJournal(t)
	withLimit(t, j)
	a, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share 1: %v", err)
	}
	b, err := j.Share(2, 0, Version{})
	if err != nil {
		t.Fatalf("Share 2: %v", err)
	}
	id := fileOf(7)
	v := writeReady(t, a, id, "hello")

	marks, err := a.Sync(context.Background(), id)
	if err != nil {
		t.Fatalf("Sync through the owner = %v, want nil", err)
	}
	if len(marks) != 1 {
		t.Fatalf("the owner's Sync returned %d marks, want the one of the write's stream: %+v", len(marks), marks)
	}
	placed := placedWrite(t, seam, id, v)
	if marks[0].Stream != placed.stream {
		t.Errorf("mark.Stream = %d, want the stream the record's segment names, %d", marks[0].Stream, placed.stream)
	}
	if marks[0].Segment != placed.segment {
		t.Errorf("mark.Segment = %d, want the record's segment %d", marks[0].Segment, placed.segment)
	}
	if want := placed.off + placed.size; marks[0].Offset != want {
		t.Errorf("mark.Offset = %d, want the record's end %d", marks[0].Offset, want)
	}
	if marks[0].Sequence != placed.seq {
		t.Errorf("mark.Sequence = %d, want the record's %d", marks[0].Sequence, placed.seq)
	}
	if marks[0].Through < marks[0].Offset {
		t.Errorf("mark.Through = %d, want at least the record's end %d", marks[0].Through, marks[0].Offset)
	}

	var other *OtherShareError
	marks, err = b.Sync(context.Background(), id)
	if !errors.As(err, &other) {
		t.Fatalf("Sync through another share = %v, want an OtherShareError", err)
	}
	if marks != nil {
		t.Errorf("the refused Sync returned marks %+v, want none: a refusal syncs nothing to prove", marks)
	}
}

// TestHandleSync_ASealedSegmentsRecordProvesNoMark covers the journal's own
// Config.Sealed wiring, which the durability rig cannot reach: a file whose
// only record sits in a segment a roll sealed -- the seal is the record's
// proof, so the mark machinery must filter it -- returns no mark, whether or
// not the layer falls back to it from the index (Config.Latest, which the
// same call exercises). A file whose record sits on the other stream, in an
// unsealed segment, is marked beside it, so a wiring that answered unsealed
// for everything would hand a mark for the sealed record too: two marks
// instead of one, and this is the check that catches it.
func TestHandleSync_ASealedSegmentsRecordProvesNoMark(t *testing.T) {
	j, seam := newSeamedJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share 1: %v", err)
	}
	id := fileOf(7)
	seg := placedWriteOf(t, seam, h, id, "tiny").segment

	// One further file on the sealed record's stream -- found by probing,
	// since only the seam says where a write landed -- and one on the other
	// stream: the roll below seals the first stream's segment holding both
	// records, and the other stream's segment never rolls.
	var sealed, other FileID
	var otherAt placedRecord
	for b := byte(2); sealed == (FileID{}) || other == (FileID{}); b++ {
		if b == 0 {
			t.Fatalf("probing the streams ran out of files")
		}
		if c := fileOf(b); c != id {
			if at := placedWriteOf(t, seam, h, c, "probe"); at.segment == seg {
				sealed = c
			} else if other == (FileID{}) {
				other, otherAt = c, at
			}
		}
	}

	// A write bigger than the segment's remainder, to the same file, splits
	// and rolls: the roll seals the segment the two records sit in.
	big := make([]byte, 96<<10)
	for i := range big {
		big[i] = byte(i)
	}
	writeReady(t, h, id, string(big))

	marks, err := h.Sync(context.Background(), sealed)
	if err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if len(marks) != 0 {
		t.Fatalf("Sync of the sealed segment's file returned marks %+v, want none: the wiring must answer sealed for segment %d, whose proof the roll's seal already wrote", marks, seg)
	}
	marks, err = h.Sync(context.Background(), sealed, other)
	if err != nil {
		t.Fatalf("Sync of both files = %v, want nil", err)
	}
	if len(marks) != 1 {
		t.Fatalf("Sync returned %d marks, want one: the sealed record's stream alone filtered, and a wiring that answered unsealed for everything would hand a mark for the sealed record too -- %+v", len(marks), marks)
	}
	// The surviving mark comes through the real latestRecord wiring -- the
	// other file's record is synced, so the fallback names it -- and is
	// asserted whole, so that the wiring's own reading of the record (its
	// stream association, its length, its sequence number) is guarded, not
	// only the segment the rig's copy would find.
	m := marks[0]
	if m.Stream != otherAt.stream {
		t.Errorf("mark.Stream = %d, want the stream the record's segment names, %d", m.Stream, otherAt.stream)
	}
	if m.Segment != otherAt.segment {
		t.Errorf("the mark names segment %d, want the other file's record's segment %d", m.Segment, otherAt.segment)
	}
	if m.Segment == seg {
		t.Errorf("the mark names the sealed segment %d", seg)
	}
	if want := otherAt.off + otherAt.size; m.Offset != want {
		t.Errorf("mark.Offset = %d, want the record's end %d", m.Offset, want)
	}
	if m.Sequence != otherAt.seq {
		t.Errorf("mark.Sequence = %d, want the record's %d", m.Sequence, otherAt.seq)
	}
	if m.Through < m.Offset {
		t.Errorf("mark.Through = %d, want at least the record's end %d", m.Through, m.Offset)
	}
	if dur := durableLen(t, seam, otherAt.segment); m.Through > dur {
		t.Errorf("mark.Through = %d, want at most the segment's durable length %d: a synced-through offset proves only durable bytes", m.Through, dur)
	}
}

// durableLen returns the length of the segment's durable image, the most any
// honest synced-through offset of it can claim.
func durableLen(t *testing.T, seam *kit.Seam, seg uint64) int64 {
	t.Helper()
	raw, err := seam.Durable(format.SegmentName(core.SegmentID(seg)))
	if err != nil {
		t.Fatalf("reading the durable bytes of segment %d: %v", seg, err)
	}
	return int64(len(raw))
}

// placedRecord is the durable write record placedWrite reports: its place
// and the stream its segment's header names.
type placedRecord struct {
	stream  uint32
	segment uint64
	off     int64
	size    int64
	seq     uint64
}

// placedWriteOf writes the payload, syncs it and reports its placed record:
// placedWrite of a record the sync made durable, since the seam's durable
// image holds only what a sync reached.
func placedWriteOf(t *testing.T, seam *kit.Seam, h *Handle, id FileID, payload string) placedRecord {
	t.Helper()
	v := writeReady(t, h, id, payload)
	if _, err := h.Sync(context.Background(), id); err != nil {
		t.Fatalf("Sync of %v = %v, want nil", id, err)
	}
	return placedWrite(t, seam, id, v)
}

// placedWrite finds the durable write record of version v of the file id, as
// the journal placed and synced it, with the stream its segment's header
// names. It fails the test when no such record is durable, and reports the
// first it finds.
func placedWrite(t *testing.T, seam *kit.Seam, id FileID, v Version) placedRecord {
	t.Helper()
	for seg := core.SegmentID(1); seg < 64; seg++ {
		raw, err := seam.Durable(format.SegmentName(seg))
		if err != nil || len(raw) < format.SegmentHeaderSize {
			continue
		}
		head, err := format.DecodeSegmentHeader(raw[:format.SegmentHeaderSize], seamedIdentity)
		if err != nil {
			continue // a spare or a segment the journal never opened
		}
		off := int64(format.SegmentHeaderSize)
		for off+format.RecordHeaderSize <= int64(len(raw)) {
			rh, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], seamedIdentity, seg, off)
			if err != nil {
				break // the zero tail of a segment ends the scan
			}
			if rh.Kind == format.KindWrite && rh.File == id && rh.Version == v {
				return placedRecord{
					stream:  head.Stream,
					segment: uint64(seg),
					off:     off,
					size:    rh.Size(),
					seq:     uint64(rh.Seq),
				}
			}
			off += rh.Size()
		}
	}
	t.Fatalf("no durable write of version %v of file %v is placed", v, id)
	return placedRecord{}
}
