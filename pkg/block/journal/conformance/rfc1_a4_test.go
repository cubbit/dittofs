//go:build journal_conformance

package conformance

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// Journal-level checks of the repack reserve and of the copy's accounting:
// the rows the offloading epic owns carry their release legs, so these drive
// the same rules through overwrites in place, which kill a record exactly as
// a release would.

// liveWrite is one extent a check wrote and expects to read back.
type liveWrite struct {
	id  journal.FileID
	off int64
	p   []byte
}

// wantReadable fails the test unless every write of ws reads back whole
// through h.
func wantReadable(t *testing.T, h *journal.Handle, ws []liveWrite, what string) {
	t.Helper()

	for _, w := range ws {
		got, missing := readBack(t, h, w.id, w.off, int64(len(w.p)))
		if len(missing) != 0 || !bytes.Equal(got, w.p) {
			t.Fatalf("%s: file %v at %d reads missing %+v or other bytes", what, w.id, w.off, missing)
		}
	}
}

// repackPasses seals the segments left idle and runs n passes over scope, each
// within bounds a call, and returns the storage the first pass that freed any
// returned and the storage all of them freed. A pass that finds nothing sealed
// yet is retried while the idle seals may still be landing.
//
// decision: a bounded number of passes, never repackAll's run to the end: a
// pass is free to choose the segments an earlier one filled with its copies,
// which a journal whose live content is most of every segment never runs out
// of, so a run to nothing freed does not end.
func repackPasses(t *testing.T, f *journaltest.Fixture, scope journal.RepackScope, n int) (first, total int64) {
	t.Helper()

	f.Advance(2 * journaltest.IdleSealAge)
	for range n {
		var freed int64
		for !settles(func() bool {
			err := within(t, f, "a repack pass", func() (err error) {
				freed, err = f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, scope)
				return err
			})
			stub(t, "Repack", err)
			if err != nil {
				t.Fatalf("repack: %v", err)
			}
			return freed > 0 || total > 0
		}) {
			t.Fatal("no pass freed anything once the idle seals had time to land")
		}
		if first == 0 {
			first = freed
		}
		total += freed
	}
	return first, total
}

// TestRepackAtCapacityJournalLevel is the journal-level form of
// TestRFC1B_RepackAtCapacity: the journal is filled to its maximum with
// content a third of which an overwrite in place left dead, so no segment is
// empty and nothing content-admissible is left; a pass still runs, returns
// space, and a write the journal refused is admitted after it.
func TestRepackAtCapacityJournalLevel(t *testing.T) {
	f := journaltest.New(t)
	const size = 128 << 10

	var live []liveWrite
	offs := map[int]int64{}
	var refused bool
	for i := 0; !refused && int64(i) < 2*journaltest.FixtureMaxBytes/size; i++ {
		k := i % 2
		id, off := journaltest.File(k), offs[k]
		p := pattern(fmt.Sprint("first", i), size)
		_, err := tryWrite(t, f.Share(0), id, off, p)
		if err == nil && i%3 != 0 {
			// The overwrite in place kills the record just written.
			p = pattern(fmt.Sprint("second", i), size)
			_, err = tryWrite(t, f.Share(0), id, off, p)
		}
		stub(t, "WriteAt", err)
		if err != nil {
			wantNoSpace(t, err, journal.LimitJournal, "a write past the journal's maximum")
			refused = true
			break
		}
		live = append(live, liveWrite{id, off, p})
		offs[k] += size
	}
	if !refused {
		t.Fatalf("writes past the journal's maximum of %d were never refused", journaltest.FixtureMaxBytes)
	}
	atCapacity(t, f.J.Stats())

	// A pass copies no more than the reserve of one segment holds, so the
	// space the write needs comes back over a few of them.
	if freed, _ := repackPasses(t, f, journal.RepackScope{}, 4); freed <= 0 {
		t.Fatal("repack at capacity, over segments a third dead, freed nothing")
	}
	write(t, f.Share(0), journaltest.File(0), 1<<30, pattern("after-repack", size))
	wantReadable(t, f.Share(0), live, "after a repack at capacity")
}

