package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// The tests in this file hold ScanSegmentHeaders to its contract: the same
// walk ScanSegment makes over the same bytes, every kind's record attached on
// its verifying header alone — kinds 15 and 16 with the rest — never reading
// a body byte to do it. A scan that trusts a record's boundaries from its
// header steps over the payload it has no reason to check, so a corrupt body
// cannot cost a record its header, and the bodies recovery needs are read
// later at their offsets, where they are verified.

// recordingReader serves b and records the (offset, length) of every read it
// is asked for, passing each read through to the bytes it holds: it never
// answers in the dependency's place.
type recordingReader struct {
	b     []byte
	reads [][2]int64
}

func (r *recordingReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads = append(r.reads, [2]int64{off, int64(len(p))})
	return copy(p, r.b[off:]), nil
}

// TestScanHeadersAttachesTheSameRecordsAsScanOnACleanSegment: a header-only
// walk over one clean segment holding every kind this format defines attaches
// each record with the same header the verifying walk attaches — its own
// offset and payload checksum included — and ends the same: the same ranges
// tile, the same seal marker stops it at the same slot. Only the bodies
// differ: the header walk never read them, so every one comes back zero.
func TestScanHeadersAttachesTheSameRecordsAsScanOnACleanSegment(t *testing.T) {
	var seg []byte
	seg = AppendSegmentHeader(seg, testSegmentHeader)
	for k := byte(1); k <= 16; k++ {
		h, body := fuzzKindBody(Kind(k), int64(len(seg)))
		seg = AppendRecord(seg, testIdentity, h, body)
	}
	seal := scanSeal(t, &seg, testIdentity, testSegment)
	seg = append(seg, make([]byte, 64)...) // footer bytes the scan must never read
	size := int64(len(seg))

	full, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error", err)
	}
	got, err := ScanSegmentHeaders(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error", err)
	}
	if len(got.Records) != len(full.Records) {
		t.Fatalf("the header walk attached %d records, want the %d ScanSegment attached", len(got.Records), len(full.Records))
	}
	for i := range got.Records {
		if got.Records[i].Header != full.Records[i].Header {
			t.Errorf("record %d at %d: header = %+v, want ScanSegment's %+v", i, full.Records[i].Header.Offset, got.Records[i].Header, full.Records[i].Header)
		}
		if !recordBodiesEqual(got.Records[i].Body, RecordBody{}) {
			t.Errorf("record %d at %d: body = %+v, want the zero body, for no body byte was read", i, got.Records[i].Header.Offset, got.Records[i].Body)
		}
		if got.Records[i].Body.Payload != nil {
			t.Errorf("record %d at %d kept a payload, want nil", i, got.Records[i].Header.Offset)
		}
	}
	if len(got.Ranges) != len(full.Ranges) || len(got.Ranges) != 0 {
		t.Errorf("the header walk's ranges = %+v, want none on a clean segment", got.Ranges)
	}
	if !got.Sealed || got.End != seal || got.End != full.End {
		t.Errorf("sealed %v at %d, want sealed at the marker's %d as ScanSegment's %d is", got.Sealed, got.End, seal, full.End)
	}
	if got.Resynced != full.Resynced || got.Resynced {
		t.Errorf("resynced %v, want no resync on a clean segment", got.Resynced)
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused = %+v, want none: a walk that reads no body refuses nothing", got.Refused)
	}
}

// TestScanHeadersReadsNoBodyByte: the header-only walk reads no byte out of
// any attached record's body. A refill under ScanSegment reads a whole
// megabyte ahead, so a window kept as is would read every body byte in
// advance of any judgment; the header walk therefore refills each slot's own
// bytes alone (see scanner.read), and every read it makes starts at a record
// slot, a marker slot or a search slot — never inside a record's body. The
// recording reader hands back every (offset, length) the walk asked for, and
// the test holds each against every attached record's body span.
func TestScanHeadersReadsNoBodyByte(t *testing.T) {
	// Four kinds with real bodies — a write's and a fill's payload bytes, a
	// loss's fixed body, a clone target's spec — so a body the walk reads by
	// mistake is a body byte for real.
	var seg []byte
	seg = AppendSegmentHeader(seg, testSegmentHeader)
	offs := make([]int64, 0, 4)
	for _, k := range []Kind{KindWrite, KindFill, KindLoss, KindCloneTarget} {
		h, body := fuzzKindBody(k, int64(len(seg)))
		off := int64(len(seg))
		seg, _ = scanAppend(t, seg, testIdentity, scanRecord{h: h, body: body})
		offs = append(offs, off)
	}
	seal := scanSeal(t, &seg, testIdentity, testSegment)
	seg = append(seg, make([]byte, 64)...) // footer bytes the walk must never read
	size := int64(len(seg))

	rec := &recordingReader{b: seg}
	got, err := ScanSegmentHeaders(rec, size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error", err)
	}
	if !got.Sealed || got.End != seal {
		t.Fatalf("sealed %v at %d, want sealed at the marker's %d", got.Sealed, got.End, seal)
	}
	if len(got.Records) != len(offs) {
		t.Fatalf("attached %d records, want the %d planted", len(got.Records), len(offs))
	}
	for i, r := range got.Records {
		if r.Header.Offset != offs[i] {
			t.Errorf("record %d attached at %d, want planted slot %d", i, r.Header.Offset, offs[i])
		}
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused = %+v, want none", got.Refused)
	}
	for _, rd := range rec.reads {
		for _, r := range got.Records {
			bodyStart := r.Header.Offset + RecordHeaderSize
			bodyEnd := r.Header.Offset + r.Header.Size()
			if rd[0] < bodyEnd && bodyStart < rd[0]+rd[1] {
				t.Errorf("a read [%d, %d) overlaps the record at %d's body [%d, %d), want no body byte read",
					rd[0], rd[0]+rd[1], r.Header.Offset, bodyStart, bodyEnd)
			}
		}
	}
}

