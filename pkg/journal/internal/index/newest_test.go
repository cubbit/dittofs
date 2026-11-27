package index

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// newestMismatch compares Newest's answer for id with the newest place the
// file's read-out names, the walk Newest replaced, and describes the first
// way they differ.
func newestMismatch(fs *files, id core.FileID) error {
	var want RecordAt
	for _, e := range fs.inspect(id) {
		if e.Record != (RecordAt{}) && (want == (RecordAt{}) || newer(e.Record, want)) {
			want = e.Record
		}
	}
	got, ok := fs.newestRecord(id)
	if ok != (want != (RecordAt{})) || got != want {
		return fmt.Errorf("index: Newest(%v) = %+v, %v; the read-out's newest place is %+v", id, got, ok, want)
	}
	return nil
}

// TestNewestFollowsTheNewestPieceLeaving checks Newest after each step of a
// file's pieces, against the walk of its read-out, through the steps its
// shortcut cannot see on its own: a piece from a lower place never takes the
// newest's place; the newest record's last held piece cut away leaves an
// older one newest, which only the walk finds; a retained piece from a newer
// place takes over, and leaves again when it is dropped; and nothing held at
// all answers none.
func TestNewestFollowsTheNewestPieceLeaving(t *testing.T) {
	var ix Index
	fs := &ix.fs
	fs.setExtentLimit(1 << 20)
	id := fid(1)
	step := func(name string, fn func(*file) error) {
		t.Helper()
		if _, err := fs.update(id, fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := newestMismatch(fs, id); err != nil {
			t.Fatalf("after %s: %v", name, err)
		}
	}
	put := func(seg core.SegmentID, off uint32, e core.Extent, v uint64) func(*file) error {
		return func(f *file) error {
			f.held.put(Piece{Extent: e, Record: RecordAt{Segment: seg, Offset: off}, Version: ver(v), Seq: core.Seq(v)})
			return nil
		}
	}
	step("a first record at [0,8)", put(2, 0, ext(0, 8), 1))
	step("a second record at [8,16)", put(3, 0, ext(8, 8), 2))
	step("a lower place over [0,4)", put(1, 64, ext(0, 4), 3))
	step("the newest record's extent cut whole", func(f *file) error {
		f.held.cut(ext(8, 8))
		return nil
	})
	step("its own extent retained again from the newest place", func(f *file) error {
		f.retained.add(Retained{Piece: Piece{Extent: ext(8, 2), Record: RecordAt{Segment: 4, Offset: 8}, Version: ver(4), Seq: 4}, By: ver(5)})
		return nil
	})
	step("the retained piece dropped", func(f *file) error {
		f.retained.drop(ext(8, 2), func(Retained) bool { return true }, nil)
		return nil
	})
	step("everything cut", func(f *file) error {
		f.held.cut(ext(0, 16))
		return nil
	})
}
