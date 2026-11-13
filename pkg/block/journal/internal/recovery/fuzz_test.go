package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// sealedSegment is the bytes of segment id, its header naming pred, holding
// recs and sealed: the seal marker, a footer entry per record and the
// trailer. The offsets of each region come back so a seed can flip one byte
// of it.
type sealedSegment struct {
	b                    []byte
	recOff               []int
	size                 []int64 // each record's whole size, header and body
	markerOff, footerOff int
}

func sealedSeg(id, pred core.SegmentID, recs ...raw.Record) sealedSegment {
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id), Predecessor: uint64(pred)}.Bytes()
	var s sealedSegment
	var entries [][raw.CatalogEntrySize]byte
	for i := range recs {
		recs[i].Identity = journalID
		recs[i].Segment = uint64(id)
		recs[i].RecordOffset = uint32(len(b))
		s.recOff = append(s.recOff, len(b))
		s.size = append(s.size, int64(len(recs[i].Bytes())))
		entries = append(entries, raw.Entry{
			FileID: recs[i].FileID, Offset: recs[i].Offset, RecordOffset: recs[i].RecordOffset,
			Length: recs[i].Length, Seq: recs[i].Seq, Version: recs[i].Version,
			Kind: recs[i].Kind, Tag: recs[i].Tag,
		}.Bytes())
		b = append(b, recs[i].Bytes()...)
	}
	s.markerOff = len(b)
	b = append(b, raw.SealMarker{Identity: journalID, Segment: uint64(id), Offset: uint64(s.markerOff)}.Bytes()...)
	s.footerOff = len(b)
	for _, e := range entries {
		b = append(b, e[:]...)
	}
	s.b = append(b, raw.Trailer(1, uint64(s.markerOff+raw.SealMarkerSize), entries...)...)
	return s
}

// fuzzRecs is the two write records a fixture segment holds: distinct files,
// sequence numbers and versions, under share tag tag.
func fuzzRecs(tag uint32, seq uint64) []raw.Record {
	mk := func(k uint8, s uint64) raw.Record {
		r := rec(k, func(r *raw.Record) {
			r.Tag, r.Seq, r.Version = tag, s, raw.Version{Counter: s}
			r.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, 16)}
		})
		r.Kind = 1
		r.FileID[0] = byte(s) // a file per record
		return r
	}
	// A clone target, so a mutated body drives the dropped-record path.
	c := rec(7, func(r *raw.Record) {
		r.Tag, r.Seq, r.Version = tag, seq+2, raw.Version{Counter: seq + 2}
		r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	})
	c.FileID[0] = byte(seq + 2)
	return []raw.Record{mk(1, seq), mk(1, seq+1), c}
}

