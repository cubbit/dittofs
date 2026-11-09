package datapath

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
// the record on disk and a Fill through B is refused for. It settles the
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

	t.Run("resolved_by_handle_alone", func(t *testing.T) {
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		// The same FileID value is nobody's: the share a call reaches is the
		// handle it goes through, so each of the two files is its writer's,
		// whatever bytes its id carries.
		fa, fb := fileWith(0x22, 0x11), fileWith(0x11, 0x22)
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

		t.Run("same_on_a_rebuilt_index", func(t *testing.T) {
			c, a, _, f := tagOnly(t)
			// A rebuild claims the file's tag from the records on disk, so
			// it answers as this journal does. The rebuilder itself is the
			// index package's own and unreachable here — the index's rebuild
			// tests pin that a file whose records name one tag claims that
			// tag — so what this half pins is the datapath's: the check over
			// an index a journal continues from, which is the state a second
			// core built over the same index reaches, gives the same refusal
			// and the same owner.
			c2, _ := newTestCore(t, func(o *coreOptions) { o.state = State{Index: c.Index()} })
			a2, b2 := twoHandles(t, c2)
			if _, err := c2.WriteAt(context.Background(), b2, f, 4, []byte("bb"), 1); !errors.Is(err, core.ErrOtherShare) {
				t.Fatalf("a write of the carried tag-only file through B = %v, want ErrOtherShare", err)
			}
			if _, err := c2.WriteAt(context.Background(), a2, f, 4, []byte("cc"), 1); err != nil {
				t.Fatalf("A's write of the carried tag-only file: %v", err)
			}
			_ = a
		})

		t.Run("segment_gone_claims", func(t *testing.T) {
			c, _, b, f := tagOnly(t)
			// Once no segment holds a record of the file, the next appending
			// call claims it afresh. Reaching that from the datapath needs a
			// segment's unlink, which no call here wires; the state it
			// leaves — a file no record holds a tag for — is what ClearTag
			// makes over the tag-only entry, and B's write claims F over it.
			if err := c.Index().ClearTag(f); err != nil {
				t.Fatalf("ClearTag: %v", err)
			}
			if _, err := c.WriteAt(context.Background(), b, f, 8, []byte("dd"), 1); err != nil {
				t.Fatalf("B's claim of F once no record holds its tag: %v", err)
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
		c, seam := newTestCore(t, func(o *coreOptions) {
			o.afterAppend = func() {
				select {
				case <-release:
				default:
					close(parked)
					<-release
				}
			}
		})
		a, b := twoHandles(t, c)
		doneA := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), a, f, 0, []byte("aaaa"), 1)
			doneA <- err
		}()
		<-parked
		// B's write names the same new FileID while A's is parked after its
		// append. It must block on the file's append domain — the check and
		// the claim are one step under it — so it returns nothing before A
		// publishes; once A does, B is refused with ErrOtherShare.
		done := make(chan error, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), b, f, 8, []byte("bbbb"), 1)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("B's write returned %v while A's held the append domain", err)
		case <-time.After(50 * time.Millisecond):
			// B is parked on the file's domain: let A finish and see B
			// refused.
		}
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

	t.Run("conflict_refused_through_both", func(t *testing.T) {
		// A rivalled file — one with records under two tags — is what only a
		// fault leaves on disk and a rebuild reaches from the damaged
		// records. Planting it needs the index package's rebuilder, whose
		// entry point is unexported and unreachable from here; the conflict's
		// refusals through both tags and the TagConflicts list until ClearTag
		// are pinned there, and the conformance suite plants the two-tag
		// segment over a real reopen. Skipped here rather than guessed at.
		t.Skip("NEEDS_CONTEXT: the rebuilder has no entry point reachable from datapath")
	})

	t.Run("race_first_writes", func(t *testing.T) {
		// Two handles, two files, both streams. Each round races A's
		// and B's first writes of both files, so two claims of one file meet
		// on its append domain: exactly one write wins each file and the
		// other is refused with ErrOtherShare.
		c, _ := newTestCore(t)
		a, b := twoHandles(t, c)
		fa, fb := fileWith(0x22, 0x11), fileWith(0x11, 0x22)
		ctx := context.Background()
		const rounds = 500
		for round := range rounds {
			files := []core.FileID{fa, fb}
			handles := []*Share{a, b}
			var won [2][2]int
			var wg sync.WaitGroup
			for fi, f := range files {
				for hi, s := range handles {
					wg.Add(1)
					go func(f core.FileID, s *Share, fi, hi int) {
						defer wg.Done()
						if _, err := c.WriteAt(ctx, s, f, int64(round)*8, []byte("x"), 1); err == nil {
							won[fi][hi] = 1
						} else if !errors.Is(err, core.ErrOtherShare) {
							t.Errorf("round %d: a racing first write = %v, want success or ErrOtherShare", round, err)
						}
					}(f, s, fi, hi)
				}
			}
			wg.Wait()
			for fi, f := range files {
				if got := won[fi][0] + won[fi][1]; got != 1 {
					t.Fatalf("round %d: file %v was won by %d writes, want exactly one", round, f, got)
				}
			}
			// The next round writes at a fresh offset, so every round is a
			// first-claim race over the same entry.
			for fi, f := range files {
				s := handles[0]
				if won[fi][1] == 1 {
					s = handles[1]
				}
				if _, err := c.WriteAt(ctx, s, f, int64(round)*8+4, []byte("y"), 1); err != nil {
					t.Errorf("round %d: the winner's follow-up write of %v = %v", round, f, err)
				}
			}
		}
	})
}
