package journal

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The tests in this file hold the spares the open's loss pass waits for
// before its first record: every stream the plan appends on starts the pass
// with no active segment, so each distinct stream rolls once, and the wait
// counts one spare per stream. A device that cannot make that many refuses
// the open before any record lands, and never parks it.

// Two files the fixture's two streams hash apart: fileOf(7) appends on one
// stream and fileOf(8) on the other. The landing row checks the premise
// against the opened journal's own stream choice; the refusing row plants
// the same pair.
var twoStreamFiles = [2]byte{7, 8}

// plantTwoStreamDrops plants one sealed segment holding, for each of the two
// files, an older record and a newer one whose header is rotted: one corrupt
// drop per stream for the open's loss pass to answer.
func plantTwoStreamDrops(t *testing.T, f *assembledJournal, id core.SegmentID) {
	t.Helper()
	a, b := fileOf(twoStreamFiles[0]), fileOf(twoStreamFiles[1])
	offs := plantSealedAt(t, f.seam, f.id, id,
		plantRecord(1, ownerTag, a, 10, 10, 0, 32),
		plantRecord(1, ownerTag, a, 11, 11, 0, 32),
		plantRecord(1, ownerTag, b, 12, 12, 0, 32),
		plantRecord(1, ownerTag, b, 13, 13, 0, 32),
	)
	f.rotHeader(t, format.SegmentName(id), int64(offs[1]))
	f.rotHeader(t, format.SegmentName(id), int64(offs[3]))
}

// openAnswer runs the public Open in the background and answers what it
// returned, closing a journal it opened when the test ends. The backstop is
// the assertion that the open does not park: no clock of the pool's can be
// advanced past a wait that never consults one, so a bounded wait on the
// open's own answer is the check, and a minute is far beyond any open of a
// handful of planted records.
func openAnswer(t *testing.T, f *assembledJournal, set func(*Settings)) (*Journal, error) {
	t.Helper()
	type answer struct {
		j   *Journal
		err error
	}
	done := make(chan answer, 1)
	go func() {
		j, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(set)...)
		done <- answer{j, err}
	}()
	select {
	case a := <-done:
		if a.j != nil {
			t.Cleanup(func() {
				if err := a.j.Close(); err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("close the reopened journal: %v", err)
				}
			})
		}
		return a.j, a.err
	case <-time.After(time.Minute):
		t.Fatal("the open parked: it neither served nor refused within a minute of a device that makes no more spares")
		return nil, nil
	}
}

// TestAnOpenOnASpareLessDeviceRefusesWithoutParking holds the loss pass's
// wait where the pool can make no spare at all — the device refuses every
// make, or the ledger already stands at journal.max_bytes: the open owes a
// record that must roll, for no stream holds an active segment when the
// pass runs, so it answers the spare refusal and lands no record rather
// than parking for a spare nothing will make.
func TestAnOpenOnASpareLessDeviceRefusesWithoutParking(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Settings)
		rule *kit.Rule
	}{
		{name: "the device refuses every make", rule: ptr(kit.Failing(kit.OpCreate, "*.spare.tmp", 0, syscall.ENOSPC))},
		// One cap: the planted segment alone fills the ledger to the maximum.
		{name: "the ledger stands at the maximum", set: func(s *Settings) { s.MaxBytes = openSegmentCap }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seededJournal(t)
			file := fileOf(7)
			offs := plantSealedAt(t, f.seam, f.id, 948,
				plantRecord(1, ownerTag, file, 10, 10, 0, 32),
				plantRecord(1, ownerTag, file, 11, 11, 0, 32))
			f.rotHeader(t, format.SegmentName(948), int64(offs[1]))

			if tc.rule != nil {
				if err := f.seam.Schedule(*tc.rule); err != nil {
					t.Fatalf("schedule the spare-less device: %v", err)
				}
			}
			_, err := openAnswer(t, f, tc.set)
			var ns *core.NoSpaceError
			if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
				t.Fatalf("the open with no spare to make returned %v, want the spare refusal", err)
			}
			f.seam.SetFault(nil)
			if n := len(f.lossRecords(t)); n != 0 {
				t.Fatalf("%d loss records on disk after the refused open, want none", n)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestAShortSparedTwoStreamOpenRefusesBeforeAnyRecord holds the wait's count
// where the plan spans two streams and the device makes spares for one roll
// only: the open refuses with the spare refusal before its first record, so
// no record is durable when the open fails, and the retry over a device that
// makes room answers the whole plan.
func TestAShortSparedTwoStreamOpenRefusesBeforeAnyRecord(t *testing.T) {
	f := seededJournal(t)
	plantTwoStreamDrops(t, f, 949)

	// The device makes one spare and refuses every other: one stream's
	// roll, where the plan needs two.
	if err := f.seam.Schedule(kit.FailingFrom(kit.OpCreate, "*.spare.tmp", 2, 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("schedule the one-roll device: %v", err)
	}
	_, err := openAnswer(t, f, nil)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
		t.Fatalf("the two-stream open on a one-roll device returned %v, want the spare refusal", err)
	}
	f.seam.SetFault(nil)
	if n := len(f.lossRecords(t)); n != 0 {
		t.Fatalf("%d loss records on disk after the refused open, want none: the refusal comes before the first record", n)
	}
}

// TestATwoStreamOpenLandsBothRecordsOnTwoRolls holds the landing half: the
// same plan over a device that makes a spare for each stream's roll opens
// and lands both loss records, one per stream.
func TestATwoStreamOpenLandsBothRecordsOnTwoRolls(t *testing.T) {
	f := seededJournal(t)
	plantTwoStreamDrops(t, f, 950)

	if err := f.seam.Schedule(kit.FailingFrom(kit.OpCreate, "*.spare.tmp", 3, 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("schedule the two-roll device: %v", err)
	}
	j, err := openAnswer(t, f, nil)
	if err != nil {
		t.Fatalf("the two-stream open on a two-roll device: %v", err)
	}
	d := j.core.Durability()
	if a, b := d.StreamIndex(fileOf(twoStreamFiles[0])), d.StreamIndex(fileOf(twoStreamFiles[1])); a == b {
		t.Fatalf("the planted files append on streams %d and %d, want two distinct streams", a, b)
	}
	if events := lossEvents(t, j, core.LossCorrupt); len(events) != 2 {
		t.Fatalf("the open's corrupt losses = %+v, want one per stream", events)
	}
	if n := len(f.lossRecords(t)); n != 2 {
		t.Fatalf("%d loss records on disk after the open, want both", n)
	}
}
