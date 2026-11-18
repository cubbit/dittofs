package reclaim_test

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// walkedBytes walks the index of every file of the fixture's share and sums
// the held and the retained bytes each segment backs.
func walkedBytes(f *copyFixture) (held, retained map[core.SegmentID]int64) {
	held, retained = map[core.SegmentID]int64{}, map[core.SegmentID]int64{}
	for _, file := range f.ix.Files(copyTag) {
		for _, e := range f.ix.FileEntries(file) {
			switch e.Kind {
			case index.EntryHeld:
				held[e.Record.Segment] += e.Extent.Len
			case index.EntryRetained:
				retained[e.Record.Segment] += e.Extent.Len
			}
		}
	}
	return held, retained
}

// TestSegmentRows_AgreeWithAWalk drives a seeded sequence of overwrites,
// seals and repacks over a real journal core and, after every step, compares
// each row with a walk of the segment set and of the index, and the rows'
// held bytes with the journal-wide figure.
func TestSegmentRows_AgreeWithAWalk(t *testing.T) {
	p := &passFixture{f: newCopyFixture(t)}
	f := p.f
	rng := rand.New(rand.NewPCG(9, 44))
	files := []core.FileID{copyFile(1), copyFile(2), copyFile(3)}

	// disagreement compares the rows with a walk of the set and the index and
	// returns what differs, or "" when they agree.
	disagreement := func() string {
		held, retained := walkedBytes(f)
		streamOf := map[core.SegmentID]uint32{}
		for _, file := range files {
			for _, e := range f.ix.FileEntries(file) {
				streamOf[e.Record.Segment] = uint32(f.core.Streams().IndexFor(file))
			}
		}
		var sum int64
		var ids, got []core.SegmentID
		snap := f.set.Snapshot()
		for _, r := range snap {
			ids = append(ids, r.ID)
		}
		for _, row := range f.core.Segments() {
			if row.Retired {
				continue
			}
			got = append(got, row.ID)
			var want segment.Row
			for _, r := range snap {
				if r.ID == row.ID {
					want = r
				}
			}
			if row.Allocated != want.Size || row.Sealed != (want.State == segment.Sealed) || row.Failed != (want.State == segment.Ended) {
				return fmt.Sprintf("segment %d row says allocated %d sealed %t failed %t, the set walk says size %d state %v", row.ID, row.Allocated, row.Sealed, row.Failed, want.Size, want.State)
			}
			if row.Held != held[row.ID] || row.Retained != retained[row.ID] {
				return fmt.Sprintf("segment %d row says held %d retained %d, the index walk says %d and %d", row.ID, row.Held, row.Retained, held[row.ID], retained[row.ID])
			}
			if wantUn := reclaim.Unreclaimed(want.Size, held[row.ID]+retained[row.ID]); row.Unreclaimed != wantUn {
				return fmt.Sprintf("segment %d row says %d unreclaimed, want %d", row.ID, row.Unreclaimed, wantUn)
			}
			if row.Damaged || row.Retired {
				return fmt.Sprintf("segment %d row says damaged %t retired %t, nothing in this sequence damages a record", row.ID, row.Damaged, row.Retired)
			}
			if want, ok := streamOf[row.ID]; ok && row.Stream != want {
				return fmt.Sprintf("segment %d row says stream %d, the stream of its files is %d", row.ID, row.Stream, want)
			}
			sum += row.Held
		}
		if !slices.Equal(got, ids) {
			return fmt.Sprintf("the rows name segments %v, the set walk %v", got, ids)
		}
		if st := f.core.Stats(); sum != st.HeldBytes {
			return fmt.Sprintf("the rows' held bytes sum to %d, Stats says %d", sum, st.HeldBytes)
		}
		return ""
	}
	// check polls the real condition: a sync's worker lifts what a record
	// superseded after the call that covered it returned, so a retained
	// figure and the index it counts settle together a moment after the
	// step, and the rows are compared once they have. A disagreement that
	// never settles fails after the backstop.
	check := func(step string) {
		t.Helper()
		deadline := time.Now().Add(copyWait)
		for {
			d := disagreement()
			if d == "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %s: %s", step, d)
			}
			runtime.Gosched()
		}
	}

	check("the open")
	for i := range 60 {
		switch op := rng.IntN(10); {
		case op < 7:
			file := files[rng.IntN(len(files))]
			v, err := f.counters.Draw()
			if err != nil {
				t.Fatalf("Draw: %v", err)
			}
			x := core.Extent{Off: int64(rng.IntN(4)) * 4096, Len: 4096}
			f.plantAt(t, copyTag, file, x, make([]byte, x.Len), 0, v)
			check("a write")
		case op < 9:
			if _, ok := f.core.Streams().For(files[rng.IntN(len(files))]).OpenSegment(); ok {
				f.sealSource(t, files[rng.IntN(len(files))])
			}
			f.waitSpares(t)
			check("a seal")
		default:
			if _, err := f.core.Repack(t.Context(), 2*copySegCap, reclaim.Scope{}); err != nil {
				t.Fatalf("step %d: Repack: %v", i, err)
			}
			f.waitSpares(t)
			check("a repack")
		}
	}
}

