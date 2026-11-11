package journal

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// plantSegment writes the whole bytes of a segment file into the seam's
// directory, durable, as a writer that crashed would have left it.
func (s seamDir) plantSegment(t *testing.T, name string, b []byte) {
	t.Helper()

	f, err := s.seam.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if len(b) > 0 {
		if _, err := f.WriteAt(b, 0); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := errors.Join(f.Sync(), f.Close(), s.seam.SyncDir()); err != nil {
		t.Fatalf("plant %s: %v", name, err)
	}
}

// TestOpen_FormatUnreadableRefusesByFlag fails an Open that judges a format
// file on the bytes a failing read returned: every read of a good file
// failing with EIO, the open answers an UnreadableRangesError whose Format
// is set and whose Ranges name none — the file names no segment id — and
// the directory is left exactly as it stands, the file read three times and
// never rewritten, truncated or removed.
func TestOpen_FormatUnreadableRefusesByFlag(t *testing.T) {
	s := newSeam(t)
	id, err := s.init(t)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	before, err := s.seam.Durable(format.FormatFileName)
	if err != nil {
		t.Fatalf("read the format file: %v", err)
	}
	if err := s.seam.Schedule(kit.Failing(kit.OpRead, format.FormatFileName, 0, syscall.EIO)); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	_, err = Open(context.Background(), s.dir, Version{}, id, nil, openOptsDir(s, kit.NewStopwatchClock())...)
	var ure *UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Open with every format read failing got err %v, want an UnreadableRangesError", err)
	}
	if !errors.Is(err, ErrUnreadable) {
		t.Errorf("Open: the unreadable refusal %v does not match ErrUnreadable", err)
	}
	if !ure.Format {
		t.Errorf("Open: the refusal's Format flag is not set; only the format file's read failed")
	}
	if len(ure.Ranges) != 0 {
		t.Errorf("Open: the refusal names segment ranges %v, want none: the format file names no segment id", ure.Ranges)
	}
	if got := s.seam.CallsFor(kit.OpRead, format.FormatFileName); got != 3 {
		t.Errorf("Open: format reads got %d, want 3, the readretry bound", got)
	}
	if after, err := s.seam.Durable(format.FormatFileName); err != nil || !slices.Equal(before, after) {
		t.Errorf("Open: the refusal changed the format file: got %x (err %v), want the bytes it held", after, err)
	}
	if names := s.listSeam(t); !slices.Equal(names, []string{format.FormatFileName}) {
		t.Errorf("Open: after the refusal the directory holds %v, want the format file alone", names)
	}
}

// TestOpen_SegmentHeaderRetryThenRefusal covers both halves of a header read
// on a failing device at the public Open: a read that faults twice and
// succeeds on the third attaches the segment, and one that faults on every
// attempt refuses the open naming the segment's range — never judging it
// zero, damaged or foreign, never taking it off disk — with three attempts
// exactly. Both opens end at the attach pass: one that read everything
// still answers ErrNotImplemented, the recovery after it being unbuilt.
func TestOpen_SegmentHeaderRetryThenRefusal(t *testing.T) {
	plan := []struct {
		name      string
		failures  int
		want      error
		wantCalls int
	}{
		{"recovers on the third attempt", 2, ErrNotImplemented, 3},
		{"fails every attempt", 0, ErrUnreadable, 3},
	}
	for _, c := range plan {
		t.Run(c.name, func(t *testing.T) {
			s := newSeam(t)
			id, err := s.init(t)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			seg := format.SegmentName(1)
			s.plantSegment(t, seg, raw.SegmentHeader{Identity: id, ID: 1}.Bytes())
			if err := s.seam.Schedule(kit.Failing(kit.OpRead, seg, c.failures, syscall.EIO)); err != nil {
				t.Fatalf("schedule: %v", err)
			}

			_, err = Open(context.Background(), s.dir, Version{}, id, nil, openOptsDir(s, kit.NewStopwatchClock())...)
			if !errors.Is(err, c.want) {
				t.Fatalf("Open with a header read that %s got err %v, want one matching %v", c.name, err, c.want)
			}
			if got := s.seam.CallsFor(kit.OpRead, seg); got != c.wantCalls {
				t.Errorf("Open: header reads got %d, want %d, the readretry bound", got, c.wantCalls)
			}
			if !slices.Contains(s.listSeam(t), seg) {
				t.Errorf("Open: the segment whose read failed is not on disk; directory holds %v", s.listSeam(t))
			}
			if c.failures == 0 {
				var ure *UnreadableRangesError
				if !errors.As(err, &ure) {
					t.Fatalf("Open: the refusal %v is not an UnreadableRangesError", err)
				}
				if ure.Format {
					t.Errorf("Open: the refusal names the format file too; only the segment's read failed")
				}
				want := []SegmentRange{{Segment: 1, Off: 0, Len: int64(format.SegmentHeaderSize)}}
				if !slices.Equal(ure.Ranges, want) {
					t.Errorf("Open: the refusal names ranges %v, want one naming segment 1", ure.Ranges)
				}
			}
		})
	}
}

