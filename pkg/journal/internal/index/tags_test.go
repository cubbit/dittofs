package index

import (
	"errors"
	"math"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// rebuildRivalled feeds one write record under tag owner and one under tag
// rival for the same file, in seq order, and finishes the rebuild, so the
// file is rebuilt rivalled: tag owner is the owner and rival the rival.
func rebuildRivalled(t *testing.T, id core.FileID, owner, rival core.ShareTag) *Index {
	t.Helper()
	ix := New(1 << 20)
	b := newRebuilder(&ix.fs, &Counters{})
	a := recd(RecWrite, id, 1, 1, 1, 0, 0, 4)
	a.Tag = owner
	if err := b.Feed(a); err != nil {
		t.Fatal(err)
	}
	c := recd(RecWrite, id, 2, 2, 2, 64, 8, 4)
	c.Tag = rival
	if err := b.Feed(c); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	return ix
}

// TestCheckTagRefusesOtherOwner: a check through a tag that does not own the
// file is refused naming both tags, and the refusal changes no entry.
func TestCheckTagRefusesOtherOwner(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	before := ix.FileEntries(testFile)

	err := ix.CheckTag(testTag+1, testFile)
	want := core.OtherShareError{File: testFile, Tag: testTag + 1, Owner: testTag}
	var got *core.OtherShareError
	if !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("CheckTag(%d, %v) = %v, want ErrOtherShare", testTag+1, testFile, err)
	}
	if !errors.As(err, &got) || *got != want {
		t.Fatalf("CheckTag(%d, %v) = %v, want %+v", testTag+1, testFile, err, &want)
	}
	if n := ix.FileEntries(testFile); len(n) != len(before) {
		t.Fatalf("a refused check changed the file's entries: %+v -> %+v", before, n)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("entries = %d, want 1: the check created one", n)
	}
}

// TestCheckTagClaimsNothing: a check claims nothing, on a file nobody owns
// and on one with no entry at all. The untagged entry is built to persist —
// a write's segment is noted, so the tag-only life lasts, then ClearTag
// drops the tag whole — so a check through another tag must leave it
// untagged: still nobody's for a later write through any tag, and empty of
// the checked tag's claim.
func TestCheckTagClaimsNothing(t *testing.T) {
	other := fid(9)
	if err := New(1<<20).CheckTag(testTag, other); err != nil {
		t.Fatalf("CheckTag of a file with no entry = %v, want nil", err)
	}
	ix := New(1 << 20)
	before := ix.fs.len()
	if err := ix.CheckTag(testTag, other); err != nil {
		t.Fatalf("CheckTag of a file with no entry = %v, want nil", err)
	}
	if n := ix.fs.len(); n != before {
		t.Fatalf("the check left %d new entries, want none", n-before)
	}

	// An untagged entry: a write claims the file and notes its segment, so
	// the entry outlives ClearTag as a tag-only one the map still holds.
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	if err := ix.ClearTag(testFile); err != nil {
		t.Fatalf("ClearTag: %v", err)
	}
	var tagged, rivalled bool
	var tag core.ShareTag
	ix.fs.view(testFile, func(f *file) {
		tag, tagged = f.owner()
		rivalled = f.rivalled
	})
	if tagged || rivalled {
		t.Fatalf("after ClearTag the entry is still tagged (%v, tag %d) or rivalled (%v)", tagged, tag, rivalled)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("entries = %d, want 1: the segment must keep the untagged entry", n)
	}

	// The check through another tag passes clean and must not claim the
	// entry for it: a write through a third tag still succeeds, and the
	// entry still holds no tag.
	if err := ix.CheckTag(testTag+1, testFile); err != nil {
		t.Fatalf("CheckTag of an untagged file = %v, want nil", err)
	}
	ix.fs.view(testFile, func(f *file) {
		tag, tagged = f.owner()
	})
	if tagged {
		t.Fatalf("the check claimed the file for tag %d, want no claim", tag)
	}
	if _, err := ix.WriteRetain(testTag+2, testFile, write(0, 4096, rec(2, 0), 2), 2); err != nil {
		t.Fatalf("WriteRetain through a third tag after the check: %v", err)
	}
}

