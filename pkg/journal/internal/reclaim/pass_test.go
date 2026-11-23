package reclaim_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestPassMovesAndFrees runs one Repack from the journal's own API over a
// sparse sealed segment: every piece reads through Handle.ReadAt once the
// pass has repointed it, the freed the pass returns is exactly what
// Stats.FreedBytes gained, and the storage in use — UsedBytes less the spares
// at the segment cap — falls by that same figure, never more.
func TestPassMovesAndFrees(t *testing.T) {
	// The idle-seal threshold runs low so the sparse write alone seals the
	// segment once the fixture's clock passes the idle age.
	f := journaltest.NewWith(t, func(s *journal.Settings) { s.IdleSealBytes = 1 << 12 })
	h := f.Share(0)
	file := journal.NewFileID([16]byte{0x11})
	first := bytes.Repeat([]byte{1}, 16<<10)
	second := bytes.Repeat([]byte{2}, 16<<10)
	if _, err := h.WriteAt(t.Context(), file, 0, first, 1); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if _, err := h.WriteAt(t.Context(), file, 512<<10, second, 2); err != nil {
		t.Fatalf("the second write: %v", err)
	}
	f.Advance(2 * journaltest.IdleSealAge)
	// The sealer runs on the journal's own goroutine, so the seal is waited
	// for as the condition itself, the copy family's wait as the backstop.
	var before journal.Stats
	for n := 0; ; n++ {
		before = f.J.Stats()
		if before.Sealed > 0 {
			break
		}
		if time.Duration(n)*5*time.Millisecond >= copyWait {
			t.Fatalf("sealed segments after the idle seal = 0, want the written one sealed within %v", copyWait)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A post-seal write rolls the file's stream a live active segment, so
	// the pass's copies land in it and the freed storage — not a spare the
	// copies turn into a segment — is what the storage in use loses.
	third := bytes.Repeat([]byte{3}, 16<<10)
	if _, err := h.WriteAt(t.Context(), file, 256<<10, third, 3); err != nil {
		t.Fatalf("the post-seal write: %v", err)
	}
	f.WaitSpares()
	before = f.J.Stats()

	freed, err := f.J.Repack(t.Context(), journaltest.FixtureSegmentCap, journal.RepackScope{})
	if err != nil {
		t.Fatalf("Repack: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("Repack freed %d, want the sealed source's storage", freed)
	}
	f.WaitSpares()
	after := f.J.Stats()
	if gained := int64(after.Counters.FreedBytes - before.Counters.FreedBytes); freed != gained {
		t.Fatalf("freed = %d, want it to equal the FreedBytes gain %d the retirer counted", freed, gained)
	}
	inUse := func(s journal.Stats) int64 {
		return s.UsedBytes - int64(s.Spares)*journaltest.FixtureSegmentCap
	}
	if got, want := inUse(after), inUse(before)-freed; got != want {
		t.Fatalf("the storage in use after the pass = %d, want %d: the %d before less the %d freed", got, want, inUse(before), freed)
	}
	if after.Counters.RepackedBytes == 0 {
		t.Fatalf("RepackedBytes after the pass = %d, want the copies' payload bytes", after.Counters.RepackedBytes)
	}
	// Every piece reads through the handle after the pass: the repointed
	// extents answer from the copy, whatever segment the index names now.
	for _, e := range []struct {
		off int64
		p   []byte
	}{
		{0, first}, {512 << 10, second},
	} {
		buf := make([]byte, len(e.p))
		read, missing, _, _, err := h.ReadAt(t.Context(), file, e.off, buf)
		if err != nil {
			t.Fatalf("ReadAt at %d after the pass: %v", e.off, err)
		}
		if read != len(e.p) {
			t.Fatalf("ReadAt at %d read %d of %d bytes, missing %+v", e.off, read, len(e.p), missing)
		}
		if !bytes.Equal(buf, e.p) {
			t.Fatalf("ReadAt at %d returned the wrong bytes after the pass", e.off)
		}
	}
}

// TestPassSplitPlacementRepointsEveryFact plants a run whose copy cannot
// land in one placement — the active target runs out of room, so the run
// yields several Copied facts — and runs one pass over it: after the pass
// every byte of the run reads back whole, so a repoint that moved only the
// first fact's pieces (one per run) would leave the rest serving from a
// segment the pass retired.
func TestPassSplitPlacementRepointsEveryFact(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(4)
	const headLen, tailLen, fillerLen = 24 << 10, 36 << 10, 59 << 10
	const patLen = 11 // every split pattern is eleven bytes long
	head := bytes.Repeat([]byte("split-head!"), headLen/patLen+1)[:headLen]
	tail := bytes.Repeat([]byte("split-tail!"), tailLen/patLen+1)[:tailLen]
	filler := bytes.Repeat([]byte("split-fill!"), fillerLen/patLen+1)[:fillerLen]
	v, err := p.f.counters.Draw()
	if err != nil {
		t.Fatalf("the run's version: %v", err)
	}
	// One run: two adjacent write records of one version and one mtime,
	// beside a filler whose plant fills the stream's next active segment
	// nearly whole.
	p.f.plantAt(t, copyTag, file, core.Extent{Len: headLen}, head, 5, v)
	tailRec, _ := p.f.plantAt(t, copyTag, file, core.Extent{Off: headLen, Len: tailLen}, tail, 5, v)
	src := tailRec.Segment
	p.f.sealSource(t, file)
	// The pool settles before the filler plants, whose roll draws the
	// stream's next active segment.
	p.f.waitSpares(t)
	// The filler's own file keeps the stream's active segment nearly whole,
	// so the pass's run cannot land in one placement.
	fillerFile := passFileOnStream(p.f, p.f.core.Streams().IndexFor(file), file)
	p.f.plantAt(t, copyTag, fillerFile, core.Extent{Off: 1 << 20, Len: fillerLen}, filler, 6, v)
	p.f.waitSpares(t)

	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err != nil {
		t.Fatalf("Repack over the split run: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack over the split run freed %d, want the source retired", r.freed)
	}
	if !p.setGone(src) {
		t.Fatal("the split run's source is still in the set, want it retired by the pass")
	}
	want := append(append([]byte{}, head...), tail...)
	if got := p.readAll(t, file, 0, int64(len(want))); !bytes.Equal(got, want) {
		t.Fatalf("the run's first %d bytes after the pass = %q…, want every byte of both its facts copied whole", len(want), string(got[:64]))
	}
	if got := p.readAll(t, fillerFile, 1<<20, fillerLen); !bytes.Equal(got, filler) {
		t.Fatal("the filler extent after the pass does not read back whole, want the untouched run left where it was")
	}
}

// TestPassDeferredLossHandover plants a deferred loss against a sealed source
// the way an open leaves one standing, holds the first sync at the seam and
// runs a pass: the source is not handed over while the record's sync is
// held, and once the hold lifts, the pass ends the deferral at the durable
// event and retires the source — the registry entry gone, the freed returned
// and counted.
func TestPassDeferredLossHandover(t *testing.T) {
	p := newPassFixture(t)
	d := p.f.core.Durability()
	file := copyFile(7)
	rec, seqW, vW := p.f.plant(t, copyTag, file, []byte("the write the deferred loss counts beside"), 0)
	src := rec.Segment
	p.f.sealSource(t, file)
	// The stream keeps an active segment beyond the sealed source, so the
	// pass's copies and its loss record append into it without a roll and
	// the pool makes nothing behind the pass: the first sync the seam can
	// hold is the pass's own.
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)
	// A deferred loss against the source: the registry entry the copy that
	// retires the source owes its record to, and the deferred count the
	// open raises per entry — planted exactly as an open's own pass leaves
	// them standing.
	l := durability.Loss{
		Share: copyTag, File: file, Extent: core.Extent{Off: 8 << 10, Len: 64},
		Version: vW, Seq: seqW + 1000, Reason: core.LossStale, Deferred: true, Seg: src,
	}
	if _, err := d.Lose(t.Context(), l); err != nil {
		t.Fatalf("planting the deferred loss: %v", err)
	}
	p.f.core.Liveness().SetDeferred(src, true)

	hold := p.f.seam.Hold(t, kit.OpSync, "*")
	done := p.goRepack(context.Background(), copySegCap*8, src)
	reachHold(t, hold)
	freedBefore := p.f.core.Stats().Counters.FreedBytes
	if p.setGone(src) {
		t.Fatal("the source left the set while its loss record's sync was held, want no hand-over before the record is durable")
	}
	if gain := p.freedGain(freedBefore); gain != 0 {
		t.Fatalf("the retirer freed %d while the sync was held, want nothing: the hand-over must not run before the record is durable", gain)
	}

	hold.Release()
	r := awaitRepack(t, done)
	if r.err != nil {
		t.Fatalf("Repack over a deferred loss: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack over a deferred loss freed %d, want the source retired once its record was durable", r.freed)
	}
	if entries := d.DeferredAgainst(src); len(entries) != 0 {
		t.Fatalf("the deferred registry after the pass holds %+v, want the entry ended at the durable event", entries)
	}
	if gain := p.freedGain(freedBefore); int64(gain) != r.freed {
		t.Fatalf("the retirer freed %d, want the %d the pass returned", gain, r.freed)
	}
}

// TestPassADeferralRaisedInTheGapKeepsTheSource pins the re-check the retire
// gate makes and the gate the deferral's end answers through: a deferral
// raised after the carry read its snapshot — the pass is blocked in its
// durability wait, so the raise provably lands after it — keeps the source in
// the set, its loss record still owed, the freed at zero. The raise the pass
// must not touch arrives while the entry the carry did snapshot has already
// left the registry, so only a held deferral whose end refused to fire can
// answer for it.
func TestPassADeferralRaisedInTheGapKeepsTheSource(t *testing.T) {
	p := newPassFixture(t)
	d := p.f.core.Durability()
	tr := p.f.core.Liveness()
	file := copyFile(9)
	rec, seqW, vW := p.f.plant(t, copyTag, file, []byte("the write the gap deferral counts beside"), 0)
	src := rec.Segment
	p.f.sealSource(t, file)
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)
	// One deferred loss stands against the source exactly as an open leaves
	// it: the registry entry, and the deferred count raised beside it.
	a := durability.Loss{
		Share: copyTag, File: file, Extent: core.Extent{Off: 9 << 10, Len: 64},
		Version: vW, Seq: seqW + 1000, Reason: core.LossStale, Deferred: true, Seg: src,
	}
	if _, err := d.Lose(t.Context(), a); err != nil {
		t.Fatalf("planting the standing deferral: %v", err)
	}
	tr.SetDeferred(src, true)

	hold := p.f.seam.Hold(t, kit.OpSync, "*")
	// A budget of one copied byte runs exactly one unit: whatever the raise
	// in the gap keeps, the pass cannot come back for the source and hide
	// the decision behind a second run.
	done := p.goRepack(context.Background(), 1, src)
	reachHold(t, hold)
	// The pass is past the carry's snapshot — its wait is the only thing
	// the seam can hold here — so the gap opens: the standing entry is
	// ended by the registry's own word, its count lowered the way the
	// emptying lowers it, and a second deferral is raised in that same
	// gap. The pass's later end of the entry it snapshotted must refuse —
	// the registry no longer holds it — and a refusal that lowered the
	// count anyway would steal the raise the gap stands on.
	if !d.EndDeferral(src, a) {
		t.Fatal("ending the standing deferral during the gap = false, want the entry to leave")
	}
	tr.SetDeferred(src, false)
	l := durability.Loss{
		Share: copyTag, File: file, Extent: core.Extent{Off: 10 << 10, Len: 64},
		Version: vW, Seq: seqW + 2000, Reason: core.LossStale, Deferred: true, Seg: src,
	}
	if _, err := d.Lose(context.Background(), l); err != nil {
		t.Fatalf("raising the gap deferral: %v", err)
	}
	tr.SetDeferred(src, true)

	hold.Release()
	r := awaitRepack(t, done)
	if r.err != nil {
		t.Fatalf("Repack with a gap deferral: %v", r.err)
	}
	if r.freed != 0 {
		t.Fatalf("Repack freed %d, want 0: the source held by the raised deferral must not be handed over", r.freed)
	}
	if !tr.Held(src) {
		t.Fatal("the source no longer answers held after the gap deferral, want the raise to keep it: the end of the spent entry must not lower the count the raise stands on")
	}
	if entries := d.DeferredAgainst(src); len(entries) != 1 {
		t.Fatalf("the deferred registry after the pass holds %d entries (%+v), want the gap raise standing alone", len(entries), entries)
	} else if entries[0] != l {
		t.Fatalf("the surviving entry = %+v, want the gap raise %+v itself", entries[0], l)
	}
	if p.setGone(src) {
		t.Fatal("the source left the set, want the gap deferral keeping it for the copy of a later pass")
	}
}

// TestPassASkippedSourceGivesWayToTheNextCandidate pins the run's own
// exclusion of a source its unit chose not to hand over: a raise nothing a
// carry can end will lower — so the unit's retire gate refuses and the unit
// stays — must neither spin the pass on a re-selection of the same segment
// nor stop it short of the candidate behind it, and the stayed source's
// claim is released when the run ends, not before.
func TestPassASkippedSourceGivesWayToTheNextCandidate(t *testing.T) {
	p := newPassFixture(t)
	fa, fb := passFilesOnDistinctStreams(p.f)
	sa := p.plantSealed(t, fa, []byte("the write whose source stays held "))
	sb := p.plantSealed(t, fb, []byte("the write whose source gives way"))
	// A raise with no registry entry behind it: nothing a carry ends owes
	// this count a lowering, so the unit over the held source stays.
	p.f.core.Liveness().SetDeferred(sa, true)

	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, sa, sb))
	if r.err != nil {
		t.Fatalf("Repack beside a staying source: %v", r.err)
	}
	if p.setGone(sa) {
		t.Fatal("the held source left the set, want its unit's retire refused")
	}
	if !p.f.core.Liveness().Held(sa) {
		t.Fatal("the staying source no longer answers held, want its raise to outlive the run")
	}
	if !p.setGone(sb) {
		t.Fatal("the next candidate is still in the set, want the pass to run it once the stayed source gave way")
	}
	if r.freed <= 0 {
		t.Fatalf("Repack freed %d, want the next candidate's retirement", r.freed)
	}
	// The run released the stayed source's claim, so a later run can take
	// it — the exclusion lives no longer than the run that excluded.
	sel := p.f.selector()
	if cands := sel.Select(reclaim.Scope{Segments: []core.SegmentID{sa}}, 1); len(cands) == 0 {
		t.Fatal("the stayed source answers no candidate after the run, want its claim released at the run's end")
	} else {
		sel.Done(cands[0].ID)
	}
}

