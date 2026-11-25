//go:build journal_conformance

package conformance

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// Journal-level checks of the failed append's shape: the offload-driven rows
// of it are driven elsewhere, so this file drives the same rules through the
// data path alone.

// TestALossRecordCarriedFromItsSegmentJournalLevel is the journal-level row of
// the registration a corrupt record's loss files: the loss record it appends
// is registered live where it landed, and a pass over that segment carries the
// record forward rather than failing over a registration that names no place
// and no share. The carry is the class rule's own spend: a carried copy keeps
// both its numbers, so it is the record's same identity, and the source stops
// holding — the pass retires the source while the loss record's marker still
// stands unsettled, its storage back in the pass's own freed figure. The
// caller's Settle, covering the marker, ends the copy's registration, read
// in the index the only way this rig can: the carried copy is the file's one
// surviving index entry before the settle and none after it — for the copy
// rides the stream's active segment, below the idle sealer's threshold, and
// retires nowhere inside this row — and after the settle a single pass finds
// nothing left to free or to fail on.
//
// decision: the row's index-entry reading rides the idle sealer's
// threshold — the copy's segment is the stream's active one and never
// seals inside this row, so the copy's registration is the only entry the
// settle can spend. A sealer change that seals the copy's segment makes
// the row red over a threshold it does not control; re-seed the copy into
// a sealed segment then, never narrow the row.
// The record it lost was a synced one, so its loss record is live while the
// marker it left is unsettled, which nothing does here until the Settle: the
// liveness has a record it reaches without any other segment standing.
func TestALossRecordCarriedFromItsSegmentJournalLevel(t *testing.T) {
	f := journaltest.New(t)
	id := journaltest.File(0)
	const size = 64 << 10
	e := ext(0, size)
	h := f.Share(0)

	// A synced record, corrupted on disk past its checksum: the read that
	// finds the corruption drops it and appends the synced loss record
	// naming it, registered live into the stream's active segment beside the
	// record it lost.
	p := distinct(0, size)
	v := write(t, h, id, 0, p)
	syncFile(t, h, id)
	loc := entryAt(t, f, id, 0).Location
	name := segmentName(loc.Segment)
	data := liveBytes(t, f, name)
	start := bytes.Index(data[loc.Offset:], p)
	if start < 0 {
		t.Fatalf("the payload is not in segment %d past offset %d", loc.Segment, loc.Offset)
	}
	flipAt(t, f, name, loc.Offset+int64(start)+int64(len(p)/2))

	// The read that finds the corruption is the one that publishes the loss
	// record and its registration; the loss event follows the record, so
	// waiting for it waits for both.
	wantExtentMissing(t, h, id, e, "the corrupted read")
	deadline := time.Now().Add(backgroundWait)
	for len(f.J.Stats().Losses) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(f.J.Stats().Losses) == 0 {
		t.Fatal("the corrupted read published no loss event, want the one for the record it dropped")
	}

	// The first pass runs over the segment the loss record holds, sealed
	// idle, and carries it into the segment the stream rolled to. A
	// registration that names neither the record's place nor its share
	// makes this pass fail over the segment for ever, freeing nothing: the
	// carry is the proof the registration names both, and under the class
	// rule the carry spends the source — a carried copy keeps both its
	// numbers, so it is the record's same identity and the source stops
	// holding — which reads in the pass's own freed figure, the source's
	// storage back this early, before any settle covers the marker.
	f.WaitSpares()
	f.Advance(2 * journaltest.IdleSealAge)
	for time.Now().Before(deadline) {
		if s, ok := segmentInfo(f, loc.Segment); ok && s.Sealed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s, ok := segmentInfo(f, loc.Segment); !ok || !s.Sealed {
		t.Fatalf("the segment %d the loss record holds is %v, want it sealed idle for a pass to take", loc.Segment, s)
	}
	freed, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{})
	if err != nil {
		t.Fatalf("the pass over the loss record's segment: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("the carrying pass freed %d bytes, want the damaged source segment's storage: a pass that failed over a registration naming no place or share frees nothing, for it retires nothing", freed)
	}

	// The source is a damaged segment, so its row stays until the journal
	// closes — the damage stays counted — and its retirement reads in
	// place, as the row's Retired bit. The retirement lands on the
	// executor's own worker, a ticker of real time, so the bit is polled
	// for, bounded, never slept to; a carry that freed the source's storage
	// but left its row unretired still answers for the damage nowhere.
	deadline = time.Now().Add(backgroundWait)
	for time.Now().Before(deadline) {
		if s, ok := segmentInfo(f, loc.Segment); ok && s.Retired {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s, ok := segmentInfo(f, loc.Segment); !ok || !s.Retired {
		t.Fatalf("the damaged source segment %d is %v after the carrying pass (in the snapshot: %v), want its row retired with the damage still counted", loc.Segment, s, ok)
	}

	// The caller's Settle, covering the marker the loss record left, ends
	// the record's liveness, and the row reads the ending where the index
	// holds it: the copy's registration is the only entry the file has
	// left — its one written extent the corrupted read dropped — so the
	// count before the settle names the carried copy standing, live
	// without its marker covered, and zero after it names the settle's
	// own spend. The copy's segment itself — the stream's active one,
	// holding no more than the copy's header past its last catalogued
	// point — sits below the idle sealer's threshold and never seals
	// within this row's window, so no retirement of it can be the settle's
	// proof: the storage story ended one phase early, with the damaged
	// source the carrying pass retired above.
	//
	// ExtentCount is journal-wide while this row seeds exactly the one
	// file, so the figure reads the copy's registration alone.
	extBefore := f.J.Stats().ExtentCount
	if extBefore < 1 {
		t.Fatalf("the index holds %d entries before the settle, want the carried copy's registration live in it: nothing else of the file survived the corrupted read's drop", extBefore)
	}
	h.Settle(id, v)
	deadline = time.Now().Add(backgroundWait)
	for time.Now().Before(deadline) {
		if f.J.Stats().ExtentCount == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if extAfter := f.J.Stats().ExtentCount; extAfter != 0 {
		t.Fatalf("the index holds %d entries after the settle, want none: the copy's registration is the liveness the settle was to spend, and a settle that spent nothing leaves it standing", extAfter)
	}

	// What the convergence pass then proves is only that nothing stands
	// for a pass to free or to fail on — it does not prove the liveness
	// ended, which the index count above does; a pass that frees or fails
	// here names a segment something still holds, or a fault to name.
	freed, err = f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{})
	if err != nil {
		t.Fatalf("the repack after the settle: %v", err)
	}
	if freed != 0 {
		t.Fatalf("the repack after the settle freed %d bytes, want none: the source retired with the carrying pass and the copy's segment stands active, so any storage freed after the settle names a segment something still holds", freed)
	}

	// What the loss record names stays lost after the passes: nothing of
	// the corrupted record came back with the copy that carried its loss.
	wantExtentMissing(t, f.Share(0), id, e, "after the passes")
}

// TestFailedAppendCarriedOnceResolvedJournalLevel is the journal-level form
// of the recycled segment's failed-append leg: a failed append into
// the stream's active segment, with a synced record beside it and an
// unsynced one left after it, ends that segment with a window owed; a pass
// that reaches the segment before the end event is taken declines it, once
// a Sync of the file resolves the window the next pass carries the records
// synced into the segment before the failure and retires it, unlinked
// rather than recycled, and the carried bytes read whole before and after
// a crash.
//
// decision: the segment the append fails into is the stream's own active
// one, whose provenance the test does not reach — driving a retired full
// segment to a recycle needs the spare pool's own classes left wanting
// while every make the pool attempts is refused, and the refused makes
// starve the headroom's record draws long before a roll reaches the
// recycled spare. The recycled provenance is the offloading rows' own leg:
// a recovery that read a recycled segment's old catalog would serve its old
// life, a shape their mark-and-release legs hold. Overturned when the pool
// learns to want a recycle a test can name, or when a row's fixture hands
// the pass a spare that came from a recycle.
func TestFailedAppendCarriedOnceResolvedJournalLevel(t *testing.T) {
	// The cap leaves room for the small records around the failed append.
	f := journaltest.NewWithSegmentCap(t, 2*recordCap, nil)
	const (
		size  = 128 << 10 // the synced record beside the failure
		piece = 4096      // the records that fill in beside it
	)
	const off = int64(0)

	h, id := f.Share(0), journaltest.File(0)

	// B, synced, and C, unsynced beside it, fill one segment; the append made
	// to fail after them ends the segment — its slot holds the head of the
	// failed record, which the torn-tail rule cannot step over — so the
	// failed append's window is owed, and C is left unsynced inside it.
	next := distinct(20, size)
	write(t, h, id, off, next)
	seg := segmentOf(t, f, id, off)
	syncFile(t, h, id)
	tail := distinct(60, piece)
	write(t, h, id, off+3*size, tail)
	if at := segmentOf(t, f, id, off+3*size); at != seg {
		t.Fatalf("C was written into segment %d, want segment %d beside B", at, seg)
	}

	// A Sync of the file, started while the segment stands open, stops at
	// the seam on the segment's fsync: with it in flight the failing append
	// below ends the segment, and the end's event — owed from the end
	// itself, recorded before the notifier starts — is stuck behind the
	// parked sync, its window not yet the durability layer's to name.
	parked := f.Seam.Hold(t, kit.OpSync, segmentName(seg))
	syncDone := make(chan error, 1)
	go func() {
		_, serr := h.Sync(t.Context(), id)
		syncDone <- serr
	}()
	select {
	case <-parked.Reached():
	case <-time.After(backgroundWait):
		t.Fatal("the sync beside C did not reach the held fsync of its segment")
	}

	f.FailNext(kit.OpWrite, segmentName(seg), 1)
	if _, err := tryWrite(t, h, id, off+4*size, distinct(30, 10<<10)); err == nil {
		t.Fatal("the append the fault was set to fail succeeded")
	}
	f.Seam.SetFault(nil)
	got, missing := readBack(t, h, id, off, size)
	if len(missing) != 0 || !bytes.Equal(got, next) {
		t.Fatalf("the write beside the failed append reads missing %+v or other bytes", missing)
	}

	// The end's event has not been taken, its window cannot be named yet,
	// and still a pass over the segment declines it: the owed end answers
	// for the window from the moment the segment ends, so the segment stays
	// — the claimed source of records the pass must not copy out from
	// under a window that has yet to open.
	if err := within(t, f, "the pass over the untaken end", func() error {
		_, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{Segments: []journal.SegmentID{seg}})
		return err
	}); err != nil {
		t.Fatalf("the pass over the segment whose end stood untaken: %v", err)
	}
	if _, ok := segmentInfo(f, seg); !ok {
		t.Fatalf("the pass retired segment %d while its end event stood untaken behind the parked sync", seg)
	}

	parked.Release()
	select {
	case err := <-syncDone:
		stub(t, "the parked Sync", err)
	case <-time.After(backgroundWait):
		t.Fatal("the parked sync did not return once the hold released")
	}

	// The file's Sync resolves the failed append's window either way it
	// goes — re-appending C, or failing it out of the index — and the
	// resolution makes the segment a candidate: the pass then carries the
	// synced record forward and retires the segment, unlinked rather than
	// recycled, for a failed one never recycles, whoever retires it.
	h = f.Share(0)
	_, serr := h.Sync(t.Context(), id)
	f.Seam.SetFault(nil)
	stub(t, "Sync", serr)
	if serr != nil && !errors.Is(serr, journal.ErrSyncFailed) {
		t.Fatalf("the Sync that resolved the failed append's window returned %v, want success or a SyncError", serr)
	}
	renames := f.Seam.CallsFor(kit.OpRename, segmentName(seg))
	repackAll(t, f, journal.RepackScope{Segments: []journal.SegmentID{seg}})
	if !retired(f, seg) {
		t.Fatalf("the failed segment %d is still the journal's after the pass that followed its window's resolution, which carried its new record forward", seg)
	}
	if n := f.Seam.CallsFor(kit.OpRename, segmentName(seg)) - renames; n > 0 {
		t.Fatalf("the failed segment %d was renamed %d time(s) into a spare rather than unlinked", seg, n)
	}
	// The carried record reads back whole between the retirement and the
	// crash, through the copy the pass made of it.
	got, missing = readBack(t, h, id, off, size)
	if len(missing) != 0 || !bytes.Equal(got, next) {
		t.Fatalf("the carried write reads missing %+v or other bytes between the retirement and the crash", missing)
	}

	f.Crash()
	h = f.Share(0)
	got, missing = readBack(t, h, id, off, size)
	if len(missing) != 0 || !bytes.Equal(got, next) {
		t.Fatalf("the carried write reads missing %+v or other bytes after a crash on the far side of its carrying pass", missing)
	}
}
