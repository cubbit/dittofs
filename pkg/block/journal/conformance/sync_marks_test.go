//go:build journal_conformance

package conformance

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// writeLead is how far the payload of a write record sits past the record's
// first byte: the header raw pins, then the 8-byte modification time.
const writeLead = raw.RecordHeaderSize + 8

// castagnoli seeds seededCRC, the checksum the journal writes into every
// record header and payload.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// seededCRC returns the CRC32C of identity followed by b, the seeds the
// journal's checksums grow from.
func seededCRC(identity [16]byte, b []byte) uint32 {
	return crc32.Update(crc32.Checksum(identity[:], castagnoli), castagnoli, b)
}

// foundRecord is one record of a write, located in a segment's image: the
// segment it lies in, the record's own offset there, and the facts its
// header's bytes carry -- the sequence number, and where the record's slot
// ends.
type foundRecord struct {
	seg journal.SegmentID
	off int64
	seq uint64
	end int64
}

// at returns where frr stands in a failure.
func (frr foundRecord) at() string {
	return fmt.Sprintf("segment %d at %d", frr.seg, frr.off)
}

// verifyWrite verifies the write record whose payload p lies at index idx
// of img, the image of segment seg, and returns it. The header must carry
// the payload's own length, the file, and the record's own offset as the
// format writes it, and its checksum must verify, so a match is a record
// the journal placed, not bytes that merely look like the payload.
func verifyWrite(t *testing.T, f *journaltest.Fixture, seg journal.SegmentID, img []byte, idx int, id journal.FileID, p []byte) foundRecord {
	t.Helper()

	off := int64(idx) - writeLead
	if off < raw.SegmentHeaderSize || off%8 != 0 || off+writeLead+int64(len(p)) > int64(len(img)) {
		t.Fatalf("the payload of file %v in segment %d sits at %d, not behind a record's header", id, seg, idx)
	}
	h := img[off : off+raw.RecordHeaderSize]
	if string(h[0:4]) != "DJR1" {
		t.Fatalf("segment %d: the %d bytes before the payload of file %v are not a record header", seg, writeLead, id)
	}

	if length := binary.LittleEndian.Uint64(h[40:48]); length != uint64(len(p)) {
		t.Fatalf("segment %d at %d: the record's header carries length %d, want the payload's %d", seg, off, length, len(p))
	}
	if file := journal.NewFileID([16]byte(h[16:32])); file != id {
		t.Fatalf("segment %d at %d: the record's header names file %v, want %v", seg, off, file, id)
	}
	if own := binary.LittleEndian.Uint32(h[88:92]); int64(own) != off {
		t.Fatalf("segment %d at %d: the record's header names its own offset %d, want %d", seg, off, own, off)
	}
	if sum := seededCRC(journalIdentity(t, f), h[0:92]); sum != binary.LittleEndian.Uint32(h[92:96]) {
		t.Fatalf("segment %d at %d: the record's header does not verify against its checksum", seg, off)
	}
	return foundRecord{seg: seg, off: off, seq: binary.LittleEndian.Uint64(h[48:56]), end: off + writeRecordBytes(len(p))}
}

// locateWrite returns the newest record holding payload p of file id, from
// the live segment images when live is set and the durable ones otherwise:
// the greatest segment, then the greatest offset, the order appends follow,
// so a re-appended record answers with its copy, never the ended segment's
// original. It is the record a Sync naming the file marks.
func locateWrite(t *testing.T, f *journaltest.Fixture, id journal.FileID, p []byte, live bool) foundRecord {
	t.Helper()

	var best foundRecord
	found := false
	for _, name := range segmentFiles(t, f) {
		img := liveBytes(t, f, name)
		if !live {
			img = durable(t, f, name)
		}
		seg := idOf(t, name)
		for at := 0; ; {
			idx := bytes.Index(img[at:], p)
			if idx < 0 {
				break
			}
			at += idx
			r := verifyWrite(t, f, seg, img, at, id, p)
			at += len(p)
			if !found || r.seg > best.seg || (r.seg == best.seg && r.off > best.off) {
				best, found = r, true
			}
		}
	}
	if !found {
		t.Fatalf("no segment holds the write of file %v", id)
	}
	return best
}