// TestPassContextRefused runs a Repack whose context is done before it
// begins: the pass answers the context's error and changes nothing — no
// record appended, no storage freed, no byte repacked, the sealed source
// still in the set and still claimable.
func TestPassContextRefused(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(5)
	src := p.plantSealed(t, file, []byte("the write the refused pass must not move"))
	before := p.f.core.Stats()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.repack(ctx, copySegCap); !errors.Is(err, context.Canceled) {
		t.Fatalf("Repack with a done context = %v, want the context's error", err)
	}
	after := p.f.core.Stats()
	if after.UsedBytes != before.UsedBytes {
		t.Fatalf("UsedBytes after the refused pass = %d, want the %d it was: the pass must append nothing", after.UsedBytes, before.UsedBytes)
	}
	if after.HeldBytes != before.HeldBytes || after.Counters.RepackedBytes != 0 {
		t.Fatalf("HeldBytes %d→%d and RepackedBytes %d after the refused pass, want nothing moved", before.HeldBytes, after.HeldBytes, after.Counters.RepackedBytes)
	}
	if after.Counters.FreedBytes != before.Counters.FreedBytes {
		t.Fatalf("FreedBytes after the refused pass = %d, want the %d it was", after.Counters.FreedBytes, before.Counters.FreedBytes)
	}
	if p.setGone(src) {
		t.Fatal("the sealed source left the set on a refused pass, want the pass to change nothing")
	}
	sel := p.f.selector()
	cands := sel.Select(reclaim.Scope{}, 1)
	if len(cands) == 0 || cands[0].ID != src {
		t.Fatalf("the sealed source answers no candidate after the refused pass (got %+v), want its claim released", cands)
	}
	sel.Done(cands[0].ID)
}

