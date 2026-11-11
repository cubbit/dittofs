package recovery

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// asideRecord is the header of one record the rebuild does not feed — a
// loss, hold, unhold, stamp or forget record — kept for the phases that
// apply those kinds' own rules and read their bodies there. The header
// verified; its body was not read here.
type asideRecord struct {
	Header format.RecordHeader
}

// segmentScan is what one own segment's walk found: the index records its
// content kinds map to, the aside kinds' headers, the tail facts the later
// phases judge by — whether the walk resynchronised past a bad range, the
// highest synced-through offset any verifying record names, the seal
// marker — and where it ended. Size is the segment file's own size.
type segmentScan struct {
	ID      core.SegmentID
	Records []index.Record
	Aside   []asideRecord

	// Resynced is the walk's resync count contribution: a RangeBad range
	// directly followed by an attached record counts the scan once, however
	// many ranges it crossed.
	Resynced bool

	// SyncedThrough is the highest SyncedThrough any verifying record named.
	SyncedThrough int64

	Sealed bool
	// End is where the walk stopped: the seal marker's offset, the failing
	// slot's offset, or the file's size.
	End int64
	// Size is the segment file's own size, as its storage reported.
	Size int64
}

// readerAt adapts the open's reader to io.ReaderAt for one file name, so
// every byte the scan walks goes through the retry bound the open's other
// reads take. A read the bound did not clear comes back as itself: the
// range the reader collected answers the open, never a verdict these bytes
// never earned.
type readerAt struct {
	rd   *reader
	name string
}

// ReadAt reads len(p) bytes at off through the bound, as io.ReaderAt.
func (a readerAt) ReadAt(p []byte, off int64) (int, error) {
	return a.rd.ReadAt(a.name, p, off)
}

// scanSegment walks one attached own segment by its record headers alone
// and turns each verifying record into what the rebuild consumes: content
// kinds into index records, loss, hold, unhold, stamp and forget headers
// into the aside list, and the tail facts — resync, synced-through, seal —
// into the scan. identity is the journal the segment headers must name;
// info is the segment as the attach pass classed it.
//
// Every read the walk makes goes through the open's bound, so an error the
// bound did not clear returns as itself and the open refuses on the range
// the reader collected; a cancellation the open itself caused returns as
// the context's error, collected nowhere. The walk's own stops — a header
// of an unknown kind, a record whose version carries a non-zero epoch —
// fail the scan naming the segment, as does a record whose version counter
// or sequence number is the largest 64-bit value, which this journal never
// issues: each comes from damage or another writer, and none may be
// stepped over or served.
//
// A segment too short to hold the first record slot scans empty: its bytes
// are the tail phases' to judge, and nothing here fails the open.
//
// decision: a reconcile or reconciled record fails the scan with
// ErrNotImplemented rather than being fed, skipped or set aside: the
// metadata reconcile that record names is not built, and an open that
// dropped it would serve without knowing a reconcile was owed. The
// ceiling is an open that cannot serve such a segment at all — no
// reconcile record may be read past until the reconcile work lands, which
// is what withdraws this marker.
func scanSegment(rd *reader, identity core.JournalID, info SegmentInfo) (segmentScan, error) {
	// The size is opened through the same fs the bounded reads take but
	// outside the bound itself: the bound retries reads of bytes, never the
	// one length every read is measured against, and a size the device will
	// not report answers the open with its own error rather than a scan
	// over bytes nobody measured.
	size, err := segSize(rd, info.ID)
	if err != nil {
		return segmentScan{ID: info.ID}, err
	}

	sc := segmentScan{ID: info.ID, Size: size}
	if size < format.SegmentHeaderSize+format.RecordHeaderSize {
		// No slot to walk: the file holds its header — the attach pass
		// verified it — and less than a record beyond. The tail phases
		// judge the bytes; nothing here fails the open.
		return sc, nil
	}

	walk, werr := format.ScanSegmentHeaders(readerAt{rd: rd, name: format.SegmentName(info.ID)},
		size, identity, info.ID)
	sc.End = walk.End
	sc.Sealed = walk.Sealed
	sc.Resynced = walk.Resynced

	for _, rec := range walk.Records {
		h := rec.Header
		if h.SyncedThrough > sc.SyncedThrough {
			sc.SyncedThrough = h.SyncedThrough
		}
		switch {
		case h.Kind <= format.KindUnmark: // the content kinds, 1–9
			r, cerr := contentRecord(rd, identity, h)
			if cerr != nil {
				return sc, cerr
			}
			sc.Records = append(sc.Records, r)
		case h.Kind <= format.KindForget: // the aside kinds, 10–14
			sc.Aside = append(sc.Aside, asideRecord{Header: h})
		default: // the reconcile kinds, 15 and 16
			return sc, fmt.Errorf("%w: a %s record in segment %d", core.ErrNotImplemented, h.Kind, info.ID)
		}
	}
	if werr != nil {
		// The walk's stop: an unknown kind or a non-zero epoch — the
		// format's own errors, wrapped here with the segment the open must
		// refuse. A read error the bound did not clear never takes this
		// branch's verdicts: it is returned as itself below, and the open
		// refuses on the collected range.
		if errors.Is(werr, format.ErrUnknownKind) || errors.Is(werr, format.ErrNonZeroEpoch) {
			return sc, fmt.Errorf("%w: segment %d: %v", core.ErrUnknownFormat, info.ID, werr)
		}
		return sc, werr
	}
	return sc, nil
}