// streamOf returns the append stream the header of segment seg names, from
// its live image when live is set and its durable one otherwise. A SyncMark
// names its stream rather than relying on its position, so the header is
// where the two must agree; the stream is the one field the raw package's
// parse leaves to the reader, at [40,44) of the header.
func streamOf(t *testing.T, f *journaltest.Fixture, seg journal.SegmentID, live bool) uint32 {
	t.Helper()

	name := segmentName(seg)
	b := liveBytes(t, f, name)
	if !live {
		b = durable(t, f, name)
	}
	if len(b) < raw.SegmentHeaderSize {
		t.Fatalf("%s is %d bytes, too short for a segment header of %d", name, len(b), raw.SegmentHeaderSize)
	}
	if _, err := raw.ParseSegmentHeader(headerSlot(t, name, b)); err != nil {
		t.Fatalf("parse the header of segment %d: %v", seg, err)
	}
	return binary.LittleEndian.Uint32(b[40:44])
}

// wantSyncMark fails the test unless m is exactly the mark of frr's record:
// the stream its segment's header names, the segment, the record's end and
// its sequence number as the header's bytes carry them, and a synced-through
// offset that covers the record and claims nothing past the durable bytes
// of its segment. Through is otherwise not compared: no public answer names
// a synced-through offset to compare with.
func wantSyncMark(t *testing.T, f *journaltest.Fixture, what string, m journal.SyncMark, frr foundRecord) {
	t.Helper()

	if m.Segment != uint64(frr.seg) {
		t.Errorf("%s: mark.Segment = %d, want the record's segment %d", what, m.Segment, frr.seg)
	}
	if m.Offset != frr.end {
		t.Errorf("%s: mark.Offset = %d, want the record's end %d (%s)", what, m.Offset, frr.end, frr.at())
	}
	if m.Sequence != frr.seq {
		t.Errorf("%s: mark.Sequence = %d, want the record's %d (%s)", what, m.Sequence, frr.seq, frr.at())
	}
	if m.Through < m.Offset {
		t.Errorf("%s: mark.Through = %d, want at least the record's end %d", what, m.Through, m.Offset)
	}
	if dur := int64(len(durable(t, f, segmentName(frr.seg)))); m.Through > dur {
		t.Errorf("%s: mark.Through = %d, want at most the segment's durable length %d: a synced-through offset proves only durable bytes", what, m.Through, dur)
	}
	if s := streamOf(t, f, frr.seg, false); m.Stream != s {
		t.Errorf("%s: mark.Stream = %d, want the stream segment %d's header names, %d", what, m.Stream, frr.seg, s)
	}
}

