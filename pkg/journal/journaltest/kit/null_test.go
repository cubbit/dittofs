package kit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/format"
)

func TestNullFSDropsRecordsAndKeepsHeaders(t *testing.T) {
	dir := t.TempDir()
	n := NewNullFS(dir)

	seg, err := n.Create("seg")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = seg.Close() }()
	head := bytes.Repeat([]byte{'h'}, format.SegmentHeaderSize)
	if _, err := seg.WriteAt(head, 0); err != nil {
		t.Fatalf("WriteAt of the header: %v", err)
	}
	record := bytes.Repeat([]byte{'r'}, 4096)
	if got, err := seg.WriteAt(record, format.SegmentHeaderSize); err != nil || got != len(record) {
		t.Fatalf("WriteAt of a record = %d, %v; want %d, nil", got, err, len(record))
	}
	if err := seg.Sync(); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got, want := n.Nulled(), int64(len(record)); got != want {
		t.Fatalf("Nulled = %d, want %d: the record write must be dropped and counted", got, want)
	}
	on, err := os.ReadFile(filepath.Join(dir, "seg"))
	if err != nil {
		t.Fatalf("read the segment back: %v", err)
	}
	if !bytes.Equal(on, head) {
		t.Fatalf("the file holds %d bytes, want the %d-byte header alone: the header reaches the file, the record does not", len(on), len(head))
	}

	fmtFile, err := n.Create(format.FormatFileName)
	if err != nil {
		t.Fatalf("Create the format file: %v", err)
	}
	defer func() { _ = fmtFile.Close() }()
	if _, err := fmtFile.WriteAt(record, format.SegmentHeaderSize); err != nil {
		t.Fatalf("WriteAt into the format file: %v", err)
	}
	if got, want := n.Nulled(), int64(len(record)); got != want {
		t.Fatalf("Nulled = %d after a format-file write, want %d: the format file is written for real", got, want)
	}
}