// TestPassBudgetStops runs a pass whose budget is one byte over two sealed
// sources on distinct streams: the one unit it runs — the lower id of the
// two equals — retires its segment and copies its bytes, and the second
// source stays untouched, so a caller's budget smaller than the work gets
// one whole unit, never two and never half.
func TestPassBudgetStops(t *testing.T) {
	p := newPassFixture(t)
	fa, fb := passFilesOnDistinctStreams(p.f)
	payload := []byte("the write a one-byte budget spends its one unit on")
	sa := p.plantSealed(t, fa, payload)
	sb := p.plantSealed(t, fb, payload)
	first, second := sa, sb
	if second < first {
		first, second = second, first
	}

	r := awaitRepack(t, p.goRepack(context.Background(), 1, first, second))
	if r.err != nil {
		t.Fatalf("Repack on a one-byte budget: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack freed %d, want the one unit's retirement", r.freed)
	}
	if !p.setGone(first) {
		t.Fatal("the lower-id source is still in the set, want the budget's one unit retired")
	}
	if p.setGone(second) {
		t.Fatal("the higher-id source left the set too, want the budget stopping the pass after one unit")
	}
	if got, want := p.f.core.Stats().Counters.RepackedBytes, uint64(len(payload)); got != want {
		t.Fatalf("RepackedBytes after the budgeted pass = %d, want %d: the one unit's copied bytes alone", got, want)
	}
}

// TestPassCancelledBetweenUnitsReturnsAfterTheUnitInFlight cancels a pass
// while its one in-flight unit blocks in its retirement's directory sync:
// the unit completes — its source retired, freed returned — and the pass
// returns the context's error before selecting the second source, which
// stays in the set.
func TestPassCancelledBetweenUnitsReturnsAfterTheUnitInFlight(t *testing.T) {
	p := newPassFixture(t)
	fa, fb := passFilesOnDistinctStreams(p.f)
	sa := p.plantSealed(t, fa, []byte("the write whose unit the cancellation must let finish"))
	sb := p.plantSealed(t, fb, []byte("the write whose unit the cancellation must never start"))
	// The cancelled unit's stream keeps an active segment, so its copies
	// append without a roll and the pool makes nothing: the first directory
	// sync the seam can hold is the retirement's own.
	p.retainActivity(t, p.f.core.Streams().IndexFor(fa), fa)

	hold := p.f.seam.Hold(t, kit.OpSyncDir, "*")
	ctx, cancel := context.WithCancel(context.Background())
	done := p.goRepack(ctx, copySegCap*8, sa, sb)
	reachHold(t, hold)
	cancel()
	hold.Release()
	r := awaitRepack(t, done)
	if r.err == nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Repack cancelled between units = %v (%d freed), want the context's error", r.err, r.freed)
	}
	if r.freed <= 0 {
		t.Fatal("the cancelled pass returned no freed, want the in-flight unit's retirement to complete after the cancellation")
	}
	if !p.setGone(sa) {
		t.Fatal("the in-flight unit's source is still in the set, want its retirement to have completed")
	}
	if p.setGone(sb) {
		t.Fatal("the second source left the set, want the cancellation to stop the pass between units")
	}
}

