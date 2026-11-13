package index

import (
	"math"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestFilesHeldAndRemovalMarkers checks the two maintained tallies through
// the mutations that move them: a first held extent, a last one removed, a
// marker added and settled.
func TestFilesHeldAndRemovalMarkers(t *testing.T) {
	ix := New(1 << 20)
	check := func(step string, held, markers int64) {
		t.Helper()
		if got := ix.FilesHeld(); got != held {
			t.Fatalf("%s: FilesHeld = %d, want %d", step, got, held)
		}
		if got := ix.RemovalMarkers(); got != markers {
			t.Fatalf("%s: RemovalMarkers = %d, want %d", step, got, markers)
		}
	}
	check("empty", 0, 0)
	for i := range 3 {
		id := fid(byte(i + 1))
		if _, err := ix.WriteRetain(testTag, id, write(0, 4096, rec(1, uint32(i)*4096), uint64(i+1)), core.Seq(i+1)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	check("three files written", 3, 0)
	if _, err := ix.WriteRetain(testTag, fid(1), write(8192, 4096, rec(1, 16384), 4), 4); err != nil {
		t.Fatal(err)
	}
	check("second extent in a held file", 3, 0)
	if _, err := ix.ApplyRemoval(testTag, fid(1), core.ChangeTruncate, toEnd(0), ver(5), 5, rec(2, 0), nil); err != nil {
		t.Fatal(err)
	}
	check("truncate to zero", 2, 1)
	if _, err := ix.ApplyRemoval(testTag, fid(2), core.ChangeDelete, toEnd(0), ver(6), 6, rec(2, 64), nil); err != nil {
		t.Fatal(err)
	}
	check("delete", 1, 2)
	ix.SettleFor(testTag, fid(1), ver(5))
	check("settle one", 1, 1)
	ix.SettleFor(testTag, fid(2), ver(6))
	check("settle the other", 1, 0)
}

// TestRebuiltTalliesMatchIncremental checks that an index rebuilt from the
// same records reports the figures the incrementally built one does.
func TestRebuiltTalliesMatchIncremental(t *testing.T) {
	a, b, c := fid(1), fid(2), fid(3)
	recs := []Record{
		recd(RecWrite, a, 1, 1, 1, 0, 0, 4096),
		recd(RecWrite, b, 2, 2, 1, 4096, 0, 4096),
		recd(RecWrite, c, 3, 3, 1, 8192, 0, 4096),
		recd(RecTruncate, a, 4, 4, 2, 0, 0, 0),
		recd(RecDelete, b, 5, 5, 2, 64, 0, 0),
	}
	ix := New(1 << 20)
	for _, r := range recs[:3] {
		p := write(r.Off, r.Len, RecordAt{Segment: r.Segment, Offset: r.At}, r.Numbers.Version.Counter)
		if _, err := ix.WriteRetain(7, r.File, p, r.Numbers.Seq); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range recs[3:] {
		k := core.ChangeTruncate
		if r.Kind == RecDelete {
			k = core.ChangeDelete
		}
		if _, err := ix.ApplyRemoval(7, r.File, k, toEnd(0), r.Numbers.Version, r.Numbers.Seq, RecordAt{Segment: r.Segment, Offset: r.At}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var fs files
	fs.setExtentLimit(math.MaxInt64)
	var ctr Counters
	rb := newRebuilder(&fs, &ctr)
	for _, r := range recs {
		if err := rb.Feed(r); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if err := rb.Finish(ver(0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got, want := fs.filesHeld.Load(), ix.FilesHeld(); got != want || want != 1 {
		t.Fatalf("rebuilt FilesHeld = %d, incremental %d, want both 1", got, want)
	}
	if got, want := fs.removalMarkers.Load(), ix.RemovalMarkers(); got != want || want != 2 {
		t.Fatalf("rebuilt RemovalMarkers = %d, incremental %d, want both 2", got, want)
	}
}
