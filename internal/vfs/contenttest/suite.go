package contenttest

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Subject is one Content under test, with what the suite needs around it.
type Subject struct {
	Content Content
	Share   entity.ShareID                   // a share bound to Content
	File    func(t *testing.T) entity.FileID // a new, empty file of Share with no name or open left, so Delete can release it
	Clock   *sim.TestClock                   // the clock Content reads
	// Other is a second share bound to Content. Zero: the case naming a
	// file through it is skipped, naming it.
	Other entity.ShareID
	// Lose loses file f's content written so far, as a failed sync window
	// does. nil: the cases that need a loss are skipped, naming it.
	Lose func(t *testing.T, f entity.FileID)
}

// Run runs the suite against the Content newSubject builds: a fresh one for
// each case.
//
// decision: the suite asserts only what a caller can observe through Content
// alone. Cases that need a capability Memory does not model are left out
// rather than faked: a crash and restart (committed existence surviving, the
// verifier changing at a restart), a release left pending by a holder or a
// running clone (Delete's retry channel), a copy cut short by its deadline or
// a failing chunk, clone_max_len, two write namespaces (ErrCross), a share
// being removed (ErrStale), and the Done of a write a loss drops. Each belongs
// to the engine's own tests until Memory gains the capability.
//
// Nor does any case assert when a file's pending existence commits: the
// engine commits it at a shrinking truncate, a deallocate and a clone, on
// source and destination (RFC 8 §8.1), calling its writes' Dones and leaving
// its Overlay not pending, where Memory keeps them pending until
// CommitExistence or Delete. Assert it once Memory commits at those points
// too.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		run  func(t *testing.T, s Subject)
	}{
		{"WrittenBytesReadBack", writtenBytesReadBack},
		{"AVersionIsAboveEveryVersionBefore", aVersionIsAboveEveryVersionBefore},
		{"OverlayReflectsAWriteStagedAnInstantBefore", overlayReflectsAWrite},
		{"OverlayReplaysInVersionOrder", overlayReplaysInVersionOrder},
		{"ASuspendedModifyTimeIsLeftAlone", aSuspendedModifyTimeIsLeftAlone},
		{"DoneIsCalledOnceByTheExistenceCommit", doneIsCalledOnceByTheExistenceCommit},
		{"TruncateReadsBackAsEndOfFileOrZeros", truncateReadsBack},
		{"DeallocateReadsBackAsZeros", deallocateReadsBackAsZeros},
		{"AllocationReportsHolesAndData", allocationReportsHolesAndData},
		{"DeleteLeavesNothing", deleteLeavesNothing},
		{"CloneCopiesTheExtentHolesIncluded", cloneCopiesTheExtent},
		{"CopyCopiesItsExtentsInOrder", copyCopiesInOrder},
		{"ARefusedCopyChangesNothing", aRefusedCopyChangesNothing},
		{"TheAdmittedHookRunsBeforeTheChange", theAdmittedHookRunsBeforeTheChange},
		{"TheVerifierIsStableWhileNothingIsLost", theVerifierIsStable},
		{"ALossChangesTheVerifierAndRaisesTheLossSequence", aLossChangesTheVerifier},
		{"AnUnboundShareIsFrozen", anUnboundShareIsFrozen},
		{"AFileOfAnotherShareIsCorrupt", aFileOfAnotherShareIsCorrupt},
		{"BadRangesAreInvalid", badRangesAreInvalid},
		{"FilesWrittenConcurrentlyKeepTheirOwnBytes", filesWrittenConcurrently},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

func (s Subject) write(t *testing.T, f entity.FileID, off int64, p string) vfs.Verifier {
	t.Helper()
	v, err := s.Content.Write(t.Context(), s.Share, f, off, []byte(p), engine.WriteOpts{})
	if err != nil {
		t.Fatalf("Write(%d, %q): %v", off, p, err)
	}
	return v
}

// read is what a Read of n bytes from off fills.
func (s Subject) read(t *testing.T, f entity.FileID, off int64, n int) string {
	t.Helper()
	p := make([]byte, n)
	got, err := s.Content.Read(t.Context(), s.Share, f, off, p)
	if err != nil {
		t.Fatalf("Read(%d, %d): %v", off, n, err)
	}
	return string(p[:got])
}

func (s Subject) wantRead(t *testing.T, f entity.FileID, off int64, n int, want string) {
	t.Helper()
	if got := s.read(t, f, off, n); got != want {
		t.Errorf("Read(%d, %d) = %q, want %q", off, n, got, want)
	}
}

func (s Subject) overlay(t *testing.T, f entity.FileID) (engine.Overlay, bool) {
	t.Helper()
	o, pending, err := s.Content.Overlay(t.Context(), s.Share, f)
	if err != nil {
		t.Fatalf("Overlay: %v", err)
	}
	return o, pending
}

func (s Subject) draw(t *testing.T, f entity.FileID) entity.JournalVersion {
	t.Helper()
	v, err := s.Content.Draw(t.Context(), s.Share, f)
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	return v
}

func (s Subject) commit(t *testing.T, f entity.FileID) (vfs.Verifier, uint64) {
	t.Helper()
	v, seq, err := s.Content.Commit(t.Context(), s.Share, f, 0, 0)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return v, seq
}

// run is one span Allocation answers: hole, or data, from off for n bytes.
type run struct {
	off, n int64
	hole   bool
}

// wantRuns checks the span Allocation answers from each run's offset.
func (s Subject) wantRuns(t *testing.T, f entity.FileID, want ...run) {
	t.Helper()
	for _, w := range want {
		sp, err := s.Content.Allocation(t.Context(), s.Share, f, w.off)
		if err != nil {
			t.Fatalf("Allocation(%d): %v", w.off, err)
		}
		if hole := sp.Class == entity.Hole; sp.Offset != w.off || sp.Length != w.n || hole != w.hole {
			t.Errorf("Allocation(%d) = [%d, +%d) hole %v, want [%d, +%d) hole %v", w.off, sp.Offset, sp.Length, hole, w.off, w.n, w.hole)
		}
	}
}

// Bytes written read back, a gap before them as zeros, and a read is short at
// the end of file, answering nothing past it.
func writtenBytesReadBack(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 0, "hello")
	s.write(t, f, 10, "world")
	s.wantRead(t, f, 0, 32, "hello\x00\x00\x00\x00\x00world")
	s.wantRead(t, f, 12, 2, "rl")
	s.wantRead(t, f, 15, 4, "")
	s.wantRead(t, f, 100, 4, "")
	s.write(t, f, 1, "EL")
	s.wantRead(t, f, 0, 5, "hELlo")
}