// largestCounter is the largest 64-bit value, the number this journal never
// issues: its version limit is exclusive above one below the top, so a
// record carrying it — as a version counter or a sequence number — comes
// from damage or another writer and fails the open.
const largestCounter = math.MaxUint64

// contentRecord converts one content-kind record header into the index
// record the rebuild feeds, reading a clone target's 56-byte spec at the
// record's own offset through the bound — the one body the walk left
// unread that the index record needs, verified whole before it is used.
func contentRecord(rd *reader, identity core.JournalID, h format.RecordHeader) (index.Record, error) {
	if h.Version.Epoch != 0 || h.Version.Counter == largestCounter || h.Seq == core.Seq(largestCounter) {
		return index.Record{}, fmt.Errorf("%w: segment %d: the record at %d carries version %v, seq %d",
			core.ErrUnknownFormat, h.Segment, h.Offset, h.Version, h.Seq)
	}

	r := index.Record{
		File:    h.File,
		Tag:     h.Tag,
		Numbers: index.Numbers{Version: h.Version, Seq: h.Seq},
	}
	switch h.Kind {
	case format.KindWrite:
		r.Kind = index.RecWrite
	case format.KindFill:
		r.Kind = index.RecFill
	case format.KindRelease:
		r.Kind = index.RecRelease
	case format.KindTruncate:
		// Off is the new size and Len is 0: the removal runs to the file's
		// end.
		r.Kind = index.RecTruncate
		r.Off, r.Len = h.Extent.Off, 0
	case format.KindDeallocate:
		r.Kind = index.RecDeallocate
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
	case format.KindDelete:
		// Both 0: the removal runs to the file's end.
		r.Kind = index.RecDelete
	case format.KindCloneTarget:
		// The extent is the spec's DstOff and Len; the spec itself is the
		// body the walk did not read.
		r.Kind = index.RecCloneTarget
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
		spec, err := cloneSpec(rd, identity, h)
		if err != nil {
			return index.Record{}, err
		}
		r.Clone = spec
	case format.KindOffloaded:
		r.Kind = index.RecOffloaded
	case format.KindUnmark:
		r.Kind = index.RecUnmark
	}
	if r.Kind != index.RecTruncate && r.Kind != index.RecDelete && r.Kind != index.RecCloneTarget {
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
	}
	r.Segment = h.Segment
	r.At = uint32(h.Offset)
	return r, nil
}

// cloneSpec reads a clone target record's 56-byte body at the record's own
// offset through the bound and verifies it whole, so no part of a record
// that does not verify is used. A body that does not verify fails the open
// naming the segment and the record's offset — never silently dropped, for
// the destination's extent would then read as zeros.
func cloneSpec(rd *reader, identity core.JournalID, h format.RecordHeader) (*core.CloneSpec, error) {
	body := make([]byte, h.BodySize())
	n, rerr := rd.ReadAt(format.SegmentName(h.Segment), body, h.Offset+format.RecordHeaderSize)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A read the bound did not clear returns as itself — a
		// cancellation or a device fault, either answered by the caller:
		// the open refuses on the collected range for the fault, or with
		// the context's own error for the cancellation.
		return nil, rerr
	}
	if int64(n) < h.BodySize() {
		return nil, fmt.Errorf("%w: segment %d: the clone target at %d holds %d of its %d body bytes",
			core.ErrCorrupt, h.Segment, h.Offset, n, h.BodySize())
	}
	verified, verr := format.VerifyRecordBody(body, identity, h)
	if verr != nil {
		return nil, fmt.Errorf("%w: segment %d: the clone target at %d: %v",
			core.ErrCorrupt, h.Segment, h.Offset, verr)
	}
	return &core.CloneSpec{
		SrcShare: verified.Clone.Share,
		Src:      verified.Clone.File,
		SrcOff:   verified.Clone.Off,
		DstOff:   h.Extent.Off,
		Len:      h.Extent.Len,
		AsOf:     verified.Clone.AsOf,
	}, nil
}

// segSize returns the segment file's size, opened through the same fs the
// bounded reads take. The open is outside the read bound: the bound
// retries reads of bytes, and a size is the one length those reads are
// measured against, not bytes the device faulted on.
func segSize(rd *reader, id core.SegmentID) (int64, error) {
	if err := rd.ctx.Err(); err != nil {
		return 0, err
	}
	f, err := rd.fs.Open(format.SegmentName(id))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return f.Size()
}
