package datapath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// recordVerdict is what reading one record whole showed.
type recordVerdict uint8

const (
	// recordVerifies is a record whose header decodes at its place, matches
	// the index entries that sent the read there, and whose body matches the
	// header's payload checksum.
	recordVerifies recordVerdict = iota + 1
	// recordHeaderDamaged is a record whose header does not decode at its
	// place: a checksum, a field or the place it names is wrong, and none of
	// its fields can be trusted.
	recordHeaderDamaged
	// recordMismatch is a record whose header decodes but disagrees with the
	// index entries that sent the read there: another file, kind, version or
	// extent.
	recordMismatch
	// recordBodyBad is a record whose header decodes and matches, and whose
	// body does not match the header's payload checksum.
	recordBodyBad
)

// readRecord reads the record at at of file id whole through st and verifies
// it against pieces, the index entries of id that name it. It reads the
// header first, decodes it at its own place and checks it against every
// piece, then reads the body and verifies it against the header's payload
// checksum. It returns the record's payload only when every check passed;
// otherwise payload is nil and verdict says which check failed. h is the
// decoded header, and zero when the header did not decode.
//
// err is an I/O error, a short read, a closed stream or a cancelled ctx: it
// shows nothing about the record, so the verdict is zero and the caller
// changes nothing. A segment that is gone comes back as an error matching
// segment.ErrSegmentGone or fs.ErrNotExist, which segmentGone tells apart.
//
// Each call takes one segment reference, makes its one or two reads through
// it and releases it, so nothing is held when it returns: a retirement of the
// segment that begins while the record is read waits for the reference, and
// one that began before finds the read refused with segment.ErrSegmentGone.
func (c *Core) readRecord(ctx context.Context, st *segment.Stream, id core.FileID, at index.RecordAt, pieces []index.Placed) (payload []byte, h format.RecordHeader, verdict recordVerdict, err error) {
	if err = ctx.Err(); err != nil {
		return nil, format.RecordHeader{}, 0, err
	}
	ref, err := st.AcquireRead(at.Segment)
	if err != nil {
		return nil, format.RecordHeader{}, 0, fmt.Errorf("datapath: reading the record at segment %d offset %d: %w", at.Segment, at.Offset, err)
	}
	// The one reference is released before readRecord returns, so before the
	// caller hands a record that fails a check to Corrupt, whose append domain
	// ranks above the storage guard.
	defer ref.Release()
	hb := make([]byte, format.RecordHeaderSize)
	if err = readFull(ref, at.Segment, hb, int64(at.Offset)); err != nil {
		return nil, format.RecordHeader{}, 0, err
	}
	identity := st.Identity()
	h, derr := format.DecodeRecordHeader(hb, identity, at.Segment, int64(at.Offset))
	if derr != nil {
		return nil, format.RecordHeader{}, recordHeaderDamaged, nil
	}
	// The overrun check comes before the body is sized from the header: a
	// record whose body the segment cannot hold is bad on its header alone,
	// as the scan rules it.
	//
	// decision: the bound is the segment size this core accounts segments
	// at, not the size of the segment's file, which the read does not learn.
	// It holds because that size is fixed for a journal's life: segments
	// written under a larger one would have their records past this one
	// refused as damaged. Overturned by a segment size that can change
	// between opens, which would bound by the segment's own recorded size.
	if int64(at.Offset)+h.Size() > c.segmentCap {
		return nil, format.RecordHeader{}, recordHeaderDamaged, nil
	}
	if !headerMatches(h, id, pieces) {
		return nil, h, recordMismatch, nil
	}
	body := make([]byte, h.BodySize())
	if err = readFull(ref, at.Segment, body, int64(at.Offset)+format.RecordHeaderSize); err != nil {
		return nil, format.RecordHeader{}, 0, err
	}
	rb, verr := format.VerifyRecordBody(body, identity, h)
	if verr != nil {
		return nil, h, recordBodyBad, nil
	}
	return rb.Payload, h, recordVerifies, nil
}

// headerMatches reports whether h is the record every piece of pieces names:
// a record with a payload, of file id, of the piece's version, whose extent
// places the piece's first byte at the piece's Skip into its payload and
// holds the whole piece.
func headerMatches(h format.RecordHeader, id core.FileID, pieces []index.Placed) bool {
	if h.Kind != format.KindWrite && h.Kind != format.KindFill {
		return false
	}
	if h.File != id {
		return false
	}
	for _, pc := range pieces {
		skip := int64(pc.Skip)
		if pc.Version != h.Version || h.Extent.Off+skip != pc.Extent.Off || skip+pc.Extent.Len > h.Extent.Len {
			return false
		}
	}
	return true
}

// readFull reads len(b) bytes of seg at off through ref, a reference to seg.
// A read that fills b and also reports io.EOF read every byte asked for; a
// read that does not fill b is an io.ErrUnexpectedEOF.
func readFull(ref segment.Ref, seg core.SegmentID, b []byte, off int64) error {
	n, err := ref.ReadAt(b, off)
	if n == len(b) && errors.Is(err, io.EOF) {
		err = nil
	}
	switch {
	case err != nil:
		return fmt.Errorf("datapath: reading %d bytes of segment %d at %d: %w", len(b), seg, off, err)
	case n < len(b):
		return fmt.Errorf("datapath: reading %d bytes of segment %d at %d: %w: %d read", len(b), seg, off, io.ErrUnexpectedEOF, n)
	}
	return nil
}

// segmentGone reports whether err is a read of a segment that no longer
// exists: one the descriptor cache knows retired, or one the storage no
// longer finds.
func segmentGone(err error) bool {
	return errors.Is(err, segment.ErrSegmentGone) || errors.Is(err, fs.ErrNotExist)
}