// Every version, drawn or staged, is above every version drawn or staged on
// the share before it, whichever of its files each was for, though the clock
// stands still.
func aVersionIsAboveEveryVersionBefore(t *testing.T, s Subject) {
	f, g := s.File(t), s.File(t)
	last := s.draw(t, f)
	above := func(what string, v entity.JournalVersion) {
		t.Helper()
		if v.Compare(last) <= 0 {
			t.Errorf("%s's version %x is not above %x, the last before it", what, v, last)
		}
		last = v
	}
	s.write(t, g, 0, "x")
	o, _ := s.overlay(t, g)
	above("a write to another file", o.Version)
	above("a draw after it", s.draw(t, f))
	if err := s.Content.Truncate(t.Context(), s.Share, g, 100); err != nil {
		t.Fatal(err)
	}
	o, _ = s.overlay(t, g)
	above("a truncate", o.Version)
	above("a draw of the truncated file", s.draw(t, g))
}

// A write is in the Overlay as soon as Write answers: its end as the size,
// the clock's time as both times, and a version above the last drawn.
func overlayReflectsAWrite(t *testing.T, s Subject) {
	f := s.File(t)
	if _, pending := s.overlay(t, f); pending {
		t.Errorf("a new file's Overlay is pending")
	}
	before := s.draw(t, f)
	s.Clock.Advance(time.Second)
	s.write(t, f, 4, "abcd")
	o, pending := s.overlay(t, f)
	now := s.Clock.Now()
	switch {
	case !pending:
		t.Fatalf("the Overlay after a write is not pending")
	case o.Size != 8 || o.SizeSet:
		t.Errorf("Overlay size %d, set %v; want 8, unset", o.Size, o.SizeSet)
	case !o.Mtime.Equal(now) || !o.Ctime.Equal(now):
		t.Errorf("Overlay mtime %v, ctime %v; want both %v", o.Mtime, o.Ctime, now)
	case o.Version.Compare(before) <= 0:
		t.Errorf("Overlay version %x is not above %x, drawn before the write", o.Version, before)
	}
}

