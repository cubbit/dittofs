package journal

import (
	"context"
	"errors"
	"maps"
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
// zero, damaged or foreign, never taking it off disk. The refusing open ends
// in the spares phase, the recovery's first writing one, at the three
// attempts the bound made and before any candidate read; the recovering one
// runs the phase past it, whose two slot reads of the record-less take it
// re-adopts bring the count to five — and still answers ErrNotImplemented,
// the recovery after the spares phase being unbuilt.
func TestOpen_SegmentHeaderRetryThenRefusal(t *testing.T) {
	plan := []struct {
		name      string
		failures  int
		want      error
		wantCalls int
	}{
		{"recovers on the third attempt", 2, ErrNotImplemented, 5},
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
	// A header that verifies by checksum, names the journal's identity and
	// names format version 2 — a newer format than the binary reads — as a
	// writer of a later version would have left it.
	s.plantSegment(t, seg, raw.SegmentHeader{Version: 2, Identity: id, ID: 1}.Bytes())
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

// TestOpen_IdentityRefusalLeavesTheDirectoryPinned fails an Open that attaches
// before the identity check answers: a wrong or zero expect is refused with
// an IdentityError having run no phase, so the `.spare.tmp` the attach pass
// would unlink is still in the directory, and every file's bytes are the
// ones it held before the open. The criterion is the attachment's criterion
// — a refused open leaves the directory byte-for-byte unchanged — held at
// the one boundary that guards it, where moving recovery before the check
// would make a refused open erase files.
func TestOpen_IdentityRefusalLeavesTheDirectoryPinned(t *testing.T) {
	for name, expect := range map[string]JournalID{
		"a wrong expect": func() JournalID {
			var wrong JournalID
			wrong[0] = 0xa5
			return wrong
		}(),
		"expect zero": {},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSeam(t)
			id, err := s.init(t)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			own := format.SegmentName(2)
			s.plantSegment(t, own, raw.SegmentHeader{Identity: id, ID: 2}.Bytes())
			spare := format.SpareName(9)
			s.plantSegment(t, spare, nil)

			before := map[string][]byte{}
			for _, name := range s.listSeam(t) {
				b, err := s.seam.Durable(name)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				before[name] = b
			}

			_, err = Open(context.Background(), s.dir, Version{}, expect, nil, openOptsDir(s, kit.NewStopwatchClock())...)
			var ie *IdentityError
			if !errors.As(err, &ie) {
				t.Fatalf("Open over a directory with %s got err %v, want an IdentityError", name, err)
			}

			if got := s.listSeam(t); !slices.Equal(got, slices.Sorted(maps.Keys(before))) {
				t.Fatalf("Open: %s: after the refusal the directory holds %v, want %v: the attach pass may not run before the identity check",
					name, got, slices.Sorted(maps.Keys(before)))
			}
			for fileName, want := range before {
				if got, derr := s.seam.Durable(fileName); derr != nil || !slices.Equal(got, want) {
					t.Errorf("Open: %s: the refusal changed %s: got %x (err %v), want the bytes %x it held",
						name, fileName, got, derr, want)
				}
			}
		})
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
