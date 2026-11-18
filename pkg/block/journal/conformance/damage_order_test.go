//go:build journal_conformance

package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// The excuse a fault buys is wide: a fault acts on a seam call, so it excuses
// every record that call carried, and a crash after a repack copy's lost sync
// and the source's unlink may lose the only durable copy without the check
// objecting. These checks pin the journal's own order at the seam instead,
// whatever the faults: they park the later step at a hold and read what the
// device holds at that moment, so an order broken ahead of any crash fails
// here, on its own assertion.

// TestDamage_SuccessorTakenAfterSealSync checks that a segment's successor
// is written to only after the predecessor's seal has returned from every
// sync it makes: a write that rolls is parked at each sync of the sealed
// segment in turn, and no segment other than the one sealing has taken a
// write at any of them. The writes the successor receives once the seal is
// let go are counted, so a roll that wrote nothing cannot pass.
func TestDamage_SuccessorTakenAfterSealSync(t *testing.T) {
	d := newDriven(t, t, "the roll", true)
	d.writeSynced(1, 1, 0, 64<<10)
	// The counts are taken before the roll starts: a successor written between
	// its take and the seal's first sync is as early as one written at the
	// last.
	base := map[string]int{}
	for _, n := range segmentFiles(t, d.f) {
		base[n] = d.f.Seam.CallsFor(kit.OpWrite, n)
	}

	p, seg := rollParked(t, d)
	if p.done == nil {
		t.Fatalf("precondition: the roll's seal ran after the write returned, so no write is parked inside it")
	}

	name := segmentName(seg)
	others := slices.DeleteFunc(slices.Collect(maps.Keys(base)), func(n string) bool { return n == name })
	total := func() (n int) {
		for _, o := range others {
			n += d.f.Seam.CallsFor(kit.OpWrite, o)
		}
		return n
	}

	untouched := func(at string) {
		t.Helper()
		for _, o := range others {
			if got := d.f.Seam.CallsFor(kit.OpWrite, o); got != base[o] {
				t.Fatalf("%s: %s took %d writes before the seal of %s returned from its sync, want none: the successor is taken only after its predecessor's seal sync", at, o, got-base[o], name)
			}
		}
	}

	trailed := func() bool { return trailerAfter(t, d.f, name, seg, indexedEnd(t, "the roll", d.f, seg)) }
	untouched("at the seal's first sync")
	for n := 0; !trailed(); n++ {
		if n == maxSealSyncs {
			t.Fatalf("precondition: no trailer behind the seal marker after %d syncs of %s", n, name)
		}

		p.held = holdNext(d.tb, d.f, kit.OpSync, name, p.held)
		waitAtHold(t, p, "the seal")
		untouched(fmt.Sprintf("at the seal's sync %d", n+2))
	}

	before := total()
	p.held.Release()
	if err := <-p.done; err != nil {
		stub(t, "WriteAt", err)
		t.Fatalf("the write that rolled, once its seal was let go: %v", err)
	}

	if got := total(); got == before {
		t.Fatalf("the segments other than %s took no write once the seal was let go, so the order was checked against a roll that wrote no successor", name)
	}
}

// TestDamage_OpenUnlinksAfterItsCopiesAreDurable checks that an open ending a
// segment unlinks it only after the copies it made are on the device: the
// open's unlink of the ended segment is parked, and the bytes of the record
// moved out of it are in the durable image of a segment that is not it. The
// open's own sync of the unproven tail is dropped, so the record the open
// verifies and re-appends is the one the copy carries.
func TestDamage_OpenUnlinksAfterItsCopiesAreDurable(t *testing.T) {
	// Unwatched: a kill leaves the dead journal unclosed (see TryKill).
	f := journaltest.NewUnwatched(t)
	writeSynced(t, f, 1, 1, 0, 64<<10)
	u := writeUnproven(t, f)

	holdsCopy := func() bool {
		for _, n := range segmentFiles(t, f) {
			if n != u.name && bytes.Contains(durable(t, f, n), u.a.p) {
				return true
			}
		}
		return false
	}
	if holdsCopy() {
		t.Fatalf("precondition: a segment other than %s holds record A durably before the open", u.name)
	}

	f.Drop(kit.OpSync, u.name)
	syncs := f.Seam.CallsFor(kit.OpSync, u.name)
	unlink := f.Seam.Hold(t, kit.OpRemove, u.name)
	done := make(chan error, 1)
	go func() { done <- f.TryKill() }()
	select {
	case <-unlink.Reached():
	case err := <-done:
		stub(t, "Open", err)
		t.Fatalf("the open returned (%v) without unlinking the ended segment %s", err, u.name)
	}

	if f.Seam.CallsFor(kit.OpSync, u.name) == syncs {
		t.Fatalf("the open made no sync of %s for the fault to drop: the leg injected nothing", u.name)
	}

	if !holdsCopy() {
		t.Fatalf("the open is at the unlink of %s and no other segment holds record A's bytes durably: an ended segment is unlinked only after the sync covering its copies", u.name)
	}

	unlink.Release()
	if err := <-done; err != nil {
		t.Fatalf("the open after the kill: %v", err)
	}

	f.Seam.SetFault(nil)
}