// TestRepackBesideShareAtLimitJournalLevel is the journal-level form of
// TestRFC1B_RepackBesideShareAtLimit: a share filled to its limit
// shares segments with another share's overwritten content. A pass aimed at
// the full share copies its records, none refused by its limit, and the copy
// is accounted to the share when the source goes: the share's figure ends
// where the live bytes put it, never below, so the limit still refuses a
// write that does not fit.
func TestRepackBesideShareAtLimitJournalLevel(t *testing.T) {
	f := journaltest.New(t)
	const (
		size  = 128 << 10
		limit = 8 << 20
		files = 4
	)
	a, b := limitedShare(t, f, limitedA, limit), f.Share(1)

	var aLive []liveWrite
	offs := map[journal.FileID]int64{}
	for i := 0; ; i++ {
		aid := journaltest.File(i % files)
		p := pattern(fmt.Sprint("a", i), size)
		_, err := tryWrite(t, a, aid, offs[aid], p)
		stub(t, "WriteAt", err)
		if err != nil {
			refusal(t, err, "a write past share A's limit", journal.LimitShare, journal.UnitBytes)
			break
		}
		aLive = append(aLive, liveWrite{aid, offs[aid], p})
		offs[aid] += size

		// B's extent beside it is overwritten in place: its first record is
		// dead weight in the segment A's records share.
		bid := journaltest.File(files + i%files)
		write(t, b, bid, offs[bid], pattern(fmt.Sprint("b1", i), size))
		write(t, b, bid, offs[bid], pattern(fmt.Sprint("b2", i), size))
		offs[bid] += size
	}

	before := f.J.Stats().Shares[limitedA].UsedBytes
	if before > limit {
		t.Fatalf("share A uses %d bytes before the pass, past its limit of %d", before, limit)
	}
	if _, freed := repackPasses(t, f, journal.RepackScope{Share: limitedA}, 3); freed <= 0 {
		t.Fatal("a pass aimed at the share at its limit freed nothing: its copies were refused or never made")
	}

	after := f.J.Stats().Shares[limitedA].UsedBytes
	if after > limit {
		t.Fatalf("share A uses %d bytes once the sources are unlinked, past its limit of %d", after, limit)
	}
	var payload int64
	for _, w := range aLive {
		payload += int64(len(w.p))
	}
	if after < payload {
		t.Fatalf("share A uses %d bytes once the sources are unlinked, under the %d bytes of live content it holds: the copies must stand in place of their sources", after, payload)
	}
	wantReadable(t, a, aLive, "after the pass")

	// What the share's figure counts now is what its limit admits against: a
	// write whose record exceeds the room left is refused. A figure that lost
	// the copies' bytes with their sources would let it through.
	_, err := tryWrite(t, a, journaltest.File(0), 1<<30, pattern("past", int(limit-after)+1))
	refusal(t, err, "a write past the share's limit after its sources were unlinked", journal.LimitShare, journal.UnitBytes)
}

// TestRepackUnitsRunOnePerReserve: the repack reserve is one segment
// and a pass is given a budget of three; each call copies no more than the
// reserve held when it began, so the units the budget names run over several
// calls, one reserve at a time, and every one of them completes.
func TestRepackUnitsRunOnePerReserve(t *testing.T) {
	f := journaltest.New(t)
	const size = 128 << 10

	// Four files' records, a third of them dead, fill enough segments for
	// three units' copies to exceed the one segment of reserve.
	var live []liveWrite
	offs := map[int]int64{}
	for i := range 24 {
		k := i % 4
		id, off := journaltest.File(k), offs[k]
		p := pattern(fmt.Sprint("first", i), size)
		write(t, f.Share(0), id, off, p)
		if i%3 != 0 {
			p = pattern(fmt.Sprint("second", i), size)
			write(t, f.Share(0), id, off, p)
		}
		live = append(live, liveWrite{id, off, p})
		offs[k] += size
	}
	f.Advance(2 * journaltest.IdleSealAge)

	var total uint64
	var passes int
	for ; passes < 64; passes++ {
		eventually(t, "the repack reserve was not whole between passes", func() bool {
			return f.J.Stats().RepackReserveBytes == journaltest.FixtureSegmentCap
		})
		reserve := f.J.Stats().RepackReserveBytes
		before := f.J.Stats().Counters.RepackedBytes
		var freed int64
		err := within(t, f, "a repack pass", func() (err error) {
			freed, err = f.J.Repack(t.Context(), 3*journaltest.FixtureSegmentCap, journal.RepackScope{})
			return err
		})
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("repack: %v", err)
		}
		moved := f.J.Stats().Counters.RepackedBytes - before
		if int64(moved) > reserve {
			t.Fatalf("a pass copied %d bytes against a repack reserve of %d, with a budget of %d", moved, reserve, 3*journaltest.FixtureSegmentCap)
		}
		total += moved
		if freed == 0 {
			break
		}
	}
	if total <= uint64(journaltest.FixtureSegmentCap) {
		t.Fatalf("the passes copied %d bytes in all, no more than the one reserve of %d: the check never had a pass to bound", total, journaltest.FixtureSegmentCap)
	}
	wantReadable(t, f.Share(0), live, "after the passes")
}
