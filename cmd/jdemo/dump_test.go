package main

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// scanDemo opens a journal and issues one write, the fixture the scan
// tests stand on.
func scanDemo(t *testing.T) (*demo, *strings.Builder) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	d, out := newTestDemo(t, dir, "")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.j.Close() })
	h, err := d.j.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if _, err := h.WriteAt(context.Background(), FILE, 0, []byte("hello world"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	return d, out
}

// segFiles is every .seg in the demo's journal dir, ascending.
func segFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".seg") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	if len(out) == 0 {
		t.Fatalf("no segment found in %s", dir)
	}
	return out
}

// findWriteStep scans all segments for the write record the fixture made.
func findWriteStep(t *testing.T, dir string) (seg string, recs []dumpRecord) {
	t.Helper()
	for _, seg := range segFiles(t, dir) {
		recs, _, err := scanSegment(seg)
		if err != nil {
			t.Fatalf("scanSegment %s: %v", seg, err)
		}
		for i := range recs {
			if recs[i].kindName() == "write" {
				return seg, recs
			}
		}
	}
	t.Fatalf("no write record found in any segment of %s", dir)
	return "", nil
}

// craftHeader builds a segment header with the transcribed layout: magic
// DJH1, version 1, identity, the segment's own id at [24,32), predecessor,
// stream. The checksum slot is left zero: the scan never verifies it.
func craftHeader(id uint64) []byte {
	h := make([]byte, dumpSegmentHeaderSize)
	copy(h[0:4], "DJH1")
	binary.LittleEndian.PutUint16(h[4:6], 1)
	binary.LittleEndian.PutUint64(h[24:32], id)
	return h
}

// craftRecord builds one record header with its body, laid out exactly as
// the transcription reads it back — the kind's own body, a write's 8-byte
// mtime prefix included. Neither checksum slot is filled: the scan's
// contract says it never verifies them.
func craftRecord(kind uint8, segID uint64, ownOff int64, extentOff, extentLen int64, payload []byte) []byte {
	h := make([]byte, dumpRecordHeaderSize)
	copy(h[0:4], "DJR1")
	h[4] = kind
	binary.LittleEndian.PutUint64(h[8:16], segID)
	binary.LittleEndian.PutUint64(h[32:40], uint64(extentOff))
	binary.LittleEndian.PutUint64(h[40:48], uint64(extentLen))
	binary.LittleEndian.PutUint32(h[88:92], uint32(ownOff))

	var body []byte
	switch kind {
	case 1: // write: an 8-byte mtime then the payload
		body = append(make([]byte, 8), payload...)
	case 2: // fill: the payload
		body = payload
	case 10, 16: // loss, reconciled
		body = make([]byte, 16)
	case 7: // clone
		body = make([]byte, 56)
	case 11, 12, 13: // hold, unhold, stamp
		body = make([]byte, 8)
	}
	pitch := (dumpRecordHeaderSize + int64(len(body)) + 7) &^ 7
	out := make([]byte, pitch)
	copy(out, h)
	copy(out[dumpRecordHeaderSize:], body)
	return out
}

// craftSegment writes a segment file: header, then the given records'
// bytes, then the tail kind names (untaken zeros, a seal marker, or torn
// garbage).
func craftSegment(t *testing.T, path string, id uint64, tail string, recs ...[]byte) {
	t.Helper()
	b := craftHeader(id)
	for _, r := range recs {
		b = append(b, r...)
	}
	switch tail {
	case "untaken":
		b = append(b, make([]byte, 64)...)
	case "sealed":
		m := make([]byte, 32)
		copy(m[0:4], "DJS1")
		binary.LittleEndian.PutUint64(m[8:16], id)
		binary.LittleEndian.PutUint64(m[16:24], uint64(len(b)))
		b = append(b, m...)
		b = append(b, make([]byte, 64)...)
	case "torn":
		b = append(b, 0xee, 0xee, 0xee, 0xee)
		b = append(b, make([]byte, 128)...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanWalksEveryFixedBodyKind(t *testing.T) {
	// reconcile (15) carries no body and the 16-byte one is reconciled
	// (16), per kinds.go. A walk that swaps them desynchronises and loses
	// every record after the first.
	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000007.seg")
	craftSegment(t, path, 7, "untaken",
		craftRecord(15, 7, 48, 0, 0, nil),        // reconcile: pitch 96
		craftRecord(16, 7, 48+96, 0, 0, nil),     // reconciled: 16-byte body, pitch 112
		craftRecord(10, 7, 48+96+112, 0, 0, nil), // loss: 16-byte body, pitch 112
	)
	recs, end, err := scanSegment(path)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.kindName())
	}
	want := []string{"reconcile", "reconciled", "loss"}
	if len(kinds) != len(want) {
		t.Fatalf("scanned kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kind %d: got %s, want %s", i, kinds[i], want[i])
		}
	}
	if end != endUntaken {
		t.Errorf("scanned tail state: got %v, want endUntaken", end)
	}
	if recs[1].recOff != 48+96 || recs[2].recOff != 48+96+112 {
		t.Errorf("record offsets: got %d and %d, want %d and %d",
			recs[1].recOff, recs[2].recOff, 48+96, 48+96+112)
	}
}

