package index

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// sortedIDs returns ids sorted, so two answers given in no order compare.
func sortedIDs(ids []core.FileID) []core.FileID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b core.FileID) int {
		ab, bb := a.Bytes(), b.Bytes()
		return slices.Compare(ab[:], bb[:])
	})
	return out
}

// TestSegmentFilesAnswersOnlyTheFilesTheSegmentHolds: of three files, only a
// and b hold records in segment 2, so the segment's own files are exactly
// those two — the copier's read phase asks no other file.
func TestSegmentFilesAnswersOnlyTheFilesTheSegmentHolds(t *testing.T) {
	var ix Index
	a, b, c := fid(1), fid(2), fid(3)
	noteRec(t, &ix.fs, a, 2, ext(0, 4), nums(1, 1))
	noteRec(t, &ix.fs, b, 2, ext(0, 4), nums(2, 2))
	noteRec(t, &ix.fs, b, 3, ext(4, 4), nums(3, 3))
	noteRec(t, &ix.fs, c, 3, ext(0, 4), nums(4, 4))
	for _, tc := range []struct {
		seg  core.SegmentID
		want []core.FileID
	}{
		{2, []core.FileID{a, b}},
		{3, []core.FileID{b, c}},
		{9, nil},
	} {
		got := sortedIDs(ix.FilesOfSegment(tc.seg))
		if !slices.Equal(got, sortedIDs(tc.want)) {
			t.Errorf("FilesOfSegment(%d) = %v, want %v", tc.seg, got, tc.want)
		}
	}
}

// segsOfEveryFile is the walk the registry replaced, re-expressed: every file
// entry's own segs table, inverted by brute force, plus the segments the
// file's read-out entries name.
func segsOfEveryFile(fs *files) (byTable, byEntry map[core.SegmentID][]core.FileID) {
	byTable, byEntry = map[core.SegmentID][]core.FileID{}, map[core.SegmentID][]core.FileID{}
	fs.walk(func(id core.FileID, f *file) {
		for _, s := range f.segs {
			byTable[s.seg] = append(byTable[s.seg], id)
		}
	})
	for _, id := range walkIDs(fs) {
		seen := map[core.SegmentID]bool{}
		for _, e := range fs.inspect(id) {
			if e.Kind != EntryMarker && !seen[e.Record.Segment] {
				seen[e.Record.Segment] = true
				byEntry[e.Record.Segment] = append(byEntry[e.Record.Segment], id)
			}
		}
	}
	return byTable, byEntry
}

// TestSegmentFilesMatchesTheEntries is the drift guard for the registry: after
// each step of a script that exercises every place a file's segment table
// changes (a record noted, a segment gone, a forget, an entry settled away),
// SegmentFiles of every segment must equal the files whose table names it,
// computed by walking every file; and every file with an entry naming the
// segment must be in the answer. A change point that misses its upkeep shows
// here as a disagreement, which nothing else would reveal.
func TestSegmentFilesMatchesTheEntries(t *testing.T) {
	var ix Index
	fs := &ix.fs
	a, b, c, d := fid(1), fid(2), fid(3), fid(4)
	check := func(step string) {
		t.Helper()
		byTable, byEntry := segsOfEveryFile(fs)
		for seg := core.SegmentID(0); seg < 8; seg++ {
			got := sortedIDs(ix.FilesOfSegment(seg))
			if want := sortedIDs(byTable[seg]); !slices.Equal(got, want) {
				t.Errorf("%s: FilesOfSegment(%d) = %v, want the files whose table names it, %v", step, seg, got, want)
			}
			for _, id := range byEntry[seg] {
				if !slices.Contains(got, id) {
					t.Errorf("%s: FilesOfSegment(%d) = %v lacks %v, whose entry names the segment", step, seg, got, id)
				}
			}
		}
	}
	check("empty")

	// Content-bearing files and a header-only one, over shared segments.
	for i, id := range []core.FileID{a, b, c} {
		seg := core.SegmentID(1 + i%2)
		_, err := fs.update(id, func(f *file) error {
			if err := f.recorded(seg, ext(0, 4), nums(uint64(i)+1, uint64(i)+1), 1, id); err != nil {
				return err
			}
			f.write(write(0, 4, rec(seg, 0), uint64(i)+1))
			return nil
		})
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	noteRec(t, fs, d, 1, ext(0, 4), nums(9, 9))
	noteRec(t, fs, d, 3, ext(4, 4), nums(10, 10))
	noteRec(t, fs, a, 3, ext(8, 4), nums(11, 11))
	check("records noted")

	// Re-noting a known segment adds nothing.
	noteRec(t, fs, a, 3, ext(12, 4), nums(12, 12))
	check("renote")

	// A repoint into another segment: c's piece at rec(1,0) moves to a
	// record in segment 5, so the destination answers with the file, and a
	// move within one segment raises nothing new.
	if _, _, err := ix.RepointRecord(c, rec(1, 0), rec(5, 32)); err != nil {
		t.Fatalf("repoint over the entry: %v", err)
	}
	check("repoint into another segment")
	if _, _, err := ix.RepointRecord(c, rec(5, 32), rec(5, 64)); err != nil {
		t.Fatalf("repoint within one segment: %v", err)
	}
	check("repoint within one segment")

	// A segment going from one file leaves it in the others.
	change(t, fs, d, func(f *file) error { f.segmentGone(1); return nil })
	check("segmentGone of one file")
	// A segment the file never held is a no-op.
	change(t, fs, d, func(f *file) error { f.segmentGone(7); return nil })
	check("segmentGone of an unknown segment")

	// The header-only file's last segment going settles its entry away.
	change(t, fs, d, func(f *file) error { f.segmentGone(3); return nil })
	check("entry settled away")

	// A forget below every record drops the file's segments with its ranges.
	if _, _, err := ix.ForgetFile(b, 1<<30, 1); err != nil {
		t.Fatalf("ForgetFile: %v", err)
	}
	check("forget")

	// A rebuilt record for a forgotten file registers it afresh.
	noteRec(t, fs, b, 2, ext(0, 4), nums(20, 20))
	check("noted after forget")
}
