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

// placedWrite is the durable write record of version v of the file id, as the
// journal placed and synced it, with the segment's stream number. It fails the
// test when no such record is durable, and reports the first it finds.
type placedRecord struct {
	stream  uint32
	segment uint64
	off     int64
	size    int64
	seq     uint64
}

func placedWrite(t *testing.T, seam *kit.Seam, id FileID, v Version) placedRecord {
	t.Helper()
	for seg := core.SegmentID(1); seg < 64; seg++ {
		raw, err := seam.Durable(format.SegmentName(seg))
		if err != nil || len(raw) < format.SegmentHeaderSize {
			continue
		}
		head, err := format.DecodeSegmentHeader(raw[:format.SegmentHeaderSize], seamedIdentity)
		if err != nil {
			t.Fatalf("decoding the header of segment %d: %v", seg, err)
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