// TestPassStopJoinedAtClose closes the fixture's core with a pass in flight
// — its unit blocked in its retirement's directory sync — and releases the
// hold: Close returns only after the pass's own stop has interrupted and
// joined it, and the interrupt lands between units with the retirer still
// running — the in-flight unit completed, the source retired and freed, and
// the core closed clean.
func TestPassStopJoinedAtClose(t *testing.T) {
	p := newPassFixture(t)
	fa, _ := passFilesOnDistinctStreams(p.f)
	sa := p.plantSealed(t, fa, []byte("the write whose retire the close must wait out"))
	p.retainActivity(t, p.f.core.Streams().IndexFor(fa), fa)

	hold := p.f.seam.Hold(t, kit.OpSyncDir, "*")
	done := p.goRepack(context.Background(), copySegCap*8, sa)
	reachHold(t, hold)
	closed := make(chan error, 1)
	go func() { closed <- p.f.core.Close() }()
	hold.Release()
	r := awaitRepack(t, done)
	switch {
	case r.err == nil:
	case errors.Is(r.err, core.ErrClosed):
		// The pass's stop interrupts between units; whether it caught the
		// loop's turn before the last candidate left is the close's own
		// race, and either answer ends the pass.
	default:
		t.Fatalf("Repack interrupted by the close = %v, want nil or ErrClosed", r.err)
	}
	if r.freed <= 0 {
		t.Fatal("the interrupted pass freed nothing, want its in-flight unit's retirement completed")
	}
	if !p.setGone(sa) {
		t.Fatal("the in-flight unit's source is still in the set, want its retirement completed before the close joined the pass")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close with a repack in flight: %v, want nil", err)
		}
	case <-time.After(copyWait):
		t.Fatalf("Close did not return within %v of the released retirement, want the pass's stop to join it", copyWait)
	}
}

// TestPassSpanAndCounters runs one pass over a traced core: the recording
// tracer sees exactly one repack_pass span, carrying the pass's segments,
// bytes and freed, and the repacked_bytes counter rises by the same bytes
// the span names.
func TestPassSpanAndCounters(t *testing.T) {
	var mu sync.Mutex
	type recordedSpan struct {
		op    core.SpanOp
		attrs []slog.Attr
		err   error
	}
	var spans []recordedSpan
	tracer := func(ctx context.Context, op core.SpanOp, attrs ...slog.Attr) (context.Context, func(error)) {
		mu.Lock()
		spans = append(spans, recordedSpan{op: op, attrs: slices.Clone(attrs)})
		i := len(spans) - 1
		mu.Unlock()
		return ctx, func(err error) {
			mu.Lock()
			spans[i].err = err
			mu.Unlock()
		}
	}
	p := newTracedPassFixture(t, tracer)
	file := copyFile(0x21)
	payload := bytes.Repeat([]byte("the bytes the span must count"), 32)
	src := p.plantSealed(t, file, payload)

	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err != nil {
		t.Fatalf("Repack the traced core ran: %v", r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	var seen []int
	for i, s := range spans {
		if s.op == core.SpanRepackPass {
			seen = append(seen, i)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("the tracer saw %d repack_pass spans, want exactly one (%+v)", len(seen), spans)
	}
	s := spans[seen[0]]
	if s.err != nil {
		t.Fatalf("the repack_pass span ended with %v, want nil", s.err)
	}
	attr := func(name string) (slog.Value, bool) {
		for _, a := range s.attrs {
			if a.Key == name {
				return a.Value, true
			}
		}
		return slog.Value{}, false
	}
	if v, ok := attr("segments"); !ok || v.Int64() != 1 {
		t.Fatalf("the span's segments attribute = %v (%v), want one unit", v, ok)
	}
	if v, ok := attr("freed"); !ok || v.Int64() != r.freed {
		t.Fatalf("the span's freed attribute = %v (%v), want the %d the pass returned", v, ok, r.freed)
	}
	v, ok := attr("bytes")
	if !ok {
		t.Fatal("the span carries no bytes attribute, want the copies' payload bytes")
	}
	if got, want := v.Uint64(), uint64(len(payload)); got != want {
		t.Fatalf("the span's bytes attribute = %d, want %d: the one unit's copied payload", got, want)
	}
	if got := p.f.core.Stats().Counters.RepackedBytes; got != uint64(len(payload)) {
		t.Fatalf("RepackedBytes after the pass = %d, want %d: the same bytes the span named", got, len(payload))
	}
}

// passHeldEntries returns every held index entry of file, straight from the
// index the pass repoints.
func passHeldEntries(t *testing.T, f *copyFixture, file core.FileID) []index.Entry {
	t.Helper()

	var out []index.Entry
	for id, es := range f.ix.InspectAll() {
		if id != file {
			continue
		}
		for _, e := range es {
			if e.Kind == index.EntryHeld {
				out = append(out, e)
			}
		}
	}
	return out
}

// TestPassAHeldCopySyncPublishesNothing runs a pass whose copies' sync is
// held at the seam and checks the index while it is: no held entry of the
// file names a copy — every one still names the planted source record — and
// the source is neither retired nor freed. Nothing is published before its
// copy is durable; once the sync completes the pass repoints the file
// in one index step and retires the source.
func TestPassAHeldCopySyncPublishesNothing(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(6)
	payload := bytes.Repeat([]byte("held-copy!"), 24)
	rec, _, _ := p.f.plant(t, copyTag, file, payload, 0)
	src := rec.Segment
	p.f.sealSource(t, file)
	// The stream keeps an active target so the copies append without a roll
	// and the pool makes nothing behind the pass: the first sync the seam
	// can hold is the pass's own.
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)

	hold := p.f.seam.Hold(t, kit.OpSync, "*")
	done := p.goRepack(context.Background(), copySegCap*8, src)
	reachHold(t, hold)

	held := passHeldEntries(t, p.f, file)
	if len(held) == 0 {
		t.Fatal("the file holds no held entries after its write, want the planted record")
	}
	for _, e := range held {
		if e.Record != rec {
			t.Fatalf("while the copies' sync was held a held entry of the file named record %+v, want the planted source record %+v: nothing is published before its copy is durable", e.Record, rec)
		}
	}
	if p.setGone(src) {
		t.Fatal("the source left the set while its copies' sync was held, want it claimed and unreclaimed")
	}
	if gain := p.freedGain(0); gain != 0 {
		t.Fatalf("the retirer freed %d while the copies' sync was held, want nothing handed over", gain)
	}

	hold.Release()
	r := awaitRepack(t, done)
	if r.err != nil {
		t.Fatalf("Repack over the held sync once released: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack freed %d, want the source retired once its copies were durable", r.freed)
	}
	held = passHeldEntries(t, p.f, file)
	if len(held) == 0 {
		t.Fatal("the file holds no held entries after the pass, want the copies")
	}
	for _, e := range held {
		if e.Record == rec {
			t.Fatalf("after the pass a held entry still names the source record %+v, want the copy the pass repointed to", rec)
		}
	}
	if got := p.readAll(t, file, 0, int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("the file does not read back whole after the pass")
	}
}

// TestPassAReaderNeverMissesARepointedExtent loops a reader over the file's
// extents while a pass copies it: every read returns the written bytes with
// nothing missing — the old or the new location, never neither — and the two
// goroutines run under the race detector, so an unindexed window between the
// copy's append and the repoint shows as a missed read. The
// reader stops once the pass has answered, and a pass that never reclaims
// under a continuous reader runs out of that answer's bounded wait.
func TestPassAReaderNeverMissesARepointedExtent(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(14)
	const n, pieceLen, patLen = 3, 512, 9
	payload := bytes.Repeat([]byte("race-run!"), n*pieceLen/patLen+1)[:n*pieceLen]
	// One run of several adjacent records, so the pass's repoint moves
	// several pieces of the file in one index step.
	v, err := p.f.counters.Draw()
	if err != nil {
		t.Fatalf("the run's version: %v", err)
	}
	rec, _ := p.f.plantAt(t, copyTag, file, core.Extent{Len: pieceLen}, payload[:pieceLen], 0, v)
	for i := 1; i < n; i++ {
		p.f.plantAt(t, copyTag, file, core.Extent{Off: int64(i) * pieceLen, Len: pieceLen}, payload[i*pieceLen:(i+1)*pieceLen], 0, v)
	}
	src := rec.Segment
	p.f.sealSource(t, file)
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)
	h := p.theShare(t)

	// The pass is parked at its copies' sync, so the reader proves its reads
	// against the source's bytes first, and continues across the repoint the
	// release lets land. Its stop signal is its own channel: the pass's
	// single answer is awaited below.
	hold := p.f.seam.Hold(t, kit.OpSync, "*")
	done := p.goRepack(context.Background(), copySegCap*8, src)
	reachHold(t, hold)
	stopped := make(chan struct{})
	var reads atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopped:
				return
			default:
			}
			buf := make([]byte, len(payload))
			read, missing, _, _, err := p.f.core.ReadAt(context.Background(), h, file, 0, buf)
			if err != nil {
				t.Errorf("ReadAt under the pass: %v", err)
				return
			}
			if int64(read) != int64(len(payload)) || len(missing) != 0 {
				t.Errorf("ReadAt under the pass read %d of %d bytes, missing %+v: a read must see the old or the new location, never neither", read, len(payload), missing)
				return
			}
			if !bytes.Equal(buf, payload) {
				t.Error("ReadAt under the pass returned other bytes than were written")
				return
			}
			reads.Add(1)
		}
	}()
	// The reader turns at least once while the pass is parked: a read that
	// must straddle the repoint starts from inside the hold. Polling the
	// read count with the copy family's wait as the backstop is bounded.
	for n := 0; reads.Load() == 0; n++ {
		if n > int(copyWait/time.Millisecond) {
			t.Fatal("the reader did not read within the wait while the pass was parked, want at least one read before the release")
		}
		time.Sleep(time.Millisecond)
	}
	hold.Release()
	r := awaitRepack(t, done)
	close(stopped)
	wg.Wait()
	if r.err != nil {
		t.Fatalf("Repack under the looping reader: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack under the looping reader freed %d, want the unit to complete within the reader's run", r.freed)
	}
	if !p.setGone(src) {
		t.Fatal("the source is still in the set, want it retired under the reader")
	}
	if got := p.readAll(t, file, 0, int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("the file does not read back whole after the pass")
	}
}