// A pending truncate sets the size exactly, a growing one moving both times,
// and a write after it raises the size only past it.
func overlayReplaysInVersionOrder(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 0, "abcd")
	s.Clock.Advance(time.Second)
	if err := s.Content.Truncate(t.Context(), s.Share, f, 16); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.overlay(t, f); !o.Mtime.Equal(s.Clock.Now()) || !o.Ctime.Equal(s.Clock.Now()) {
		t.Errorf("after a growing truncate, Overlay mtime %v, ctime %v; want both %v", o.Mtime, o.Ctime, s.Clock.Now())
	}
	for _, w := range []struct {
		off  int64
		p    string
		size int64
	}{{0, "ABCD", 16}, {20, "wxyz", 24}} {
		s.write(t, f, w.off, w.p)
		if o, pending := s.overlay(t, f); !pending || o.Size != w.size || !o.SizeSet {
			t.Errorf("after a write of [%d, %d) over a truncate to 16, Overlay pending %v, size %d, set %v; want true, %d, true",
				w.off, w.off+int64(len(w.p)), pending, o.Size, o.SizeSet, w.size)
		}
	}
}

// A write that suspends the modification time moves the change time only.
func aSuspendedModifyTimeIsLeftAlone(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 0, "a")
	first := s.Clock.Now()
	s.Clock.Advance(time.Second)
	if _, err := s.Content.Write(t.Context(), s.Share, f, 1, []byte("b"), engine.WriteOpts{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.overlay(t, f); !o.Mtime.Equal(first) || !o.Ctime.Equal(s.Clock.Now()) {
		t.Errorf("after a write suspending mtime, Overlay mtime %v, ctime %v; want %v, %v", o.Mtime, o.Ctime, first, s.Clock.Now())
	}
}

// A write's Done is called once, by the existence commit, never while the
// write is pending; the commit leaves nothing pending and the bytes in place.
// A write of no byte stages nothing, growing no file, and has its Done
// called too.
func doneIsCalledOnceByTheExistenceCommit(t *testing.T, s Subject) {
	f := s.File(t)
	var calls atomic.Int32
	opts := engine.WriteOpts{Done: func() { calls.Add(1) }}
	if _, err := s.Content.Write(t.Context(), s.Share, f, 0, []byte("abcd"), opts); err != nil {
		t.Fatalf("Write(0, \"abcd\") with a Done: %v", err)
	}
	if _, pending := s.overlay(t, f); pending && calls.Load() != 0 {
		t.Errorf("Done called %d times while its write is pending", calls.Load())
	}
	for i := range 2 {
		if err := s.Content.CommitExistence(t.Context(), s.Share, f); err != nil {
			t.Fatal(err)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("after existence commit %d, Done called %d times, want 1", i+1, n)
		}
	}
	if _, pending := s.overlay(t, f); pending {
		t.Errorf("the Overlay is pending after the existence commit")
	}
	s.wantRead(t, f, 0, 8, "abcd")

	var empty atomic.Int32
	if _, err := s.Content.Write(t.Context(), s.Share, f, 100, nil, engine.WriteOpts{Done: func() { empty.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if _, pending := s.overlay(t, f); pending {
		t.Errorf("a write of no byte left the Overlay pending")
	}
	s.wantRead(t, f, 0, 128, "abcd")
	if err := s.Content.CommitExistence(t.Context(), s.Share, f); err != nil {
		t.Fatal(err)
	}
	if n := empty.Load(); n != 1 {
		t.Errorf("a write of no byte had its Done called %d times, want 1", n)
	}
}

// What a truncate cuts reads as end of file, and a grow after it as zeros,
// never as the bytes cut.
func truncateReadsBack(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 0, "abcdefgh")
	if err := s.Content.Truncate(t.Context(), s.Share, f, 4); err != nil {
		t.Fatal(err)
	}
	s.wantRead(t, f, 0, 8, "abcd")
	if err := s.Content.Truncate(t.Context(), s.Share, f, 8); err != nil {
		t.Fatal(err)
	}
	s.wantRead(t, f, 0, 16, "abcd\x00\x00\x00\x00")
	s.wantRuns(t, f, run{0, 4, false}, run{4, 4, true})
}

// A deallocated extent reads as zeros and is a hole; one reaching past the
// end of file leaves the size where it was.
func deallocateReadsBackAsZeros(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 0, "abcdefgh")
	if err := s.Content.Deallocate(t.Context(), s.Share, f, 2, 3); err != nil {
		t.Fatal(err)
	}
	s.wantRead(t, f, 0, 16, "ab\x00\x00\x00fgh")
	s.wantRuns(t, f, run{0, 2, false}, run{2, 3, true}, run{5, 3, false})
	if err := s.Content.Deallocate(t.Context(), s.Share, f, 6, 100); err != nil {
		t.Fatal(err)
	}
	s.wantRead(t, f, 0, 16, "ab\x00\x00\x00f\x00\x00")
}

// Allocation answers the longest run of hole or data from an offset: a gap a
// write leaves is a hole, and zeros written are data.
func allocationReportsHolesAndData(t *testing.T, s Subject) {
	f := s.File(t)
	s.write(t, f, 8, "xy")
	s.wantRuns(t, f, run{0, 8, true}, run{3, 5, true}, run{8, 2, false})
	s.write(t, f, 0, "\x00\x00")
	s.wantRuns(t, f, run{0, 2, false}, run{2, 6, true}, run{9, 1, false})
	// At or past the end of file the span is empty, whatever its class.
	for _, off := range []int64{10, 20} {
		sp, err := s.Content.Allocation(t.Context(), s.Share, f, off)
		if err != nil {
			t.Fatalf("Allocation(%d): %v", off, err)
		}
		if sp.Offset != off || sp.Length != 0 {
			t.Errorf("Allocation(%d), at or past the end of file, = [%d, +%d); want [%d, +0)", off, sp.Offset, sp.Length, off)
		}
	}
}

// A release leaves a file that reads and allocates as stale and has nothing
// pending, and calls the Done of each write it found pending.
func deleteLeavesNothing(t *testing.T, s Subject) {
	f := s.File(t)
	var calls atomic.Int32
	if _, err := s.Content.Write(t.Context(), s.Share, f, 0, []byte("abcd"), engine.WriteOpts{Done: func() { calls.Add(1) }}); err != nil {
		t.Fatalf("Write(0, \"abcd\") with a Done: %v", err)
	}
	retry, err := s.Content.Delete(t.Context(), s.Share, f)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if retry != nil {
		select {
		case err := <-retry:
			if err != nil {
				t.Fatalf("the pending release: %v", err)
			}
		case <-time.After(10 * time.Second): // no event marks a release still pending: the bound is the assertion
			t.Fatal("the pending release did not finish within 10s")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the released file's pending write had its Done called %d times, want 1", n)
	}
	p := make([]byte, 4)
	if n, err := s.Content.Read(t.Context(), s.Share, f, 0, p); n != 0 || !errors.Is(err, vfs.ErrStale) {
		t.Errorf("Read of a released file = %q, %v; want ErrStale", p[:n], err)
	}
	if sp, err := s.Content.Allocation(t.Context(), s.Share, f, 0); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("Allocation(0) of a released file = [%d, +%d) %v, %v; want ErrStale", sp.Offset, sp.Length, sp.Class, err)
	}
	if _, pending, err := s.Content.Overlay(t.Context(), s.Share, f); pending || err != nil {
		t.Errorf("Overlay of a released file: pending %v, %v; want not pending, no error", pending, err)
	}
}

// A clone makes the destination extent the source's, its holes holes over
// the data they replace, and leaves the rest of the destination and the
// source alone; within one file it moves between extents that only touch.
func cloneCopiesTheExtent(t *testing.T, s Subject) {
	src, dst := s.File(t), s.File(t)
	s.write(t, src, 0, "abcd")
	s.write(t, src, 8, "ef")
	s.write(t, dst, 0, "XXXXXXXXXXXXXXXX")
	if err := s.Content.Clone(t.Context(), s.Share, src, s.Share, dst, vfs.Extent{SrcOff: 0, DstOff: 2, Len: 10}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	s.wantRead(t, dst, 0, 32, "XXabcd\x00\x00\x00\x00efXXXX")
	s.wantRuns(t, dst, run{0, 6, false}, run{6, 4, true}, run{10, 6, false})
	s.wantRead(t, src, 0, 32, "abcd\x00\x00\x00\x00ef")
	if err := s.Content.Clone(t.Context(), s.Share, src, s.Share, src, vfs.Extent{SrcOff: 0, DstOff: 4, Len: 4}); err != nil {
		t.Fatalf("a clone between touching extents of one file: %v", err)
	}
	s.wantRead(t, src, 0, 32, "abcdabcdef")
}

// A copy runs its extents in order, a later one overwriting an earlier one's
// destination, and answers the bytes it copied.
func copyCopiesInOrder(t *testing.T, s Subject) {
	src, dst := s.File(t), s.File(t)
	s.write(t, src, 0, "abcdefgh")
	n, err := s.Content.Copy(t.Context(), s.Share, src, s.Share, dst, []vfs.Extent{{SrcOff: 0, DstOff: 0, Len: 4}, {SrcOff: 4, DstOff: 0, Len: 4}, {SrcOff: 0, DstOff: 4, Len: 2}})
	if err != nil || n != 10 {
		t.Fatalf("Copy = %d, %v; want 10, nil", n, err)
	}
	s.wantRead(t, dst, 0, 16, "efghab")
}

// A copy or clone with any invalid extent is refused before it copies
// anything, its valid extents included, as is one whose source extent ends
// past the source's size.
func aRefusedCopyChangesNothing(t *testing.T, s Subject) {
	src, dst := s.File(t), s.File(t)
	s.write(t, src, 0, "abcdefgh")
	n, err := s.Content.Copy(t.Context(), s.Share, src, s.Share, dst, []vfs.Extent{{SrcOff: 0, DstOff: 0, Len: 4}, {SrcOff: 4, DstOff: 4, Len: 0}})
	if n != 0 || !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Copy with an empty second extent = %d, %v; want 0, ErrInvalid", n, err)
	}
	s.wantRead(t, dst, 0, 8, "")
	if err := s.Content.Clone(t.Context(), s.Share, src, s.Share, src, vfs.Extent{SrcOff: 0, DstOff: 2, Len: 4}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a clone overlapping itself within one file = %v, want ErrInvalid", err)
	}
	s.wantRead(t, src, 0, 8, "abcdefgh")

	short := s.File(t)
	s.write(t, short, 0, "abcd")
	past := vfs.Extent{SrcOff: 4, DstOff: 0, Len: 5}
	if err := s.Content.Clone(t.Context(), s.Share, short, s.Share, dst, past); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a clone ending past the source's size = %v, want ErrInvalid", err)
	}
	if n, err := s.Content.Copy(t.Context(), s.Share, short, s.Share, dst, []vfs.Extent{past}); n != 0 || !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("a copy ending past the source's size = %d, %v; want 0, ErrInvalid", n, err)
	}
	s.wantRead(t, dst, 0, 8, "")
}

