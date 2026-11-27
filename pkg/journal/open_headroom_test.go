package journal_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
)

// headroomRecordCharge is what one header-only record draws on the headroom:
// a 96-byte header, at most 56 bytes of payload and a 72-byte footer entry.
const headroomRecordCharge = 96 + 56 + 72

// fillToMaximum writes file id through h until the journal refuses a write
// naming its maximum.
func fillToMaximum(t *testing.T, h *journal.Handle, id journal.FileID) {
	t.Helper()
	chunk := make([]byte, 256<<10)
	for off := int64(0); ; off += int64(len(chunk)) {
		if off > 2*journaltest.FixtureMaxBytes {
			t.Fatalf("%d bytes written into a journal of %d were never refused", off, journaltest.FixtureMaxBytes)
		}
		err := journaltest.RetrySpare(func() error {
			_, err := h.WriteAt(t.Context(), id, off, chunk, 0)
			return err
		})
		var ns *journal.NoSpaceError
		if errors.As(err, &ns) && ns.Limit == journal.LimitJournal {
			return
		}
		if err != nil {
			t.Fatalf("WriteAt at %d before the maximum: %v", off, err)
		}
	}
}

// TestOpenWiresTheHeadroom checks, through Stats, that a journal the
// production open built draws a truncate at its maximum on the headroom its
// settings reserve: the draw is counted and HeadroomBytes, the headroom's
// unused reservation, falls by one record's charge. It runs once on the
// journal Init made, once on the same directory reopened through the
// recovery path, the two places the open builds the core.
func TestOpenWiresTheHeadroom(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopen=%v", reopen), func(t *testing.T) {
			f := journaltest.New(t)
			h, id := f.Share(0), journaltest.File(0)
			fillToMaximum(t, h, id)
			if reopen {
				f.Reopen()
				h = f.Share(0)
			}

			st := f.J.Stats()
			before, unused := st.Counters.HeadroomDraws, st.HeadroomBytes
			if _, err := h.Truncate(t.Context(), id, 4096); err != nil {
				t.Fatalf("Truncate at the journal's maximum: %v", err)
			}
			st = f.J.Stats()
			if got := st.Counters.HeadroomDraws; got != before+1 {
				t.Fatalf("HeadroomDraws = %d after a truncate at the maximum, want %d", got, before+1)
			}
			if want := unused - headroomRecordCharge; st.HeadroomBytes != want {
				t.Fatalf("HeadroomBytes = %d after a truncate drew on it, want %d: one record's charge below %d", st.HeadroomBytes, want, unused)
			}
		})
	}
}

// TestRestartResetsTheHeadroomPin pins the headroom ledger's restart
// decision rather than guarding a change: the ledger starts empty at every
// open, so HeadroomBytes after a restart reads what a journal that never
// drew reads, whatever the process before it drew at its maximum. Nothing
// seeds the ledger at open, so it cannot fail as the code stands; it fails
// once the drawing side is made to persist across an open, which is when it
// must be rewritten with that decision.
func TestRestartResetsTheHeadroomPin(t *testing.T) {
	f := journaltest.New(t)
	whole := f.J.Stats().HeadroomBytes
	h, id := f.Share(0), journaltest.File(0)
	fillToMaximum(t, h, id)
	for i := range int64(8) {
		if _, err := h.Deallocate(t.Context(), id, journal.Extent{Off: i * 8192, Len: 4096}); err != nil {
			t.Fatalf("Deallocate %d at the journal's maximum: %v", i, err)
		}
	}
	if got, want := f.J.Stats().HeadroomBytes, whole-8*headroomRecordCharge; got != want {
		t.Fatalf("HeadroomBytes = %d after eight draws, want %d", got, want)
	}
	f.Reopen()
	if got := f.J.Stats().HeadroomBytes; got != whole {
		t.Fatalf("HeadroomBytes = %d after a restart, want %d, the headroom whole", got, whole)
	}
}

// headroomSpentAtTheLimit builds a journal of headroomStreams streams at its
// maximum whose headroom spares the streams' removals rolled into, then
// filled with content to the limit, so the open's early seal of those
// segments returns no tail worth a segment, and returns it with the
// headroom's unused reservation before any draw.
func headroomSpentAtTheLimit(t *testing.T) (f *journaltest.Fixture, whole int64) {
	t.Helper()
	f = journaltest.NewWith(t, func(s *journal.Settings) { s.Streams = headroomStreams })
	whole = f.J.Stats().HeadroomBytes
	h := f.Share(0)
	fillToMaximum(t, h, journaltest.File(0))
	// One removal of each of files spread over every stream, at the
	// maximum: each stream's first rolls into a headroom spare, well above
	// half the headroom.
	for k := range 64 {
		if _, err := h.Deallocate(t.Context(), journaltest.File(1+k), journal.Extent{Off: 0, Len: 4096}); err != nil {
			t.Fatalf("Deallocate of file %d at the journal's maximum: %v", k, err)
		}
	}
	// Content fills what the removals left of every stream's active segment —
	// the headroom spares its rolls took — so the open's early seal of those
	// segments returns no tail worth a segment.
	for _, size := range []int{4096, 64} {
		for k := range 64 {
			id := journaltest.File(1 + k)
			for off := int64(1 << 40); ; off += int64(size) {
				_, err := h.WriteAt(t.Context(), id, off, make([]byte, size), 0)
				var ns *journal.NoSpaceError
				if errors.As(err, &ns) && ns.Limit == journal.LimitJournal {
					break
				}
				if err != nil {
					t.Fatalf("a %d-byte write of file %d at the maximum: %v", size, k, err)
				}
			}
		}
	}
	return f, whole
}

