//go:build journal_conformance

package conformance

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// Journal-level checks of the failed append's shape: the rows the offloading
// epic owns carry their offload-driven legs, so this file drives the same
// rules through the data path alone.

// TestFailedAppendCarriedOnceResolvedJournalLevel is the journal-level form
// of the recycled segment's failed-append leg (R68): a failed append into
// the stream's active segment, with a synced record beside it and an
// unsynced one left after it, ends that segment with a window owed; once a
// Sync of the file resolves the window the next pass carries the records
// synced into the segment before the failure and retires it, unlinked
// rather than recycled, and the carried bytes read whole before and after
// a crash. The window-open leg itself is not this test's — the pointer at
// the resolution says why.
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
	f.FailNext(kit.OpWrite, segmentName(seg), 1)
	if _, err := tryWrite(t, h, id, off+4*size, distinct(30, 10<<10)); err == nil {
		t.Fatal("the append the fault was set to fail succeeded")
	}
	f.Seam.SetFault(nil)
	got, missing := readBack(t, h, id, off, size)
	if len(missing) != 0 || !bytes.Equal(got, next) {
		t.Fatalf("the write beside the failed append reads missing %+v or other bytes", missing)
	}

	// No decline-while-open leg runs here: the decision at the pass's R5
	// gate (internal/reclaim/pass.go) names the ceiling — the window the
	// failed append opens is the durability layer's, which learns of the
	// ended segment only from the end event its own worker takes, so a
	// pass racing the take is, by its deferred re-read, allowed to reclaim
	// the segment. The pointer replaces the leg the first revision of this
	// test ran, which raced that take.

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