// A clone or copy runs its context's admitted hook (engine.WithAdmitted)
// once admitted and before the destination changes: a refusal at admission
// runs none, the hook's error refuses the call with the destination
// unchanged, and a call it admits lands.
func theAdmittedHookRunsBeforeTheChange(t *testing.T, s Subject) {
	src, dst := s.File(t), s.File(t)
	s.write(t, src, 0, "abcd")
	s.write(t, dst, 0, "XXXX")
	ran := 0
	hook := func(err error) context.Context {
		return engine.WithAdmitted(t.Context(), func() error { ran++; return err })
	}
	if err := s.Content.Clone(hook(nil), s.Share, dst, s.Share, dst, vfs.Extent{SrcOff: 0, DstOff: 1, Len: 2}); !errors.Is(err, vfs.ErrInvalid) || ran != 0 {
		t.Errorf("a clone overlapping itself = %v, hook ran %d times; want ErrInvalid, none", err, ran)
	}
	refused := errors.New("refused by the hook")
	if err := s.Content.Clone(hook(refused), s.Share, src, s.Share, dst, vfs.Extent{Len: 4}); !errors.Is(err, refused) || ran != 1 {
		t.Errorf("a clone whose hook refuses = %v, hook ran %d times; want the hook's error, once", err, ran)
	}
	if n, err := s.Content.Copy(hook(refused), s.Share, src, s.Share, dst, []vfs.Extent{{Len: 4}}); n != 0 || !errors.Is(err, refused) {
		t.Errorf("a copy whose hook refuses = %d, %v; want 0 and the hook's error", n, err)
	}
	s.wantRead(t, dst, 0, 8, "XXXX")
	ran = 0
	if err := s.Content.Clone(hook(nil), s.Share, src, s.Share, dst, vfs.Extent{Len: 4}); err != nil || ran == 0 {
		t.Errorf("a clone whose hook admits it = %v, hook ran %d times; want nil, at least once", err, ran)
	}
	s.wantRead(t, dst, 0, 8, "abcd")
}

