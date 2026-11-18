package jdemo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/walk"
)

// scanDemo opens a journal and issues one write, the fixture the dump
// tests stand on. The handle is kept on the demo so tests can read back.
func scanDemo(t *testing.T) (*Demo, *strings.Builder) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	d, out := newTestDemo(t, dir, "")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.J.Close() })
	h, err := d.J.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	d.H[1] = h
	if _, err := h.WriteAt(context.Background(), StoryFile, 0, []byte("hello world"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	return d, out
}

// findWrite reads every segment with the journal's own scanner and returns
// the segment holding the fixture's 11-byte write, its header and its body.
func findWrite(t *testing.T, d *Demo) (string, walk.RecordHeader, []byte) {
	t.Helper()
	st, err := LoadState(StatePath(d.Dir))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".seg") {
			continue
		}
		path := filepath.Join(d.Dir, e.Name())
		seg, err := readSegment(path, st.ID)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, r := range seg.Scan.Records {
			if r.Header.Kind.String() == "write" && r.Header.Extent.Len == 11 {
				return path, r.Header, seg.Bodies[i]
			}
		}
	}
	t.Fatalf("no 11-byte write record in any segment of %s", d.Dir)
	return "", walk.RecordHeader{}, nil
}

func TestDumpShowsTheWriteAndItsPayload(t *testing.T) {
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
	if !strings.Contains(screen, StoryFileHex[:12]) {
		t.Errorf("dump: the file's identity missing:\n%s", screen)
	}
	if !strings.Contains(screen, "checksums verified") {
		t.Errorf("dump: the view does not say its records were verified:\n%s", screen)
	}

	_, h, _ := findWrite(t, d)
	if h.Extent.Off != 0 || h.Extent.Len != 11 {
		t.Errorf("scanned write extent: got off=%d len=%d, want 0 and 11", h.Extent.Off, h.Extent.Len)
	}
	if h.Offset%8 != 0 {
		t.Errorf("scanned record offset %d is not 8-aligned", h.Offset)
	}
}

func TestDumpNamesATornTail(t *testing.T) {
	d, out := scanDemo(t)
	seg, h, body := findWrite(t, d)
	// Cut inside the write's own body: truncating the preallocated tail
	// would only remove unwritten zeros and prove nothing.
	if err := os.Truncate(seg, h.Offset+walk.RecordHeaderSize+int64(len(body))/2); err != nil {
		t.Fatal(err)
	}
	d.viewDump()
	screen := out.String()
	if !strings.Contains(screen, "tail torn") {
		t.Errorf("a segment cut mid-record does not say its tail is torn:\n%s", screen)
	}
	if strings.Contains(screen, "68 65 6c 6c 6f 20 77 6f") {
		t.Errorf("the incomplete record's payload was still shown:\n%s", screen)
	}
}

func TestDumpAgreesWithTheJournalOwnRead(t *testing.T) {
	// The dump and a read are two roads to the same bytes: the scanner's
	// payload for the write must be what the journal serves for the range.
	d, out := scanDemo(t)
	p := make([]byte, 11)
	if _, _, _, _, err := d.H[1].ReadAt(context.Background(), StoryFile, 0, p); err != nil {
		t.Fatalf("read: %v", err)
	}
	_, _, body := findWrite(t, d)
	d.viewDump()
	if !bytes.Equal(body[8:], p) {
		t.Errorf("the scanned payload %q disagrees with the journal's own ReadAt %q", body[8:], p)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("the dump screen lacks the payload the journal serves:\n%s", out.String())
	}
}
