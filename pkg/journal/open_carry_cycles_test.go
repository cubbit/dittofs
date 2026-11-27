package journal

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// carryCycleBound bounds the waits for a retirement the reopened journal owes:
// the retire worker's ticker runs on real time, one second a tick, which no
// test clock the journal opens with reaches, so the poll is the wait, with
// the bound covering several of its ticks — the soak's own drain walks
// converge the same way.
const carryCycleBound = 20 * time.Second

// carryCycleStay is how long a leg that must NOT see a retirement watches
// the set: the retire worker's own tick is one second of real time, so the
// watch outlives several of them before the assertion reads — the bounded
// way of showing that a segment stays.
const carryCycleStay = 4 * time.Second

// carryCycleBudget is the repack budget each leg's pass runs under: far
// above the one header-only record a unit carries, so no leg's answer is a
// budget's refusal.
const carryCycleBudget = int64(8 * (1 << 20))

// carryCycleTag is the share tag both legs' files live under.
const carryCycleTag = ShareTag(3)

// openCarryCycleLeg opens the deferral-shaped journal and plants the shape
// both crash-window legs crash from: one file with a written extent its own
// journal-Truncate then cuts to nothing — the removal record appended into
// the same segment the write sits in, its marker left unsettled — that
// segment sealed through its own stream, the pool settled behind the seal,
// and the truncate record registered live with the liveness the journal's
// own removal path walks: a sealed, index-empty source whose only live thing
// is the undroppable removal record it holds. The Rec the tracker knows the
// record by is answered whole, its placement's pair read straight off the
// sealed segment's own bytes.
// The leg's seal is its caller's choice: the reopen legs seal early, never
// full — a full seal keeps the segment accounted at its cap and marks it
// sealed full, which hands the retirer the recycle-or-unlink choice of the
// retirement itself: the carry's roll takes the target out of the pool's
// reserve class, and on the rare draw where wantRecycle still sees that
// class short before the refill's reservation lands, the source's retirement
// recycles it — a rename into a spare that touches no file the legs'
// OpRemove holds name, so the park the legs crash at never happens and the
// pass returns whole. Early-sealed the source is never a recycle candidate,
// so its retirement always reaches the unlink both legs park at, whichever
// retirement runs it. The recycle guard asks for the full seal on purpose:
// the recycle is the retirement it crashes inside, and the guard parks the
// pool's refill so the choice is deterministic on the pass's own roll.
func openCarryCycleLeg(t *testing.T, early bool) (j *Journal, th *Handle, seam *kit.Seam, dir string, id JournalID, clock kit.DrivenClock, file FileID, tr reclaim.Rec, src SegmentID, vT Version) {
	t.Helper()
	j, seam, dir, id, clock = openDeferralJournal(t)
	th, err := j.Share(carryCycleTag, 0, Version{})
	if err != nil {
		t.Fatalf("attach the leg's share: %v", err)
	}
	file = NewFileID([16]byte{'c', 'a', 'r', 'r', 'y', '-', 'c', 'y', 'c', 'l', 'e'})
	payload := make([]byte, 4<<10)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if _, err := th.WriteAt(t.Context(), file, 0, payload, 0); err != nil {
		t.Fatalf("the write the truncate cuts: %v", err)
	}
	vT, err = th.Truncate(t.Context(), file, 0)
	if err != nil {
		t.Fatalf("the truncate cutting the write to nothing: %v", err)
	}
	if _, err := th.Sync(t.Context(), file); err != nil {
		t.Fatalf("the sync making the write's record and the truncate's own durable before any pass: %v", err)
	}
	// The segment both records landed in is sealed through the stream itself,
	// early or full by the caller's choice, so it holds nothing beside the
	// dead write and the undroppable removal record: index-empty, the shape
	// a retirement must free.
	if _, err := j.core.Streams().For(core.FileID(file)).Seal(early); err != nil {
		t.Fatalf("sealing the segment holding the removal record: %v", err)
	}
	if err := j.core.WaitSpares(t.Context()); err != nil {
		t.Fatalf("waiting the pool behind the seal: %v", err)
	}
	tr, src = carryCycleTruncateOf(t, seam, id, file, vT)
	if j.core.Liveness().Droppable(tr) {
		t.Fatal("Droppable of the truncate before the pass = true, want false: its marker never settled, so the record stays live for the carry")
	}
	if !j.core.Liveness().Held(core.SegmentID(src)) {
		t.Fatal("Held of the sealed source before the pass = false, want true: nothing but the truncate record's own live registration keeps the index-empty segment")
	}
	if st, err := j.core.Set().State(core.SegmentID(src)); err != nil || st != segment.Sealed {
		t.Fatalf("the segment holding the truncate answers state %v err %v, want Sealed in the set", st, err)
	}
	return j, th, seam, dir, id, clock, file, tr, src, vT
}