// TestPassAbandonsItsUnitOnAFailedCopyWindow runs a pass whose copies' sync
// fails at the seam: the unit is abandoned whole — nothing repointed, the
// source not retired, freed nothing, the error returned — and the file still
// reads whole from its original record, so a later pass finds the extents
// where the abandoned one left them.
func TestPassAbandonsItsUnitOnAFailedCopyWindow(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(13)
	payload := bytes.Repeat([]byte("abandoned-window-copy!"), 8)
	rec, _, _ := p.f.plant(t, copyTag, file, payload, 0)
	src := rec.Segment
	p.f.sealSource(t, file)
	// The stream keeps a live active target, as the held-sync walks need.
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)

	// Every sync onto every segment file fails, so whichever segment holds
	// the copies when the pass reaches them — the stream may have rolled
	// under the re-append that an earlier failure of the durability worker's
	// own bounded timer resolved — answers the window the pass waits on
	// with its failure. The pool's refills sync their own temporary names,
	// which the fault spares.
	boom := errors.New("the copies' window fails")
	if err := p.f.seam.Schedule(kit.Failing(kit.OpSync, "*.seg", 0, boom)); err != nil {
		t.Fatalf("scheduling the failing sync: %v", err)
	}
	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err == nil || !strings.Contains(r.err.Error(), boom.Error()) {
		t.Fatalf("Repack over a failed copy window = %v, want the window's own error", r.err)
	}
	if r.freed != 0 {
		t.Fatalf("Repack over a failed copy window freed %d, want nothing retired", r.freed)
	}
	if p.setGone(src) {
		t.Fatal("the abandoned unit's source left the set, want the abandoned unit to retire nothing")
	}
	held := passHeldEntries(t, p.f, file)
	if len(held) == 0 {
		t.Fatal("the file holds no held entries after the abandoned window, want the planted record")
	}
	for _, e := range held {
		if e.Record != rec {
			t.Fatalf("after the abandoned window a held entry names record %+v, want the planted source record %+v: nothing is repointed", e.Record, rec)
		}
	}
	if got := p.readAll(t, file, 0, int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("the file does not read back whole after the abandoned window")
	}
	// The abandoned unit handed its copies to their shares' tallies on its
	// way out, so nothing stays held out of them for a source that stays:
	// the copies are the shares' records now, kept out of no limit.
	if held := p.f.core.CopyHeldBytes(); held != 0 {
		t.Fatalf("CopyHeldBytes after the abandoned window = %d, want 0: the abandon hands the copies it counted over", held)
	}
	// A later pass still finds the extents: the abandon cost its copies
	// alone, so the source answers a selection again, its pieces whole where
	// the index holds them. The run that retires it cannot ride this
	// fixture: a stream whose segments failed in a row stays stopped until
	// a later open, and the fault that fails the copies' window does not
	// spare the ones the durability worker's own timer sync leads, so the
	// stream here is stopped whichever window the pass took.
	sel := p.f.selector()
	cands := sel.Select(reclaim.Scope{Segments: []core.SegmentID{src}}, 1)
	if len(cands) == 0 || cands[0].ID != src {
		t.Fatalf("the abandoned source answers no candidate for a later pass (got %+v), want the abandon to cost its copies alone", cands)
	}
	sel.Done(cands[0].ID)
}

