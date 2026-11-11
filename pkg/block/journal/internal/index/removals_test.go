package index

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// publish writes p through the handle for id under tag and fails the test on a
// refusal.
func publish(t *testing.T, ix *Index, tag core.ShareTag, id core.FileID, p Placed, seq core.Seq) HeldDelta {
	t.Helper()
	d, err := ix.WriteRetain(tag, id, p, seq)
	if err != nil {
		t.Fatalf("WriteRetain(%+v): %v", p, err)
	}
	return d
}

// entriesEqual reports whether two read-outs hold the same entries, Clone by
// value.
func entriesEqual(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameEntry(a[i], b[i]) {
			return false
		}
	}
	return true
}

// markerOf returns id's one marker of kind k, failing the test when the file
// has none or several.
func markerOf(t *testing.T, ix *Index, id core.FileID, k core.ChangeKind) Entry {
	t.Helper()
	var got []Entry
	for _, en := range ix.FileEntries(id) {
		if en.Kind == EntryMarker && en.Removal == k {
			got = append(got, en)
		}
	}
	if len(got) != 1 {
		t.Fatalf("markers of kind %d on %v: got %d, want 1\n%s", k, id, len(got), fmtEntries(ix.FileEntries(id)))
	}
	return got[0]
}

// assertNothingHeld checks that no held entry of id covers any byte of e: a
// removal drops every held piece inside it, whatever its offloaded bit.
func assertNothingHeld(t *testing.T, ix *Index, id core.FileID, e core.Extent) {
	t.Helper()
	for _, en := range ix.FileEntries(id) {
		if en.Kind != EntryHeld {
			continue
		}
		lo, hi := max(en.Extent.Off, e.Off), min(extentEnd(en.Extent), extentEnd(e))
		if lo < hi {
			t.Fatalf("held entry %+v still covers [%d, %d) after a removal of %+v", en, lo, hi, e)
		}
	}
}

// padTo lifts the index's entry count to n with another file's writes made
// under the file's lock: a write's own admission refuses above the bound, so a
// populated index is reached only past it.
func padTo(t *testing.T, ix *Index, n int64) {
	t.Helper()
	id := fid(200)
	for k := ix.fs.extentCount(); k < n; k++ {
		if _, err := ix.fs.update(id, func(f *file) error {
			f.write(Placed{
				Extent:  fillExtent(k),
				Record:  rec(core.SegmentID(2+k/60000), uint32(k)*4096),
				Version: ver(uint64(k) + 1),
			})
			return nil
		}); err != nil {
			t.Fatalf("padding the count to %d: %v", n, err)
		}
	}
}

// TestRemovalTruncateNarrowsAndLeavesAMarker: a truncate inside a held extent
// keeps its head, drops nothing else, and leaves a marker from the new size to
// the end of the file at the removal's version, which Since yields. A marker
// carrying the record's length 0 instead of the to-end extent would read as a
// removal of nothing.
func TestRemovalTruncateNarrowsAndLeavesAMarker(t *testing.T) {
	ix := New(1 << 20)
	id := testFile
	publish(t, ix, testTag, id, write(0, 16, rec(1, 0), 2), 2)

	hd, err := ix.Truncate(testTag, id, 4, ver(3), 7, rec(2, 0))
	if err != nil {
		t.Fatalf("Truncate = %v, want no error", err)
	}
	want := []Entry{
		{Kind: EntryHeld, Extent: ext(0, 4), Version: ver(2), Tag: testTag, Record: rec(1, 0)},
		{Kind: EntryMarker, Extent: ext(4, math.MaxInt64-4), Version: ver(3), Tag: testTag, Removal: core.ChangeTruncate},
	}
	if got := ix.FileEntries(id); !entriesEqual(got, want) {
		t.Fatalf("entries after the truncate:\n got%s\n want%s", fmtEntries(got), fmtEntries(want))
	}
	if hd.HeldBytes != -12 || hd.DirtyBytes != -12 || hd.Tag != testTag {
		t.Fatalf("delta = %+v, want -12 held and dirty bytes under tag %d", hd, testTag)
	}

	wantChanges := []Change{
		{Kind: core.ChangeWrite, Extent: ext(0, 4), Version: ver(2), Record: rec(1, 0)},
		{Kind: core.ChangeTruncate, Extent: ext(4, math.MaxInt64-4), Version: ver(3)},
	}
	if got := ix.fs.since(id, core.Version{}, nil); !slices.Equal(got, wantChanges) {
		t.Fatalf("since:\n got  %+v\n want %+v", got, wantChanges)
	}
}