// carryCycleTruncateOf answers the one truncate record of file the seam's
// live bytes hold, decoded straight off the disk, with its pair the tracker
// keys the record by — the Rec the live journal registered and the rebuild
// will — and the segment it sits in. It fails the test if the record does
// not sit alone exactly once.
func carryCycleTruncateOf(t *testing.T, seam *kit.Seam, id JournalID, file FileID, vT Version) (reclaim.Rec, core.SegmentID) {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found []reclaim.Rec
	for _, name := range names {
		kind, seg := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		f, err := seam.Open(name)
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		size, err := f.Size()
		if err != nil {
			t.Fatalf("Size of %s: %v", name, err)
		}
		raw := make([]byte, size)
		if _, err := f.ReadAt(raw, 0); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		_ = f.Close()
		// The walk starts past the segment header the file opens with, and
		// stops where the records end: the past-the-last bytes hold a seal
		// marker or a footer, no record.
		for off := format.SegmentHeaderSize; off+format.RecordHeaderSize <= len(raw); {
			h, derr := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], core.JournalID(id), seg, int64(off))
			if derr != nil {
				break
			}
			if h.Kind == format.KindTruncate && h.File == core.FileID(file) {
				found = append(found, reclaim.Rec{
					Kind: format.KindTruncate, File: h.File, Seg: seg, Ext: h.Extent,
					Pair: index.Numbers{Version: h.Version, Seq: h.Seq}, Seq: h.Seq, Tag: h.Tag,
				})
			}
			off += int(h.Size())
		}
	}
	if len(found) != 1 {
		t.Fatalf("the seam holds %d truncate records of the leg's file (%v), want the one the leg planted", len(found), found)
	}
	if found[0].Pair.Version != core.Version(vT) {
		t.Fatalf("the truncate's version on disk = %v, want the version Truncate answered %v", found[0].Pair.Version, vT)
	}
	return found[0], found[0].Seg
}

// carryCycleGone polls the set until s answers absent, failing the test at
// the bound. The retire worker runs on real time, which no event of the
// reopened journal exposes to the test, so the poll is the wait.
func carryCycleGone(t *testing.T, j *Journal, s SegmentID, when string) {
	t.Helper()
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if _, err := j.core.Set().State(core.SegmentID(s)); err != nil {
			return
		}
		if time.Since(start) >= carryCycleBound {
			t.Fatalf("%s: segment %d is still in the set after %v, want the reclamation its own liveness freed to retire it", when, s, carryCycleBound)
			return
		}
	}
}

// carryCycleStays watches the set for the whole bound, failing the test the
// moment s answers absent. The retire worker's ticker is a second of real
// time, so the bound outlives several of its ticks: that is the bounded way
// of showing it never took the segment.
func carryCycleStays(t *testing.T, j *Journal, s SegmentID, when string) {
	t.Helper()
	for start := time.Now(); time.Since(start) < carryCycleStay; time.Sleep(20 * time.Millisecond) {
		if _, err := j.core.Set().State(core.SegmentID(s)); err != nil {
			t.Fatalf("%s: segment %d left the set within %v, want it kept", when, s, carryCycleStay)
			return
		}
	}
}

// carryCycleTruncatePlacements answers every placement the seam's own bytes
// hold of the file's truncate records, decoded straight off the disk: the
// ground truth of how many copies of the removal record stand anywhere, and
// in which segments, whatever the set or the index answer about them.
func carryCycleTruncatePlacements(t *testing.T, seam *kit.Seam, id JournalID, file FileID) []index.RecordAt {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []index.RecordAt
	for _, name := range names {
		kind, segID := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		f, ferr := seam.Open(name)
		if ferr != nil {
			t.Fatalf("Open %s: %v", name, ferr)
		}
		size, serr := f.Size()
		if serr != nil {
			t.Fatalf("Size of %s: %v", name, serr)
		}
		raw := make([]byte, size)
		if _, rerr := f.ReadAt(raw, 0); rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		_ = f.Close()
		// The walk starts past the segment header the file opens with, and
		// stops where the records end: the past-the-last bytes hold a seal
		// marker or a footer, no record.
		for off := format.SegmentHeaderSize; off+format.RecordHeaderSize <= len(raw); {
			h, derr := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], core.JournalID(id), segID, int64(off))
			if derr != nil {
				break
			}
			if h.Kind == format.KindTruncate && h.File == core.FileID(file) {
				out = append(out, index.RecordAt{Segment: segID, Offset: uint32(off)})
			}
			off += int(h.Size())
		}
	}
	return out
}

