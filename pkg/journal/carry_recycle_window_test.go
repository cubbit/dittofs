package journal

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestACrashAfterItsSourcesRecycleKeepsTheCarriedRemoval runs the leg's repack
// to completion over a source that retires by the recycle — the full-sealed
// shape the retirer turns into a spare under a fresh id — and crashes once the
// pass has returned, with the whole recycle durable: the rename, the directory
// sync that made it durable, the zeroing of the spare's header and trailer
// slots and the file sync that made them durable, all landed. The crash at that
// point is the crash a power loss lands anywhere inside the recycle's last
// step, for the retirement's own rest — the ledger's move and the pool's take —
// changes no byte on disk: the carried removal record's copy in the stream's
// active segment is its surviving placement, the recycled original unreadable
// under the fresh id (a record header refuses any segment but its own, so the
// rename alone demolishes the original), and the source's bytes are gone for
// the reopen by design.
//
// What the leg pins is the reopen's answer in that window: the carried copy is
// synced but unstamped — no writer-window stamp covers it and no mark names its
// segment — so the tail proof takes it as a candidate, reads its bytes whole
// off the device it was synced to and keeps them: the proof proves a synced
// record by the device's own bytes, never by a stamp, the stamp only bounds the
// records it must read. The rebuild feeds the copy as the removal's one
// placement, the reopened journal serves exactly the one truncate it stands
// for, and the copy's segment errs on the held side — the unsettled removal
// marker keeps it, and the settlement that covers the marker releases it
// through the ordinary reclamation. The leg guards the whole window: a reopen
// that ever judged the unstamped copy never written — or dropped it from the
// rebuild with no placement left to keep — answers zero truncates, no placement
// on disk and no held segment, and fails here.
func TestACrashAfterItsSourcesRecycleKeepsTheCarriedRemoval(t *testing.T) {
	j, th, seam, dir, id, clock, file, _, src, vT := openCarryCycleLeg(t, false)
	defer func() { _ = th.Close() }()

	// The pre-pass ids, for the recycled spare's name: the retire's recycle
	// renames the source under a fresh id no pre-pass name holds.
	namesBefore, err := seam.List()
	if err != nil {
		t.Fatalf("List before the pass: %v", err)
	}
	var maxID uint64
	for _, name := range namesBefore {
		if kind, seg := format.ClassifyName(name); kind == format.NameSegment && uint64(seg) > maxID {
			maxID = uint64(seg)
		}
	}

	// The pool's refill parked at its spare make, so the pass's roll drawing
	// the last ready reserve spare leaves its class below target with nobody
	// able to refill it: wantRecycle answers true at the source's retirement
	// and the retirer takes the recycle rather than the unlink — the refill
	// bumps no incoming count before its make, so the parked make answers
	// nothing the want's ready-plus-incoming read sees.
	spareHold := seam.Hold(t, kit.OpCreate, "*")
	defer spareHold.Release()

	if _, err := j.Repack(t.Context(), carryCycleBudget, RepackScope{Segments: []SegmentID{src}}); err != nil {
		t.Fatalf("the repack over the leg's source: %v", err)
	}
	if _, err := j.core.Set().State(core.SegmentID(src)); err == nil {
		t.Fatalf("the source %d is still in the set after the pass, want its retirement", src)
	}
	namesAfter, err := seam.List()
	if err != nil {
		t.Fatalf("List after the pass: %v", err)
	}
	recycled := false
	for _, name := range namesAfter {
		if kind, seg := format.ClassifyName(name); kind == format.NameSegment && uint64(seg) > maxID {
			recycled = true
		}
	}
	if !recycled {
		t.Fatalf("the source's retirement left no fresh segment id behind (names %v), want the recycle's rename under a fresh id", namesAfter)
	}
	at := carryCycleTruncatePlacements(t, seam, id, file)
	if len(at) != 1 {
		t.Fatalf("the disk holds %d placements of the removal after the pass (%v), want 1: the recycled original is unreadable and the carried copy stands", len(at), at)
	}

	crashDeferralJournal(t, seam, j)

	j2 := reopenDeferralJournal(t, seam, dir, id, clock)
	th2, err := j2.Share(carryCycleTag, 0, Version{})
	if err != nil {
		t.Fatalf("attach the share on the reopened journal: %v", err)
	}
	defer func() { _ = th2.Close() }()

	// The reopened disk keeps the carried copy whole — the proof verified its
	// synced bytes on the device and no phase of the open ends a segment below
	// a record it holds — and the reopened journal serves the removal it
	// stands for.
	at = carryCycleTruncatePlacements(t, seam, id, file)
	if len(at) != 1 {
		t.Fatalf("after the reopen the disk holds %d placements of the removal (%v), want the 1 carried copy the crash kept", len(at), at)
	}
	truncs := 0
	for ch, cerr := range th2.Since(t.Context(), file, Version{}) {
		if cerr != nil {
			t.Fatalf("Since of the reopened journal: %v", cerr)
		}
		if ch.Kind == core.ChangeTruncate && ch.Version == Version(vT) {
			truncs++
		}
	}
	if truncs != 1 {
		t.Fatalf("Since of the reopened file yields %d truncate changes at the removal's version %v, want 1: the removal survived the crash whole", truncs, vT)
	}

	// The reopened hold is honest liveness, never a wedge: the unsettled
	// removal marker keeps the copy's segment, and the settlement that covers
	// the marker releases it into the ordinary reclamation.
	if !j2.core.Liveness().Held(at[0].Segment) {
		t.Fatalf("Held of the copy's segment %d after the reopen = false, want true: the removal's unsettled marker keeps it, errs held", at[0].Segment)
	}
	carryCycleStays(t, j2, at[0].Segment, "held by the removal the crash left this journal")
	th2.Settle(file, vT)
	if j2.core.Liveness().Held(at[0].Segment) {
		t.Fatalf("Held of the copy's segment %d after the settlement = true, want false: the settle's re-test spent the registration that kept it", at[0].Segment)
	}
	carryCycleGone(t, j2, at[0].Segment, "the settle that released the reopened segment")
}