// With nothing lost, every Write and Commit of the share answers one
// verifier, whichever file and however much later, and a file's loss
// sequence stays at 0, where every file's starts.
func theVerifierIsStable(t *testing.T, s Subject) {
	f, g := s.File(t), s.File(t)
	if seq := s.Content.LossSeq(s.Share, f); seq != 0 {
		t.Errorf("a new file's loss sequence is %d, want 0", seq)
	}
	v := s.write(t, f, 0, "a")
	check := func(what string, got vfs.Verifier) {
		t.Helper()
		if got != v {
			t.Errorf("%s answered verifier %x, want %x, the first write's", what, got, v)
		}
	}
	check("a write to another file", s.write(t, g, 0, "b"))
	got, seq := s.commit(t, f)
	check("a commit", got)
	if seq != 0 || s.Content.LossSeq(s.Share, f) != 0 {
		t.Errorf("with nothing lost, Commit's loss sequence %d and LossSeq %d, want 0", seq, s.Content.LossSeq(s.Share, f))
	}
	s.Clock.Advance(time.Minute)
	got, _ = s.commit(t, g)
	check("a commit a minute later", got)
	check("a write a minute later", s.write(t, f, 1, "c"))
}

// A loss changes the share's verifier and raises the file's loss sequence,
// another file's left where it was; the Commit after it answers both, at
// most once failing, and the verifier then holds again.
func aLossChangesTheVerifier(t *testing.T, s Subject) {
	if s.Lose == nil {
		t.Skip("the Content under test cannot be made to lose a write")
	}
	f, g := s.File(t), s.File(t)
	s.write(t, g, 0, "g")
	before, other := s.Content.LossSeq(s.Share, f), s.Content.LossSeq(s.Share, g)
	v := s.write(t, f, 0, "abcd")
	s.Lose(t, f)
	var (
		got vfs.Verifier
		seq uint64
		err error
	)
	for range 2 { // the stability point the loss failed may answer its error once
		if got, seq, err = s.Content.Commit(t.Context(), s.Share, f, 0, 0); err == nil {
			break
		}
	}
	switch {
	case err != nil:
		t.Fatalf("the second Commit after a loss: %v", err)
	case got == v:
		t.Errorf("the Commit after a loss answered the verifier %x of the write it lost", v)
	case seq <= before || s.Content.LossSeq(s.Share, f) != seq:
		t.Errorf("after a loss, Commit's loss sequence %d and LossSeq %d; want both above %d", seq, s.Content.LossSeq(s.Share, f), before)
	}
	if seq := s.Content.LossSeq(s.Share, g); seq != other {
		t.Errorf("another file's loss sequence moved from %d to %d", other, seq)
	}
	if w := s.write(t, f, 0, "abcd"); w != got {
		t.Errorf("the write after the loss answered verifier %x, want %x, the Commit's", w, got)
	}
	if w := s.write(t, g, 1, "h"); w != got {
		t.Errorf("a write to another file after the loss answered verifier %x, want %x, the Commit's", w, got)
	}
	if again, seq2 := s.commit(t, f); again != got || seq2 != seq {
		t.Errorf("a second Commit with no new loss = %x, %d; want %x, %d", again, seq2, got, seq)
	}
}