// carryCycleRequirePlacements asserts that the disk holds exactly one
// placement of the leg's removal record, standing in the source named.
func carryCycleRequirePlacements(t *testing.T, seam *kit.Seam, id JournalID, file FileID, src SegmentID, when string) {
	t.Helper()
	at := carryCycleTruncatePlacements(t, seam, id, file)
	if len(at) != 1 {
		t.Fatalf("%s: the disk holds %d placements of the removal record (%v), want 1: only the original survives", when, len(at), at)
	}
	if at[0].Segment != core.SegmentID(src) {
		t.Fatalf("%s: the surviving placement of the removal record sits in segment %d, want the source %d", when, at[0].Segment, src)
	}
}

// TestAReopenBetweenTheCarryAndTheUnlinkLeavesNoWedge parks one real pass
// at its source's unlink — the carried removal record's copy durable in the
// segment it landed in — and crashes the journal there: the window the
// issue's wedge was said to survive, and the window both placements of the
// record are demonstrably on disk (the leg asserts them at the park and
// after the power loss). Both placements verify at the reopen — the tail
// proof reads the candidate's device bytes, and a synced copy passes — and
// the equal-numbers collapse keeps the greater (segment, offset), this
// run's original, while the losing copy's target reads index-empty,
// answers not held, and the open's ordinary reclamation unlinks it: one
// placement stands on the disk the reopen serves. The safe direction is
// what the leg pins: the reopen errs on the held side, and the reopened
// source is never again wedged — it stays through several of the reopened
// journal's own retire-worker ticks while its marker stands unsettled, and
// the settlement that covers the removal releases the registration and
// retires the source through the ordinary reclamation. The removal itself
// survives the window whole: the reopened journal serves exactly one
// truncate of the file. The two-placement reopen itself — the rebuild
// registering both, the class holding neither while both stand — stays
// pinned where its seam exists: the tracker's RegisterAtOpen transition
// (the reclaim package's unit tests) reads the registrations a rebuild of
// both feeds. The leg guards crash safety — errs held through the window,
// released by the settle — not the hold rule.
func TestAReopenBetweenTheCarryAndTheUnlinkLeavesNoWedge(t *testing.T) {
	j, th, seam, dir, id, clock, file, _, src, vT := openCarryCycleLeg(t, true)
	defer func() { _ = th.Close() }()

	hold := seam.Hold(t, kit.OpRemove, format.SegmentName(core.SegmentID(src)))
	done := make(chan error, 1)
	go func() {
		_, err := j.Repack(t.Context(), carryCycleBudget, RepackScope{Segments: []SegmentID{src}})
		done <- err
	}()
	select {
	case <-hold.Reached():
	case <-time.After(carryCycleBound):
		t.Fatal("the pass never reached its source's retirement, want the carried record's second placement to free the source to retire")
	}
	// The crash lands between the copy's durable placement and the source's
	// unlink acting: both placements of the removal record stand on disk —
	// the original in the source and the carried copy in the target — and
	// the power loss keeps what the pass's sync covered.
	at := carryCycleTruncatePlacements(t, seam, id, file)
	if len(at) != 2 {
		t.Fatalf("the disk holds %d placements of the removal record at the parked retirement (%v), want 2: the original and the carried copy", len(at), at)
	}
	crashDeferralJournal(t, seam, j)
	hold.Release()
	select {
	case err := <-done:
		// The dead journal's abandoned unit answers its own error, which
		// the log alone reads: the reopened journal below owns the shape
		// from here.
		t.Logf("the crashed pass returned on the power-lost journal: %v", err)
	case <-time.After(carryCycleBound):
		t.Fatal("the crashed pass did not return once the hold released, want the unlink's call to fail on the closed journal")
	}
	at = carryCycleTruncatePlacements(t, seam, id, file)
	if len(at) != 2 {
		t.Fatalf("the disk holds %d placements of the removal record after the power loss (%v), want the 2 it held at the park: the sync covered both", len(at), at)
	}

	j2 := reopenDeferralJournal(t, seam, dir, id, clock)
	// Both placements feed the rebuild and both verify — the tail proof reads
	// the copy's device bytes, and a synced copy passes; the stamp it carries
	// only bounds the candidate set. Equal-numbers records collapse to their
	// greater (segment, offset) — here the original's — so the copy's target reads
	// index-empty, answers not held, and the open's ordinary reclamation
	// retires it: one placement stands, errs held. Should the proof stop
	// reading device bytes, the copy drops from the rebuild and this
	// assertion fails with no placement left to hold the source.
	carryCycleRequirePlacements(t, seam, id, file, src, "after the reopen")
	carryCycleHeldSafeAndDrainable(t, j2, file, src, vT)
}

