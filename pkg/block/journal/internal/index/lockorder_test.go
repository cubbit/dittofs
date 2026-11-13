package index

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// recordedIDs is the files the recorded run writes to, alternating between
// two share tags so tag claims are exercised too. Four files over two shards
// is the least that puts two files' entries behind one shard lock, which the
// concurrent run needs: each entry takes the file index domain of its own
// file, never another's, and the map lock never covers a second acquisition.
var recordedIDs = []core.FileID{fid(1), fid(2), fid(3), fid(4)}

// goneIDs is a second set of files the run only loses and settles: a loss
// notes no record, so settling the loss drives the entry out of the map
// whole — the gone path, where view and commit find an entry another
// goroutine has just settled out and look the file up again. Pairs of
// goroutines share one, so the retry runs for real.
var goneIDs = []core.FileID{fid(9), fid(10), fid(11), fid(12)}

const recordedRounds = 200

// recordedBound is the one backstop the recorded run's join waits behind:
// crossing it proves the run deadlocked, never that it was slow, and the
// test then fails with every goroutine's stack from its own goroutine.
const recordedBound = 60 * time.Second

// launchRecorded runs fn on n goroutines and waits for all of them, on the
// WaitGroup and not on a clock; the bound only turns a hang into a failure
// that carries the stacks, where the binary's own timeout would carry none.
func launchRecorded(t *testing.T, n int, fn func(i int)) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			fn(i)
		}()
	}
	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(recordedBound):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("the recorded run did not return within %v; the goroutine stacks follow:\n%s", recordedBound, buf)
	}
}

// TestIndexUnderRecorder: a concurrent mix of the index's own operations,
// under a recorder that polices the lock order, records no violation and
// leaves every goroutine holding nothing.
func TestIndexUnderRecorder(t *testing.T) {
	ix := New(1 << 20)
	recorder := lockorder.NewRecorder()
	ix.SetRecorder(recorder)

	// Every goroutine writes its own versions, so one file's versions rise
	// monotonically and a stale fill is a real refusal, not an accident of
	// interleaving. Core growth needs no composition: every extent below 4 KiB
	// occupied is exactly one held piece.
	e := core.Extent{Off: 0, Len: 512}
	r := RecordAt{Segment: core.SegmentID(1), Offset: 0}
	launchRecorded(t, 8, func(i int) {
		v := uint64(i + 1)
		for range recordedRounds {
			tag := core.ShareTag(i%2 + 1)
			if _, err := ix.WriteRetain(tag, recordedIDs[i%len(recordedIDs)], Placed{Extent: e, Record: r, Version: core.Version{Counter: v + 8}}, core.Seq(v+8)); err != nil {
				t.Errorf("WriteRetain: %v", err)
				return
			}
			ix.Span(recordedIDs[i%len(recordedIDs)], e)
			ix.Lookup(recordedIDs[i%len(recordedIDs)], 0)
			ix.Retained(recordedIDs[i%len(recordedIDs)])
			ix.AsOf(recordedIDs[i%len(recordedIDs)])
			ix.FileEntries(recordedIDs[i%len(recordedIDs)])
			if _, err := ix.Fill(tag, recordedIDs[i%len(recordedIDs)], Placed{Extent: e, Record: r, Version: core.Version{Counter: v}}, core.Seq(v), core.Version{Counter: v + 4}); err != nil && !errors.Is(err, core.ErrStaleFill) {
				t.Errorf("Fill: %v", err)
				return
			}
			if _, _, err := ix.Lose(recordedIDs[(i+1)%len(recordedIDs)], e, core.Version{Counter: v + 4}); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("Lose: %v", err)
				return
			}
			if _, _, err := ix.ApplyRemoval(tag, recordedIDs[(i+2)%len(recordedIDs)], core.ChangeDeallocate, e, core.Version{Counter: v + 4}, core.Seq(v+4), r, nil); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("ApplyRemoval: %v", err)
				return
			}
			// The gone path: the loss leaves only a marker, and the settle
			// drops it and with it the entry.
			gone := goneIDs[i%len(goneIDs)]
			if _, _, err := ix.Lose(gone, e, core.Version{Counter: v + 4}); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("Lose on the gone path: %v", err)
				return
			}
			ix.fs.settleRemovals(gone, core.Version{Counter: v + 4})
			v++
		}
		// On this very goroutine, after its last release: a hold the test
		// leaked would be on the books of no other goroutine but this one.
		if held := recorder.Held(); len(held) != 0 {
			t.Errorf("worker %d holds ranks %v after the run, want none", i, held)
		}
	})

	if vs := recorder.Violations(); len(vs) != 0 {
		for _, v := range vs {
			t.Errorf("recorder violation: %s acquiring %s over %v\n%s", v.Kind, v.Rank, v.Held, v.Stack)
		}
		return
	}
	if held := recorder.Held(); len(held) != 0 {
		t.Fatalf("main goroutine holds ranks %v after the run, want none", held)
	}

	// A run of acquisitions the recorder never saw would pass every check
	// above: rank-0 locks are invisible to it. So the same recorder is made
	// to prove it saw the index's locks: with a higher rank held on this
	// goroutine, an index read takes a shard lock and then the file's lock,
	// and each that goes unreported would mean an unranked lock. A second
	// read drives the same through an entry made after SetRecorder, the
	// easiest one to leave unranked. The first run's zero is already
	// asserted, so every violation below is the probe's.
	probe := lockorder.SegmentAppend
	recorder.Acquire(probe, probe)
	ix.AsOf(recordedIDs[0])
	vs := recorder.Violations()
	if len(vs) != 2 {
		t.Fatalf("the probe run recorded %d violations, want 2: %+v", len(vs), vs)
	}
	sawFileMap, sawFileIndex := false, false
	for _, v := range vs {
		if v.Kind != lockorder.OutOfOrder {
			t.Errorf("probe violation kind = %v, want out of order (acquiring %s over %v)", v.Kind, v.Rank, v.Held)
		}
		switch v.Rank {
		case lockorder.FileMap:
			sawFileMap = true
		case lockorder.FileIndex:
			sawFileIndex = true
		default:
			t.Errorf("probe violation rank = %s, want file map or file index", v.Rank)
		}
	}
	if !sawFileMap || !sawFileIndex {
		t.Fatalf("probe violations = %v; want one file map and one file index, i.e. the rank 0 of a dropped Init must fail here", vs)
	}
	// Entries made after SetRecorder are Init'ed from the same recorder; the
	// probe must find the late entry's locks recorded too: its read takes the
	// shard lock and then the file's, one violation each.
	late := fid(21)
	if _, err := ix.WriteRetain(1, late, Placed{Extent: ext(0, 64), Record: rec(2, 0), Version: ver(1)}, core.Seq(1)); err != nil {
		t.Fatalf("WriteRetain of the late entry: %v", err)
	}
	before := len(recorder.Violations())
	ix.Span(late, ext(0, 64))
	vs = recorder.Violations()
	if n := len(vs) - before; n != 2 {
		t.Fatalf("the late entry's read recorded %d new violations, want 2: %+v", n, vs[before:])
	}
	for i, want := range []lockorder.Rank{lockorder.FileMap, lockorder.FileIndex} {
		if v := vs[before+i]; v.Kind != lockorder.OutOfOrder || v.Rank != want {
			t.Fatalf("late-entry violation %d = %s of %s, want out of order of %s", i, v.Kind, v.Rank, want)
		}
	}
	recorder.Release(probe)
	if held := recorder.Held(); len(held) != 0 {
		t.Fatalf("main goroutine holds ranks %v after the probe's release, want none", held)
	}
}