// TestScanHeadersAttachesARecordWithACorruptBody: a record whose header
// verifies but whose body does not — a body byte flipped, the checksums left
// stale — attaches whole on the header alone, for the header walk reads no
// body to refuse. The verifying walk hands its header back in Refused and
// steps over it as bad bytes; the header walk never learns what its body
// bytes hold, so Refused stays empty and the scan reads clean.
func TestScanHeadersAttachesARecordWithACorruptBody(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)
	size := int64(len(seg))

	// One payload byte of the write at the first slot, its checksums left
	// stale: the header still verifies, the body does not.
	seg[offs[0]+RecordHeaderSize+13] ^= 0x40

	full, err := ScanSegment(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegment = %v, want no error: a record that does not verify is walked over, never returned", err)
	}
	got, err := ScanSegmentHeaders(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error", err)
	}
	if len(full.Records) != 4 {
		t.Fatalf("ScanSegment attached %d records, want the 4 around the corrupt one", len(full.Records))
	}
	if len(got.Records) != 5 {
		t.Fatalf("the header walk attached %d records, want the 5 including the record whose corrupted body it never read", len(got.Records))
	}
	if got.Records[0].Header.Offset != offs[0] {
		t.Errorf("the corrupt-bodied record attached at %d, want its own slot %d", got.Records[0].Header.Offset, offs[0])
	}
	if !recordBodiesEqual(got.Records[0].Body, RecordBody{}) {
		t.Errorf("the corrupt-bodied record's body = %+v, want the zero body", got.Records[0].Body)
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused = %+v, want none: a walk that reads no body refuses nothing", got.Refused)
	}
	if len(got.Ranges) != 0 {
		t.Errorf("ranges = %+v, want none: every header the walk judged verified", got.Ranges)
	}
	if got.End != size || got.Sealed || got.Resynced {
		t.Errorf("end %d sealed %v resynced %v, want a clean walk to the size %d", got.End, got.Sealed, got.Resynced, size)
	}
}

// TestScanHeadersResyncOverCorruptHeader: a record whose header does not
// verify — its length corrupted, the checksums stale — is bad bytes to the
// header walk exactly as to the verifying one, so the forward search runs,
// reports the record's own span one RangeBad, finds the record past it at
// the next usable slot and counts the scan resynchronised.
func TestScanHeadersResyncOverCorruptHeader(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)
	size := int64(len(seg))

	// The second record's length bytes, its checksums left stale: no length
	// the header names can be trusted, so its slot is bad.
	binary.LittleEndian.PutUint64(seg[offs[1]:offs[2]][40:48], 4<<20)

	got, err := ScanSegmentHeaders(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error", err)
	}
	if len(got.Records) != 4 {
		t.Fatalf("attached %d records, want the 4 around the bad slot", len(got.Records))
	}
	for i, idx := range []int{0, 2, 3, 4} {
		if got.Records[i].Header.Offset != offs[idx] {
			t.Errorf("record %d attached at %d, want planted slot %d", i, got.Records[i].Header.Offset, offs[idx])
		}
	}
	want := ScanRange{Off: offs[1], End: offs[2], Kind: RangeBad}
	if len(got.Ranges) != 1 || got.Ranges[0] != want {
		t.Errorf("ranges = %+v, want the one RangeBad over the corrupt header, %+v", got.Ranges, want)
	}
	if !got.Resynced {
		t.Errorf("resynced, want true: a bad range followed by an attached record")
	}
	if got.End != size || got.Sealed {
		t.Errorf("end %d sealed %v, want the walk run to the size %d unsealed", got.End, got.Sealed, size)
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused = %+v, want none", got.Refused)
	}
}

