package enginetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

var (
	fileA = journal.NewFileID([16]byte{'a'})
	fileB = journal.NewFileID([16]byte{'b'})
)

func ver(n uint64) journal.Version { return journal.Version{Counter: n} }

func ext(off, n int64) journal.Extent { return journal.Extent{Off: off, Len: n} }

// fake returns a fake journal with a version limit and a handle on tag 1
// with the given limit.
func fake(t *testing.T, limit int64) (*FakeJournal, engine.Handle) {
	t.Helper()
	j := NewFakeJournal()
	if err := j.SetVersionLimit(ver(1 << 32)); err != nil {
		t.Fatal(err)
	}
	h, err := j.Share(1, limit, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	return j, h
}

func write(t *testing.T, h engine.Handle, id journal.FileID, off int64, p string) journal.Version {
	t.Helper()
	v, err := h.WriteAt(t.Context(), id, off, []byte(p), 7)
	if err != nil {
		t.Fatalf("write %q at %d: %v", p, off, err)
	}
	return v
}

func changes(t *testing.T, h engine.Handle, id journal.FileID, from journal.Version) []journal.Change {
	t.Helper()
	var out []journal.Change
	for c, err := range h.Since(t.Context(), id, from) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// A read serves the newest bytes held, names each part's version, reports
// every part not held as missing with its bytes untouched, and answers the
// file's latest change as asOf.
func TestFakeReadServesHeldAndReportsMissing(t *testing.T) {
	_, h := fake(t, 0)
	v1 := write(t, h, fileA, 2, "aaaaaa") // [2, 8)
	v2 := write(t, h, fileA, 4, "bb")     // [4, 6)
	if v2.Compare(v1) <= 0 {
		t.Fatalf("versions %v then %v do not rise", v1, v2)
	}
	p := []byte("..........")
	n, missing, held, asOf, err := h.ReadAt(t.Context(), fileA, 0, p)
	if err != nil {
		t.Fatal(err)
	}
	if string(p) != "..aabbaa.." || n != 6 {
		t.Errorf("read %q, n %d; want ..aabbaa.., 6", p, n)
	}
	if want := []journal.Extent{ext(0, 2), ext(8, 2)}; !slices.Equal(missing, want) {
		t.Errorf("missing %v, want %v", missing, want)
	}
	if want := []journal.Held{{Extent: ext(2, 2), Version: v1}, {Extent: ext(4, 2), Version: v2}, {Extent: ext(6, 2), Version: v1}}; !slices.Equal(held, want) {
		t.Errorf("held %v, want %v", held, want)
	}
	if asOf != v2 {
		t.Errorf("asOf %v, want %v", asOf, v2)
	}
}

// Removals stop holding their range and leave markers; Since yields what is
// above its from in version order, and Settle drops markers at or below its
// version from Since while Markers keeps them all.
func TestFakeRemovalsLeaveMarkersUntilSettled(t *testing.T) {
	j, h := fake(t, 0)
	ctx := t.Context()
	w := write(t, h, fileA, 0, "abcdefgh")
	d, err := h.Deallocate(ctx, fileA, ext(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := h.Truncate(ctx, fileA, 6)
	if err != nil {
		t.Fatal(err)
	}
	got := changes(t, h, fileA, journal.Version{})
	kinds := []journal.ChangeKind{journal.ChangeWrite, journal.ChangeWrite, journal.ChangeDeallocate, journal.ChangeTruncate}
	if len(got) != len(kinds) {
		t.Fatalf("Since = %+v, want kinds %v", got, kinds)
	}
	for i, c := range got {
		if c.Kind != kinds[i] {
			t.Errorf("change %d is kind %v, want %v", i, c.Kind, kinds[i])
		}
	}
	if got[0].Extent != ext(0, 2) || got[1].Extent != ext(4, 2) || got[0].Version != w || got[0].Mtime != 7 {
		t.Errorf("held after removals: %+v, %+v; want [0,2) and [4,6) at %v, mtime 7", got[0], got[1], w)
	}
	if got := changes(t, h, fileA, d); len(got) != 1 || got[0].Version != tr {
		t.Errorf("Since(%v) = %+v; want the truncate alone", d, got)
	}
	h.Settle(fileA, d)
	if got := changes(t, h, fileA, w); len(got) != 1 || got[0].Kind != journal.ChangeTruncate {
		t.Errorf("after Settle(%v): Since = %+v; want the truncate alone", d, got)
	}
	if m := j.Markers(fileA); len(m) != 2 || m[0].Version != d || m[1].Version != tr {
		t.Errorf("Markers after Settle(%v) = %+v; want the deallocate and the truncate, settled or not", d, m)
	}
}

// A loss stops holding the extent, is reported by Since at the version it
// dropped and joins the loss ring; a corrupt loss and a failed window's raise
// the generation and a stale one does not; a failed window's, never synced,
// is not reported by Since; a loss of nothing held records nothing.
func TestFakeLossRingAndGeneration(t *testing.T) {
	j, h := fake(t, 0)
	v := write(t, h, fileA, 0, "abcd")
	j.Lose(fileA, ext(0, 2), journal.LossCorrupt)
	j.Lose(fileB, ext(0, 2), journal.LossCorrupt)
	s := j.Stats()
	if s.LossGeneration != 1 || len(s.Losses) != 1 {
		t.Fatalf("generation %d, ring %+v; want one loss", s.LossGeneration, s.Losses)
	}
	if e := s.Losses[0]; e.File != fileA || e.Extent != ext(0, 2) || e.Version != v || e.Reason != journal.LossCorrupt {
		t.Errorf("loss event %+v", e)
	}
	_, missing, _, _, _ := h.ReadAt(t.Context(), fileA, 0, make([]byte, 4))
	if !slices.Equal(missing, []journal.Extent{ext(0, 2)}) {
		t.Errorf("missing after the loss: %v", missing)
	}
	if got := changes(t, h, fileA, journal.Version{}); len(got) != 2 || got[1].Kind != journal.ChangeLoss {
		t.Errorf("Since = %+v; want the write's rest, then the loss", got)
	}
	j.Lose(fileA, ext(2, 1), journal.LossStale)
	if s := j.Stats(); s.LossGeneration != 1 || len(s.Losses) != 2 {
		t.Errorf("after a stale loss: generation %d, ring %+v; want generation 1, two losses", s.LossGeneration, s.Losses)
	}
	j.Lose(fileA, ext(3, 1), journal.LossFailedWindow)
	if s := j.Stats(); s.LossGeneration != 2 || len(s.Losses) != 3 {
		t.Errorf("after a failed window's loss: generation %d, ring %+v; want generation 2, three losses", s.LossGeneration, s.Losses)
	}
	if got := changes(t, h, fileA, journal.Version{}); len(got) != 2 || got[1].Extent != ext(2, 1) {
		t.Errorf("Since after a failed window's loss = %+v; want the two synced losses, the stale one last", got)
	}
}

// Each refusal is the journal's named one.
func TestFakeRefusals(t *testing.T) {
	j, h := fake(t, 4)
	ctx := t.Context()
	write(t, h, fileA, 0, "abc")
	var ns *journal.NoSpaceError
	if _, err := h.WriteAt(ctx, fileA, 3, []byte("de"), 0); !errors.As(err, &ns) || ns.Limit != journal.LimitShare {
		t.Errorf("write past the share's limit: %v", err)
	}
	other, err := j.Share(2, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.WriteAt(ctx, fileA, 0, []byte("x"), 0); !errors.Is(err, journal.ErrOtherShare) {
		t.Errorf("write to another share's file: %v", err)
	}
	// A file belongs to the tag of its records: reading one with none claims
	// nothing.
	if _, _, _, _, err := other.ReadAt(ctx, fileB, 0, make([]byte, 1)); err != nil {
		t.Errorf("read of a file with no records: %v", err)
	}
	for _, err := range other.Since(ctx, fileB, journal.Version{}) {
		t.Errorf("Since of a file with no records: %v", err)
	}
	if _, err := h.WriteAt(ctx, fileB, 0, []byte("y"), 0); err != nil {
		t.Errorf("write to a file another share only read: %v", err)
	}
	if _, err := h.Deallocate(ctx, fileA, ext(1, 0)); !errors.Is(err, journal.ErrZeroLength) {
		t.Errorf("zero-length deallocate: %v", err)
	}
	if err := j.SetVersionLimit(ver(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Draw(); !errors.As(err, &ns) || ns.Limit != journal.LimitVersion {
		t.Errorf("draw at the version limit: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Truncate(ctx, fileA, 0); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("truncate after Close: %v", err)
	}
}

// Share raises the counter above its floor, and nothing is issued before a
// version limit.
func TestFakeVersionsAboveFloorAndLimit(t *testing.T) {
	j := NewFakeJournal()
	if _, err := j.Draw(); !errors.Is(err, journal.ErrNoSpace) {
		t.Errorf("draw before a version limit: %v", err)
	}
	if err := j.SetVersionLimit(ver(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Share(1, 0, ver(50)); err != nil {
		t.Fatal(err)
	}
	if v, err := j.Draw(); err != nil || v != ver(51) {
		t.Errorf("draw after a floor of 50 = %v, %v; want 51", v, err)
	}
}

// Strand records the tag, once however often it is named, refuses tag 0 as
// Share does, answers ErrClosed once the journal is closed, and is forgotten
// at a reopen: RFC 1 §3 keeps it in memory, so the caller names its tags
// again at every start.
func TestFakeStrandRecordsTheTagAndForgetsItAtAReopen(t *testing.T) {
	j := NewFakeJournal()

	if err := j.Strand(5); err != nil {
		t.Fatalf("Strand(5): %v", err)
	}

	if err := j.Strand(5); err != nil {
		t.Fatalf("Strand(5) again: %v", err)
	}

	if got := j.Stranded(); !slices.Equal(got, []journal.ShareTag{5}) {
		t.Errorf("stranded %v after Strand(5) twice, want [5]", got)
	}

	if err := j.Strand(0); !errors.Is(err, journal.ErrInvalidSetting) {
		t.Errorf("Strand(0) = %v, want journal.ErrInvalidSetting", err)
	}

	j.Reopen()

	if got := j.Stranded(); len(got) != 0 {
		t.Errorf("stranded %v after a reopen, want none", got)
	}

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	if err := j.Strand(5); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("Strand(5) after Close = %v, want journal.ErrClosed", err)
	}
}

// A tag's dirty bytes are its held bytes less those marked offloaded, as
// RFC 1 §3.7 has them (dirty is at most held).
func TestFakeStatsReportsDirtyBytesBelowHeld(t *testing.T) {
	j, h := fake(t, 0)

	write(t, h, fileA, 0, string(make([]byte, 8192)))
	j.SetOffloaded(1, 4096)

	got := j.Stats().Shares[1]
	if got.HeldBytes != 8192 || got.DirtyBytes != 4096 {
		t.Errorf("held %d dirty %d, want held 8192 dirty 4096", got.HeldBytes, got.DirtyBytes)
	}
}

// refusalOf is the capacity refusal naming limit l.
func refusalOf(l journal.Limit) error {
	return &journal.NoSpaceError{Limit: l, Missing: 1, Unit: journal.UnitBytes}
}

// fullDevice is the error a full device surfaces as: never a NoSpaceError.
func fullDevice() error {
	return &fs.PathError{Op: "write", Path: "segment", Err: syscall.ENOSPC}
}

// callFake makes one call of kind c on file A and answers its error.
func callFake(t *testing.T, j *FakeJournal, h engine.Handle, c Call) error {
	t.Helper()

	ctx := t.Context()

	var err error

	switch c {
	case CallWriteAt:
		_, err = h.WriteAt(ctx, fileA, 0, []byte("x"), 0)
	case CallDraw:
		_, err = j.Draw()
	case CallTruncate:
		_, err = h.Truncate(ctx, fileA, 0)
	case CallDeallocate:
		_, err = h.Deallocate(ctx, fileA, ext(0, 1))
	case CallDelete:
		_, err = h.Delete(ctx, fileA)
	case CallCloneTarget:
		_, err = h.CloneTarget(ctx, fileA, journal.CloneSpec{DstOff: 0, Len: 1})
	case CallReserve:
		_, err = h.Reserve(1, 1)
	}

	return err
}

// A refused call answers the armed error exactly, for every pair RFC 1 lets
// the journal answer, and issues no version; a pair RFC 1 does not allow is
// refused by Refuse and arms nothing; a nil error disarms.
func TestFakeRefusesOnlyWhatTheJournalCan(t *testing.T) {
	call := func(j *FakeJournal, h engine.Handle, c Call) error {
		return callFake(t, j, h, c)
	}

	allowed := map[Call][]error{
		CallWriteAt: {
			refusalOf(journal.LimitShare), refusalOf(journal.LimitJournal),
			refusalOf(journal.LimitIndex), refusalOf(journal.LimitVersion),
			refusalOf(journal.LimitSpare), refusalOf(journal.LimitHeadroom), fullDevice(),
		},
		CallDraw:     {refusalOf(journal.LimitVersion)},
		CallTruncate: {refusalOf(journal.LimitHeadroom), refusalOf(journal.LimitVersion)},
		CallDelete:   {refusalOf(journal.LimitHeadroom), refusalOf(journal.LimitVersion)},
		CallDeallocate: {
			refusalOf(journal.LimitHeadroom), refusalOf(journal.LimitVersion),
			refusalOf(journal.LimitIndex),
		},
		CallCloneTarget: {
			refusalOf(journal.LimitHeadroom), refusalOf(journal.LimitVersion),
			refusalOf(journal.LimitIndex),
		},
		CallReserve: {
			refusalOf(journal.LimitShare), refusalOf(journal.LimitJournal),
			refusalOf(journal.LimitIndex), refusalOf(journal.LimitVersion),
			refusalOf(journal.LimitSpare),
		},
	}

	for c, errs := range allowed {
		for _, want := range errs {
			j, h := fake(t, 0)
			write(t, h, fileA, 0, "seed")

			before := j.Stats().NextVersion

			if err := j.Refuse(c, want); err != nil {
				t.Errorf("call %d: Refuse(%v) = %v, want nil", c, want, err)

				continue
			}

			if got := call(j, h, c); got != want {
				t.Errorf("call %d armed with %v answered %v", c, want, got)
			}

			if got := j.Stats().NextVersion; got != before {
				t.Errorf(
					"call %d refused with %v moved the next version %v to %v",
					c,
					want,
					before,
					got,
				)
			}

			if err := j.Refuse(c, nil); err != nil {
				t.Errorf("call %d: Refuse(nil) = %v, want nil", c, err)
			}

			if got := call(j, h, c); got != nil {
				t.Errorf("call %d after Refuse(nil) answered %v, want nil", c, got)
			}
		}
	}

	// The forbidden pairs are the complement of allowed: every call times every
	// limit the journal names, walked as the refusal table's test walks them,
	// and a full device; a failing device is none of the calls' to answer.
	candidates := []error{fullDevice(), syscall.EIO}

	for l := journal.LimitShare; !strings.HasPrefix(l.String(), "Limit("); l++ {
		candidates = append(candidates, refusalOf(l))
	}

	forbidden := 0

	for c := CallWriteAt; c <= CallReserve; c++ {
		for _, err := range candidates {
			isAllowed := func(ok error) bool {
				return sameRefusal(ok, err)
			}

			if slices.ContainsFunc(allowed[c], isAllowed) {
				continue
			}

			forbidden++

			j, h := fake(t, 0)
			write(t, h, fileA, 0, "seed")

			if got := j.Refuse(c, err); got == nil {
				t.Errorf("call %d: Refuse(%v) = nil, want an error", c, err)
			}

			if got := call(j, h, c); got != nil {
				t.Errorf("call %d answered %v after a rejected Refuse(%v), want nil", c, got, err)
			}
		}
	}

	// 7 calls times 6 limits and a full device is 49 pairs and RFC 1 allows 23,
	// so 26 are forbidden, and so is a failing device for each of the 7 calls.
	if forbidden != 26+7 {
		t.Errorf("checked %d forbidden pairs, want 33", forbidden)
	}
}

// sameRefusal reports whether a and b are the same capacity refusal: the same
// limit, or both a full device.
func sameRefusal(a, b error) bool {
	var x, y *journal.NoSpaceError

	if errors.As(a, &x) || errors.As(b, &y) {
		return errors.As(a, &x) && errors.As(b, &y) && x.Limit == y.Limit
	}

	return errors.Is(a, syscall.ENOSPC) && errors.Is(b, syscall.ENOSPC)
}

// A failed call answers the armed error exactly, for every pair RFC 1 lets the
// journal answer: a file of another share, and a read-only journal, for the
// calls that name a file, and EIO for WriteAt, which a failed write answers
// (RFC 28 §2.2). Everything else is refused by Fail and arms nothing: a
// capacity refusal is Refuse's, and EIO is no answer of any other call.
func TestFakeFailsOnlyWhatTheJournalCan(t *testing.T) {
	otherShare := &journal.OtherShareError{File: fileA, Tag: 1, Owner: 2}
	readOnly := fmt.Errorf("%w: %w", journal.ErrJournalReadOnly, syscall.EROFS)
	named := []Call{CallWriteAt, CallTruncate, CallDeallocate, CallDelete, CallCloneTarget}

	allowed := map[Call][]error{}
	for _, c := range named {
		allowed[c] = []error{otherShare, readOnly}
	}

	allowed[CallWriteAt] = append(allowed[CallWriteAt], syscall.EIO)

	for c, errs := range allowed {
		for _, want := range errs {
			j, h := fake(t, 0)
			write(t, h, fileA, 0, "seed")

			before := j.Stats().NextVersion

			if err := j.Fail(c, want); err != nil {
				t.Errorf("call %d: Fail(%v) = %v, want nil", c, want, err)

				continue
			}

			if got := callFake(t, j, h, c); got != want {
				t.Errorf("call %d armed with %v answered %v", c, want, got)
			}

			if got := j.Stats().NextVersion; got != before {
				t.Errorf("call %d failed with %v moved the version %v to %v", c, want, before, got)
			}

			if err := j.Fail(c, nil); err != nil {
				t.Errorf("call %d: Fail(nil) = %v, want nil", c, err)
			}

			if got := callFake(t, j, h, c); got != nil {
				t.Errorf("call %d after Fail(nil) answered %v, want nil", c, got)
			}
		}
	}

	// Every call times every error a test could arm: the two allowed ones, a
	// failing device, a closed journal, a canceled context, every limit the
	// journal names and a full device.
	candidates := []error{
		otherShare, readOnly, syscall.EIO, journal.ErrClosed, context.Canceled, fullDevice(),
	}

	for l := journal.LimitShare; !strings.HasPrefix(l.String(), "Limit("); l++ {
		candidates = append(candidates, refusalOf(l))
	}

	forbidden := 0

	for c := CallWriteAt; c <= CallReserve; c++ {
		for _, err := range candidates {
			isAllowed := func(ok error) bool {
				return ok == err
			}

			if slices.ContainsFunc(allowed[c], isAllowed) {
				continue
			}

			forbidden++

			j, h := fake(t, 0)
			write(t, h, fileA, 0, "seed")

			if got := j.Fail(c, err); got == nil {
				t.Errorf("call %d: Fail(%v) = nil, want an error", c, err)
			}

			if got := callFake(t, j, h, c); got != nil {
				t.Errorf("call %d answered %v after a rejected Fail(%v), want nil", c, got, err)
			}
		}
	}

	// 7 calls times 12 candidates is 84 pairs and the journal's contract allows
	// 11 of them: two for each of the five calls that name a file, and EIO for
	// WriteAt.
	if forbidden != 73 {
		t.Errorf("checked %d forbidden pairs, want 73", forbidden)
	}
}