// TestRemovalDeallocateAcrossExtents: a deallocate crossing two pieces drops
// both parts inside it, narrows the piece it enters, and leaves a marker over
// its extent.
func TestRemovalDeallocateAcrossExtents(t *testing.T) {
	ix := New(1 << 20)
	id := testFile
	publish(t, ix, testTag, id, write(0, 8, rec(1, 0), 2), 2)
	publish(t, ix, testTag, id, write(16, 8, rec(1, 64), 3), 3)

	hd, err := ix.Deallocate(testTag, id, ext(4, 20), ver(4), 9, rec(2, 0))
	if err != nil {
		t.Fatalf("Deallocate = %v, want no error", err)
	}
	want := []Entry{
		{Kind: EntryHeld, Extent: ext(0, 4), Version: ver(2), Tag: testTag, Record: rec(1, 0)},
		{Kind: EntryMarker, Extent: ext(4, 20), Version: ver(4), Tag: testTag, Removal: core.ChangeDeallocate},
	}
	if got := ix.FileEntries(id); !entriesEqual(got, want) {
		t.Fatalf("entries after the deallocate:\n got%s\n want%s", fmtEntries(got), fmtEntries(want))
	}
	if hd.HeldBytes != -12 {
		t.Fatalf("delta HeldBytes = %d, want -12", hd.HeldBytes)
	}
}

// TestRemovalDeleteClearsAll: a delete stops holding every extent of the file
// and leaves one marker over it, running to the end of the file.
func TestRemovalDeleteClearsAll(t *testing.T) {
	ix := New(1 << 20)
	id := testFile
	publish(t, ix, testTag, id, write(0, 8, rec(1, 0), 2), 2)
	publish(t, ix, testTag, id, write(16, 8, rec(1, 64), 3), 3)

	hd, err := ix.Delete(testTag, id, ver(4), 5, rec(2, 0))
	if err != nil {
		t.Fatalf("Delete = %v, want no error", err)
	}
	want := []Entry{
		{Kind: EntryMarker, Extent: ext(0, math.MaxInt64-0), Version: ver(4), Tag: testTag, Removal: core.ChangeDelete},
	}
	if got := ix.FileEntries(id); !entriesEqual(got, want) {
		t.Fatalf("entries after the delete:\n got%s\n want%s", fmtEntries(got), fmtEntries(want))
	}
	if hd.HeldBytes != -16 {
		t.Fatalf("delta HeldBytes = %d, want -16", hd.HeldBytes)
	}
}

// TestRemovalCloneTargetKeepsItsSpec: a clone target removes its destination
// range as a deallocate does, and its marker keeps its own copy of the spec,
// unchanged by anything the caller does to its copy afterwards. A marker that
// kept only the extent would hand a restart a plain removal, and the
// destination would read zeros.
func TestRemovalCloneTargetKeepsItsSpec(t *testing.T) {
	ix := New(1 << 20)
	id := testFile
	spec := core.CloneSpec{
		SrcShare: core.ShareID{0: 7},
		Src:      fid(9),
		SrcOff:   1 << 20,
		DstOff:   4,
		Len:      8,
		AsOf:     ver(11),
	}
	want := spec
	publish(t, ix, testTag, id, write(0, 16, rec(1, 0), 2), 2)

	hd, err := ix.CloneTarget(testTag, id, spec, ver(12), 6, rec(2, 0))
	if err != nil {
		t.Fatalf("CloneTarget = %v, want no error", err)
	}
	m := markerOf(t, ix, id, core.ChangeCloneTarget)
	if m.Clone == nil || *m.Clone != want {
		t.Fatalf("clone marker's spec = %+v, want %+v", m.Clone, want)
	}
	if m.Extent != ext(4, 8) || m.Version != ver(12) {
		t.Fatalf("clone marker = %+v, want extent (4, 8) at v12", m)
	}
	if hd.HeldBytes != -8 {
		t.Fatalf("delta HeldBytes = %d, want -8", hd.HeldBytes)
	}

	spec.Src, spec.Len = fid(10), 99
	if got := markerOf(t, ix, id, core.ChangeCloneTarget).Clone; *got != want {
		t.Fatalf("clone spec = %+v after the caller changed its copy, want %+v", *got, want)
	}
}