// TestRestartAtTheLimitRefusesTheRollPin pins the ceiling of the ledger's
// restart decision rather than guarding a change: a journal at its maximum
// whose removals rolled into its headroom spares is restarted, the ledger
// starts empty and reads whole, but the spares those rolls took are segments
// now, which the pool cannot make again inside the maximum. Each stream's
// first removal after the restart rolls — a fresh stream holds no active
// segment — and once the spares the open re-adopted are taken the next roll
// finds neither a content nor a headroom spare and is refused naming the
// headroom, though the ledger, which never refused it, reads nearly whole. It must change with the
// decision: once the open recounts the ledger from the headroom spares
// actually ready, the refusal comes from the ledger, or not at all.
func TestRestartAtTheLimitRefusesTheRollPin(t *testing.T) {
	f, whole := headroomSpentAtTheLimit(t)
	f.Reopen()
	if got := f.J.Stats().HeadroomBytes; got != whole {
		t.Fatalf("HeadroomBytes = %d after the restart, want %d, the ledger whole", got, whole)
	}
	// Removals over every stream: each stream rolls on its first, and once
	// the headroom spares left over from before the restart are taken the
	// next roll is refused, a few records into a ledger that reads nearly
	// whole.
	h := f.Share(0)
	for i := int64(0); i < 64; i++ {
		_, err := h.Deallocate(t.Context(), journaltest.File(int(1+i)), journal.Extent{Off: 1<<30 + i*8192, Len: 4096})
		if err == nil {
			continue
		}
		var ns *journal.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != journal.LimitHeadroom {
			t.Fatalf("removal %d after the restart = %v, want admitted or the roll refused naming the headroom", i, err)
		}
		if got := f.J.Stats().HeadroomBytes; got <= whole/2 {
			t.Fatalf("HeadroomBytes = %d at the refusal, want above half of %d: the ledger, not the roll, refused", got, whole)
		}
		return
	}
	t.Fatal("64 removals over every stream after the restart were all admitted, want a roll refused naming the headroom")
}

// TestRestartAtTheLimitKeepsTheRepackReserve checks that a restart at the
// maximum whose directory holds fewer spares than the headroom's target
// re-adopts the repack reserve's spare as the reserve's: repack is the one
// way out at the limit, where the pool can make no spare, and its copies draw
// on that reserve (RFC 1 section 6), so a reserve spare re-adopted as a
// headroom one would leave repack nothing to carry into. A repack pass then
// runs at the limit without a refusal.
func TestRestartAtTheLimitKeepsTheRepackReserve(t *testing.T) {
	f, _ := headroomSpentAtTheLimit(t)
	if got := f.J.Stats().RepackReserveBytes; got != journaltest.FixtureSegmentCap {
		t.Fatalf("RepackReserveBytes = %d before the restart, want %d, the reserve's spare", got, journaltest.FixtureSegmentCap)
	}
	f.Reopen()
	if got := f.J.Stats().RepackReserveBytes; got != journaltest.FixtureSegmentCap {
		t.Fatalf("RepackReserveBytes = %d after the restart at the limit, want %d: the reserve's spare was re-adopted as another class", got, journaltest.FixtureSegmentCap)
	}
	if _, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{}); err != nil {
		t.Fatalf("Repack at the limit after the restart: %v", err)
	}
}

// TestRepackAfterARestartAtTheLimitGivesTheReserveBack checks that, after a
// restart at the maximum with the headroom short, each repack pass gives the
// repack reserve back what its copies spent (RFC 1 sections 6 and 7.2): the
// retirement's freed segment refills the reserve before the headroom, so a
// second pass still has its spare and carries. Each pass frees the three
// quarters of file 0 a round of removals left unheld in half of the file.
func TestRepackAfterARestartAtTheLimitGivesTheReserveBack(t *testing.T) {
	f, _ := headroomSpentAtTheLimit(t)
	f.Reopen()
	h := f.Share(0)
	const chunk = int64(256 << 10)
	half := int64(journaltest.FixtureMaxBytes / 2)
	for pass, from := range []int64{0, half} {
		for off := from; off < from+half; off += 4 * chunk {
			for k := int64(1); k < 4; k++ {
				if _, err := h.Deallocate(t.Context(), journaltest.File(0), journal.Extent{Off: off + k*chunk, Len: chunk}); err != nil {
					t.Fatalf("pass %d: Deallocate at %d: %v", pass, off+k*chunk, err)
				}
			}
		}
		freed, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{})
		if err != nil {
			t.Fatalf("pass %d: Repack at the limit: %v", pass, err)
		}
		if freed <= 0 {
			t.Fatalf("pass %d: Repack freed %d bytes, want the removed three quarters back: it found no reserve spare to carry into", pass, freed)
		}
		// The recycle that refills the reserve lands off the pass's own
		// path; the poll waits on the figure itself, the deadline a backstop.
		for deadline := time.Now().Add(10 * time.Second); f.J.Stats().RepackReserveBytes != journaltest.FixtureSegmentCap; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				st := f.J.Stats()
				t.Fatalf("pass %d: RepackReserveBytes = %d after a pass that freed %d, want %d: the freed segments went to the headroom first", pass, st.RepackReserveBytes, freed, journaltest.FixtureSegmentCap)
			}
		}
	}
}

