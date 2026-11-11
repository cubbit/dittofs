package reclaim

import (
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// segCap is the cap the fixture's segments are accounted at.
const segCap = int64(64 << 10)

// tag is the share tag the seeded records claim their files under.
const tag = core.ShareTag(1)

func ver(n uint64) core.Version { return core.Version{Counter: n} }

func fid(n byte) core.FileID { return core.NewFileID([16]byte{15: n}) }

// fixture is a real index, a real segment ledger, and one real segment on a
// recorded storage seam: the segment exists on disk, holding nothing.
type fixture struct {
	ix   *index.Index
	set  *segment.Set
	seam *kit.Seam
	seg  core.SegmentID
}

// newFixture makes one spare over the seam and takes it into the set as the
// segment the tests' records live in.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(segCap, nil)
	maker := segment.NewMaker(seam.FS(), set, 1<<62, 0, nil)
	seg, err := maker.Make()
	if err != nil {
		t.Fatalf("the maker could not make a spare: %v", err)
	}
	if err := set.Take(seg); err != nil {
		t.Fatalf("Take: %v", err)
	}
	return &fixture{ix: index.New(1 << 20), set: set, seam: seam, seg: seg}
}

// write seeds one held extent of id inside the fixture's segment, at offset
// at in the segment, content version v and sequence number s. Disjoint
// extents supersede nothing, so nothing is retained.
func (f *fixture) write(t *testing.T, id core.FileID, off, n int64, at uint32, v, s uint64) {
	t.Helper()
	p := index.Placed{
		Extent:  core.Extent{Off: off, Len: n},
		Record:  index.RecordAt{Segment: f.seg, Offset: at},
		Version: ver(v),
	}
	if _, err := f.ix.WriteRetain(tag, id, p, core.Seq(s)); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
}

// remove seeds one removal of id over one seeded extent, at a version above
// every write's: it leaves a marker and stops the segment's bytes holding the
// extent, exactly as a settled truncate leaves it.
func (f *fixture) remove(t *testing.T, id core.FileID, off, n int64, v uint64) {
	t.Helper()
	if _, err := f.ix.Lose(id, core.Extent{Off: off, Len: n}, ver(v)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
}

// TestHeld_ZeroBytesNoRecords_NotHeld_NoReads: a segment whose three
// removals settled — the extents they cut are gone and no marker unsettled
// below them remains — reports not held from counters alone, with no
// registered record beside the bytes. The no-reads guarantee is structural:
// the tracker holds no handle to any segment's storage and reads only the
// index's counters and its own memory, so the seam below, the one the
// segment's records were made over, shows nothing while Held answers; the
// same call is made over a segment whose backing file is gone from the
// directory, which any read of the segment would fail on rather than
// answer.
func TestHeld_ZeroBytesNoRecords_NotHeld_NoReads(t *testing.T) {
	f := newFixture(t)
	// The seed reached the storage: the call log the no-reads check reads
	// is one that has counted calls, not one that asserts nothing.
	if got := f.seam.Calls(kit.OpAllocate); got == 0 {
		t.Fatalf("the seed made %d allocate calls, want some: the call log asserts nothing", got)
	}

	id := fid(1)
	tr := New(f.ix, nil)

	// Three records held bytes of the segment.
	for i := 0; i < 3; i++ {
		f.write(t, id, int64(i)*4096, 4096, uint32(i), uint64(i)+1, uint64(i)+1)
	}
	if b := f.ix.SegmentBytes(f.seg); b.Held != 3*4096 || b.Retained != 0 {
		t.Fatalf("the segment's bytes after three writes = held %d retained %d, want held %d retained 0", b.Held, b.Retained, 3*4096)
	}
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held while the segment backs bytes = %v, want true", got)
	}

	// Three removals above them settle the records: nothing below their
	// markers waits for a settle, and the segment backs nothing.
	for i := 0; i < 3; i++ {
		f.remove(t, id, int64(i)*4096, 4096, 10+uint64(i))
	}
	if b := f.ix.SegmentBytes(f.seg); b.Held != 0 || b.Retained != 0 {
		t.Fatalf("the segment's bytes after three removals = held %d retained %d, want 0 and 0", b.Held, b.Retained)
	}
	if got := f.ix.MarkerSettled(id, ver(9)); !got {
		t.Fatalf("MarkerSettled below the removals = %v, want true: every marker sits above 9", got)
	}

	reads := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of a segment of settled removals, bytes gone, records none = %v, want false", got)
	}
	if got := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect) - reads; got != 0 {
		t.Fatalf("Held made %d storage reads, want 0: the answer comes from counters alone", got)
	}

	// The same answer over a segment whose storage is gone: the backing
	// file is unlinked from the directory, so the answer still comes from
	// the counters and any read of the segment would have failed to make
	// it. The file was made through the same seam this test reads the
	// call log of.
	if err := f.seam.Remove(format.SegmentName(f.seg)); err != nil {
		t.Fatalf("Remove of the segment's file: %v", err)
	}
	reads = f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of a segment of settled removals, file gone = %v, want false", got)
	}
	if got := f.seam.Calls(kit.OpRead) + f.seam.Calls(kit.OpReadDirect) - reads; got != 0 {
		t.Fatalf("Held over a segment with no file made %d storage reads, want 0", got)
	}
}

