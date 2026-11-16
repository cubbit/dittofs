package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// scanDemo opens a journal and issues one write, the fixture both dump
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

// findWriteStep scans all segments and returns the field where the write
// record the test wrote lives — stream 2's segment is as valid a home as
// stream 1's.
func findWrite(t *testing.T, dir string) (seg string, recs []dumpRecord) {
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
	_, recs := findWrite(t, d.dir)
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
	seg, _ := findWrite(t, d.dir)
	info, err := os.Stat(seg)
	if err != nil {
		t.Fatal(err)
	}
	// Cut mid-body of the write's record: 60 bytes off its end tears the
	// payload in half.
	if err := os.Truncate(seg, info.Size()-60); err != nil {
		t.Fatal(err)
	}
	recs, torn, err := scanSegment(seg)
	if err != nil {
		t.Fatalf("scanSegment on a torn tail: %v", err)
	}
	if !torn {
		t.Errorf("scanSegment: torn tail not reported (records=%d)", len(recs))
	}
	for i, r := range recs {
		if r.recOff+dumpRecordHeaderSize+int64(len(r.body)) > info.Size()-60 {
			t.Errorf("scanSegment: record %d claims bytes past the cut", i)
		}
	}
}
