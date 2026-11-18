package reclaim_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
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
// gate makes: a deferral raised after the carry read its snapshot — the pass
// is blocked in its durability wait, so the raise provably lands after it —
// keeps the source in the set, its loss record still owed, the freed at
// zero.
func TestPassADeferralRaisedInTheGapKeepsTheSource(t *testing.T) {
	p := newPassFixture(t)
	d := p.f.core.Durability()
	file := copyFile(9)
	rec, seqW, vW := p.f.plant(t, copyTag, file, []byte("the write the gap deferral counts beside"), 0)
	src := rec.Segment
	p.f.sealSource(t, file)
	p.retainActivity(t, p.f.core.Streams().IndexFor(file), file)

	hold := p.f.seam.Hold(t, kit.OpSync, "*")
	// A budget of one copied byte runs exactly one unit: whatever the raise
	// in the gap keeps, the pass cannot come back for the source and hide
	// the decision behind a second run.
	done := p.goRepack(context.Background(), 1, src)
	reachHold(t, hold)
	l := durability.Loss{
		Share: copyTag, File: file, Extent: core.Extent{Off: 9 << 10, Len: 64},
		Version: vW, Seq: seqW + 2000, Reason: core.LossStale, Deferred: true, Seg: src,
	}
	if _, err := d.Lose(context.Background(), l); err != nil {
		t.Fatalf("raising the gap deferral: %v", err)
	}
	p.f.core.Liveness().SetDeferred(src, true)

	hold.Release()
	r := awaitRepack(t, done)
	if r.err != nil {
		t.Fatalf("Repack with a gap deferral: %v", r.err)
	}
	if r.freed != 0 {
		t.Fatalf("Repack freed %d, want 0: the source held by the raised deferral must not be handed over", r.freed)
	}
	if !p.f.core.Liveness().Held(src) {
		t.Fatal("the source no longer answers held after the gap deferral, want the raise to keep it")
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
