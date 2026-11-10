package datapath

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// tagA and tagB are the two shares' tags. The files below carry bytes that
// would read as the other share's tag, so nothing may derive a share from a
// FileID's own bytes.
const (
	tagA core.ShareTag = 0x11
	tagB core.ShareTag = 0x22
)

// twoHandles attaches the two shares on c: A's and B's handles.
func twoHandles(t *testing.T, c *Core) (a, b *Share) {
	t.Helper()
	return mustShare(t, c, tagA), mustShare(t, c, tagB)
}

// twoFilesOnTwoStreams returns two files whose ids carry each other's tagged
// bytes — F1's read as B's, F2's as A's — and asserts they sit on the core's
// two distinct streams, so sharing one stream cannot pass unsaid.
func twoFilesOnTwoStreams(t *testing.T, c *Core) (f1, f2 core.FileID) {
	t.Helper()
	f1, f2 = fileWith(0x22, 0x01), fileWith(0x11, 0x01)
	if c.Streams().For(f1) == c.Streams().For(f2) {
		t.Fatal("the two files share one stream; pick ids the streams differ on")
	}
	return f1, f2
}

// wantOtherShare asserts err is an ErrOtherShare of f asked through tag and
// owned by owner, naming both tags.
func wantOtherShare(t *testing.T, err error, f core.FileID, tag, owner core.ShareTag) {
	t.Helper()
	if !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("the call through %d = %v, want ErrOtherShare", tag, err)
	}
	var e *core.OtherShareError
	if !errors.As(err, &e) || e.File != f || e.Tag != tag || e.Owner != owner {
		t.Fatalf("the error = %#v, want an OtherShareError of %v through %d owned by %d", e, f, tag, owner)
	}
}

// tagOnly leaves A's one write of F unsynced and fails its sync, so the file's
// held piece is dropped and the entry F keeps is kept by the segment that
// still holds the record: F's tag-only life, the state a rebuild reaches from
// the record on disk and a call through B is refused for. It settles the
// failure first, so the assertions below see the applied failure, and returns
// with A still owning F.
func tagOnly(t *testing.T) (*Core, *Share, *Share, core.FileID) {
	t.Helper()
	c, seam := newTestCore(t)
	a, b := twoHandles(t, c)
	f := testFile(17)
	if _, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1); err != nil {
		t.Fatalf("A's WriteAt of F: %v", err)
	}
	// The segment holding the record, named by the piece the write published.
	p, ok := c.Index().Lookup(f, 0)
	if !ok {
		t.Fatalf("F holds no piece at 0 after A's write")
	}
	seg := format.SegmentName(p.Record.Segment)
	if err := seam.Schedule(kit.Failing(kit.OpSync, seg, 1, errors.New("injected sync failure"))); err != nil {
		t.Fatalf("the seam refused the failing sync rule: %v", err)
	}
	if err := c.Durability().Sync(context.Background(), f); err == nil {
		t.Fatal("the sync of F's unsynced record returned nil")
	} else if !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("the failed sync returned %v, want a SyncError", err)
	}
	// The failure is applied once the window's records are dropped: F then
	// holds no piece, and its record — still in its ended segment on disk —
	// is the one thing that keeps its entry and its tag.
	eventually(t, "F's held piece to be dropped by the failed window", func() bool {
		_, ok := c.Index().Lookup(f, 0)
		return !ok
	})
	if err := c.checkTag(b, f); err == nil {
		t.Fatal("the tag-only file checks clean through B, want ErrOtherShare")
	} else {
		wantOtherShare(t, err, f, tagB, tagA)
	}
	if _, err := c.WriteAt(context.Background(), b, f, 4, []byte("bb"), 1); !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("a write of the tag-only file through B = %v, want ErrOtherShare", err)
	}
	// No record of B's refused write reached the disk.
	for _, rec := range readRecords(t, seam) {
		if rec.Header.File == f && rec.Header.Tag == tagB {
			t.Fatalf("a record of B's refused write reached the disk: %+v", rec.Header)
		}
	}
	// The tag-only life refuses B and serves A: A still writes F.
	if _, err := c.WriteAt(context.Background(), a, f, 4, []byte("cc"), 1); err != nil {
		t.Fatalf("A's write of the tag-only file: %v", err)
	}
	return c, a, b, f
}