// Every call naming a share with no binding answers a Delay with
// DelayFrozen, a copy or clone with either side unbound included, a refused
// write calls no Done, and LossSeq answers 0.
func anUnboundShareIsFrozen(t *testing.T, s Subject) {
	f, g := s.File(t), s.File(t)
	s.write(t, f, 0, "abcd")
	if s.Lose != nil {
		s.write(t, g, 0, "g")
		s.Lose(t, g) // so a LossSeq that ignored the share would answer above 0
	}
	u := entity.NewShareID()
	ctx := t.Context()
	var dones atomic.Int32
	x := vfs.Extent{SrcOff: 0, DstOff: 0, Len: 4}
	calls := map[string]func() error{
		"Draw": func() error { _, err := s.Content.Draw(ctx, u, f); return err },
		"Write": func() error {
			_, err := s.Content.Write(ctx, u, f, 0, []byte("x"), engine.WriteOpts{Done: func() { dones.Add(1) }})
			return err
		},
		"Commit":          func() error { _, _, err := s.Content.Commit(ctx, u, f, 0, 0); return err },
		"CommitExistence": func() error { return s.Content.CommitExistence(ctx, u, f) },
		"Read":            func() error { _, err := s.Content.Read(ctx, u, f, 0, make([]byte, 4)); return err },
		"Truncate":        func() error { return s.Content.Truncate(ctx, u, f, 0) },
		"Deallocate":      func() error { return s.Content.Deallocate(ctx, u, f, 0, 0) },
		"Delete":          func() error { _, err := s.Content.Delete(ctx, u, f); return err },
		"Clone to it":     func() error { return s.Content.Clone(ctx, s.Share, f, u, s.File(t), x) },
		"Clone from it":   func() error { return s.Content.Clone(ctx, u, f, s.Share, s.File(t), x) },
		"Copy to it": func() error {
			_, err := s.Content.Copy(ctx, s.Share, f, u, s.File(t), []vfs.Extent{x})
			return err
		},
		"Copy from it": func() error {
			_, err := s.Content.Copy(ctx, u, f, s.Share, s.File(t), []vfs.Extent{x})
			return err
		},
		"Overlay":    func() error { _, _, err := s.Content.Overlay(ctx, u, f); return err },
		"Allocation": func() error { _, err := s.Content.Allocation(ctx, u, f, 0); return err },
	}
	for name, call := range calls {
		var d *vfs.Delay
		if err := call(); !errors.As(err, &d) || d.Cause != vfs.DelayFrozen {
			t.Errorf("%s on an unbound share = %v, want a Delay with DelayFrozen", name, err)
		}
	}
	if n := dones.Load(); n != 0 {
		t.Errorf("a write refused for its share had its Done called %d times", n)
	}
	if seq := s.Content.LossSeq(u, g); seq != 0 {
		t.Errorf("LossSeq on an unbound share = %d, want 0", seq)
	}
	s.wantRead(t, f, 0, 8, "abcd")
}