// TestTagConflictsFromRebuild: a file rebuilt with records under two tags is
// one conflict naming both tags, and a check through either is refused.
func TestTagConflictsFromRebuild(t *testing.T) {
	var a, b core.ShareTag = 7, 9
	ix := rebuildRivalled(t, fid(1), a, b)

	cs := ix.TagConflicts()
	want := []Conflict{{File: fid(1), Tags: [2]core.ShareTag{a, b}}}
	if len(cs) != 1 || cs[0] != want[0] {
		t.Fatalf("TagConflicts() = %+v, want %+v", cs, want)
	}
	for _, tag := range []core.ShareTag{a, b, 12} {
		if err := ix.CheckTag(tag, fid(1)); !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("CheckTag(%d) on the rivalled file = %v, want ErrOtherShare", tag, err)
		}
	}
}

// TestClearTagEndsConflict: clearing either tag of a rivalled file empties
// the conflict list and lets a write through the cleared tag back in.
func TestClearTagEndsConflict(t *testing.T) {
	var a, b core.ShareTag = 7, 9
	id := fid(1)
	ix := rebuildRivalled(t, id, a, b)
	if len(ix.TagConflicts()) != 1 {
		t.Fatalf("TagConflicts() before the clear = %+v, want one conflict", ix.TagConflicts())
	}

	if err := ix.ClearTag(id); err != nil {
		t.Fatalf("ClearTag: %v", err)
	}
	if cs := ix.TagConflicts(); len(cs) != 0 {
		t.Fatalf("TagConflicts() after the clear = %+v, want none", cs)
	}
	// A write through either tag claims the file afresh — the fault the clear
	// resolves is forgiven whole — so the rival's own write lands here.
	if _, err := ix.WriteRetain(b, id, write(0, 4096, rec(3, 0), 3), 3); err != nil {
		t.Fatalf("WriteRetain through the rival tag after the clear: %v", err)
	}
	if cs := ix.TagConflicts(); len(cs) != 0 {
		t.Fatalf("TagConflicts() after a rewrite = %+v, want none", cs)
	}

	// Clearing a file with no entry is a no-op.
	if err := ix.ClearTag(fid(2)); err != nil {
		t.Fatalf("ClearTag of a file with no entry = %v, want nil", err)
	}
}

// TestTagConflictsFollowTheRivalRoute: a second tag noted at run time — as
// only a fault leaves on disk — joins the list as a conflict naming both
// tags, the same list the rebuild feeds.
func TestTagConflictsFollowTheRivalRoute(t *testing.T) {
	ix := New(1 << 20)
	id := fid(1)
	// The first record claims the file; a second tag's record, as only a
	// fault leaves on disk, marks it rivalled.
	_, _ = ix.fs.update(id, func(f *file) error {
		return f.recorded(1, ext(0, 4), nums(1, 1), testTag, id)
	})
	_, _ = ix.fs.update(id, func(f *file) error {
		return f.recorded(9, ext(0, 4), nums(1, 1), testTag+1, id)
	})
	cs := ix.TagConflicts()
	want := []Conflict{{File: id, Tags: [2]core.ShareTag{testTag, testTag + 1}}}
	if len(cs) != 1 || cs[0] != want[0] {
		t.Fatalf("TagConflicts() = %+v, want %+v", cs, want)
	}
}

// TestTagConflictsLeaveWithTheEntry: a rivalled file whose last record leaves
// the index in one update — the update marks it rivalled and notes the rival's
// record on a segment the same update has already reclaimed — keeps no
// conflict, since the file's records are gone with its entry; the set must
// never hold a file the map says is gone.
func TestTagConflictsLeaveWithTheEntry(t *testing.T) {
	ix := New(1 << 20)
	id := fid(3)
	_, _ = ix.fs.update(id, func(f *file) error {
		return f.recorded(1, ext(0, 4), nums(1, 1), testTag, id)
	})
	// One update both marks the file rivalled — the rival's record is noted
	// — and reclaims the last segments holding the file's records: the entry
	// is empty and rivalled when it settles, leaves the map, and the conflict
	// set must not keep it.
	_, _ = ix.fs.update(id, func(f *file) error {
		err := f.recorded(9, ext(0, 4), nums(2, 2), testTag+1, id)
		f.segmentGone(1)
		f.segmentGone(9)
		return err
	})
	if got := ix.fs.len(); got != 0 {
		t.Fatalf("entries = %d, want 0: the settle must take the empty rivalled entry", got)
	}
	if cs := ix.TagConflicts(); len(cs) != 0 {
		t.Fatalf("TagConflicts() after the rivalled entry left the map = %+v, want none", cs)
	}
	if err := ix.CheckTag(testTag+1, id); err != nil {
		t.Fatalf("CheckTag after the entry left = %v, want nil", err)
	}
}