func TestRFC1C_FileIsItsSharesShare(t *testing.T) {
	t.Run("refused_through_other_share", func(t *testing.T) {
		c, seam := newTestCore(t)
		a, b := twoHandles(t, c)
		// F's own bytes name B's tag, so the handle alone decides the share.
		f := fileWith(0x22, 0x11)
		if _, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("A's WriteAt of F: %v", err)
		}
		written, next := c.WrittenBytes(), c.counters.NextVersion().Counter
		seq, err := c.counters.NextSeq()
		if err != nil {
			t.Fatal(err)
		}
		entries := c.Index().FileEntries(f)
		recs := len(readRecords(t, seam))
		h, d, r := c.heldBytes()

		p := []byte("bbbb")
		v, err := c.WriteAt(context.Background(), b, f, 8, p, 1)
		wantOtherShare(t, err, f, tagB, tagA)
		if v != (core.Version{}) {
			t.Fatalf("the refused write returned version %v", v)
		}
		// Serves, appends, changes and offers nothing: the payload, the
		// counters, the counters' next issue, the entries, the tag and the
		// records on disk are all as they were. seq was drawn by this test,
		// so the next one the counters owe is seq+1: any less and the
		// refused write drew one.
		if string(p) != "bbbb" {
			t.Fatalf("the refused write changed the payload: %q", p)
		}
		if got := c.WrittenBytes(); got != written {
			t.Fatalf("WrittenBytes = %d after the refusal, want %d", got, written)
		}
		if got, err := c.counters.NextSeq(); err != nil || got != seq+1 {
			t.Fatalf("NextSeq = %d, %v; the refused write drew a sequence number", got, err)
		}
		if got := c.counters.NextVersion().Counter; got != next {
			t.Fatalf("the next version after the refusal = %d, want %d: a version was drawn", got, next)
		}
		if gh, gd, gr := c.heldBytes(); gh != h || gd != d || gr != r {
			t.Fatalf("held/dirty/retained after the refusal = %d/%d/%d, want %d/%d/%d", gh, gd, gr, h, d, r)
		}
		if got := len(c.Index().FileEntries(f)); got != len(entries) {
			t.Fatalf("F holds %d entries after the refusal, want %d: %+v -> %+v", got, len(entries), entries, c.Index().FileEntries(f))
		}
		if piece, ok := c.Index().Lookup(f, 8); ok {
			t.Fatalf("a piece of the refused write was published: %+v", piece)
		}
		if got := len(readRecords(t, seam)); got != recs {
			t.Fatalf("%d records on disk after the refusal, want %d", got, recs)
		}
		for i, rec := range readRecords(t, seam) {
			if rec.Header.File == f && rec.Header.Tag == tagB {
				t.Fatalf("record %d of F carries tag %d: the refused write appended", i, rec.Header.Tag)
			}
		}
		// The same refusal through the check every later file-naming call
		// makes first, and a clean check on a file nobody owns.
		if err := c.checkTag(b, f); err == nil {
			t.Fatal("checkTag through B of A's file = nil, want ErrOtherShare")
		} else {
			wantOtherShare(t, err, f, tagB, tagA)
		}
		if err := c.checkTag(b, fileWith(0x33, 0x44)); err != nil {
			t.Fatalf("checkTag of a file nobody owns = %v, want nil", err)
		}
	})

	t.Run("refused_before_other_validation", func(t *testing.T) {
		// The check is the first thing a call does after its context and
		// handle checks, so even a call that would change nothing on its own
		// merits — a zero-length write, a negative offset, an overflowing
		// one — names a FileID and is refused through the wrong handle
		// before the shape of what it asked is even looked at.
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		f := testFile(21)
		if _, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("A's WriteAt of F: %v", err)
		}
		if v, err := c.WriteAt(context.Background(), b, f, 0, nil, 1); err == nil || !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("a zero-length write through B on A's file = %v, %v, want ErrOtherShare", v, err)
		} else if v != (core.Version{}) {
			t.Fatalf("the refused zero-length write returned version %v", v)
		}
		for _, off := range []int64{-1, math.MaxInt64 - 2} {
			if v, err := c.WriteAt(context.Background(), b, f, off, []byte("bbbb"), 1); !errors.Is(err, core.ErrOtherShare) {
				t.Fatalf("a write of F through B at offset %d = %v, %v, want ErrOtherShare", off, v, err)
			}
		}

		// Through the owner and on an untagged file the shapes stand on
		// their own: a zero-length write returns the zero version and nil
		// and claims nothing, so a later write through the other handle
		// still claims the file.
		if v, err := c.WriteAt(context.Background(), a, f, 0, nil, 1); err != nil || v != (core.Version{}) {
			t.Fatalf("a zero-length write through A = %v, %v, want the zero version and nil", v, err)
		}
		g := testFile(22)
		if v, err := c.WriteAt(context.Background(), a, g, 0, nil, 1); err != nil || v != (core.Version{}) {
			t.Fatalf("a zero-length write on an untagged file = %v, %v, want the zero version and nil", v, err)
		}
		if err := c.Index().CheckTag(tagA, g); err != nil {
			t.Fatalf("the zero-length write claimed G for A: %v", err)
		}
		if _, err := c.WriteAt(context.Background(), b, g, 0, []byte("bbbb"), 1); err != nil {
			t.Fatalf("B's claim of G after A's zero-length write: %v", err)
		}
	})

	t.Run("resolved_by_handle_alone", func(t *testing.T) {
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		// The same FileID value is nobody's: the share a call reaches is the
		// handle it goes through, so each of the two files is its writer's,
		// whatever bytes its id carries. The two ids carry each other's
		// tagged bytes and sit on the two streams.
		fa, fb := twoFilesOnTwoStreams(t, c)
		if _, err := c.WriteAt(context.Background(), a, fa, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("A's write of F1: %v", err)
		}
		if _, err := c.WriteAt(context.Background(), b, fb, 0, []byte("bbbb"), 1); err != nil {
			t.Fatalf("B's write of F2: %v", err)
		}
		if _, err := c.WriteAt(context.Background(), b, fa, 4, []byte("cc"), 1); !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("a write of A's file through B = %v, want ErrOtherShare", err)
		}
		if _, err := c.WriteAt(context.Background(), a, fb, 4, []byte("cc"), 1); !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("a write of B's file through A = %v, want ErrOtherShare", err)
		}
		if _, err := c.WriteAt(context.Background(), a, fa, 4, []byte("cc"), 1); err != nil {
			t.Fatalf("a write of A's file through A: %v", err)
		}
		if _, err := c.WriteAt(context.Background(), b, fb, 4, []byte("cc"), 1); err != nil {
			t.Fatalf("a write of B's file through B: %v", err)
		}
	})

	t.Run("forget_hands_file_over", func(t *testing.T) {
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		f := testFile(16)
		if _, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("A's WriteAt of F: %v", err)
		}
		// ClearTag drops a file's tag whole: the index side of forgetting a
		// share's tag. F is nobody's again, so B's next write claims it.
		if err := c.Index().ClearTag(f); err != nil {
			t.Fatalf("ClearTag: %v", err)
		}
		if err := c.checkTag(b, f); err != nil {
			t.Fatalf("a check through B after the clear = %v, want nil", err)
		}
		v, err := c.WriteAt(context.Background(), b, f, 0, []byte("bbbb"), 1)
		if err != nil {
			t.Fatalf("B's write after the clear: %v", err)
		}
		if v == (core.Version{}) {
			t.Fatal("B's claiming write drew no version")
		}
		if _, err := c.WriteAt(context.Background(), a, f, 4, []byte("dd"), 1); !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("a write of B's file through A after the handover = %v, want ErrOtherShare", err)
		}
	})

	t.Run("tag_outlives_entry", func(t *testing.T) {
		t.Run("the_index_keeps_the_tag", func(t *testing.T) {
			tagOnly(t)
		})

		t.Run("same_through_a_second_core", func(t *testing.T) {
			c, _, _, f := tagOnly(t)
			// This is not a rebuild: the second core continues from the same
			// index object, so what it pins is the carried tag-only life
			// answering through another core exactly as it does through the
			// first. A rebuild is pinned at the index level, where the
			// rebuilder is reachable: TestTagConflictsFromRebuild and its
			// neighbours in index/tags_test.go hold that a file whose records
			// name one tag claims that tag, a rivalled one its conflict. The
			// state a segment's last record leaving the disk leaves — the
			// next claim of the file a fresh one — is pinned there too, by
			// TestTagOutlivesChangeVersion and TestTagConflictsLeaveWithTheEntry.
			c2, _ := newTestCore(t, func(o *coreOptions) { o.state = State{Index: c.Index()} })
			a2, b2 := twoHandles(t, c2)
			if _, err := c2.WriteAt(context.Background(), b2, f, 4, []byte("bb"), 1); !errors.Is(err, core.ErrOtherShare) {
				t.Fatalf("a write of the carried tag-only file through B = %v, want ErrOtherShare", err)
			}
			if _, err := c2.WriteAt(context.Background(), a2, f, 4, []byte("cc"), 1); err != nil {
				t.Fatalf("A's write of the carried tag-only file: %v", err)
			}
		})
	})

	t.Run("read_does_not_claim", func(t *testing.T) {
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		f := testFile(18)
		// The check a read names the file with claims nothing, so A's later
		// write still claims F for A, as it would with no read at all.
		if err := c.Index().CheckTag(tagB, f); err != nil {
			t.Fatalf("CheckTag through B of an untagged file = %v, want nil", err)
		}
		if err := c.checkTag(b, f); err != nil {
			t.Fatalf("checkTag through B of an untagged file = %v, want nil", err)
		}
		if _, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("A's write after B's read-named check: %v", err)
		}
		if err := c.checkTag(b, f); err == nil {
			t.Fatal("a check through B after A's claim = nil, want ErrOtherShare")
		} else {
			wantOtherShare(t, err, f, tagB, tagA)
		}
	})

	t.Run("refused_write_leaves_untagged", func(t *testing.T) {
		counters := &index.Counters{}
		if err := counters.SetVersionLimit(core.Version{Counter: 1}); err != nil {
			t.Fatal(err)
		}
		c, _ := newTestCore(t, func(o *coreOptions) { o.state = State{Counters: counters} })
		a, b := twoHandles(t, c)
		g := testFile(19)
		// A write refused before any state changes leaves the file untagged,
		// whatever refused it: G stays nobody's, so a write through B once
		// the version limit is raised claims G for B, never A.
		if _, err := c.WriteAt(context.Background(), a, g, 0, []byte("aaaa"), 1); !errors.Is(err, core.ErrNoSpace) {
			t.Fatalf("WriteAt at the version limit = %v, want ErrNoSpace", err)
		}
		if err := c.Index().CheckTag(tagA, g); err != nil {
			t.Fatalf("the refused write claimed G for A: %v", err)
		}
		if err := counters.SetVersionLimit(core.Version{Counter: 1 << 40}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.WriteAt(context.Background(), b, g, 0, []byte("aaaa"), 1); err != nil {
			t.Fatalf("B's write once the limit was raised: %v", err)
		}
		if err := c.Index().CheckTag(tagA, g); err == nil {
			t.Fatal("G answers to A after B's claim, want B")
		}
	})

	t.Run("claim_is_one_step", func(t *testing.T) {
		f := testFile(20)
		parked := make(chan struct{})
		release := make(chan struct{})
		var armed atomic.Bool
		waiting := make(chan struct{})
		c, seam := newTestCore(t, func(o *coreOptions) {
			o.afterAppend = func() {
				select {
				case <-release:
				default:
					close(parked)
					<-release
				}
			}
			// The domain table closes waiting the first time B's write asks
			// for F's domain, which proves B passed its in-front-of-the-
			// domain check: that check, against the entry before A's publish,
			// must have found F untagged. Only a write that reaches the
			// domain can be told the claim it could not see — a timer here
			// would only catch B if it arrived inside the timer's window and
			// would pass, in-domain check removed, whenever it arrived later
			// and its early check met A's published claim.
			o.wrapDomains = func(d durability.Domains) durability.Domains {
				return lockSignal{d, f, &armed, waiting}
			}
		})
		a, b := twoHandles(t, c)
		doneA := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1)
			doneA <- err
		}()
		<-parked
		armed.Store(true)
		// B's write names the same new FileID while A's is parked after its
		// append. It passes its early check — F is still untagged, for A has
		// not published — and queues on F's append domain. Only the check
		// under the domain can meet A's claim there, so B is refused with
		// ErrOtherShare once A publishes, without appending anything.
		done := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), b, f, 8, []byte("bbbb"), 1)
			done <- err
		}()
		<-waiting
		close(release)
		if err := <-doneA; err != nil {
			t.Fatalf("A's parked write: %v", err)
		}
		wantOtherShare(t, <-done, f, tagB, tagA)
		// A's piece is intact and published; B's nothing — not even a record
		// on disk, for the refusal happened before any version was drawn.
		if p, ok := c.Index().Lookup(f, 0); !ok || p.Extent != (core.Extent{Off: 0, Len: 4}) {
			t.Fatalf("A's piece after the release = %+v, %v", p, ok)
		}
		if _, ok := c.Index().Lookup(f, 8); ok {
			t.Fatal("B's refused write published a piece")
		}
		for _, rec := range readRecords(t, seam) {
			if rec.Header.File == f && rec.Header.Tag == tagB {
				t.Fatalf("a record of B's refused write reached the disk: %+v", rec.Header)
			}
		}
	})

	// A rivalled file's refusals through both tags and its TagConflicts entry
	// are pinned in the index package's own tests (TestTagConflictsFromRebuild
	// and its neighbours in index/tags_test.go); a conflicted-file test here
	// needs an exported rebuild entry point, which does not exist yet.

	t.Run("race_first_writes", func(t *testing.T) {
		// Two handles, both streams. Every round races A's and B's first
		// writes of a fresh pair of files, so two claims of one file meet on
		// its append domain: exactly one write wins each file, and the loser
		// gains nothing — the round drew two versions, two sequence numbers
		// and two written bytes, which is what the check before the draw
		// adds over a refusal at the publish, and the whole run holds one
		// record per winner: a refusal at the publish would have drawn the
		// loser's numbers and left its record on disk.
		c, seam := newTestCore(t)
		a, b := twoHandles(t, c)
		ctx := context.Background()
		nextPair := racePairPicker(t, c)
		const rounds = 500
		for round := range rounds {
			fa, fb := nextPair()
			before := takeRaceCounters(t, c)
			var mu sync.Mutex
			var won, lost int
			var wg sync.WaitGroup
			for _, s := range []*Share{a, b} {
				wg.Add(1)
				go func(s *Share) {
					defer wg.Done()
					var w, l int
					for _, f := range []core.FileID{fa, fb} {
						if _, err := c.WriteAt(ctx, s, f, 0, []byte("x"), 1); err == nil {
							w++
						} else if errors.Is(err, core.ErrOtherShare) {
							l++
						} else {
							t.Errorf("round %d: a racing first write = %v, want success or ErrOtherShare", round, err)
						}
					}
					mu.Lock()
					won, lost = won+w, lost+l
					mu.Unlock()
				}(s)
			}
			wg.Wait()
			if won != 2 || lost != 2 {
				t.Fatalf("round %d: %d racing writes won and %d were refused, want 2 and 2", round, won, lost)
			}
			after := takeRaceCounters(t, c)
			// The two winners drew two versions, wrote two bytes and drew two
			// seqs plus the one this second counting read consumes.
			if after.version != before.version+2 || after.seq != before.seq+3 || after.written != before.written+2 {
				t.Fatalf("round %d: after the race the round drew %d versions, %d seqs and %d written bytes (want +2, +3, +2): the refused writes left numbers behind",
					round, after.version-before.version, after.seq-before.seq, after.written-before.written)
			}
		}
		// One record per winning write, none for a refused one: 2*rounds
		// records on disk, each of a file the round named through its
		// winning handle.
		if got := len(readRecords(t, seam)); got != 2*rounds {
			t.Fatalf("%d records on disk after %d rounds, want %d: a refused write appended", got, rounds, 2*rounds)
		}
	})
}