// headroomStreams is the append streams the per-stream headroom checks open
// with: more than the one segment the fixture's headroom records fill, so a
// headroom sized by its records alone falls short of one spare per stream.
const headroomStreams = 4

// TestOpenMakesAHeadroomSparePerStream checks that the open readies one
// headroom spare per append stream, so every stream can roll once at the
// maximum. The headroom spares are the spares on disk beyond the content
// spares Stats reports and the one repack reserve spare. It runs on
// the journal Init made and on the same directory reopened.
func TestOpenMakesAHeadroomSparePerStream(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopen=%v", reopen), func(t *testing.T) {
			f := journaltest.NewWith(t, func(s *journal.Settings) { s.Streams = headroomStreams })
			if reopen {
				f.Reopen()
			}
			f.WaitSpares()
			if got := headroomSpares(t, f); got != headroomStreams {
				t.Fatalf("headroom spares = %d, want %d: one per stream", got, headroomStreams)
			}
		})
	}
}

// headroomSpares counts the headroom spares on f's disk: the spares beyond
// the content spares Stats reports and the repack reserve's. A made spare is
// renamed into a segment's name, so the spares are the segment files the
// journal counts as no segment.
func headroomSpares(t *testing.T, f *journaltest.Fixture) int {
	t.Helper()
	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	st := f.J.Stats()
	spares := -st.Segments
	for _, name := range names {
		if strings.HasSuffix(name, ".seg") {
			spares++
		}
	}
	return spares - st.Spares - int(st.RepackReserveBytes/journaltest.FixtureSegmentCap)
}

// TestEveryStreamRollsIntoTheHeadroomAtTheMaximum checks, at the caller that
// sees the journal's refusal, that at the maximum a removal on every stream
// is admitted though each stream's active segment is full: each stream's
// roll finds a headroom spare of its own, none refused naming LimitHeadroom.
// The removals' new segments cover every stream, and the headroom spares on
// disk fall by one per stream.
func TestEveryStreamRollsIntoTheHeadroomAtTheMaximum(t *testing.T) {
	f := journaltest.NewWith(t, func(s *journal.Settings) { s.Streams = headroomStreams })
	h := f.Share(0)
	fillToMaximum(t, h, journaltest.File(0))
	// Files enough to land on every stream: each fills its stream's tail
	// until a write is refused at the maximum, so the removals below roll.
	const files = 4 * headroomStreams
	offs := make([]int64, files)
	for _, size := range []int{4096, 64} {
		for k := range files {
			id := journaltest.File(100 + k)
			for {
				_, err := h.WriteAt(t.Context(), id, offs[k], make([]byte, size), 0)
				var ns *journal.NoSpaceError
				if errors.As(err, &ns) && ns.Limit == journal.LimitJournal {
					break
				}
				if err != nil {
					t.Fatalf("a %d-byte write of file %d at the maximum: %v", size, k, err)
				}
				offs[k] += int64(size)
				if offs[k] > journaltest.FixtureMaxBytes {
					t.Fatalf("file %d took %d bytes past the journal's maximum", k, offs[k])
				}
			}
		}
	}
	before := map[journal.SegmentID]bool{}
	for _, seg := range f.J.Inspect().Segments() {
		before[seg.ID] = true
	}
	spares := headroomSpares(t, f)
	for k := range files {
		if _, err := h.Deallocate(t.Context(), journaltest.File(100+k), journal.Extent{Off: 0, Len: 64}); err != nil {
			t.Fatalf("Deallocate of file %d at the journal's maximum: %v, want it admitted on the headroom", k, err)
		}
	}
	rolled := map[int]bool{}
	for _, seg := range f.J.Inspect().Segments() {
		if !before[seg.ID] {
			rolled[int(seg.Stream)] = true
		}
	}
	if len(rolled) != headroomStreams {
		t.Fatalf("the removals rolled on streams %v, want all %d: the files did not cover every stream", rolled, headroomStreams)
	}
	if got := spares - headroomSpares(t, f); got != headroomStreams {
		t.Fatalf("headroom spares fell by %d over the removals, want %d: one per stream", got, headroomStreams)
	}
}
