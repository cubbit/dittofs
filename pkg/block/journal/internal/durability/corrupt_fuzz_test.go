package durability

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// fuzzPayload is the payload of the record the byte flip lands in. Its 12-byte
// body (modification time, then the payload) leaves the record four bytes of
// padding after the body, which no checksum covers.
const fuzzPayload = "abcd"

// errCatalogMismatch is the refusal of a header that verifies but does not
// match the catalog's entry for the record.
var errCatalogMismatch = errors.New("header does not match the catalog entry")

// FuzzCorruptEveryByte flips one byte of a durable write record, at any offset
// of its header, body or padding, and runs the check a reader runs on the
// bytes it read: the header's checksum and place, the header against the
// catalog's entry for the record, and the body's checksum. When that check
// refuses the record, Corrupt runs and the property is that the record's
// extent no longer reads, so no byte of it is served and no zero stands in for
// it, while a neighbour in the same segment still reads. When the check
// accepts the flipped bytes, what it hands out must be exactly what was
// written and the flip must have landed in the padding; a covered byte that
// passes the check is a failure.
//
// It proves the format's checksum coverage, a flipped covered byte being
// refused, and the drop that follows a refusal. The reader that runs the check
// on a real read is not on this base, so the check here stands in for it.
//
// Every offset of the record is a seed, so the plain test run flips each byte
// once; the fuzzer then mutates the offset and the mask.
func FuzzCorruptEveryByte(f *testing.F) {
	size := recordSize()
	for off := range size {
		f.Add(uint16(off), byte(0x01))
	}
	f.Add(uint16(0), byte(0xff))
	f.Add(uint16(size-1), byte(0x80))
	f.Fuzz(func(t *testing.T, offset uint16, mask byte) {
		if mask == 0 {
			return // nothing flipped
		}
		cr := newCorruptRig(t, nil)
		f, g := file(1), file(2)
		w := cr.write(t, f, 0, fuzzPayload)
		nb := cr.write(t, g, 0, "wxyz")
		if w.loc.Segment != nb.loc.Segment {
			t.Fatal("the two records landed in different segments")
		}
		if err := cr.d.Sync(context.Background(), f); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		raw, err := cr.seam.Durable(format.SegmentName(w.loc.Segment))
		if err != nil {
			t.Fatalf("Durable: %v", err)
		}
		if int64(size) != w.loc.Len {
			t.Fatalf("the record takes %d bytes, recordSize says %d", w.loc.Len, size)
		}
		start := w.loc.Off
		rec := append([]byte(nil), raw[start:start+int64(size)]...)
		at := int(offset) % size
		rec[at] ^= mask

		served, err := verifyAsAReader(rec, w, start)
		if err == nil {
			// The check accepted the flipped bytes. Only the padding lies
			// outside both checksums (the header checksum covers bytes
			// [0,92) and the payload checksum the body), so a flip there
			// leaves nothing to handle: skip it after checking that what the
			// reader serves is what was written. A flip anywhere else that
			// passes is a flipped byte served.
			if at < format.RecordHeaderSize+bodySize() {
				t.Fatalf("flipping byte %d with mask %#x passed verification: a flipped byte of the record would be served", at, mask)
			}
			if string(served) != fuzzPayload {
				t.Fatalf("a flip at padding byte %d changed the served payload to %q", at, served)
			}
			return
		}

		cr.corrupt(t, w.c)
		for off := range int64(len(fuzzPayload)) {
			if p, ok := cr.ix.Lookup(f, off); ok {
				t.Fatalf("offset %d still reads %+v after Corrupt of the record flipped at byte %d (mask %#x)", off, p, at, mask)
			}
		}
		if p, ok := cr.ix.Lookup(g, 0); !ok || p.Version != nb.c.Version {
			t.Fatalf("the neighbour reads %+v, %v after Corrupt, want its own version", p, ok)
		}
	})
}

// bodySize is the body bytes of one record of fuzzPayload, the bytes its
// payload checksum covers.
func bodySize() int {
	h := format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: int64(len(fuzzPayload))}}
	return int(h.BodySize())
}

// recordSize is the bytes one record of fuzzPayload takes in its segment.
func recordSize() int {
	h := format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: int64(len(fuzzPayload))}}
	return int(h.Size())
}

// verifyAsAReader is the reader's verification of the bytes of one record: it
// returns the payload it would serve, or the reason it refuses the record.
func verifyAsAReader(rec []byte, w written, start int64) ([]byte, error) {
	h, err := format.DecodeRecordHeader(rec[:format.RecordHeaderSize], rigIdentity, w.loc.Segment, start)
	if err != nil {
		return nil, err
	}
	if h.File != w.c.File || h.Seq != w.c.Seq || h.Version != w.c.Version || h.Extent != w.c.Extent {
		return nil, errCatalogMismatch
	}
	body, err := format.VerifyRecordBody(rec[format.RecordHeaderSize:format.RecordHeaderSize+int(h.BodySize())], rigIdentity, h)
	if err != nil {
		return nil, err
	}
	return body.Payload, nil
}
