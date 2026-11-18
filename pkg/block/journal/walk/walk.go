// Package walk reads a journal directory's segment files record by record
// with the journal's own scanner, for a tool that looks at the bytes on disk
// without opening the journal: the records that verify, what ended the walk,
// and the body bytes behind each one.
//
// It is a thin seam over internal/format: nothing here decodes a byte of its
// own, so the layout a viewer shows is the layout the journal reads.
package walk

import (
	"errors"
	"io"
	"os"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// Tail is how a segment's walk ended.
type Tail uint8

const (
	// Untaken: what follows the last record is zeros, an unwritten tail.
	Untaken Tail = iota
	// Sealed: a seal marker that verifies ends the segment.
	Sealed
	// Torn: bytes that are neither a record, a seal marker nor zeros, or a
	// header this format version cannot read.
	Torn
)

func (t Tail) String() string {
	switch t {
	case Untaken:
		return "untaken"
	case Sealed:
		return "sealed"
	case Torn:
		return "torn"
	}
	return "unknown"
}

// Record is one record whose header and body both verified.
type Record struct {
	Kind    string
	File    [16]byte
	Off     int64 // file offset the record covers
	Len     int64 // length it covers
	Seq     uint64
	Epoch   uint64
	Counter uint64
	Tag     uint32
	RecOff  int64  // the record's own offset in its segment
	Body    []byte // the bytes after the header, padding excluded
	BodyOff int64  // where Body starts in the segment
}

// Segment is what a walk of one file found.
type Segment struct {
	// Spare is a file that was never taken as a segment: nothing to walk.
	Spare   bool
	Records []Record
	Tail    Tail
}

// File walks the segment file at path under the journal identity id. A file
// whose header is all zeros is a spare. A header that is neither zeros nor a
// verifying segment header of this journal is an error.
func File(path string, id [16]byte) (Segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return Segment{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return Segment{}, err
	}
	size := st.Size()

	hdr := make([]byte, format.SegmentHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil && !errors.Is(err, io.EOF) {
		return Segment{}, err
	}
	h, err := format.DecodeSegmentHeader(hdr, core.JournalID(id))
	if err != nil {
		if allZero(hdr) {
			return Segment{Spare: true}, nil
		}
		return Segment{}, err
	}

	sc, scanErr := format.ScanSegment(f, size, core.JournalID(id), h.ID)
	seg := Segment{Tail: Untaken}
	switch {
	case scanErr != nil:
		seg.Tail = Torn
	case sc.Sealed:
		seg.Tail = Sealed
	default:
		for _, r := range sc.Ranges {
			if r.Kind == format.RangeBad {
				seg.Tail = Torn
			}
		}
	}
	for _, r := range sc.Records {
		hd := r.Header
		n := hd.BodySize()
		body := make([]byte, n)
		bodyOff := hd.Offset + format.RecordHeaderSize
		if n > 0 {
			if _, err := f.ReadAt(body, bodyOff); err != nil {
				return Segment{}, err
			}
		}
		seg.Records = append(seg.Records, Record{
			Kind:    hd.Kind.String(),
			File:    hd.File.Bytes(),
			Off:     hd.Extent.Off,
			Len:     hd.Extent.Len,
			Seq:     uint64(hd.Seq),
			Epoch:   hd.Version.Epoch,
			Counter: hd.Version.Counter,
			Tag:     uint32(hd.Tag),
			RecOff:  hd.Offset,
			Body:    body,
			BodyOff: bodyOff,
		})
	}
	return seg, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
