package enginetest

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// segFake is a fake with the segment model on, segments of 100 bytes, and a
// handle on tag 1.
func segFake(t *testing.T, max int64) (*FakeJournal, engine.Handle) {
	t.Helper()

	j, h := fake(t, 0)
	j.SegmentCap(100, max)

	return j, h
}

// overwrite writes 40 bytes of file id twice at offset 0, so 40 bytes are
// superseded in the first record's segment.
func overwrite(t *testing.T, h engine.Handle, id journal.FileID) {
	t.Helper()

	write(t, h, id, 0, strings.Repeat("a", 40))
	write(t, h, id, 0, strings.Repeat("b", 40))
}

func ended(j *FakeJournal) int64 { return j.Stats().UnreclaimedEndedBytes }

// Without the segment model the fake's Repack answers ErrNotImplemented, as the
// journal's does.
func TestRepackWithoutSegmentsIsNotImplemented(t *testing.T) {
	j, _ := fake(t, 0)

	if _, err := j.Repack(t.Context(), 1<<20, journal.RepackScope{}); !errors.Is(
		err,
		journal.ErrNotImplemented,
	) {
		t.Errorf("Repack = %v, want ErrNotImplemented", err)
	}
}

// Superseded bytes are unreclaimed at once, or with the sync timer held only
// from the file's next Sync, and an active segment's bytes are not ended.
func TestSupersededBytesAreUnreclaimedFromTheSync(t *testing.T) {
	j, h := segFake(t, 0)
	j.HoldSyncTimer(true)

	overwrite(t, h, fileA)

	if got := j.Stats().UnreclaimedBytes; got != 0 {
		t.Fatalf("UnreclaimedBytes = %d before the file's Sync, want 0", got)
	}

	if _, err := h.Sync(t.Context(), fileA); err != nil {
		t.Fatal(err)
	}

	s := j.Stats()
	if s.UnreclaimedBytes != 40 || s.Shares[1].UnreclaimedBytes != 40 {
		t.Errorf(
			"UnreclaimedBytes = %d, share %d, want 40 and 40",
			s.UnreclaimedBytes,
			s.Shares[1].UnreclaimedBytes,
		)
	}

	if s.UnreclaimedEndedBytes != 0 {
		t.Errorf(
			"UnreclaimedEndedBytes = %d for an active segment, want 0",
			s.UnreclaimedEndedBytes,
		)
	}
}

// A share-scoped pass seals the active segment holding the share's unreclaimed
// bytes, selects it and retires it, returning its allocation.
func TestScopedRepackSealsAndRetires(t *testing.T) {
	j, h := segFake(t, 0)
	overwrite(t, h, fileA)

	freed, err := j.Repack(t.Context(), 100, journal.RepackScope{Share: 1})
	if err != nil {
		t.Fatal(err)
	}

	s := j.Stats()
	if freed != 80 || s.UnreclaimedBytes != 0 || s.Shares[1].UnreclaimedEndedBytes != 0 {
		t.Errorf("freed %d unreclaimed %d ended %d, want 80, 0, 0: an early seal returns its tail",
			freed, s.UnreclaimedBytes, s.Shares[1].UnreclaimedEndedBytes)
	}
}

// The zero scope seals too, and a scope naming segments seals nothing.
func TestZeroScopeSealsAndASegmentScopeDoesNot(t *testing.T) {
	j, h := segFake(t, 0)
	overwrite(t, h, fileA)

	byID := journal.RepackScope{Segments: []journal.SegmentID{1}}

	if _, err := j.Repack(t.Context(), 0, byID); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 0 {
		t.Fatalf("a segment-set scope sealed: ended %d, want 0", got)
	}

	if _, err := j.Repack(t.Context(), 0, journal.RepackScope{}); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 40 {
		t.Errorf("the zero scope left ended = %d, want 40 sealed", got)
	}
}

// A pass seals at most one segment per stream until repack retires it.
func TestAPassSealsOnePerStream(t *testing.T) {
	j, h := segFake(t, 0)
	overwrite(t, h, fileA)

	// Budget 0 seals but cannot copy the live 40 bytes, so nothing retires.
	if _, err := j.Repack(t.Context(), 0, journal.RepackScope{Share: 1}); err != nil {
		t.Fatal(err)
	}

	write(t, h, fileA, 200, strings.Repeat("a", 40))
	write(t, h, fileA, 200, strings.Repeat("b", 40))

	if _, err := j.Repack(t.Context(), 0, journal.RepackScope{Share: 1}); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 40 {
		t.Errorf("ended = %d after two passes on one stream, want 40: one seal per stream", got)
	}

	j.StreamOf(fileB, 1)
	overwrite(t, h, fileB)

	if _, err := j.Repack(t.Context(), 0, journal.RepackScope{Share: 1}); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 80 {
		t.Errorf("ended = %d, want 80: the second stream seals its own", got)
	}
}