// TestScanHeadersStopsAtKind17AndEpoch: the header walk stops at a header
// this format version cannot read over — an unknown kind, or a non-zero
// version epoch — exactly as the verifying walk does, never stepping the
// record over by any length, keeping the records attached before it and End
// at the stopping slot. The epoch read is the header's own, so the header
// walk stops on a header's epoch without reading the record's body at all.
func TestScanHeadersStopsAtKind17AndEpoch(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(rec []byte)
		want error
	}{{
		name: "unknown kind 17",
		set: func(rec []byte) {
			rec[4] = 17
			rechecksumRecord(rec, testIdentity)
		},
		want: ErrUnknownKind,
	}, {
		name: "non-zero version epoch",
		set: func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[56:64], 1)
			rechecksumRecord(rec, testIdentity)
		},
		want: ErrNonZeroEpoch,
	}} {
		recs := scanTestRecords()
		seg, offs := scanSegment(t, testIdentity, recs...)

		c.set(seg[offs[1] : offs[1]+96])

		got, err := ScanSegmentHeaders(bytes.NewReader(seg), int64(len(seg)), testIdentity, testSegment)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: ScanSegmentHeaders = %v, want %v", c.name, err, c.want)
		}
		if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
			t.Errorf("%s: records = %+v, want the one before the stop", c.name, got.Records)
		}
		if got.End != offs[1] {
			t.Errorf("%s: end %d, want the stopping slot %d itself", c.name, got.End, offs[1])
		}
		if len(got.Ranges) != 0 || got.Sealed || got.Resynced {
			t.Errorf("%s: ranges %d sealed %v resynced %v, want a clean walk before the stop", c.name, len(got.Ranges), got.Sealed, got.Resynced)
		}
		if len(got.Refused) != 0 {
			t.Errorf("%s: refused = %+v, want none", c.name, got.Refused)
		}
	}
}

// TestScanHeadersBodyPastTheSizeIsNotAttached: a verifying header whose body
// runs past the size the caller named — the cut-off shape a crash mid-append
// leaves, judged against the segment's short tail — attaches nothing: the
// record does not fit, so its slot is bad bytes, and the records its span
// holds join the one RangeBad the forward search reports to the size. Its
// header stays judgeable, but nothing about the record can be trusted past
// the length that does not fit, so no length reaches the caller either.
func TestScanHeadersBodyPastTheSizeIsNotAttached(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)

	// The fill at the second slot runs 120 bytes whole; the size stops
	// 8 bytes into its body, whole header readable, body cut short.
	size := offs[1] + RecordHeaderSize + 8

	got, err := ScanSegmentHeaders(bytes.NewReader(seg), size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error: a record that does not fit is walked over, never returned", err)
	}
	if len(got.Records) != 1 || got.Records[0].Header.Offset != offs[0] {
		t.Fatalf("records = %+v, want the first alone, for the second's body runs past the size", got.Records)
	}
	want := ScanRange{Off: offs[1], End: size, Kind: RangeBad}
	if len(got.Ranges) != 1 || got.Ranges[0] != want {
		t.Errorf("ranges = %+v, want the one bad range from the unfit record's slot to the size, %+v", got.Ranges, want)
	}
	if got.End != size || got.Sealed || got.Resynced {
		t.Errorf("end %d sealed %v resynced %v, want the walk run to the size %d", got.End, got.Sealed, got.Resynced, size)
	}
	if len(got.Refused) != 0 {
		t.Errorf("refused = %+v, want none", got.Refused)
	}
}

// TestScanHeadersZeroTailReadsOncePerWindow: a segment whose records are
// followed by a multi-megabyte zero tail is searched to its end in reads of
// the window's size, not one read per 8-byte slot. The bound is a small
// constant times the window count plus the records the walk steps through.
func TestScanHeadersZeroTailReadsOncePerWindow(t *testing.T) {
	recs := scanTestRecords()
	seg, offs := scanSegment(t, testIdentity, recs...)
	size := int64(4*scanWindow) + int64(len(seg))
	seg = append(seg, make([]byte, size-int64(len(seg)))...)

	rec := &recordingReader{b: seg}
	got, err := ScanSegmentHeaders(rec, size, testIdentity, testSegment)
	if err != nil {
		t.Fatalf("ScanSegmentHeaders = %v, want no error", err)
	}
	if len(got.Records) != len(offs) {
		t.Fatalf("attached %d records, want the %d planted", len(got.Records), len(offs))
	}
	if got.End != size {
		t.Fatalf("end %d, want the size %d", got.End, size)
	}
	limit := 2*(size/scanWindow+1) + int64(len(offs)) + 2
	if int64(len(rec.reads)) > limit {
		t.Errorf("a zero tail of %d bytes took %d reads, want at most %d: one per window, not one per slot",
			size, len(rec.reads), limit)
	}
}