// FuzzOpen runs the open's recovery over one mutated segment: the fuzz input
// is segment 2's whole file, planted after a sealed segment 1 it names as
// predecessor. Whatever the bytes, Run either fails with one of the named
// open errors or returns an index whose read-out names only records inside a
// segment it scanned. The format file's own flips are the attach pass's
// refusals and outside this input by design.
func FuzzOpen(f *testing.F) {
	valid := sealedSeg(2, 1, fuzzRecs(7, 10)...)
	f.Add(valid.b)
	for _, at := range []int{
		20,                   // a segment-header byte
		valid.recOff[0] + 40, // a record-header byte
		valid.recOff[1] + raw.RecordHeaderSize + 8 + 3, // a write body's payload byte
		valid.markerOff + 10,                           // a seal-marker byte
		valid.footerOff + 5,                            // a footer-entry byte
		len(valid.b) - 5,                               // a trailer byte
	} {
		b := append([]byte(nil), valid.b...)
		b[at] ^= 0x01
		f.Add(b)
	}
	seg1 := sealedSeg(1, 0, fuzzRecs(1, 1)...)
	f.Fuzz(func(t *testing.T, seg2 []byte) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, seg1.b)
		r.plantSeg(2, seg2)

		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			if !errors.Is(err, core.ErrUnknownFormat) && !errors.Is(err, core.ErrNotImplemented) && !errors.Is(err, core.ErrUnreadable) {
				t.Fatalf("Run: got unnamed error %v, want ErrUnknownFormat, ErrNotImplemented or ErrUnreadable", err)
			}
			return
		}
		size := map[core.SegmentID]int64{}
		for _, sc := range st.Rebuilt.Scans {
			size[sc.ID] = sc.Size
		}
		if len(st.Tails.Fates) != len(st.Rebuilt.Scans) {
			t.Fatalf("Fates %+v, want one per scanned segment (%d)", st.Tails.Fates, len(st.Rebuilt.Scans))
		}
		syncedThrough := map[core.SegmentID]int64{}
		for _, sc := range st.Rebuilt.Scans {
			syncedThrough[sc.ID] = sc.SyncedThrough
		}
		for _, f := range st.Tails.Fates {
			// R50: a cut never goes below a synced-through a verifying header names.
			if (f.Kind == fateTornTail || f.Kind == fateEnded) && f.Offset != 0 && f.Offset < syncedThrough[f.ID] {
				t.Fatalf("fate %+v: cut below the synced-through %d", f, syncedThrough[f.ID])
			}
			if sz := size[f.ID]; f.Offset != 0 && (f.Offset < format.SegmentHeaderSize || f.Offset > sz) {
				t.Fatalf("fate %+v: offset outside [%d,%d]", f, format.SegmentHeaderSize, sz)
			}
		}
		bytesOf := map[core.SegmentID][]byte{1: seg1.b, 2: seg2}
		// header decodes the record header a segment's bytes hold at off, as
		// the scan would read it; false when it does not decode there, where
		// no property about it holds.
		header := func(seg core.SegmentID, off int64) (format.RecordHeader, bool) {
			b := bytesOf[seg]
			if off < 0 || off+format.RecordHeaderSize > int64(len(b)) {
				return format.RecordHeader{}, false
			}
			h, err := format.DecodeRecordHeader(b[off:off+format.RecordHeaderSize], journalID, seg, off)
			return h, err == nil
		}
		indexed := map[int64]bool{} // the record offsets of segment 2 the index names
		for _, tag := range []core.ShareTag{1, 7} {
			for _, id := range st.Rebuilt.Index.Files(tag) {
				for _, e := range st.Rebuilt.Index.FileEntries(id) {
					if e.Record.Segment == 2 {
						indexed[int64(e.Record.Offset)] = true
					}
					if e.Kind != index.EntryHeld && e.Kind != index.EntryRetained {
						continue
					}
					sz, ok := size[e.Record.Segment]
					if !ok || int64(e.Record.Offset) < format.SegmentHeaderSize {
						t.Fatalf("entry %+v: record %+v names no slot of a scanned segment (sizes %v)", e, e.Record, size)
					}
					// The whole record lies inside the segment: a header that
					// decodes where the entry names it sets its size.
					if h, ok := header(e.Record.Segment, int64(e.Record.Offset)); ok && int64(e.Record.Offset)+h.Size() > sz {
						t.Fatalf("entry %+v: record of %d bytes at %d runs past its segment's %d", e, h.Size(), e.Record.Offset, sz)
					}
				}
			}
		}

		// Every seed record whose header still verifies is indexed or dropped
		// as corrupt, when its segment was attached at all. A clone target is
		// left out: its body may rot, which drops it as a torn slot or tail.
		// The walk reads the records in order, so one before this whose
		// verifying header changed its length may have swallowed the rest.
		corrupt := map[int64]bool{}
		for _, c := range st.Tails.Corrupt {
			if c.Segment == 2 {
				corrupt[c.at()] = true
			}
		}
		if _, scanned := size[2]; !scanned {
			return
		}
		for i, off := range valid.recOff {
			h, ok := header(2, int64(off))
			// A record the segment cannot hold is the tail's, neither indexed nor dropped.
			if !ok || h.Size() != valid.size[i] || int64(off)+valid.size[i] > int64(len(seg2)) {
				break
			}
			if h.Kind <= format.KindUnmark && h.Kind != format.KindCloneTarget && !indexed[int64(off)] && !corrupt[int64(off)] {
				t.Fatalf("record %d at %d verifies but is neither indexed nor dropped as corrupt (corrupt %+v)", i, off, st.Tails.Corrupt)
			}
		}
	})
}