// TestSegmentRows_SplitWriteRepacks plants writes until the segment cap splits
// one, then repacks the file's segments whole: a split record holds only the
// bytes its placement placed, so its pieces must name the sub-extents the way
// the writer's path publishes them — a repack that stitches a full-extent
// piece against a partial record's payload reads past it.
func TestSegmentRows_SplitWriteRepacks(t *testing.T) {
	f := newCopyFixture(t)
	file := copyFile(9)
	const want = 17 * 4096
	var planted []core.SegmentID
	for i := range 17 {
		v, err := f.counters.Draw()
		if err != nil {
			t.Fatalf("Draw: %v", err)
		}
		rec, _ := f.plantAt(t, copyTag, file, core.Extent{Off: int64(i) * 4096, Len: 4096}, make([]byte, 4096), 0, v)
		if !slices.Contains(planted, rec.Segment) {
			planted = append(planted, rec.Segment)
		}
	}
	f.sealSource(t, file)
	// The repacks run until the core's worker retires every segment the
	// planted writes landed in, the split record's among them, so the whole
	// file's content has been copied and repointed. Polling the rows, one
	// repack per round, with the fixture's own wait as the backstop.
	for deadline := time.Now().Add(copyWait); ; {
		var left []core.SegmentID
		for _, row := range f.core.Segments() {
			if !row.Retired && slices.Contains(planted, row.ID) {
				left = append(left, row.ID)
			}
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the segments %v of the split writes are still not retired after %v of repacks, want them copied and retired by the core's worker", left, copyWait)
		}
		if _, err := f.core.Repack(t.Context(), 2*copySegCap, reclaim.Scope{}); err != nil {
			t.Fatalf("Repack: %v", err)
		}
		f.waitSpares(t)
	}
	if st := f.core.Stats(); st.HeldBytes != want {
		t.Fatalf("after the repacks the journal holds %d bytes, want the %d planted", st.HeldBytes, want)
	}
}

// TestSegmentRows_SuspectAndClear reads a segment's record through the unread
// set under a read fault: the row reports the segment suspect with the range
// that failed, and reports neither once the range reads.
func TestSegmentRows_SuspectAndClear(t *testing.T) {
	p := &passFixture{f: newCopyFixture(t)}
	f := p.f
	file := copyFile(1)
	rec, _, _ := f.plant(t, copyTag, file, make([]byte, 4096), 0)
	f.sealSource(t, file)
	f.waitSpares(t)

	row := func() (suspect bool, ranges []core.SegmentRange) {
		for _, r := range f.core.Segments() {
			if r.ID == rec.Segment {
				return r.Suspect, r.Unread
			}
		}
		t.Fatalf("segment %d has no row", rec.Segment)
		return
	}
	if s, r := row(); s || len(r) != 0 {
		t.Fatalf("the row of a segment nothing failed to read says suspect %t with %v, want neither", s, r)
	}

	want := core.SegmentRange{Segment: rec.Segment, Off: int64(rec.Offset), Len: format.RecordHeaderSize}
	ref := unread.Ref{SegmentRange: want, Kind: unread.Content, File: file}
	stream := f.core.Streams().For(file)
	read := func() ([]byte, error) {
		buf := make([]byte, want.Len)
		_, err := stream.ReadAt(rec.Segment, buf, want.Off)
		return buf, err
	}
	if err := f.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(rec.Segment), 1, 0, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := f.core.Unread().Try(t.Context(), ref, read); err == nil {
		t.Fatal("the read under the fault returned no error")
	}
	if s, r := row(); !s || !slices.Equal(r, []core.SegmentRange{want}) {
		t.Errorf("the row after the failed read says suspect %t with %v, want suspect with %v", s, r, want)
	}

	f.seam.SetFault(nil)
	if _, err := f.core.Unread().Try(t.Context(), ref, read); err != nil {
		t.Fatalf("the read after the fault cleared: %v", err)
	}
	if s, r := row(); s || len(r) != 0 {
		t.Errorf("the row after the range read says suspect %t with %v, want neither: a row that never clears", s, r)
	}
}