// TestRemovalZeroLengthRefused: a zero-length deallocate or clone target is
// refused with ErrZeroLength and changes nothing — no entry, no marker, no
// version, no count and no drop range. Its record's length 0 would read, after
// a restart, as a removal to the end of the file. The file it names has no
// entry, so a removal that claimed it before refusing leaves one where this
// check looks.
func TestRemovalZeroLengthRefused(t *testing.T) {
	for name, call := range map[string]func(ix *Index, id core.FileID) (HeldDelta, error){
		"deallocate": func(ix *Index, id core.FileID) (HeldDelta, error) {
			return ix.Deallocate(testTag, id, ext(4, 0), ver(3), 5, rec(2, 0))
		},
		"clone target": func(ix *Index, id core.FileID) (HeldDelta, error) {
			return ix.CloneTarget(testTag, id, core.CloneSpec{DstOff: 4, Len: 0}, ver(3), 5, rec(2, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			ix := New(1 << 20)
			id := fid(11)
			if _, err := call(ix, id); !errors.Is(err, core.ErrZeroLength) {
				t.Fatalf("err = %v, want ErrZeroLength", err)
			}
			if got := ix.FileEntries(id); len(got) != 0 {
				t.Fatalf("entries after a refused removal:\n%s\n want none", fmtEntries(got))
			}
			if present(&ix.fs, id) {
				t.Fatal("a refused removal left a file entry")
			}
			if got := ix.fs.extentCount(); got != 0 {
				t.Fatalf("entry count = %d after a refused removal, want 0", got)
			}
			if got := ix.fs.dropRangeCount(); got != 0 {
				t.Fatalf("drop ranges = %d after a refused removal, want 0: its record was noted", got)
			}
			if got := ix.Refused(); got != 0 {
				t.Fatalf("refusals = %d after a zero-length refusal, want 0", got)
			}
		})
	}
}

// TestRemovalSplittingRefusedAtTheCeiling: the removal bound's rule at the
// handle, for both the pre-check and the removal itself. One below the removal
// ceiling a splitting removal is admitted and the check changes nothing; at
// the ceiling a splitting removal is refused, naming the index, counting one
// refusal, and changing nothing, while a removal that splits nothing is
// admitted and counts none. A pre-check that reserved, or a removal admitted
// one past the ceiling, would let a trim pass end the index's headroom.
func TestRemovalSplittingRefusedAtTheCeiling(t *testing.T) {
	const limit = 80
	const ceiling = limit + limit/8
	ix := New(limit)
	id := testFile
	publish(t, ix, testTag, id, write(0, 3*4096, rec(1, 0), 1), 1)

	// One below the ceiling: the check admits and leaves the file as it found
	// it, and the removal itself is admitted.
	padTo(t, ix, ceiling-1)
	if got := ix.fs.extentCount(); got != ceiling-1 {
		t.Fatalf("count = %d, want %d", got, ceiling-1)
	}
	before := ix.FileEntries(id)
	if err := ix.CheckRemoval(id, ext(4096, 4096)); err != nil {
		t.Fatalf("CheckRemoval one below the ceiling = %v, want nil", err)
	}
	if got := ix.FileEntries(id); !entriesEqual(got, before) {
		t.Fatalf("the pre-check changed the file:\n got%s\n want%s", fmtEntries(got), fmtEntries(before))
	}

	// At the ceiling: the check and the removal are both refused, naming the
	// index, counting one refusal each, and changing nothing.
	padTo(t, ix, ceiling)
	if err := ix.CheckRemoval(id, ext(4096, 4096)); err == nil {
		t.Fatal("CheckRemoval at the ceiling admitted a splitting removal")
	} else {
		assertIndexRefusal(t, err)
	}
	if got := ix.Refused(); got != 1 {
		t.Fatalf("refusals = %d after the refused pre-check, want 1", got)
	}
	if _, err := ix.Deallocate(testTag, id, ext(4096, 4096), ver(2), 9, rec(3, 0)); err == nil {
		t.Fatal("a splitting deallocate at the ceiling was admitted")
	} else {
		assertIndexRefusal(t, err)
	}
	if got := ix.Refused(); got != 2 {
		t.Fatalf("refusals = %d after the refused removal, want 2", got)
	}
	if got := ix.FileEntries(id); !entriesEqual(got, before) {
		t.Fatalf("the refused removal changed the file:\n got%s\n want%s", fmtEntries(got), fmtEntries(before))
	}
	if got := ix.AsOf(id); got != ver(1) {
		t.Fatalf("asOf = %v after a refused removal, want 1", got)
	}

	// A removal that narrows a piece splits nothing, so it is admitted at the
	// same count and counts no refusal.
	if err := ix.CheckRemoval(id, ext(0, 4096)); err != nil {
		t.Fatalf("CheckRemoval of a narrowing removal at the ceiling = %v, want nil", err)
	}
	if _, err := ix.Deallocate(testTag, id, ext(0, 4096), ver(2), 10, rec(3, 0)); err != nil {
		t.Fatalf("a narrowing deallocate at the ceiling = %v, want no error", err)
	}
	if got := ix.Refused(); got != 2 {
		t.Fatalf("refusals = %d after an admitted removal, want 2", got)
	}
	// A file the index has never seen has nothing to split, so it is never
	// refused here either.
	if err := ix.CheckRemoval(fid(9), ext(0, 8)); err != nil {
		t.Fatalf("CheckRemoval of a file with no entry = %v, want nil", err)
	}
}

// TestRemovalDropsOffloadedExtents: every removal drops the held content inside
// its extent whatever the offloaded bit, keeping what lies outside it, and the
// pieces it keeps keep theirs. A removal gated on the bit, as a release is,
// would let a gone file come back from the remote tier.
func TestRemovalDropsOffloadedExtents(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*Index, core.FileID) error
		e     core.Extent // the removed extent
		want  []Entry     // the held entries left
	}{
		{"truncate", func(ix *Index, id core.FileID) error {
			_, err := ix.Truncate(testTag, id, 4, ver(3), 5, rec(2, 0))
			return err
		}, ext(4, math.MaxInt64-4), []Entry{
			{Kind: EntryHeld, Extent: ext(0, 4), Version: ver(2), Tag: testTag, Record: rec(1, 0), Offloaded: true},
		}},
		{"deallocate", func(ix *Index, id core.FileID) error {
			_, err := ix.Deallocate(testTag, id, ext(4, 4), ver(3), 5, rec(2, 0))
			return err
		}, ext(4, 4), []Entry{
			{Kind: EntryHeld, Extent: ext(0, 4), Version: ver(2), Tag: testTag, Record: rec(1, 0), Offloaded: true},
			{Kind: EntryHeld, Extent: ext(8, 4), Version: ver(2), Tag: testTag, Record: rec(1, 0), Skip: 8, Offloaded: true},
		}},
		{"delete", func(ix *Index, id core.FileID) error {
			_, err := ix.Delete(testTag, id, ver(3), 5, rec(2, 0))
			return err
		}, ext(0, math.MaxInt64-0), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ix := New(1 << 20)
			id := testFile
			publish(t, ix, testTag, id, write(0, 12, rec(1, 0), 2), 2)
			if _, err := ix.fs.update(id, func(f *file) error {
				f.held.markOffloaded(whole, core.Version{}, newest)
				return nil
			}); err != nil {
				t.Fatalf("marking the content offloaded: %v", err)
			}
			for _, en := range ix.FileEntries(id) {
				if en.Kind == EntryHeld && !en.Offloaded {
					t.Fatalf("entry %+v is not offloaded after the mark", en)
				}
			}

			if err := c.apply(ix, id); err != nil {
				t.Fatalf("%s = %v, want no error", c.name, err)
			}
			assertNothingHeld(t, ix, id, c.e)
			var held []Entry
			for _, en := range ix.FileEntries(id) {
				if en.Kind == EntryHeld {
					held = append(held, en)
				}
			}
			if !entriesEqual(held, c.want) {
				t.Fatalf("held entries after the %s:\n got%s\n want%s", c.name, fmtEntries(held), fmtEntries(c.want))
			}
		})
	}
}