// TestTagConflictsDropWhenTheEntryLeavesLater: a rivalled file that is in the
// conflict set — the marking update has returned — and whose entry leaves the
// map only in a later update, which reclaims the file's last segments, is
// taken back out of the set by the settle that removes it. This is the drop
// settle itself makes, distinct from the one-update window above, where the
// file never reached the set at all.
func TestTagConflictsDropWhenTheEntryLeavesLater(t *testing.T) {
	ix := New(1 << 20)
	id := fid(4)
	// Two tags' records note two segments, so the file is rivalled and the
	// two segments keep both its tag and its entry.
	_, _ = ix.fs.update(id, func(f *file) error {
		return f.recorded(1, ext(0, 4), nums(1, 1), testTag, id)
	})
	_, _ = ix.fs.update(id, func(f *file) error {
		return f.recorded(9, ext(0, 4), nums(2, 2), testTag+1, id)
	})
	if cs := ix.TagConflicts(); len(cs) != 1 {
		t.Fatalf("TagConflicts() with the rivalled entry present = %+v, want one conflict", cs)
	}
	// A later update reclaims both segments, so the entry empties and leaves
	// the map with its marks still set.
	segs := []core.SegmentID{1, 9}
	_, _ = ix.fs.update(id, func(f *file) error {
		for _, seg := range segs {
			f.segmentGone(seg)
		}
		return nil
	})
	if got := ix.fs.len(); got != 0 {
		t.Fatalf("entries = %d, want 0: reclaiming the last segments must empty the entry", got)
	}
	if cs := ix.TagConflicts(); len(cs) != 0 {
		t.Fatalf("TagConflicts() after the entry left the map = %+v, want none", cs)
	}
}

// TestOwnerNamesTheFilesTag checks Owner answers the tag a write claimed the
// file for, and nothing for a file with no entry.
func TestOwnerNamesTheFilesTag(t *testing.T) {
	ix := New(1 << 20)
	if _, ok := ix.Owner(testFile); ok {
		t.Fatal("Owner answers a file with no entry")
	}
	seqWrite(t, ix, write(0, 10, rec(1, 64), 1), 1)
	if tag, ok := ix.Owner(testFile); !ok || tag != testTag {
		t.Fatalf("Owner = %d, %v; want %d", tag, ok, testTag)
	}
}

