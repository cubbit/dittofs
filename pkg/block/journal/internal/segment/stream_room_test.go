package segment_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
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