// carryCycleHeldSafeAndDrainable pins the half both crash-window legs share:
// the reopened journal errs on the held side — the removal's registration
// keeps a source that nothing else holds, and the source stays through
// several of the retire worker's own real-time ticks — and the hold is
// honest liveness, never a wedge: the settlement that covers the removal's
// marker re-tests the file, spends the registration, and the source retires
// through the ordinary reclamation the reopened journal runs.
func carryCycleHeldSafeAndDrainable(t *testing.T, j *Journal, file FileID, src SegmentID, vT Version) {
	t.Helper()
	th, err := j.Share(carryCycleTag, 0, Version{})
	if err != nil {
		t.Fatalf("attach the share on the reopened journal: %v", err)
	}
	defer func() { _ = th.Close() }()
	truncs := 0
	for ch, cerr := range th.Since(t.Context(), file, Version{}) {
		if cerr != nil {
			t.Fatalf("Since of the reopened journal: %v", cerr)
		}
		if ch.Kind == core.ChangeTruncate && ch.Version == Version(vT) {
			truncs++
		}
	}
	if truncs != 1 {
		t.Fatalf("Since of the reopened file yields %d truncate changes at the removal's version %v, want 1: the removal survived the crash whole, whichever placement the recovery resolved to", truncs, vT)
	}
	if _, err := j.core.Set().State(core.SegmentID(src)); err != nil {
		t.Fatalf("the source %d is already absent from the reopened set, want it standing: this leg crashes before its unlink could act", src)
	}
	if !j.core.Liveness().Held(core.SegmentID(src)) {
		t.Fatal("Held of the reopened source = false, want true: the removal's registration keeps the sealed, index-empty source — errs held, the safe direction")
	}
	carryCycleStays(t, j, src, "held by the removal the crash left this journal")
	th.Settle(file, vT)
	if j.core.Liveness().Held(core.SegmentID(src)) {
		t.Fatal("Held of the source after the settlement = true, want false: the settle's re-test spent the registration that kept it, which is the whole difference between a hold and a wedge")
	}
	carryCycleGone(t, j, src, "the settle that released the reopened source")
}

// TestACrashThatLosesTheCarriedCopyReholdsItsOriginal parks the pass at the
// carried record's own durability sync and crashes the journal there: the
// copy's bytes never reached the disk, and the crash drops them. The
// rebuild, finding nothing beside the original, registers the original
// alone — a class of one, holding its segment: errs held, the safe
// direction. The re-hold is honest liveness, not a leak: the held source
// stays through several of the retire worker's own ticks, and the
// settlement that covers the removal's marker releases the registration and
// retires the source through the same ordinary reclamation. The leg guards
// that crash direction alone, not the hold rule: one placement is one
// placement under any rule.
func TestACrashThatLosesTheCarriedCopyReholdsItsOriginal(t *testing.T) {
	j, th, seam, dir, id, clock, file, _, src, vT := openCarryCycleLeg(t, true)
	defer func() { _ = th.Close() }()

	hold := seam.Hold(t, kit.OpSync, "*.seg")
	done := make(chan error, 1)
	go func() {
		_, err := j.Repack(t.Context(), carryCycleBudget, RepackScope{Segments: []SegmentID{src}})
		done <- err
	}()
	select {
	case <-hold.Reached():
	case <-time.After(carryCycleBound):
		t.Fatal("the pass never reached its carried record's durability sync, want the unit carrying it there")
	}
	// The crash lands with the copy appended but unsynced: the power loss
	// the kit models drops what no sync covered, and the carried record's
	// bytes go with it. The abandoned unit's failed window sits on the clock
	// the crash stopped; its goroutine leaves on the test context's cancel
	// — and on the crashed storage's closed-file refusals — when the test
	// ends, and the reopened journal below owns the story from the disk up.
	crashDeferralJournal(t, seam, j)
	hold.Release()

	j2 := reopenDeferralJournal(t, seam, dir, id, clock)
	// The disk holds nothing beside the original — the identity the crash
	// left is one placement wide — so the class the rebuild registers is the
	// original alone, holding its source: errs held, the safe direction, and
	// the settlement releases it as the shared half pins.
	carryCycleRequirePlacements(t, seam, id, file, src, "after the reopen that lost the copy")
	carryCycleHeldSafeAndDrainable(t, j2, file, src, vT)
}