// shareGap is the set's settled bytes for the fixture's tag, less the repack
// copies held out, less the share's figure: zero when the figure is the set's
// own, and constant across a pass that counts what it lands. The fixture's
// planted records bypass the tally, so the gap a test starts with is the
// planted bytes and what must hold is that it never moves.
func shareGap(p *passFixture) int64 { return shareGapOf(p, copyTag) }

// shareGapOf is shareGap for the share of tag.
func shareGapOf(p *passFixture, tag core.ShareTag) int64 {
	c := p.f.core
	return c.Set().ShareBytes(tag) - c.CopyHeldBytes() - c.ShareFigure(tag)
}

func (p *passFixture) requireGap(t *testing.T, when string, want int64) {
	t.Helper()
	p.requireGapOf(t, copyTag, when, want)
}

func (p *passFixture) requireGapOf(t *testing.T, tag core.ShareTag, when string, want int64) {
	t.Helper()
	// The set counts a record inside the append and the share's figure takes
	// it a moment later, so a read while the pass's own copy append is
	// between the two sees a gap that closes on its own; the retire worker's
	// removal of a copy's segment lowers the set before the figure in the
	// same way. The pass and the worker run on their own goroutines, so a
	// read from the test can land in either gap. The check
	// polls the real condition, with one generous backstop, so a figure that
	// really missed or over-took bytes never converges and fails here.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := shareGapOf(p, tag)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: set less share figure = %d, want %d: the figure missed or over-took bytes landed outside a write", when, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// tagSegments returns the bytes of tag each segment of the set holds, for a
// test that finds the segment a pass added bytes to by the growth.
func (p *passFixture) tagSegments(tag core.ShareTag) map[core.SegmentID]int64 {
	held := make(map[core.SegmentID]int64)
	for _, row := range p.f.core.Set().Snapshot() {
		if row.Tags[tag] > 0 {
			held[row.ID] = row.Tags[tag]
		}
	}
	return held
}

// retireCopies retires the segment a failed unit's copies of tag landed in,
// the one whose bytes of tag grew most since before, taken ahead of the pass
// (a copy may land in a segment that already held other bytes of the tag, and
// a pass that rolls mid-unit splits its copies over two: either one holds
// copies, and the lower id answers a tie), through the executor the core
// wires, so the set's retired figure rises and the core's hook lowers the
// share figure.
func (p *passFixture) retireCopies(t *testing.T, file core.FileID, tag core.ShareTag, before map[core.SegmentID]int64) {
	t.Helper()
	var seg core.SegmentID
	var most int64
	for id, n := range p.tagSegments(tag) {
		if grew := n - before[id]; grew > most || (grew == most && grew > 0 && id < seg) {
			seg, most = id, grew
		}
	}
	if seg == 0 {
		t.Fatal("no segment grew in bytes of the tag across the pass, want the abandoned copies' segment")
	}
	if _, err := p.f.core.Streams().For(file).Seal(true); err != nil && !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("Seal: %v", err)
	}
	// A segment a failed append ended retires under its own reason; a
	// sealed one, once the seal above has run, under the other.
	reason := segment.NotHeld
	if st, err := p.f.core.Set().State(seg); err == nil && st == segment.Ended {
		reason = segment.ReasonEnded
	}
	if _, err := p.f.core.Retirer().Retire(seg, reason); err != nil {
		t.Fatalf("Retire(%d): %v", seg, err)
	}
}

// TestPassACarryFailureHandsOverAndLeavesNothingHeldOut drives the other
// error path that releases a source with its copies on disk: a removal whose
// bytes the carry reads back rotted fails the unit after Copy landed its
// content, and the unit's way out is the abandon's — the tallies hand the
// copies over, so nothing stays held out, and the source stands for a later
// pass to reach with its records still named where the index holds them.
// The RepointFile error shares this way out by construction; its refusal has
// no real shape in the in-memory index, which only errors on its callers'
// own bugs, so the carry's failure carries the pairing.
func TestPassACarryFailureHandsOverAndLeavesNothingHeldOut(t *testing.T) {
	p := newPassFixture(t)
	file := copyFile(14)
	payload := bytes.Repeat([]byte("a copy beside a rotted removal!"), 4)
	// A write through the core seeds the tag's share figure, so the copies
	// the failed unit lands are measured against a tally that takes them.
	if _, err := p.f.core.WriteAt(t.Context(), p.theShare(t), copyFile(15), 0, make([]byte, 4<<10), 1); err != nil {
		t.Fatalf("the seeding write: %v", err)
	}
	// A live removal beside the content, registered live so the carry must
	// reach it — its bytes then rotted on disk past its checksum.
	// The removal's marker is left unsettled, so the record stays live for
	// the carry to reach: a settled one that outranks nothing would drop at
	// the unit's re-test and never be read.
	rem, _, _, _ := p.f.plantRemoval(t, copyTag, file, format.KindDelete, core.ChangeDelete,
		core.Extent{}, core.Extent{Len: math.MaxInt64}, false)
	rec, _, _ := p.f.plant(t, copyTag, file, payload, 0)
	src := rec.Segment
	if rem.Seg != src {
		t.Fatalf("the removal landed in segment %d, want the content's %d", rem.Seg, src)
	}
	// The fixtures register into their own tracker, but this test drives the
	// core's own pass: the removal joins the liveness registry the pass
	// walks.
	p.f.core.Liveness().Register(rem)
	// Rotted on disk past any decode: the removal's own bytes carry the
	// header the carry must fail to read back.
	f, ferr := p.f.seam.Open(format.SegmentName(src))
	if ferr != nil {
		t.Fatalf("opening segment %d: %v", src, ferr)
	}
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, rem.Loc.Off); err != nil {
		t.Fatalf("reading the removal's first byte: %v", err)
	}
	one[0] ^= 0xff
	if _, err := f.WriteAt(one, rem.Loc.Off); err != nil {
		t.Fatalf("rotting the removal's first byte: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing segment %d: %v", src, err)
	}
	p.f.sealSource(t, file)
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)
	gap, setBefore := shareGap(p), p.f.core.Set().ShareBytes(copyTag)
	segsBefore := p.tagSegments(copyTag)
	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err == nil || !strings.Contains(r.err.Error(), "does not decode or verify") {
		t.Fatalf("Repack over a rotten removal = %v, want the carry's verify failure", r.err)
	}
	if p.setGone(src) {
		t.Fatal("the carry-failed source left the set, want its claim released with the unit abandoned")
	}
	if held := p.f.core.CopyHeldBytes(); held != 0 {
		t.Fatalf("CopyHeldBytes after the carry's failure = %d, want 0: the copies Copy landed before the failure are the share's records, held out of no tally", held)
	}
	// The copies stand in the set under the share's tag and no source will
	// retire to hand them over, so the share's figure takes them: it keeps
	// the gap it had to the set's settled bytes, or the abandoned copies'
	// later retirement lowers the figure by bytes it never held.
	if landed := p.f.core.Set().ShareBytes(copyTag) - setBefore; landed <= 0 {
		t.Fatalf("the failed unit landed %d bytes under the tag, want its copies in the set", landed)
	}
	p.requireGap(t, "with the abandoned copies standing", gap)
	p.retireCopies(t, file, copyTag, segsBefore)
	p.requireGap(t, "after the abandoned copies retired", gap)
	// The content the copies serve is still the index's: the rotten removal
	// published nothing, so the file reads whole from its original record.
	if got := p.readAll(t, file, 0, int64(len(payload))); !bytes.Equal(got, payload) {
		t.Fatal("the file does not read back whole after the carry's failure")
	}
}