// TestRFC1A_SyncMarksNameTheRecordsTheyProve checks that a Sync naming two
// files whose fresh writes sit on two different streams returns one mark
// per stream -- the stream's number, the segment, the record's end and its
// sequence number, all matching the record the sync named, ordered by
// stream number. Each mark is checked against the durable bytes the raw
// layout pins, which is also as far as the marks' purpose can be checked
// today: a reopen is not handed them yet, so the fields they carry -- the
// ones recovery reads cut into a scanned tail -- are what is asserted.
func TestRFC1A_SyncMarksNameTheRecordsTheyProve(t *testing.T) {
	f := journaltest.New(t)
	h := f.Share(0)
	const size = 4 << 10

	// Two files of one share probed until their records sat on two
	// different streams: mapping files to streams is the journal's own.
	type marked struct {
		id    journal.FileID
		fresh []byte
	}
	var ms [2]marked
	var stream0 uint32
	n := 0
	for i := 0; n < 2; i++ {
		if i == 32 {
			t.Fatalf("no two of %d probed files sit on different streams", i)
		}
		id := journaltest.File(i)
		p := distinct(i, size)
		write(t, h, id, 0, p)
		s := streamOf(t, f, locateWrite(t, f, id, p, true).seg, true)
		if n == 0 {
			stream0 = s
		} else if s == stream0 {
			continue
		}
		ms[n] = marked{id: id}
		n++
	}

	// One fresh write per file, unsynced, and one Sync naming both.
	for i := range ms {
		ms[i].fresh = distinct(30+i, size)
		write(t, h, ms[i].id, size, ms[i].fresh)
	}
	marks, err := h.Sync(t.Context(), ms[0].id, ms[1].id)
	stub(t, "Sync", err)
	if err != nil {
		t.Fatalf("sync of the two files: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("a sync of two files on two streams returned %d marks, want one per stream: %+v", len(marks), marks)
	}
	if marks[0].Stream >= marks[1].Stream {
		t.Fatalf("the marks %+v are not ordered by stream number", marks)
	}

	// Each file's named record is claimed by exactly one mark, whole.
	for _, c := range ms {
		frr := locateWrite(t, f, c.id, c.fresh, false)
		n := 0
		for _, m := range marks {
			if m.Sequence == frr.seq {
				n++
				wantSyncMark(t, f, fmt.Sprintf("the mark of the stream holding file %v", c.id), m, frr)
			}
		}
		if n != 1 {
			t.Fatalf("%d of the %d marks name record %d, the fresh write of file %v, want exactly the one", n, len(marks), frr.seq, c.id)
		}
	}
}

// TestRFC1A_FailedWindowsCarryNoMarks checks that a Sync naming a file of a
// failed sync window returns its report -- the SyncError -- with no marks,
// since a mark would prove records the sync lost, and that the file whose
// first Sync named another file of the window receives its own report from
// the next one, marks none again. A window the journal resolved by
// re-appending returns the copy's mark instead, and the later Sync of the
// other file whatever mark its copy still owes its own; the file's next
// record is marked again after either answer, the failed window wedging
// nothing.
func TestRFC1A_FailedWindowsCarryNoMarks(t *testing.T) {
	f := journaltest.New(t)
	const size = 4 << 10

	for k := range 2 {
		h, idA := f.Share(k), journaltest.File(k)

		// A partner whose fresh write sits in idA's segment, probed for
		// until it lands there: one window fails both, and each file is
		// owed its report alone. Mapping files to streams is the journal's
		// own.
		pa := distinct(k, size)
		write(t, h, idA, 0, pa)
		seg := locateWrite(t, f, idA, pa, true).seg
		var idB journal.FileID
		var pb []byte
		for i := 40 + 24*k; ; i++ {
			if i == 64+24*k {
				t.Fatalf("share %d: no probed file landed in file %v's segment %d", k, idA, seg)
			}
			idB = journaltest.File(i)
			pb = distinct(i, size)
			write(t, h, idB, 0, pb)
			if locateWrite(t, f, idB, pb, true).seg == seg {
				break
			}
		}

		failWindow(t, f, seg)
		marks, err := h.Sync(t.Context(), idB)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		windowAnswer(t, k, f, "the Sync of the second file of the failed window", marks, err, idB, pb, true)

		// The file no Sync named yet takes its report from the next one,
		// with no marks again, or nothing unsynced once the window was
		// re-appended.
		marks, err = h.Sync(t.Context(), idA)
		stub(t, "Sync", err)
		windowAnswer(t, k, f, "the first Sync of the other file of the window", marks, err, idA, pa, false)

		// Either answer leaves the file writing on, and its next record is
		// marked again.
		pa2 := distinct(10+k, size)
		write(t, h, idA, size, pa2)
		marks, err = h.Sync(t.Context(), idA)
		stub(t, "Sync", err)
		if err != nil {
			t.Fatalf("share %d: the Sync after the failed window's report: %v, the failed window wedged the file", k, err)
		}
		if len(marks) != 1 {
			t.Fatalf("share %d: the Sync after the failed window returned %d marks, want the new record's", k, len(marks))
		}
		wantSyncMark(t, f, "the mark after the failed window", marks[0], locateWrite(t, f, idA, pa2, false))
	}
}

// windowAnswer checks the answer of a Sync that met a failed window: an
// error must be the SyncError, the report the file was owed, carrying the
// file's own name and no marks; on success each mark must name the file's
// newest record whole, a re-appended window returning its copy's mark, which
// requireMark demands -- a later Sync of the file finds nothing unsynced
// and may carry no mark at all.
func windowAnswer(t *testing.T, k int, f *journaltest.Fixture, what string, marks []journal.SyncMark, err error, id journal.FileID, p []byte, requireMark bool) {
	t.Helper()

	if err != nil && !errors.Is(err, journal.ErrSyncFailed) {
		t.Fatalf("share %d: %s returned %v, want a SyncError or a re-append", k, what, err)
	}
	if errors.Is(err, journal.ErrSyncFailed) {
		var se *journal.SyncError
		if !errors.As(err, &se) {
			t.Fatalf("share %d: %s returned %v wrapping ErrSyncFailed, want a *SyncError", k, what, err)
		}
		if !slices.Contains(se.Files, id) {
			t.Fatalf("share %d: %s reported a SyncError naming files %v, want file %v among them", k, what, se.Files, id)
		}
		if marks != nil {
			t.Fatalf("share %d: %s returned marks %+v with its SyncError, want none: a mark would prove records the sync lost", k, what, marks)
		}
		return
	}
	for _, m := range marks {
		wantSyncMark(t, f, "the mark of the re-appended record", m, locateWrite(t, f, id, p, false))
	}
	if requireMark && len(marks) != 1 {
		t.Fatalf("share %d: %s returned %d marks over the re-appended window, want the one of the copy", k, what, len(marks))
	}
}