// A file written through one share and named through another bound share is
// corrupt, to a write and a read alike, and the write changes nothing.
func aFileOfAnotherShareIsCorrupt(t *testing.T, s Subject) {
	if s.Other == (entity.ShareID{}) {
		t.Skip("the Content under test has no second bound share")
	}
	f := s.File(t)
	s.write(t, f, 0, "abcd")
	if _, err := s.Content.Write(t.Context(), s.Other, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); !errors.Is(err, vfs.ErrCorrupt) {
		t.Errorf("a write through another share = %v, want ErrCorrupt", err)
	}
	if _, err := s.Content.Read(t.Context(), s.Other, f, 0, make([]byte, 4)); !errors.Is(err, vfs.ErrCorrupt) {
		t.Errorf("a read through another share = %v, want ErrCorrupt", err)
	}
	s.wantRead(t, f, 0, 8, "abcd")
}

// A range below zero or past the largest offset is ErrInvalid, as is a clone
// or copy extent that is empty or overlaps itself within one file; a refused
// write calls no Done.
func badRangesAreInvalid(t *testing.T, s Subject) {
	f, g := s.File(t), s.File(t)
	ctx, c, sh := t.Context(), s.Content, s.Share
	var dones atomic.Int32
	done := engine.WriteOpts{Done: func() { dones.Add(1) }}
	const top = math.MaxInt64
	clone := func(src, dst entity.FileID, x vfs.Extent) func() error {
		return func() error { return c.Clone(ctx, sh, src, sh, dst, x) }
	}
	cp := func(src, dst entity.FileID, x vfs.Extent) func() error {
		return func() error { _, err := c.Copy(ctx, sh, src, sh, dst, []vfs.Extent{x}); return err }
	}
	calls := []struct {
		name string
		call func() error
	}{
		{"Write below zero", func() error { _, err := c.Write(ctx, sh, f, -1, []byte("x"), done); return err }},
		{"Write past the largest offset", func() error { _, err := c.Write(ctx, sh, f, top-1, []byte("xy"), done); return err }},
		{"Read below zero", func() error { _, err := c.Read(ctx, sh, f, -1, make([]byte, 1)); return err }},
		{"Read past the largest offset", func() error { _, err := c.Read(ctx, sh, f, top-1, make([]byte, 2)); return err }},
		{"Commit below zero", func() error { _, _, err := c.Commit(ctx, sh, f, -1, 0); return err }},
		{"Commit of a negative length", func() error { _, _, err := c.Commit(ctx, sh, f, 0, -1); return err }},
		{"Commit past the largest offset", func() error { _, _, err := c.Commit(ctx, sh, f, top, 1); return err }},
		{"Truncate below zero", func() error { return c.Truncate(ctx, sh, f, -1) }},
		{"Deallocate below zero", func() error { return c.Deallocate(ctx, sh, f, -1, 1) }},
		{"Deallocate of a negative length", func() error { return c.Deallocate(ctx, sh, f, 0, -1) }},
		{"Deallocate past the largest offset", func() error { return c.Deallocate(ctx, sh, f, top, 1) }},
		{"Allocation below zero", func() error { _, err := c.Allocation(ctx, sh, f, -1); return err }},
		{"Clone of no byte", clone(f, g, vfs.Extent{Len: 0})},
		{"Clone of a negative length", clone(f, g, vfs.Extent{Len: -1})},
		{"Clone from below zero", clone(f, g, vfs.Extent{SrcOff: -1, Len: 1})},
		{"Clone to below zero", clone(f, g, vfs.Extent{DstOff: -1, Len: 1})},
		{"Clone past the largest offset", clone(f, g, vfs.Extent{DstOff: top, Len: 1})},
		{"Clone overlapping itself", clone(f, f, vfs.Extent{SrcOff: 0, DstOff: 1, Len: 2})},
		{"Copy of no byte", cp(f, g, vfs.Extent{Len: 0})},
		{"Copy from below zero", cp(f, g, vfs.Extent{SrcOff: -1, Len: 1})},
		{"Copy overlapping itself", cp(f, f, vfs.Extent{SrcOff: 1, DstOff: 0, Len: 2})},
	}
	for _, x := range calls {
		if err := x.call(); !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", x.name, err)
		}
	}
	if n := dones.Load(); n != 0 {
		t.Errorf("a refused write had its Done called %d times", n)
	}
}