// TestRemovalNotesTheRecordAndClaimsTheFile: each removal notes its record in
// the file's drop ranges, over the extent a rebuild notes for the same record —
// the removal's own, or the to-end range a truncate or delete reaches — and
// claims the file for its tag, so a rebuild finds the file owned and a removal
// outranking an older record is kept alive by the segment carrying it. A
// removal that noted nothing would leave its record outranking nothing, and
// one noted over its header's length 0 would not cover what it removed.
func TestRemovalNotesTheRecordAndClaimsTheFile(t *testing.T) {
	ix := New(1 << 20)
	older := Numbers{Version: ver(2), Seq: 1}
	// note records an older record of id in another segment, which the
	// removal's own record must outrank wherever the two overlap.
	note := func(id core.FileID, seg core.SegmentID, e core.Extent) {
		t.Helper()
		if _, err := ix.fs.update(id, func(f *file) error {
			return f.recorded(seg, e, older, testTag, id)
		}); err != nil {
			t.Fatalf("noting an older record: %v", err)
		}
	}

	// A removal on a file the index has never seen claims it for the tag and
	// notes its record.
	id := testFile
	if _, err := ix.Deallocate(testTag, id, ext(4, 8), ver(3), 9, rec(5, 64)); err != nil {
		t.Fatalf("Deallocate = %v, want no error", err)
	}
	if tag, ok := ix.Owner(id); !ok || tag != testTag {
		t.Fatalf("owner = %d, %v after a removal, want tag %d, true", tag, ok, testTag)
	}
	if n := ix.fs.dropRangeCount(); n != 1 {
		t.Fatalf("drop ranges = %d after one removal, want 1", n)
	}
	note(id, 9, ext(4, 8))
	if !ix.fs.mayOutrank(id, 5, ext(4, 8), Numbers{Version: ver(3), Seq: 9}) {
		t.Fatal("the deallocate's record does not outrank an older record under it: its extent was not noted")
	}
	if ix.fs.mayOutrank(id, 5, ext(100, 8), Numbers{Version: ver(3), Seq: 9}) {
		t.Fatal("the deallocate's record was noted outside its extent")
	}

	// A truncate notes the to-end extent a rebuild notes.
	tid := fid(3)
	if _, err := ix.Truncate(testTag, tid, 8, ver(3), 4, rec(6, 0)); err != nil {
		t.Fatalf("Truncate = %v, want no error", err)
	}
	note(tid, 9, ext(8, 8))
	if !ix.fs.mayOutrank(tid, 6, ext(8, 100), Numbers{Version: ver(3), Seq: 4}) {
		t.Fatal("the truncate's record was not noted from its size to the end of the file")
	}
	if ix.fs.mayOutrank(tid, 6, ext(0, 8), Numbers{Version: ver(3), Seq: 4}) {
		t.Fatal("the truncate's record was noted below its size")
	}

	// A delete notes the whole file.
	did := fid(4)
	if _, err := ix.Delete(testTag, did, ver(3), 4, rec(7, 0)); err != nil {
		t.Fatalf("Delete = %v, want no error", err)
	}
	note(did, 9, ext(0, 8))
	if !ix.fs.mayOutrank(did, 7, ext(0, 8), Numbers{Version: ver(3), Seq: 4}) {
		t.Fatal("the delete's record was not noted over the whole file")
	}

	// A removal through another share's tag is refused, appending, noting and
	// reserving nothing.
	before, refused := ix.FileEntries(id), ix.Refused()
	if _, err := ix.Deallocate(testTag+1, id, ext(20, 4), ver(4), 10, rec(8, 0)); !errors.Is(err, core.ErrOtherShare) {
		t.Fatalf("a removal through another share = %v, want OtherShareError", err)
	}
	if got := ix.FileEntries(id); !entriesEqual(got, before) {
		t.Fatalf("the refused removal changed the file:\n got%s\n want%s", fmtEntries(got), fmtEntries(before))
	}
	if got := ix.Refused(); got != refused {
		t.Fatalf("refusals = %d after a tag refusal, want %d", got, refused)
	}
}