// TestHeld_OneLiveRecord: one registered record keeps a segment that backs
// no bytes held, its file is kept under its segment, and the candidate fires
// exactly once when the segment's held state leaves through the tracker's
// own release — never while bytes or another record still hold it, and never
// for a release nothing counted.
func TestHeld_OneLiveRecord(t *testing.T) {
	f := newFixture(t)
	var fired []core.SegmentID
	tr := New(f.ix, func(seg core.SegmentID) { fired = append(fired, seg) })
	id := fid(7)

	if got := tr.Held(f.seg); got {
		t.Fatalf("Held of the empty segment = %v, want false", got)
	}

	rec := Rec{
		Kind: format.KindTruncate,
		File: id,
		Seg:  f.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(4), Seq: 9},
		Seq:  9,
	}
	tr.Register(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with one live record = %v, want true", got)
	}
	if _, ok := tr.stateOf(f.seg).files[id]; !ok {
		t.Fatalf("the record's file was not kept under its segment")
	}

	// Bytes beside the record: releasing the record leaves the segment
	// held by what it still backs, and the candidate stays quiet.
	f.write(t, fid(8), 0, 4096, 100, 20, 20)
	tr.Unregister(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held after the record's release, bytes still backed = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the segment held bytes, want 0", got)
	}

	// The bytes leaving through the index moves the answer to false, but a
	// release the tracker did not make fires nothing.
	f.remove(t, fid(8), 0, 4096, 30)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the bytes left = %v, want false", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times for an index-driven release, want 0", got)
	}

	// The transition itself: two records, one release leaving the other
	// live, then the last release firing the candidate once.
	second := rec
	second.File = fid(9)
	second.Ext = core.Extent{Off: 4096, Len: 4096}
	tr.Register(rec)
	tr.Register(second)
	tr.Unregister(rec)
	if got := tr.Held(f.seg); !got {
		t.Fatalf("Held with the second record still live = %v, want true", got)
	}
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while a record was still live, want 0", got)
	}
	tr.Unregister(second)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after the last record's release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: one held-to-not-held transition", fired, f.seg)
	}

	// A release of what nothing counted changes nothing.
	tr.Unregister(second)
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after a release of nothing = %v, want false", got)
	}
	if got := len(fired); got != 1 {
		t.Fatalf("the candidate fired %d times after a release of nothing, want 1", got)
	}
}

// TestRegisterUnregisterAllocations checks that an update to a segment whose
// state already exists, of a file the segment already knows, allocates
// nothing — the steady update to an existing segment and file, which is the
// common shape; a segment's or a file's first record may allocate its state
// and its table entries. Measured outside -race, which changes allocation
// counts.
func TestRegisterUnregisterAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	f := newFixture(t)
	tr := New(f.ix, nil)
	rec := Rec{
		Kind: format.KindTruncate,
		File: fid(7),
		Seg:  f.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(4), Seq: 9},
		Seq:  9,
	}

	// Warm the segment's state and its files table so the measured runs
	// find both in place.
	tr.Register(rec)
	tr.Unregister(rec)

	rg := testing.AllocsPerRun(1000, func() { tr.Register(rec) })
	// Enough live records that every measured release decrements one.
	for i := 0; i < 2000; i++ {
		tr.Register(rec)
	}
	un := testing.AllocsPerRun(1000, func() { tr.Unregister(rec) })
	if rg != 0 || un != 0 {
		t.Fatalf("allocations per Register = %v, per Unregister = %v, want 0 and 0", rg, un)
	}
}

