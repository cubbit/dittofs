package index

import (
	"errors"
	"sync"
	"testing"

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

// launchRecorded runs fn on n goroutines and waits for all of them, on the
// WaitGroup and not on a clock.
func launchRecorded(n int, fn func(i int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			fn(i)
		}()
	}
	wg.Wait()
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
	launchRecorded(8, func(i int) {
		// The gone path: a delete removes the file's marker only through a
		// settle, so the entry stays in the map until the settle below; that
		// pair drives out what had an entry, over rounds.
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
			if _, err := ix.Lose(recordedIDs[(i+1)%len(recordedIDs)], e, core.Version{Counter: v + 4}); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("Lose: %v", err)
				return
			}
			if _, err := ix.ApplyRemoval(tag, recordedIDs[(i+2)%len(recordedIDs)], core.ChangeDeallocate, e, core.Version{Counter: v + 4}, core.Seq(v+4), r, nil); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("ApplyRemoval: %v", err)
				return
			}
			// The gone path: the loss leaves only a marker, and the settle
			// drops it and with it the entry.
			gone := goneIDs[i%len(goneIDs)]
			if _, err := ix.Lose(gone, e, core.Version{Counter: v + 4}); err != nil && !errors.Is(err, core.ErrZeroLength) {
				t.Errorf("Lose on the gone path: %v", err)
				return
			}
			ix.fs.settleRemovals(gone, core.Version{Counter: v + 4})
			v++
		}
	})

	if vs := recorder.Violations(); len(vs) != 0 {
		for _, v := range vs {
			t.Errorf("recorder violation: %s acquiring %s over %v\n%s", v.Kind, v.Rank, v.Held, v.Stack)
		}
	}
	if held := recorder.Held(); len(held) != 0 {
		t.Fatalf("main goroutine holds ranks %v after the run, want none", held)
	}
	launchRecorded(8, func(int) {
		if held := recorder.Held(); len(held) != 0 {
			t.Errorf("worker goroutine holds ranks %v after the run, want none", held)
		}
	})
}

// TestIndexLocksNameTheirRanks: the file map and the file index are separate
// domains, and each ranked lock names its own. A file lock taken under a
// shard lock is reported as an acquire-under-map, and no acquisition the index
// itself makes is reported; polling the locks directly, so the check never
// depends on which call an index operation chose to make.
func TestIndexLocksNameTheirRanks(t *testing.T) {
	ix := New(1 << 20)
	recorder := lockorder.NewRecorder()
	ix.SetRecorder(recorder)

	// In-rank: a file lock over the shard lock is the order the index uses.
	// The write makes the file's entry — the one operation that creates
	// entries — under the recorder without a violation of its own.
	id := recordedIDs[0]
	if _, err := ix.WriteRetain(1, id, Placed{Extent: ext(0, 64), Record: rec(1, 0), Version: ver(1)}, core.Seq(1)); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	f := EntryAt(&ix.fs, id)
	unlockFile := LockFile(f)
	unlockShard := LockShard(&ix.fs, id)
	unlockShard()
	unlockFile()
	if vs := recorder.Violations(); len(vs) != 0 {
		t.Fatalf("in-order acquisitions recorded %d violations, want 0: %+v", len(vs), vs)
	}

	// Out-of-rank: a file lock under the shard lock is an acquire under the
	// map domain, the same report the first recorder rule would find for any
	// lock the index took that way.
	unlockShard = LockShard(&ix.fs, id)
	unlockFile = LockFile(f)
	unlockFile()
	unlockShard()
	vs := recorder.Violations()
	if len(vs) != 1 {
		t.Fatalf("a file lock under the shard lock recorded %d violations, want 1", len(vs))
	}
	if vs[0].Kind != lockorder.AcquireUnderMap {
		t.Fatalf("violation kind = %s, want AcquireUnderMap", vs[0].Kind)
	}
	if vs[0].Rank != lockorder.FileIndex {
		t.Fatalf("violation rank = %s, want file index", vs[0].Rank)
	}
	if len(vs[0].Held) != 1 || vs[0].Held[0] != lockorder.FileMap {
		t.Fatalf("violation held = %v, want [file map]", vs[0].Held)
	}
	if held := recorder.Held(); len(held) != 0 {
		t.Fatalf("goroutine holds ranks %v after the releases, want none", held)
	}
}
