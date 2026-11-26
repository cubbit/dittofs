package segment_test

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
)

// TestStreamAPooledBufferPutsNoStaleByteOnDisk checks that a content record
// encoded into a recycled buffer reaches the disk with none of the earlier
// record's bytes the buffer still holds: an earlier, larger record with a
// 0xFF payload is read back off the segment and seeded into its class, a
// smaller record is appended, and its bytes on the segment — its payload and
// its padding — are read back raw. It catches a buffer handed out unemptied
// and an encode that leaves any byte in its range unwritten.
func TestStreamAPooledBufferPutsNoStaleByteOnDisk(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	// 4096 bytes of payload fill an 8-byte grid exactly: 4200 bytes.
	const staleLen = format.RecordHeaderSize + 8 + 4096
	ph, pb := streamWriteHeader(streamFileID(1), 0, bytes.Repeat([]byte{0xFF}, 4096), 7, core.Version{Counter: 1})
	prev := streamAppendWrite(t, e, ph, pb)
	stale := make([]byte, staleLen)
	if _, err := e.s.ReadAt(prev.Loc.Segment, stale, prev.Loc.Off); err != nil {
		t.Fatalf("ReadAt of the earlier record: %v", err)
	}

	// Several rounds, so that a draw sync.Pool drops under the race
	// detector cannot leave every round off the seeded buffers.
	drawn := 0
	for round := range 4 {
		seeded := segment.SeedRecordBufs(stale, 8)

		// 3001 bytes of payload leave 7 bytes of padding, in the range the
		// earlier record's payload filled.
		payload := bytes.Repeat([]byte{byte(0x10 + round)}, 3001)
		h, body := streamWriteHeader(streamFileID(2+round), 0, payload, 7, core.Version{Counter: uint64(2 + round)})
		p := streamAppendWrite(t, e, h, body)
		end := format.RecordHeaderSize + 8 + len(payload)
		if want := int64(end+7) &^ 7; p.Loc.Len != want {
			t.Fatalf("round %d: the record's length is %d, want %d, its payload's end padded to the 8-byte grid", round, p.Loc.Len, want)
		}

		raw := make([]byte, staleLen)
		if _, err := e.s.ReadAt(p.Loc.Segment, raw, p.Loc.Off); err != nil {
			t.Fatalf("round %d: ReadAt of the record: %v", round, err)
		}
		if got := raw[format.RecordHeaderSize+8 : end]; !bytes.Equal(got, payload) {
			t.Errorf("round %d: the payload on disk holds %d stale 0xFF bytes, want none", round, bytes.Count(got, []byte{0xFF}))
		}
		if got := raw[end:p.Loc.Len]; !bytes.Equal(got, make([]byte, len(got))) {
			t.Errorf("round %d: the padding on disk is %x, want %d zero bytes", round, got, len(got))
		}
		streamReadRecord(t, e, p.Loc)

		for _, b := range seeded {
			if bytes.Equal(b[:p.Loc.Len], raw[:p.Loc.Len]) {
				drawn++
				break
			}
		}
	}
	if drawn == 0 {
		t.Fatalf("no append drew a seeded buffer: the check above ran on fresh buffers only")
	}
}