// TestHeld_ConcurrentRelease: many goroutines register and release the same
// segment's records, and the candidate fires exactly once for the one
// held-to-not-held transition, whichever goroutine's release crosses the
// line. The second case runs in two ordered steps, so nothing in it depends
// on how the releases interleave with the index: first the same release
// storm over a segment whose bytes hold it — no release crosses the line the
// bytes keep, and the candidate fires nothing while they last — then, with
// the releases joined, one removal leaves the records the only thing held,
// and a single register-and-release pair on the main goroutine is the one
// transition the candidate answers.
func TestHeld_ConcurrentRelease(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	// Case 1: records alone. Every goroutine registers its records, waits
	// on a barrier so all are live, then releases them.
	f := newFixture(t)
	var mu sync.Mutex
	fired := make([]core.SegmentID, 0, 2)
	tr := New(f.ix, func(seg core.SegmentID) {
		mu.Lock()
		fired = append(fired, seg)
		mu.Unlock()
	})
	var ready, done sync.WaitGroup
	ready.Add(goroutines)
	done.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer done.Done()
			rec := Rec{
				Kind: format.KindTruncate,
				File: fid(byte(g + 1)),
				Seg:  f.seg,
				Ext:  core.Extent{Off: int64(g) * 4096, Len: 4096},
				Pair: index.Numbers{Version: ver(uint64(g) + 1), Seq: core.Seq(g)},
				Seq:  core.Seq(g),
			}
			for i := 0; i < perGoroutine; i++ {
				tr.Register(rec)
			}
			ready.Done()
			ready.Wait()
			for i := 0; i < perGoroutine; i++ {
				tr.Unregister(rec)
			}
		}(g)
	}
	done.Wait()
	if got := tr.Held(f.seg); got {
		t.Fatalf("Held after every record's release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f.seg {
		t.Fatalf("the candidate fired %v, want [%d]: one transition however many releases raced", fired, f.seg)
	}

	// Case 2a: bytes beside the records. The same release storm, joined
	// before anything else moves: the bytes hold the segment through every
	// one of the 400 releases, so no release crosses the line and the
	// candidate fires nothing, and the segment still answers held.
	f2 := newFixture(t)
	mu.Lock()
	fired = fired[:0]
	mu.Unlock()
	tr2 := New(f2.ix, func(seg core.SegmentID) {
		mu.Lock()
		fired = append(fired, seg)
		mu.Unlock()
	})
	owner := fid(90)
	f2.write(t, owner, 0, 4096, 0, 50, 50)
	var ready2, done2 sync.WaitGroup
	ready2.Add(goroutines)
	done2.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer done2.Done()
			rec := Rec{
				Kind: format.KindTruncate,
				File: fid(byte(g + 1)),
				Seg:  f2.seg,
				Ext:  core.Extent{Off: int64(g) * 4096, Len: 4096},
				Pair: index.Numbers{Version: ver(uint64(g) + 1), Seq: core.Seq(g)},
				Seq:  core.Seq(g),
			}
			for i := 0; i < perGoroutine; i++ {
				tr2.Register(rec)
			}
			ready2.Done()
			ready2.Wait()
			for i := 0; i < perGoroutine; i++ {
				tr2.Unregister(rec)
			}
		}(g)
	}
	done2.Wait()
	if got := len(fired); got != 0 {
		t.Fatalf("the candidate fired %d times while the bytes held the segment, want 0: no release crosses the line it holds", got)
	}
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held after the releases, bytes still backed = %v, want true", got)
	}

	// Case 2b: the bytes leave, then one release on the main goroutine —
	// after the removal, the single register-and-release pair is the
	// tracker's own transition, and the candidate fires exactly once.
	f2.remove(t, owner, 0, 4096, 60)
	last := Rec{
		Kind: format.KindTruncate,
		File: owner,
		Seg:  f2.seg,
		Ext:  core.Extent{Off: 0, Len: 4096},
		Pair: index.Numbers{Version: ver(60), Seq: 60},
		Seq:  60,
	}
	tr2.Register(last)
	if got := tr2.Held(f2.seg); !got {
		t.Fatalf("Held with one live record after the removal = %v, want true", got)
	}
	tr2.Unregister(last)
	if got := tr2.Held(f2.seg); got {
		t.Fatalf("Held after the removal and the last release = %v, want false", got)
	}
	if len(fired) != 1 || fired[0] != f2.seg {
		t.Fatalf("the candidate fired %v, want [%d]: the release after the bytes left is the one transition", fired, f2.seg)
	}
}