// TestApplyRemovalAppliesPastTheCeiling: a removal whose record is already in
// its segment is applied at the count its own admission would refuse, leaves
// its marker, and counts no index refusal. A removal left unapplied there is
// one a rebuild applies while the caller was told it did not happen.
func TestApplyRemovalAppliesPastTheCeiling(t *testing.T) {
	const limit = 80
	const ceiling = limit + limit/8
	ix := New(limit)
	id := testFile
	publish(t, ix, testTag, id, write(0, 3*4096, rec(1, 0), 1), 1)
	padTo(t, ix, ceiling)
	refused := ix.Refused()

	hd, err := ix.ApplyRemoval(testTag, id, core.ChangeDeallocate, ext(4096, 4096), ver(2), 9, rec(3, 0), nil)
	if err != nil {
		t.Fatalf("ApplyRemoval at the ceiling = %v, want no error", err)
	}
	if hd.HeldBytes != -4096 {
		t.Fatalf("delta HeldBytes = %d, want -4096", hd.HeldBytes)
	}
	if got := ix.Refused(); got != refused {
		t.Fatalf("refusals = %d after an applied-past removal, want %d", got, refused)
	}
	m := markerOf(t, ix, id, core.ChangeDeallocate)
	if m.Extent != ext(4096, 4096) || m.Version != ver(2) {
		t.Fatalf("marker = %+v, want extent (4096, 4096) at v2", m)
	}
	for _, en := range ix.FileEntries(id) {
		if en.Kind == EntryHeld {
			if lo, hi := max(en.Extent.Off, 4096), min(extentEnd(en.Extent), 8192); lo < hi {
				t.Fatalf("held entry %+v still covers the removed range", en)
			}
		}
	}
}