// TestPassACarryNoteFailureCountsTheRecordItPlaced fails the carry's note of a
// record that already landed: the index refuses to note a removal record under
// a tag other than the one that owns its file. The copy of the record stands in
// the set under its share's tag with no source to retire for it, so that
// share's figure holds it, standing and after its segment retires, as it does
// for every abandoned unit's records.
func TestPassACarryNoteFailureCountsTheRecordItPlaced(t *testing.T) {
	const rival = core.ShareTag(2)
	p := newPassFixture(t)
	file := copyFile(14)
	payload := bytes.Repeat([]byte("a copy beside a rival removal!"), 4)
	// A write through the rival's own share seeds its figure, so the record
	// the failed unit lands is measured against a tally that takes it.
	rivalShare := p.f.share(t, rival)
	if _, err := p.f.core.WriteAt(t.Context(), rivalShare, copyFile(15), 0, make([]byte, 4<<10), 1); err != nil {
		t.Fatalf("the seeding write: %v", err)
	}
	// The removal sits on disk and in the registry under the rival's tag while
	// the index applied it for the file's own owner.
	rem, _, _, _ := p.f.plantRemovalUnderTags(t, rival, copyTag, file, format.KindDelete, core.ChangeDelete,
		core.Extent{}, core.Extent{Len: math.MaxInt64}, false)
	rec, _, _ := p.f.plant(t, copyTag, file, payload, 0)
	src := rec.Segment
	if rem.Seg != src {
		t.Fatalf("the removal landed in segment %d, want the content's %d", rem.Seg, src)
	}
	p.f.core.Liveness().Register(rem)
	p.f.sealSource(t, file)
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)
	gap := shareGapOf(p, rival)
	segsBefore := p.tagSegments(rival)
	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err == nil || !strings.Contains(r.err.Error(), "noting the carried record") {
		t.Fatalf("Repack over a rival's removal = %v, want the carry's note failure", r.err)
	}
	if sumBytes(p.tagSegments(rival)) <= sumBytes(segsBefore) {
		t.Fatalf("the failed unit landed no bytes under the rival's tag, want its carried record in the set")
	}
	p.requireGapOf(t, rival, "with the carried record standing", gap)
	p.retireCopies(t, file, rival, segsBefore)
	p.requireGapOf(t, rival, "after the carried record retired", gap)
}

func sumBytes(m map[core.SegmentID]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// TestPassACopyFailureCountsTheCopiesItLanded fails Copy itself after its
// first copy landed: a later write of the unit's appends is refused at the seam.
// The copies already in the set are the share's records with no source to
// retire for them, so the share figure holds them, standing and after their
// segment retires.
func TestPassACopyFailureCountsTheCopiesItLanded(t *testing.T) {
	p := newPassFixture(t)
	first := copyFile(16)
	second := passFileOnStream(p.f, p.f.core.Streams().IndexFor(first), first)
	payload := bytes.Repeat([]byte("a copy then a refused append!"), 4)
	if _, err := p.f.core.WriteAt(t.Context(), p.theShare(t), copyFile(18), 0, make([]byte, 4<<10), 1); err != nil {
		t.Fatalf("the seeding write: %v", err)
	}
	p.f.waitSpares(t)
	rec, _, _ := p.f.plant(t, copyTag, first, payload, 0)
	p.f.plant(t, copyTag, second, payload, 0)
	src := rec.Segment
	p.f.sealSource(t, first)
	p.retainActivity(t, p.f.core.Streams().IndexFor(first), first)
	boom := errors.New("the second copy's append is refused")
	if err := p.f.seam.Schedule(kit.FailingFrom(kit.OpWrite, "*.seg", 2, 1, boom)); err != nil {
		t.Fatalf("scheduling the failing write: %v", err)
	}
	gap, setBefore := shareGap(p), p.f.core.Set().ShareBytes(copyTag)
	segsBefore := p.tagSegments(copyTag)
	r := awaitRepack(t, p.goRepack(context.Background(), copySegCap*8, src))
	if r.err == nil || !strings.Contains(r.err.Error(), boom.Error()) {
		t.Fatalf("Repack over a refused append = %v, want the append's own error", r.err)
	}
	if p.setGone(src) {
		t.Fatal("the failed unit's source left the set, want its claim released and the source standing")
	}
	if landed := p.f.core.Set().ShareBytes(copyTag) - setBefore; landed <= 0 {
		t.Fatalf("Copy landed %d bytes under the tag before it failed, want the first file's copies in the set", landed)
	}
	if held := p.f.core.CopyHeldBytes(); held != 0 {
		t.Fatalf("CopyHeldBytes after the failed Copy = %d, want 0", held)
	}
	p.requireGap(t, "with the copies of the failed Copy standing", gap)
	_ = p.f.seam.Schedule()
	p.retireCopies(t, first, copyTag, segsBefore)
	p.requireGap(t, "after the copies of the failed Copy retired", gap)
}

// TestPassFreedWaitsForTheRetiresDirSync holds the retire's directory sync
// at the seam while its unit reclaims a source: the pass has not returned,
// the storage in use has not moved and nothing is freed until that sync
// completes — then freed equals the storage the retirer returned and the
// storage in use falls by it, never before it frees. A read of another file
// completes while the directory sync is held, for reclamation holds nothing
// a reader of another segment waits on.
func TestPassFreedWaitsForTheRetiresDirSync(t *testing.T) {
	p := newPassFixture(t)
	fa, _ := passFilesOnDistinctStreams(p.f)
	payload := []byte("the write whose retirement's directory sync holds the freed")
	src := p.plantSealed(t, fa, payload)
	p.retainActivity(t, p.f.core.Streams().IndexFor(fa), fa)
	// The filler record of the stream's active segment is the other file the
	// held reclamation must not stop a read of.
	var other core.FileID
	var otherOff int64 = 1 << 20
	for b := byte(1); b != 0; b++ {
		if f := copyFile(b); f != fa && p.f.core.Streams().IndexFor(f) == p.f.core.Streams().IndexFor(fa) {
			other = f
			p.f.plantAt(t, copyTag, other, core.Extent{Off: otherOff, Len: 48}, []byte("the other file's bytes, read under the held sync"), 0, core.Version{Counter: 7})
			break
		}
	}
	if other == (core.FileID{}) {
		t.Fatal("no second file on the source's stream to read under the held sync")
	}
	p.f.waitSpares(t)

	inUse := func() int64 {
		s := p.f.core.Stats()
		return s.UsedBytes - int64(s.Spares)*copySegCap
	}
	before := inUse()
	freedBefore := p.f.core.Stats().Counters.FreedBytes

	hold := p.f.seam.Hold(t, kit.OpSyncDir, "*")
	done := p.goRepack(context.Background(), copySegCap*8, src)
	reachHold(t, hold)
	select {
	case r := <-done:
		t.Fatalf("the pass returned (freed %d, err %v) before the retire's directory sync completed", r.freed, r.err)
	default:
	}
	if got := inUse(); got != before {
		t.Fatalf("the storage in use moved %d → %d while the retire's directory sync was held, want it to fall only once the unlink is durable", before, got)
	}
	if gain := p.freedGain(freedBefore); gain != 0 {
		t.Fatalf("the retirer freed %d while its directory sync was held, want the freed to be counted only once the unlink is durable", gain)
	}
	// The other file's extent reads to its end while the directory sync
	// stands held: a reader of another segment waits on nothing the
	// reclamation holds. The bounded wait is the assertion.
	otherRead := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 48)
		read, missing, _, _, err := p.f.core.ReadAt(context.Background(), p.theShare(t), other, otherOff, buf)
		if err != nil || int64(read) != 48 || len(missing) != 0 {
			t.Errorf("the other file's read under the held directory sync: read %d, missing %+v, err %v", read, missing, err)
			otherRead <- nil
			return
		}
		otherRead <- buf
	}()
	var read []byte
	select {
	case read = <-otherRead:
	case <-time.After(copyWait):
		t.Fatal("the other file's read did not return while the retire's directory sync was held, want a reader of another segment to wait on nothing the reclamation holds")
	}
	if read == nil {
		t.Fatal("the other file's read failed under the held directory sync")
	}

	hold.Release()
	r := awaitRepack(t, done)
	if r.err != nil {
		t.Fatalf("Repack over the held directory sync once released: %v", r.err)
	}
	if r.freed <= 0 {
		t.Fatalf("Repack freed %d, want the source's retirement", r.freed)
	}
	if gain := p.freedGain(freedBefore); int64(gain) != r.freed {
		t.Fatalf("the retirer freed %d, want the %d the pass returned", gain, r.freed)
	}
	// The pool's refill behind the recycled-or-unlinked storage settles
	// before the accounting is read.
	p.f.waitSpares(t)
	if got, want := inUse(), before-r.freed; got != want {
		t.Fatalf("the storage in use after the pass = %d, want %d: the %d before less the %d freed, and no sooner than the freed", got, want, before, r.freed)
	}
	srcread := p.readAll(t, fa, 0, int64(len(payload)))
	if !bytes.Equal(srcread, payload) {
		t.Fatal("the repacked file does not read back whole after the pass")
	}
}