// TestFilesClaimedByNamesOwnerAndRival checks the enumeration the forget
// drops by: every file whose owner tag or rival tag is the asked one, and
// only those — a file nothing holds but its records still ride their
// segments is named by its tag alone.
func TestFilesClaimedByNamesOwnerAndRival(t *testing.T) {
	const owner, rival, third = core.ShareTag(7), core.ShareTag(8), core.ShareTag(9)
	ix := New(1 << 20)
	f1, f2, f4 := fid(1), fid(2), fid(4)
	// f1: held content of the owner.
	if _, err := ix.WriteRetain(owner, f1, write(0, 10, rec(1, 0), 1), 1); err != nil {
		t.Fatal(err)
	}
	// f2: a file nothing holds any more, its records still naming segments —
	// the shape a settled deletion of every extent leaves: claimed, unlisted
	// by pieces or markers.
	if _, err := ix.WriteRetain(owner, f2, write(0, 10, rec(1, 0), 1), 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.ApplyRemoval(owner, f2, core.ChangeDelete, core.Extent{Off: 0, Len: math.MaxInt64},
		core.Version{Counter: 2}, 3, RecordAt{Segment: 1, Offset: 64}, nil); err != nil {
		t.Fatal(err)
	}
	ix.Settle(f2, core.Version{Counter: 2})
	// f4: another tag's file, named by neither.
	if _, err := ix.WriteRetain(third, f4, write(0, 10, rec(1, 0), 1), 4); err != nil {
		t.Fatal(err)
	}

	got := ix.FilesClaimedBy(owner)
	want := map[core.FileID]bool{f1: true, f2: true}
	if len(got) != len(want) {
		t.Fatalf("FilesClaimedBy(%d) = %v, want exactly %v", owner, got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("FilesClaimedBy(%d) names %v, want exactly %v", owner, got, want)
		}
	}

	// The rival answers as fully as the owner on a rivalled file.
	rix := rebuildRivalled(t, fid(3), owner, rival)
	for _, tag := range []core.ShareTag{owner, rival} {
		if got := rix.FilesClaimedBy(tag); len(got) != 1 || got[0] != fid(3) {
			t.Fatalf("FilesClaimedBy(%d) over a rivalled file = %v, want exactly the rivalled one", tag, got)
		}
	}
}

// TestNotePlacedDoesNotRecreateADroppedFile checks the note's no-recreation
// rule: a record's placement re-creates no entry a forget's drop removed,
// claiming nothing and adding no drop range, while an entry that still lives
// notes its record as ever.
func TestNotePlacedDoesNotRecreateADroppedFile(t *testing.T) {
	ix := New(1 << 20)
	f := fid(1)
	if _, err := ix.WriteRetain(core.ShareTag(7), f, write(0, 10, rec(1, 0), 1), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.ForgetFile(f, 1000, core.ShareTag(7)); err != nil {
		t.Fatal(err)
	}
	before := ix.DropRanges()
	if _, ok := ix.Owner(f); ok {
		t.Fatal("the file still has an owner after the forget's drop")
	}

	if err := ix.NotePlaced(core.ShareTag(7), f, 2, core.Extent{Off: 0, Len: 10}, Numbers{Seq: 999}); err != nil {
		t.Fatal(err)
	}
	if owner, ok := ix.Owner(f); ok {
		t.Fatalf("the note re-created the dropped entry, claiming tag %d", owner)
	}
	if got := ix.DropRanges(); got != before {
		t.Fatalf("the note of the dropped file added %d drop ranges, want none", got-before)
	}

	// An entry that still lives notes its record: the same note against a
	// held file claims and ranges as ever.
	if _, err := ix.WriteRetain(core.ShareTag(7), f, write(0, 10, rec(1, 0), 1), 2); err != nil {
		t.Fatal(err)
	}
	if err := ix.NotePlaced(core.ShareTag(7), f, 2, core.Extent{Off: 0, Len: 10}, Numbers{Seq: 998}); err != nil {
		t.Fatal(err)
	}
	if tag, ok := ix.Owner(f); !ok || tag != core.ShareTag(7) {
		t.Fatalf("the note against the live entry claimed tag %d, %v", tag, ok)
	}
	if got := ix.DropRanges(); got <= before {
		t.Fatalf("the note against the live entry added no drop range: %d, want more than %d", got, before)
	}
}

// TestForgetFileDropsTheForgottenSideAlone checks the tagged drop over a
// rivalled file: the forgotten tag's pieces leave, the surviving tag's stay
// held with their bytes, and the entry keeps the survivor's records where
// they were.
func TestForgetFileDropsTheForgottenSideAlone(t *testing.T) {
	const owner, rival = core.ShareTag(7), core.ShareTag(8)
	id := fid(3)
	ix := rebuildRivalled(t, id, owner, rival)
	// A second pair of long, disjoint pieces — the shape a planted second
	// tag's records give the reopen — beside the tiny ones above: the drop
	// must tell them apart just the same, by tag and not by size. A rivalled
	// file refuses the runtime entry points, so the pair is planted through
	// the rebuilder, as recovery plants what it feeds.
	b := newRebuilder(&ix.fs, &Counters{})
	long := recd(RecWrite, id, 3, 11, 11, 0, 1<<20, 65536)
	long.Tag = owner
	if err := b.Feed(long); err != nil {
		t.Fatal(err)
	}
	longR := recd(RecWrite, id, 4, 12, 12, 0, 1<<20+65536, 65536)
	longR.Tag = rival
	if err := b.Feed(longR); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(13)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ix.ForgetFile(id, 1<<30, rival); err != nil {
		t.Fatal(err)
	}
	held := map[int64]bool{}
	ix.fs.view(id, func(f *file) {
		f.held.visit(core.Extent{Off: 0, Len: math.MaxInt64}, func(p Piece) bool {
			held[p.Extent.Off] = p.Tag == owner
			return true
		})
	})
	for _, off := range []int64{0, 1 << 20} {
		if !held[off] {
			t.Fatalf("the owner's piece at %d did not survive the rival's forget: held %v", off, held)
		}
	}
	for _, off := range []int64{8, 1<<20 + 65536} {
		if _, ok := held[off]; ok {
			t.Fatalf("the rival's piece at %d survived its own forget: held %v", off, held)
		}
	}
}

// TestForgetKeepsTheSurvivorsSideThroughASecondRivalry pins the survivor's
// flag through the owner-forgotten flip: forgetting a rivalled file's owner
// re-owns the surviving rival's pieces, so their side flags clear with the
// marks — a second rivalry on the same file must not resolve them to the new
// rival, and a forget of the second rival must not take the survivor's
// content with its own.
func TestForgetKeepsTheSurvivorsSideThroughASecondRivalry(t *testing.T) {
	const o, r1, r2 = core.ShareTag(7), core.ShareTag(8), core.ShareTag(9)
	id := fid(4)
	ix := New(1 << 20)

	// The first rivalry: O's record claims the file, R1's record marks the
	// rival side, both pieces held.
	b1 := newRebuilder(&ix.fs, &Counters{})
	a1 := recd(RecWrite, id, 1, 1, 1, 0, 0, 4)
	a1.Tag = o
	if err := b1.Feed(a1); err != nil {
		t.Fatal(err)
	}
	r1a := recd(RecWrite, id, 2, 2, 1, 8, 8, 4)
	r1a.Tag = r1
	if err := b1.Feed(r1a); err != nil {
		t.Fatal(err)
	}
	if err := b1.Finish(ver(3)); err != nil {
		t.Fatal(err)
	}

	// The forget of the OWNER: the two steps the datapath forget runs, the
	// drop then the marks, in that order. The surviving pieces are R1's,
	// and the flip that makes R1 the owner must re-own their side flags.
	if _, _, err := ix.ForgetFile(id, 1<<30, o); err != nil {
		t.Fatal(err)
	}
	if err := ix.ForgetTagMarks(id, o); err != nil {
		t.Fatal(err)
	}
	if p, ok := ix.Lookup(id, 8); !ok || p.Tag != r1 {
		t.Fatalf("the survivor's piece reads tag %d (held %v) after its rival became the owner, want %d", p.Tag, ok, r1)
	}

	// The second rivalry, fault-shaped as the first: a record of R2 beside
	// the survivor's on the same file. The survivor's pieces must resolve
	// as the owner's still, not as the new rival's.
	b2 := newRebuilder(&ix.fs, &Counters{})
	r2a := recd(RecWrite, id, 3, 3, 3, 0, 16, 4)
	r2a.Tag = r2
	if err := b2.Feed(r2a); err != nil {
		t.Fatal(err)
	}
	if err := b2.Finish(ver(5)); err != nil {
		t.Fatal(err)
	}
	if p, ok := ix.Lookup(id, 8); !ok || p.Tag != r1 {
		t.Fatalf("the survivor's piece reads tag %d (held %v) under the second rivalry, want its owner's %d", p.Tag, ok, r1)
	}

	// The forget of the second rival takes R2's piece alone and spares the
	// survivor's content.
	if _, _, err := ix.ForgetFile(id, 1<<30, r2); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Lookup(id, 16); ok {
		t.Fatalf("the second rival's piece survived its own forget")
	}
	if p, ok := ix.Lookup(id, 8); !ok || p.Tag != r1 {
		t.Fatalf("the survivor's piece went with the second rival's forget: held %v, tag %d, want it kept as its owner's %d", ok, p.Tag, r1)
	}
}
