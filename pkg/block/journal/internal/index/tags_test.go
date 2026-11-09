package index

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
	want := &core.OtherShareError{File: testFile, Tag: testTag + 1, Owner: testTag}
	if err == nil || *err.(*core.OtherShareError) != *want {
		t.Fatalf("CheckTag(%d, %v) = %v, want %+v", testTag+1, testFile, err, want)
	}
	if got := ix.FileEntries(testFile); len(got) != len(before) {
		t.Fatalf("a refused check changed the file's entries: %+v -> %+v", before, got)
	}
	if n := ix.fs.len(); n != 1 {
		t.Fatalf("entries = %d, want 1: the check created one", n)
	}
}

// TestCheckTagClaimsNothing: a check of a file nobody owns passes clean on an
// untagged entry and on one with no entry at all, creating nothing, and a
// later write through the checked tag still succeeds.
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
	// The owner's own tag checks clean too, and the file still takes a write
	// after the check.
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 1))
	if err := ix.CheckTag(testTag, testFile); err != nil {
		t.Fatalf("CheckTag of the owner's own file = %v, want nil", err)
	}
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 2))
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