// Files written, read and drawn concurrently each read back their own bytes,
// and no two draws of the share answer one version.
func filesWrittenConcurrently(t *testing.T, s Subject) {
	const writers, rounds = 8, 32
	files := make([]entity.FileID, writers)
	for i := range files {
		files[i] = s.File(t)
	}
	versions := make(chan entity.JournalVersion, writers*rounds)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Go(func() {
			ctx, want := t.Context(), bytes.Repeat([]byte{byte('a' + i)}, rounds)
			for r := range rounds {
				v, err := s.Content.Draw(ctx, s.Share, f)
				if err != nil {
					t.Errorf("Draw: %v", err)
					return
				}
				versions <- v
				if _, err := s.Content.Write(ctx, s.Share, f, int64(r), want[r:r+1], engine.WriteOpts{}); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				if _, _, err := s.Content.Overlay(ctx, s.Share, f); err != nil {
					t.Errorf("Overlay: %v", err)
					return
				}
			}
			got := make([]byte, rounds)
			if n, err := s.Content.Read(ctx, s.Share, f, 0, got); err != nil || !bytes.Equal(got[:n], want) {
				t.Errorf("file %d read back %q, %v; want %q", i, got[:n], err, want)
			}
		})
	}
	wg.Wait()
	close(versions)
	seen := map[entity.JournalVersion]bool{}
	for v := range versions {
		if seen[v] {
			t.Errorf("two draws answered version %x", v)
		}
		seen[v] = true
	}
}