// plantVersionTwoHeader writes a segment whose header verifies by checksum,
// names the journal's identity, and names format version 2 — a newer format
// than the binary reads and than the directory's format file names — as a
// writer of a later version would have left it.
func plantVersionTwoHeader(t *testing.T, s seamDir, id JournalID, seg SegmentID) {
	t.Helper()

	b := make([]byte, format.SegmentHeaderSize)
	copy(b, "DJH1")
	binary.LittleEndian.PutUint16(b[4:6], 2)
	copy(b[8:24], id[:])
	binary.LittleEndian.PutUint64(b[24:32], uint64(seg))
	// The checksum over the bytes before it, so the decoder reads the
	// header whole and refuses the version alone, not a damaged checksum.
	binary.LittleEndian.PutUint32(b[44:48], crc32.Checksum(b[:44], crc32.MakeTable(crc32.Castagnoli)))
	s.plantSegment(t, format.SegmentName(seg), b)
}

// TestOpen_NewerHeaderVersionFailsTheOpen pins that a segment header of a
// newer format version — a version above the binary's and, with only one
// format version in existence, above the format file's too, which is the
// same refusal until a second one exists — fails the open with
// ErrUnknownFormat and leaves the directory exactly as it stands: the
// segment is neither unlinked, zeroed nor judged damaged, and its bytes
// are never read past the header the decoder refused.
func TestOpen_NewerHeaderVersionFailsTheOpen(t *testing.T) {
	s := newSeam(t)
	id, err := s.init(t)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	seg := format.SegmentName(1)
	plantVersionTwoHeader(t, s, id, 1)
	before, derr := s.seam.Durable(seg)
	if derr != nil {
		t.Fatalf("read the planted segment: %v", derr)
	}

	_, err = Open(context.Background(), s.dir, Version{}, id, nil, openOptsDir(s, kit.NewStopwatchClock())...)
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Open over a segment of format version 2 got err %v, want ErrUnknownFormat", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("Open: the refusal %q does not name the segment it refused", err)
	}
	if after, derr := s.seam.Durable(seg); derr != nil || !slices.Equal(before, after) {
		t.Errorf("Open: the refusal changed the segment: got %x (err %v), want the bytes it arrived with", after, derr)
	}
	// One header read, not three: a header that verifies by checksum fails
	// no read, so the retry bound is never spent on it.
	if got := s.seam.CallsFor(kit.OpRead, seg); got != 1 {
		t.Errorf("Open: header reads got %d, want 1, the single read a verifying header takes", got)
	}
}

// TestOpen_AttachPassLeavesDirectoryPinned pins what a completed attach pass
// leaves in the directory at the public Open: every .spare.tmp unlinked, an
// unrecognised name and a foreign segment's bytes untouched, and the open
// answering ErrNotImplemented with the recovery after the pass unbuilt —
// the file names the pass changed are the spares' temporaries alone.
func TestOpen_AttachPassLeavesDirectoryPinned(t *testing.T) {
	s := newSeam(t)
	id, err := s.init(t)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	own := format.SegmentName(2)
	s.plantSegment(t, own, raw.SegmentHeader{Identity: id, ID: 2}.Bytes())
	foreign := format.SegmentName(3)
	s.plantSegment(t, foreign, raw.SegmentHeader{ID: 3}.Bytes())
	backup := "00000000000000000042.seg.bak"
	notes := "notes.txt"
	for _, name := range []string{backup, notes} {
		s.plantSegment(t, name, []byte("bytes of a file the journal did not write"))
	}
	s.plantSegment(t, format.SpareName(9), nil)
	foreignBytes, err := s.seam.Durable(foreign)
	if err != nil {
		t.Fatalf("read the foreign segment: %v", err)
	}

	_, err = Open(context.Background(), s.dir, Version{}, id, nil, openOptsDir(s, kit.NewStopwatchClock())...)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Open over an attached directory got err %v, want ErrNotImplemented: the recovery after the attach pass is not built", err)
	}

	got := s.listSeam(t)
	if slices.Contains(got, format.SpareName(9)) {
		t.Errorf("Open: the .spare.tmp was not unlinked; directory holds %v", got)
	}
	for _, name := range []string{own, foreign, backup, notes} {
		if !slices.Contains(got, name) {
			t.Errorf("Open: %s was taken off disk; directory holds %v", name, got)
		}
	}
	if after, err := s.seam.Durable(foreign); err != nil || !slices.Equal(foreignBytes, after) {
		t.Errorf("Open: the foreign segment's bytes changed: got %x (err %v), want the bytes it arrived with", after, err)
	}
}