// lockSignal wraps the file append domain table and closes waiting the first
// time Lock asks for f's domain once armed, before handing the ask on — the
// signal that a call has passed whatever runs before it and queued on the
// domain, which a timer can only catch by racing the caller to it.
type lockSignal struct {
	durability.Domains
	f       core.FileID
	armed   *atomic.Bool
	waiting chan struct{}
}

func (l lockSignal) Lock(id core.FileID) func() {
	if id == l.f && l.armed.CompareAndSwap(true, false) {
		close(l.waiting)
	}
	return l.Domains.Lock(id)
}

// racePairPicker returns a function handing out fresh FileIDs in pairs whose
// members sit on the core's two distinct streams, so a round's two raced
// claims are on both streams and every round races a first claim of files the
// core has not seen. The ids spread a counter over three bytes, so none is
// the all-zero identifier the record format refuses, and the picker advances
// until a pair's two ids sit on two streams before handing the pair out.
func racePairPicker(t *testing.T, c *Core) func() (core.FileID, core.FileID) {
	t.Helper()
	// The ids start past the bytes the other subtests name.
	n := 31
	return func() (core.FileID, core.FileID) {
		for c.Streams().For(pickRaceFile(n)) == c.Streams().For(pickRaceFile(n+1)) {
			n++
		}
		fa, fb := pickRaceFile(n), pickRaceFile(n+1)
		n += 2
		return fa, fb
	}
}

// pickRaceFile returns the id of rank n: the counter spread over the id's
// first three bytes, with the fourth held at 1 so no id is all zero.
func pickRaceFile(n int) core.FileID {
	var id [16]byte
	id[0] = byte(n)
	id[1] = byte(n >> 8)
	id[2] = byte(n >> 16)
	id[3] = 1
	return core.NewFileID(id)
}

// raceCounters is the core's drawn version, drawn seq and written bytes, so
// a race round can assert the refused half of its writes changed nothing.
// Reading the next sequence number consumes one, so the expected difference
// between two readings is the winners' two records plus the one the second
// reading drew itself.
type raceCounters struct {
	version uint64
	seq     uint64
	written uint64
}

func takeRaceCounters(t *testing.T, c *Core) raceCounters {
	t.Helper()
	seq, err := c.counters.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	return raceCounters{
		version: c.counters.NextVersion().Counter - 1,
		seq:     uint64(seq),
		written: c.WrittenBytes(),
	}
}
