package segment_test

import (
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// streamSealRoom is the seal's reserve over n records, the same closed form
// the stream's sealRoom computes: the marker, one footer entry per record and
// the trailer.
func streamSealRoom(n int64) int64 {
	return format.SealMarkerSize + n*format.CatalogEntrySize + format.TrailerSize
}

// streamRoomOf is the payload room a segment of cap C holds once its next free
// slot is at next and it holds n records, the payload grid the stream's own
// fit test draws: the cap less the bytes spent and the seal's reserve over all
// n records, less a record's own overhead, floored whole.
func streamRoomOf(cap, next, n int64) int64 {
	return ((cap - next - streamSealRoom(n)) &^ 7) -
		(int64(format.RecordHeaderSize) + format.RecordHeader{Kind: format.KindWrite}.BodySize())
}

// TestStreamRoom checks the stream's read of the room its active segment holds
// for one more record: zero before the first append installs one, the very
// figure the stream's own fit test draws after a record lands, a figure that
// falls by the record's padded size, and zero — never negative — once nothing
// more fits.
func TestStreamRoom(t *testing.T) {
	e := streamOver(t, streamCap, func(streamEnv) {})
	if got := e.s.Room(); got != 0 {
		t.Fatalf("a fresh stream with no segment reports Room = %d, want 0", got)
	}

	// One write of n bytes to the empty segment: Room is what remains of the
	// cap after the cap-sized slice of address the header's bytes hold, the
	// record's padded size and the seal's reserve over both records, on the
	// payload grid.
	const n = 1000
	h, body := streamWriteHeader(streamFileID(1), 0, make([]byte, n), 0, core.Version{Counter: 1})
	loc := streamAppendWrite(t, e, h, body)
	want := streamRoomOf(streamCap, format.SegmentHeaderSize+loc.Loc.Len, 2)
	if got := e.s.Room(); got != want {
		t.Fatalf("after one %d-byte write of %d padded bytes, Room = %d, want %d", n, loc.Loc.Len, got, want)
	}

	// Filling exactly what Room names leaves the stream reporting zero, never
	// negative: each record that fits is appended whole, and the next read
	// finds whatever remains after the record and its seal's entry.
	for i := 2; e.s.Room() > 0; i++ {
		fill := int(e.s.Room())
		hi, b := streamWriteHeader(streamFileID(1), int64(i)*streamCap, make([]byte, fill), 0, core.Version{Counter: uint64(i)})
		p := streamAppendWrite(t, e, hi, b)
		// The whole record fit only when it filled the segment as told; a
		// piece that did not fit whole was split at the cap and lands in
		// the next segment, so the loop's next read sees the fresh room.
		if p.N == int64(fill) {
			if got := e.s.Room(); got != 0 {
				t.Fatalf("after a write of exactly the reported room, Room = %d, want 0", got)
			}
			break
		}
	}
}

// TestStreamRoomFollowsTheSegmentsLife checks the room Room reads without the
// append lock against every section that moves it: a reservation lowers it to
// the stream's own fit figure, a seal and an end of the segment by a failed
// write zero it, and the segment that takes over after a seal reports its own
// room from its first record on.
func TestStreamRoomFollowsTheSegmentsLife(t *testing.T) {
	e := streamOver(t, streamCap, func(streamEnv) {})
	fid := streamFileID(1)
	afterOne := func(p segment.Placed) int64 {
		return streamRoomOf(streamCap, format.SegmentHeaderSize+p.Loc.Len, 2)
	}

	h, body := streamWriteHeader(fid, 0, make([]byte, 1000), 0, core.Version{Counter: 1})
	first := streamAppendWrite(t, e, h, body)
	if got, want := e.s.Room(), afterOne(first); got != want {
		t.Fatalf("after the first record, Room = %d, want %d", got, want)
	}

	if _, err := e.s.Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := e.s.Room(); got != 0 {
		t.Fatalf("after the seal, Room = %d, want 0", got)
	}

	h, body = streamWriteHeader(fid, 1000, make([]byte, 1000), 0, core.Version{Counter: 2})
	second := streamAppendWrite(t, e, h, body)
	if second.Loc.Segment == first.Loc.Segment {
		t.Fatalf("the append after the seal landed in the sealed segment %d", first.Loc.Segment)
	}
	if got, want := e.s.Room(), afterOne(second); got != want {
		t.Fatalf("after the first record of the segment that took over, Room = %d, want %d", got, want)
	}

	if err := e.seam.Schedule(kit.Failing(kit.OpWrite, streamSegName(second.Loc.Segment), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	h, body = streamWriteHeader(fid, 2000, make([]byte, 1000), 0, core.Version{Counter: 3})
	if _, err := e.s.Append(h, body); err == nil {
		t.Fatal("Append over a failing write returned no error")
	}
	if got := e.s.Room(); got != 0 {
		t.Fatalf("after a failed write ended the segment, Room = %d, want 0", got)
	}
}