// TestPassFreedOmitsANonDurableRetirement runs a pass whose source's
// retirement meets a failing directory sync: the unlink happened — the
// segment left the set and the retirer counted its bytes — but the removal is
// not durable, so the pass's freed answers 0. The freed a pass reports is the
// sum of the retirements the executor reports durable, never the bytes a
// failed sync may still be missing: the next open's scan settles what the
// failed sync left, and no later sync of this process makes it durable.
func TestPassFreedOmitsANonDurableRetirement(t *testing.T) {
	p := newPassFixture(t)
	fa, _ := passFilesOnDistinctStreams(p.f)
	payload := []byte("the write whose retirement's directory sync failed")
	src := p.plantSealed(t, fa, payload)
	p.retainActivity(t, p.f.core.Streams().IndexFor(fa), fa)
	p.f.waitSpares(t)

	if err := p.f.seam.Schedule(kit.FailingFrom(kit.OpSyncDir, "*", 1, 1, syscall.EIO)); err != nil {
		t.Fatalf("schedule the failing directory sync: %v", err)
	}
	freed, err := p.repack(context.Background(), copySegCap*8, src)
	p.f.seam.SetFault(nil)
	if err != nil {
		t.Fatalf("Repack over the retired source: %v; want the retirement done, its failed directory sync a Warn", err)
	}
	if freed != 0 {
		t.Fatalf("Repack freed %d over a retirement whose directory sync failed, want 0: the removal may not have reached the directory", freed)
	}
	if _, serr := p.f.set.State(src); serr == nil {
		t.Fatalf("segment %d is still in the set, want its retirement done under the failed sync", src)
	}
}

// TestPassDeclinesAnOwedEndUntilItsTake runs the pass over a segment whose end
// someone recorded but whose end event its consumer has not taken: the
// selection and the pass's ended re-read both answer the segment window-open,
// so it is declined at once — the fast assertion of the gate, bounded by the
// claim the selector holds to answer rather than a wait inside the pass.
func TestPassDeclinesAnOwedEndUntilItsTake(t *testing.T) {
	p := newPassFixture(t)
	fa, _ := passFilesOnDistinctStreams(p.f)
	payload := bytes.Repeat([]byte("an ended segment oweing its take"), 8)
	src := p.plantSealed(t, fa, payload)
	// The owed end, as a failed write or sync would record it, without its
	// notifier's wait: the take the fixture's worker would run is never
	// armed here, so the flag stands until the takedown below.
	p.f.set.OweEnd(src)

	freed, err := p.repack(context.Background(), copySegCap*8, src)
	if err != nil {
		t.Fatalf("Repack over the owed end: %v; want the decline answered without an error", err)
	}
	if freed != 0 {
		t.Fatalf("Repack over the owed end freed %d, want 0: the unit never ran", freed)
	}
	if p.setGone(src) {
		t.Fatal("the segment left the set over its owed end, want the pass to decline it at the window the flag answers")
	}
	// The take clears the owed end and the very next pass reclaims the
	// segment: the decline never outlives the take.
	p.f.set.TakeEnd(src)
	if freed, err := p.repack(context.Background(), copySegCap*8, src); err != nil {
		t.Fatalf("Repack over the taken end: %v", err)
	} else if freed == 0 || !p.setGone(src) {
		t.Fatalf("Repack over the taken end freed %d with the segment left in the set=%v, want the unit to retire it", freed, !p.setGone(src))
	}
}