// Repack copies at most budget bytes: a segment whose live bytes pass it stays.
func TestRepackSpendsItsBudget(t *testing.T) {
	j, h := segFake(t, 0)
	overwrite(t, h, fileA)

	if _, err := j.Repack(t.Context(), 39, journal.RepackScope{Share: 1}); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 40 {
		t.Errorf("ended = %d, want 40: the segment's 40 live bytes pass a budget of 39", got)
	}
}

// A segment a running pass selected still counts, a failed one counts whether
// or not its window resolved, and the pass retires only a resolved one.
func TestTheFigureCountsSelectedAndFailedSegments(t *testing.T) {
	j, _ := segFake(t, 0)
	j.AddEnded(1, 30)
	j.AddFailed(1, 20, true)

	var during int64

	j.AtRepackSelected(func(context.Context) { during = ended(j) })

	if _, err := j.Repack(t.Context(), 100, journal.RepackScope{}); err != nil {
		t.Fatal(err)
	}

	if during != 50 {
		t.Errorf(
			"ended = %d inside the pass, want 50: the selected 30 and the unresolved failed 20",
			during,
		)
	}

	if got := ended(j); got != 20 {
		t.Errorf("ended = %d after the pass, want 20: an unresolved window is never repacked", got)
	}

	j.ResolveWindows()

	if _, err := j.Repack(t.Context(), 100, journal.RepackScope{}); err != nil {
		t.Fatal(err)
	}

	if got := ended(j); got != 0 {
		t.Errorf("ended = %d after the window resolved, want 0", got)
	}
}

// A write under a reservation passes a headroom refusal; a plain write does not.
func TestAReservedWritePassesTheHeadroom(t *testing.T) {
	j, h := segFake(t, 0)

	headroom := &journal.NoSpaceError{
		Limit:   journal.LimitHeadroom,
		Missing: 1,
		Unit:    journal.UnitBytes,
	}

	if err := j.Refuse(CallWriteAt, headroom); err != nil {
		t.Fatal(err)
	}

	r, err := h.Reserve(10, 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.WriteAt(t.Context(), fileA, 0, []byte("x"), 1); err != nil {
		t.Errorf("a reserved write: %v, want accepted", err)
	}

	var ns *journal.NoSpaceError

	if _, err := h.WriteAt(t.Context(), fileB, 0, []byte("x"), 1); !errors.As(err, &ns) ||
		ns.Limit != journal.LimitHeadroom {
		t.Errorf("a plain write: %v, want the headroom refusal", err)
	}
}

// A roll past the maximum is refused naming the journal, and on a full device
// with ENOSPC; writes into the allocated segment go on.
func TestARollIsRefusedAtTheMaximumAndOnAFullDevice(t *testing.T) {
	_, h := segFake(t, 100)
	write(t, h, fileA, 0, strings.Repeat("a", 60))

	var ns *journal.NoSpaceError

	_, err := h.WriteAt(t.Context(), fileA, 100, []byte(strings.Repeat("b", 60)), 1)
	if !errors.As(err, &ns) || ns.Limit != journal.LimitJournal {
		t.Fatalf("a roll at the maximum: %v, want the journal's limit", err)
	}

	write(t, h, fileA, 100, strings.Repeat("c", 30))

	j, h2 := segFake(t, 0)
	write(t, h2, fileA, 0, "x")
	j.DeviceFull(true)

	write(t, h2, fileA, 1, "y")

	if _, err := h2.WriteAt(t.Context(), fileB, 0, []byte(strings.Repeat("d", 99)), 1); !errors.Is(
		err,
		syscall.ENOSPC,
	) {
		t.Errorf("a roll on a full device: %v, want ENOSPC", err)
	}
}

// A share's limit counts the record bytes still on disk, superseded ones
// included, until repack retires them.
func TestTheShareLimitCountsUnreclaimedBytes(t *testing.T) {
	j, h := fake(t, 100)
	j.SegmentCap(100, 0)
	overwrite(t, h, fileA)

	var ns *journal.NoSpaceError

	_, err := h.WriteAt(t.Context(), fileB, 0, []byte(strings.Repeat("c", 30)), 1)
	if !errors.As(err, &ns) || ns.Limit != journal.LimitShare {
		t.Fatalf("a write past the share's record bytes on disk: %v, want the share's limit", err)
	}

	if _, err := j.Repack(t.Context(), 100, journal.RepackScope{Share: 1}); err != nil {
		t.Fatal(err)
	}

	write(t, h, fileB, 0, strings.Repeat("c", 30))
}