func TestScanHonoursWhoAndWhere(t *testing.T) {
	// A record naming another segment was written somewhere else, and one
	// whose own offset disagrees with where the walk read it was too;
	// both stop the walk rather than showing as this segment's records —
	// the guard against a recycled segment's stale bytes reading as live.
	dir := t.TempDir()

	foreign := filepath.Join(dir, "00000000000000000009.seg")
	craftSegment(t, foreign, 9, "untaken",
		craftRecord(1, 8, 48, 0, 5, []byte("hello")))
	recs, end, _ := scanSegment(foreign)
	if len(recs) != 0 || end != endTorn {
		t.Errorf("foreign-segment record: got %d records, end %v; want the walk stopped", len(recs), end)
	}

	wrongPlace := filepath.Join(dir, "00000000000000000010.seg")
	craftSegment(t, wrongPlace, 10, "untaken",
		craftRecord(1, 10, 56, 0, 5, []byte("hello"))) // own offset 56, walk reads 48
	recs, end, _ = scanSegment(wrongPlace)
	if len(recs) != 0 || end != endTorn {
		t.Errorf("wrong-place record: got %d records, end %v; want the walk stopped", len(recs), end)
	}

	// And a header that is not DJH1 at all: the file was never taken as a
	// segment — a spare — so nothing walks and nothing reads as torn.
	spare := filepath.Join(dir, "00000000000000000011.seg")
	if err := os.WriteFile(spare, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, end, _ = scanSegment(spare)
	if len(recs) != 0 || end != endUntaken {
		t.Errorf("never-taken spare: got %d records, end %v; want none and endUntaken", len(recs), end)
	}
}

func TestScanTailStates(t *testing.T) {
	dir := t.TempDir()
	sealed := filepath.Join(dir, "00000000000000000012.seg")
	craftSegment(t, sealed, 12, "sealed", craftRecord(1, 12, 48, 0, 5, []byte("hello")))
	_, end, _ := scanSegment(sealed)
	if end != endSealed {
		t.Errorf("sealed segment: got tail %v, want endSealed", end)
	}

	torn := filepath.Join(dir, "00000000000000000013.seg")
	craftSegment(t, torn, 13, "torn", craftRecord(1, 13, 48, 0, 5, []byte("hello")))
	recs, end, _ := scanSegment(torn)
	if len(recs) != 1 || end != endTorn {
		t.Errorf("torn segment: got %d records, tail %v, want the one record and endTorn", len(recs), end)
	}
}

func TestScanRejectsImpossibleLengths(t *testing.T) {
	// A record whose length column carries the high bit decodes to a
	// negative int64: the walk must stop at it, not slice with it and
	// panic. The write before it still walks.
	dir := t.TempDir()
	path := filepath.Join(dir, "00000000000000000014.seg")
	// The good write's pitch is 112, so the bad record sits at 48+112 with
	// its own offset agreeing — the length column is the only thing wrong.
	bad := craftRecord(1, 14, 48+112, 0, 16, make([]byte, 8))
	binary.LittleEndian.PutUint64(bad[40:48], 0x8000000000000000)
	craftSegment(t, path, 14, "untaken",
		craftRecord(1, 14, 48, 0, 5, []byte("hello")),
		bad,
	)
	recs, end, err := scanSegment(path) // must not panic
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	if len(recs) != 1 || end != endTorn {
		t.Errorf("impossible length: got %d records, tail %v; want the good one walked and endTorn", len(recs), end)
	}
}

func TestScanSegmentFindsTheWrite(t *testing.T) {
	d, out := scanDemo(t)
	d.viewDump()
	screen := out.String()
	if !strings.Contains(screen, "write") {
		t.Errorf("dump: no write record shown:\n%s", screen)
	}
	// A hexdump lane is 16 bytes wide and the write's 8-byte mtime prefix
	// comes first, so the payload crosses lanes. The screen shows its hex.
	if !strings.Contains(screen, "68 65 6c 6c 6f 20 77 6f") {
		t.Errorf("dump: the written payload's hex missing from the lanes:\n%s", screen)
	}
	if !strings.Contains(screen, fileHex[:12]) {
		t.Errorf("dump: the file's identity missing:\n%s", screen)
	}

	// The decoder itself, not just the screen: one write record with the
	// exact fields the format table names, in whichever stream's segment
	// the write landed.
	_, recs := findWriteStep(t, d.dir)
	var w *dumpRecord
	for i := range recs {
		if recs[i].kindName() == "write" {
			w = &recs[i]
		}
	}
	if w == nil {
		t.Fatalf("scanSegment: record set lacks the write:\n%+v", recs)
	}
	if w.off != 0 || w.length != 11 {
		t.Errorf("scanned write extent: got off=%d len=%d, want 0 and 11", w.off, w.length)
	}
	if string(w.body[8:]) != "hello world" {
		t.Errorf("scanned write body after its mtime: got %q, want %q", w.body[8:], "hello world")
	}
	if w.recOff%8 != 0 {
		t.Errorf("scanned record offset %d is not 8-aligned", w.recOff)
	}
}

func TestScanStopsAtTornTail(t *testing.T) {
	d, _ := scanDemo(t)
	seg, recs := findWriteStep(t, d.dir)
	var w *dumpRecord
	for i := range recs {
		if recs[i].kindName() == "write" {
			w = &recs[i]
		}
	}
	if w == nil {
		t.Fatal("fixture lost its write")
	}
	// Cut inside the write's own body — truncating the preallocated tail
	// would only remove unwritten zeros and prove nothing.
	cut := w.recOff + dumpRecordHeaderSize + int64(len(w.body))/2
	if err := os.Truncate(seg, cut); err != nil {
		t.Fatal(err)
	}
	kept, end, err := scanSegment(seg)
	if err != nil {
		t.Fatalf("scanSegment on a torn tail: %v", err)
	}
	if end != endTorn {
		t.Errorf("scanSegment: torn tail state %v, want endTorn", end)
	}
	if len(kept) != 0 {
		t.Errorf("scanSegment: %d records from a mid-body torn segment, want the incomplete one dropped", len(kept))
	}
}