// TestDamage_RefusedOpenNamesAndServesNothing checks, for each class of open
// the journal refuses over a directory as it stands, that the refusal names
// what it refused, serves no journal and writes nothing to the directory: a
// lost synced tail, a range that names no record, and a read that no bounded attempt completes. Each leg asserts
// the class through the error's identity. The unreadable read also asserts the
// typed ranges; the other two refusals carry no typed value, so those legs
// match the whole phrase naming the segment and the range or mark.
func TestDamage_RefusedOpenNamesAndServesNothing(t *testing.T) {
	nothingServed := func(t *testing.T, f *journaltest.Fixture, writes int) {
		t.Helper()
		if f.J != nil {
			t.Fatalf("the refused open left a journal open")
		}

		if got := changingCalls(f); got != writes {
			t.Fatalf("the refused open made %d changing calls on the directory, want none beyond the %d before it", got-writes, writes)
		}
	}

	t.Run("lost_synced_tail", func(t *testing.T) {
		f := journaltest.NewUnwatched(t)
		a := writeRec(t, f, 0, 0, 0, 64<<10)
		loc := entryAt(t, f, a.id, 0).Location
		seg := loc.Segment
		f.Drop(kit.OpSync, "*.seg")
		syncs := f.Seam.CallsFor(kit.OpSync, segmentName(seg))
		marks, err := f.Share(0).Sync(t.Context(), a.id)
		stub(t, "Sync", err)
		if err != nil || len(marks) == 0 {
			t.Fatalf("the sync whose bytes the device lost returned %d marks and %v, want its marks and no error", len(marks), err)
		}

		if f.Seam.CallsFor(kit.OpSync, segmentName(seg)) == syncs {
			t.Fatalf("the sync made no call of %s for the fault to drop: the leg injected nothing", segmentName(seg))
		}

		f.Seam.SetFault(nil)
		f.Marks = marks
		if err := f.Seam.Crash(); err != nil {
			t.Fatalf("crash: %v", err)
		}

		f.Abandon()
		writes := changingCalls(f)
		err = f.TryReopen()
		stub(t, "Open", err)
		if !errors.Is(err, journal.ErrOpenRefused) {
			t.Fatalf("the open over a synced tail the device lost returned %v, want ErrOpenRefused", err)
		}

		// The segment's scan refuses the range below the acknowledged mark.
		if want := fmt.Sprintf("segment %d: a refused range at [%d,", seg, loc.Offset); !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "below a mark's Through") {
			t.Fatalf("the refusal is %q, want it to name segment %d and a range below the mark's Through", err, seg)
		}

		nothingServed(t, f, writes)
	})

	t.Run("range_naming_no_record", func(t *testing.T) {
		f := journaltest.NewUnwatched(t)
		const size = journaltest.FixtureSegmentCap - 400
		a := writeSynced(t, f, 0, 0, 0, size)
		writeSynced(t, f, 0, 0, size, 64<<10)
		loc := entryAt(t, f, a.id, 0).Location
		name := segmentName(loc.Segment)
		f.Shutdown()
		// A sealed segment scanned for want of a readable trailer, whose only
		// record's header does not verify: the range names no record.
		// The refused range is the one record: its header, the 8-byte write
		// time and the payload, from where the scan starts.
		end := loc.Offset + raw.RecordHeaderSize + 8 + size
		flipBit(t, f, name, loc.Offset+92, 0)
		flipBit(t, f, name, int64(len(durable(t, f, name)))-raw.TrailerSize, 0)
		writes := changingCalls(f)
		err := f.TryReopen()
		stub(t, "Open", err)
		if !errors.Is(err, journal.ErrOpenRefused) {
			t.Fatalf("the open over a range that names no record returned %v, want ErrOpenRefused", err)
		}

		if want := fmt.Sprintf("segment %d: a refused range at [%d,%d) that names no record", loc.Segment, loc.Offset, end); !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal is %q, want it to contain %q", err, want)
		}

		nothingServed(t, f, writes)
	})

	t.Run("read_that_fails", func(t *testing.T) {
		f := journaltest.NewUnwatched(t)
		a := writeSynced(t, f, 0, 0, 0, 64<<10)
		seg := entryAt(t, f, a.id, 0).Location.Segment
		name := segmentName(seg)
		f.Shutdown()
		if err := f.Seam.Schedule(kit.Failing(kit.OpRead, name, 0, syscall.EIO)); err != nil {
			t.Fatalf("schedule the failing reads: %v", err)
		}

		writes, reads := changingCalls(f), f.Seam.CallsFor(kit.OpRead, name)
		err := f.TryReopen()
		stub(t, "Open", err)
		var ure *journal.UnreadableRangesError
		if !errors.As(err, &ure) || !errors.Is(err, journal.ErrUnreadable) {
			t.Fatalf("the open over a segment no read completes returned %v, want an UnreadableRangesError matching ErrUnreadable", err)
		}

		if !slices.ContainsFunc(ure.Ranges, func(r journal.SegmentRange) bool { return r.Segment == seg && r.Len > 0 }) {
			t.Fatalf("the refusal names the ranges %+v, want one of segment %d", ure.Ranges, seg)
		}

		if f.Seam.CallsFor(kit.OpRead, name) == reads {
			t.Fatalf("the open read %s no time for the fault to fail: the leg injected nothing", name)
		}

		nothingServed(t, f, writes)
	})
}

// changingCalls counts the calls on the directory that change it, over every
// file.
func changingCalls(f *journaltest.Fixture) int {
	n := 0
	for _, op := range changingOps {
		n += f.Seam.Calls(op)
	}
	return n
}
