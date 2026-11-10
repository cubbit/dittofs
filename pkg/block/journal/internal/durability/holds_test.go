package durability

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// reappendTogether publishes a write of each file at offset 0, so the writes
// share one segment, seals that segment, and appends each write again under
// its own number and version, the index moved to the second copy, as reappend
// does for one write. Each copy the table can name sits in the one segment.
func (cr *corruptRig) reappendTogether(t *testing.T, files ...core.FileID) []reappended {
	t.Helper()
	type first struct {
		f       core.FileID
		payload string
		orig    segment.Location
		v       core.Version
		seq     core.Seq
		kept    uint32
	}
	var firsts []first
	for i, f := range files {
		payload := strings.Repeat(string(rune('a'+i)), 4)
		orig, v := cr.publish(t, f, 0, payload)
		firsts = append(firsts, first{f, payload, orig, v, core.Seq(cr.lastSeq.Load()), cr.keptChecksum(t, orig)})
	}
	seg := firsts[0].orig.Segment
	for _, w := range firsts {
		if w.orig.Segment != seg {
			t.Fatalf("the writes landed in segments %d and %d, want one", seg, w.orig.Segment)
		}
	}
	// The appends after the seal roll into a spare the pool must have ready.
	cr.settlePool(t)
	if _, err := cr.streams.For(files[0]).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	eventually(t, "the sealed records' entries to complete", func() bool {
		return !slices.ContainsFunc(firsts, func(w first) bool { return cr.inLedger(w.orig) })
	})

	out := make([]reappended, 0, len(firsts))
	for _, w := range firsts {
		x := core.Extent{Off: 0, Len: int64(len(w.payload))}
		unlock := cr.domains.Lock(w.f)
		p, err := cr.streams.For(w.f).Append(format.RecordHeader{
			Kind: format.KindWrite, File: w.f, Extent: x, Seq: w.seq, Version: w.v, Tag: 1,
		}, format.RecordBody{Payload: []byte(w.payload), MTime: 1})
		if err != nil {
			unlock()
			t.Fatalf("Append of the second copy: %v", err)
		}
		from := index.RecordAt{Segment: w.orig.Segment, Offset: uint32(w.orig.Off)}
		to := index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}
		moved, _, err := cr.ix.RepointRecord(w.f, from, to)
		unlock()
		if err != nil || moved == 0 {
			t.Fatalf("RepointRecord = %d, %v, want the record moved", moved, err)
		}
		if p.Loc.Segment == seg {
			t.Fatalf("the second copy landed in the first copy's segment %d", seg)
		}
		out = append(out, reappended{orig: w.orig, again: p.Loc, kept: w.kept, c: Corruption{
			Share: 1, File: w.f, Record: to, Seq: w.seq, Version: w.v, Extent: x,
		}})
	}
	return out
}

// register puts each write's first copy in the copy table.
func (cr *corruptRig) register(rs ...reappended) {
	for _, r := range rs {
		cr.d.RegisterCopy(r.c.Seq, r.orig, r.kept)
	}
}

// useCopy calls Corrupt for r and fails the test unless it serves r's copy.
func (cr *corruptRig) useCopy(t *testing.T, r reappended) {
	t.Helper()
	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
	}
}

// holdView is what a test sees of a segment's hold.
type holdView struct {
	seqs           []core.Seq
	claimed, owed  bool
	withheld, held bool
}

// holdOf returns the hold on seg; held is false when there is none.
func (r *rig) holdOf(seg core.SegmentID) holdView {
	st := r.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.holds[seg]
	if !ok {
		return holdView{}
	}
	v := holdView{claimed: h.claimed, owed: h.owed, withheld: h.withheld, held: true}
	for seq := range h.records {
		v.seqs = append(v.seqs, seq)
	}
	slices.Sort(v.seqs)
	return v
}

// noResolved fails the test if a segment has been announced resolved.
func (cr *corruptRig) noResolved(t *testing.T) {
	t.Helper()
	select {
	case seg := <-cr.resolved:
		t.Fatalf("segment %d was announced resolved, want no announcement", seg)
	default:
	}
}

// tickUntil moves the clock past the sync bound until cond holds, so the
// worker's timer fires: the worker arms it after its pass, and a move made
// before it is armed is not seen by it.
func (cr *corruptRig) tickUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventually(t, what, func() bool {
		cr.clock.Advance(2 * core.SyncBound)
		return cond()
	})
}

// passes counts the worker's steps over the held segments.
type passes struct{ n atomic.Int64 }

func (p *passes) count() { p.n.Add(1) }

// after ticks until the worker has finished two more steps than it had, so at
// least one whole step began after the call.
func (p *passes) after(t *testing.T, cr *corruptRig) {
	t.Helper()
	from := p.n.Load()
	cr.tickUntil(t, "two more passes", func() bool { return p.n.Load() >= from+2 })
}

// afterWaking is after with the worker also woken each time, for a test that
// does not rest on what arms its timer.
func (p *passes) afterWaking(t *testing.T, cr *corruptRig) {
	t.Helper()
	from := p.n.Load()
	cr.tickUntil(t, "two more passes", func() bool {
		cr.d.made[0].wake()
		return p.n.Load() >= from+2
	})
}

// landed is one call of the re-append double, with what the index named when
// it ran.
type landed struct {
	c ownedCopy
	// atRecord and atCopy say whether the index named the record that did not
	// verify, and the copy.
	atRecord, atCopy bool
	// domainHeld says the record's file append domain was held when it ran.
	domainHeld bool
}

// landing is a re-append double. A landing moves the index from the copy to
// an untracked place, as the layer's re-append moves it to the new place, and
// reports that place, which the hold reads complete. Without block, every
// call lands at once.
// With block, every call is reported on calls and returns what land hands it,
// so a test sees the worker between two landings.
type landing struct {
	ix      *index.Index
	domains *testDomains
	block   bool

	mu  sync.Mutex
	got []landed
	// to is where each landing moved the index, by sequence number.
	to map[core.Seq]index.RecordAt

	calls   chan ownedCopy
	proceed chan error
	stop    chan struct{}
	once    sync.Once
}

func newLanding(block bool) *landing {
	return &landing{block: block, calls: make(chan ownedCopy, 16), proceed: make(chan error), stop: make(chan struct{})}
}

// install puts the double on the layer, before it starts.
func (l *landing) install(cr *corruptRig) {
	l.ix, l.domains = cr.ix, cr.domains
	cr.d.reappend = l.reappend
}

// untrackedSeg is the segment of the places the re-append double reports:
// none that a ledger, window or lost set names, so the hold reads each
// complete as soon as it is returned.
const untrackedSeg = core.SegmentID(1 << 40)

// moveOff moves the index from c's copy to an untracked place of its own,
// under the file's domain, and returns that place.
func (l *landing) moveOff(c ownedCopy) (segment.Location, error) {
	l.mu.Lock()
	n := len(l.got)
	l.mu.Unlock()
	to := segment.Location{Segment: untrackedSeg, Off: int64(n) * 4096, Len: c.Copy.Len}
	from := index.RecordAt{Segment: c.Copy.Segment, Offset: uint32(c.Copy.Off)}
	unlock := l.domains.Lock(c.File)
	defer unlock()
	if l.ix.Names(c.File, from) {
		if _, _, err := l.ix.RepointRecord(c.File, from, placeAt(to)); err != nil {
			return segment.Location{}, err
		}
	}
	l.mu.Lock()
	if l.to == nil {
		l.to = map[core.Seq]index.RecordAt{}
	}
	l.to[c.Seq] = placeAt(to)
	l.mu.Unlock()
	return to, nil
}

// movedTo returns where the last landing of seq moved the index.
func (l *landing) movedTo(seq core.Seq) index.RecordAt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.to[seq]
}

func (l *landing) reappend(_ *stream, c ownedCopy) (segment.Location, error) {
	at := index.RecordAt{Segment: c.Copy.Segment, Offset: uint32(c.Copy.Off)}
	l.mu.Lock()
	l.got = append(l.got, landed{c: c, atRecord: l.ix.Names(c.File, c.Record), atCopy: l.ix.Names(c.File, at), domainHeld: l.domains.heldBy(c.File)})
	l.mu.Unlock()
	if !l.block {
		return l.moveOff(c)
	}
	select {
	case l.calls <- c:
	case <-l.stop:
		return segment.Location{}, errors.New("the test ended")
	}
	select {
	case err := <-l.proceed:
		if err != nil {
			return segment.Location{}, err
		}
		return l.moveOff(c)
	case <-l.stop:
		return segment.Location{}, errors.New("the test ended")
	}
}

// close lets every waiting call return; registered as a cleanup after the
// rig's, so it runs before the layer's Stop.
func (l *landing) close() { l.once.Do(func() { close(l.stop) }) }

// all returns the calls so far.
func (l *landing) all() []landed {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

// next waits for the worker to call the double.
func (l *landing) next(t *testing.T) ownedCopy {
	t.Helper()
	select {
	case c := <-l.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never re-appended")
		return ownedCopy{}
	}
}

// nextTicking waits for the worker to call the double, moving the clock past
// the sync bound meanwhile.
func (l *landing) nextTicking(t *testing.T, cr *corruptRig) ownedCopy {
	t.Helper()
	var c ownedCopy
	cr.tickUntil(t, "a re-append", func() bool {
		select {
		case c = <-l.calls:
			return true
		default:
			return false
		}
	})
	return c
}

// land returns err from the call the double is waiting in.
func (l *landing) land(err error) { l.proceed <- err }

// copyAt is where the index names r's copy.
func copyAt(r reappended) index.RecordAt {
	return index.RecordAt{Segment: r.orig.Segment, Offset: uint32(r.orig.Off)}
}

// wantRetirable fails unless a retirement can claim seg, and lets the claim
// go again.
func (r *rig) wantRetirable(t *testing.T, seg core.SegmentID) {
	t.Helper()
	if _, _, err := r.set.ClaimRetirement(seg); err != nil {
		t.Fatalf("ClaimRetirement(%d) = %v, want the claim released", seg, err)
	}
	r.set.Release(seg)
}

// TestTwoCopiesOneSegmentResolvesWhenBothLanded covers two records served from
// copies in one segment: the segment stays claimed and unresolved until both
// re-appends have landed, the first landing releasing nothing, and neither
// record is a loss.
func TestTwoCopiesOneSegmentResolvesWhenBothLanded(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(true)
	cr := newCorruptRig(t, l.install)
	t.Cleanup(l.close)
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	seg := rs[0].orig.Segment

	cr.useCopy(t, rs[0])
	first := l.next(t)
	cr.useCopy(t, rs[1])
	if v := cr.holdOf(seg); !v.held || len(v.seqs) != 2 || !v.claimed || v.owed {
		t.Fatalf("hold = %+v, want both records held under a claim", v)
	}
	l.land(nil)

	second := l.next(t)
	if second.Seq == first.Seq {
		t.Fatalf("seq %d was re-appended twice", first.Seq)
	}
	// The first landing released nothing.
	if v := cr.holdOf(seg); !v.held || !slices.Equal(v.seqs, []core.Seq{second.Seq}) || !v.claimed {
		t.Fatalf("hold after the first landing = %+v, want only seq %d, claimed", v, second.Seq)
	}
	if _, _, err := cr.set.ClaimRetirement(seg); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("ClaimRetirement after the first landing = %v, want ErrClaimed", err)
	}
	cr.noResolved(t)

	l.land(nil)
	if got := cr.awaitResolved(t); got != seg {
		t.Fatalf("resolved segment %d, want %d", got, seg)
	}
	if v := cr.holdOf(seg); v.held {
		t.Fatalf("hold after both landed = %+v, want none", v)
	}
	cr.wantRetirable(t, seg)
	l.wantNoDomain(t)
	cr.noResolved(t)
	cr.wantNoLoss(t)
}

// TestClaimRefusedByRepackServesFromCopyAndOwesClaim covers a claim a repack's
// mid-move claim refuses: each read is served from its verified copy, nothing
// is repointed, the set and the unresolved mark stay with the claim owed, and
// no retry takes the claim while the repack holds it.
func TestClaimRefusedByRepackServesFromCopyAndOwesClaim(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	p := &passes{}
	cr := newCorruptRig(t, func(cr *corruptRig) { l.install(cr); cr.d.afterHolds = p.count })
	f, g := file(1), file(2)
	rs := cr.reappendTogether(t, f, g)
	cr.register(rs...)
	seg := rs[0].orig.Segment
	if !cr.set.Claim(seg) {
		t.Fatal("the repack could not claim the segment")
	}

	cr.useCopy(t, rs[0])
	if v := cr.holdOf(seg); !v.held || !slices.Equal(v.seqs, []core.Seq{rs[0].c.Seq}) || v.claimed || !v.owed {
		t.Fatalf("hold = %+v, want f's record held with the claim owed", v)
	}
	if got := cr.placedAt(t, f, 0); got != rs[0].c.Record {
		t.Fatalf("f is served from %+v, want nothing repointed from %+v", got, rs[0].c.Record)
	}
	if n := cr.queuedCorrupt(); n != 0 {
		t.Fatalf("%d corrupt losses queued, want none", n)
	}

	// A second read on the segment is also served from its own copy.
	cr.useCopy(t, rs[1])
	if v := cr.holdOf(seg); len(v.seqs) != 2 || v.claimed || !v.owed {
		t.Fatalf("hold = %+v, want both records held with the claim owed", v)
	}
	if got := cr.placedAt(t, g, 0); got != rs[1].c.Record {
		t.Fatalf("g is served from %+v, want nothing repointed from %+v", got, rs[1].c.Record)
	}

	// The worker's retries find the repack still holding the segment.
	p.after(t, cr)
	if v := cr.holdOf(seg); len(v.seqs) != 2 || v.claimed || !v.owed {
		t.Fatalf("hold after retries = %+v, want it still owed", v)
	}
	if got := l.all(); len(got) != 0 {
		t.Fatalf("re-appended %+v while the claim is owed, want nothing", got)
	}
	cr.noResolved(t)
	cr.wantNoLoss(t)
}

// TestRepeatedReadWhileOwedAddsNothing covers one record read corrupt again
// and again while its claim is owed: it is one entry in the set, and the
// segment is let go once its one re-append lands.
func TestRepeatedReadWhileOwedAddsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	p := &passes{}
	cr := newCorruptRig(t, func(cr *corruptRig) { l.install(cr); cr.d.afterHolds = p.count })
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.register(r)
	seg := r.orig.Segment
	if !cr.set.Claim(seg) {
		t.Fatal("the repack could not claim the segment")
	}

	for range 5 {
		cr.useCopy(t, r)
	}
	if v := cr.holdOf(seg); !slices.Equal(v.seqs, []core.Seq{r.c.Seq}) || !v.owed {
		t.Fatalf("hold = %+v, want the one record once, owed", v)
	}

	// With nothing left unsynced, no record's deadline arms the worker's
	// timer: only the owed claim's own tick keeps it retrying.
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	eventually(t, "the ledger to empty", func() bool { return cr.ledgerLen(0) == 0 })
	p.after(t, cr)
	p.after(t, cr)

	cr.set.Release(seg)
	cr.tickUntil(t, "the hold to be let go", func() bool { return !cr.holdOf(seg).held })
	if got := cr.awaitResolved(t); got != seg {
		t.Fatalf("resolved segment %d, want %d", got, seg)
	}
	if got := l.all(); len(got) != 1 || got[0].c.Seq != r.c.Seq {
		t.Fatalf("re-appended %+v, want the one record once", got)
	}
	cr.wantRetirable(t, seg)
	l.wantNoDomain(t)
	cr.wantNoLoss(t)
}

// TestOverwrittenOwedRecordRemovedWithoutReappend covers an owed record a
// later write overwrote: the worker removes it from the set with no
// re-append, and the segment is announced resolved once the other lands.
func TestOverwrittenOwedRecordRemovedWithoutReappend(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	cr := newCorruptRig(t, l.install)
	f, g := file(1), file(2)
	rs := cr.reappendTogether(t, f, g)
	cr.register(rs...)
	seg := rs[0].orig.Segment
	if !cr.set.Claim(seg) {
		t.Fatal("the repack could not claim the segment")
	}
	cr.useCopy(t, rs[0])
	cr.useCopy(t, rs[1])

	// f's record is overwritten, and the overwrite synced, so the index keeps
	// no retained piece of it either.
	cr.publish(t, f, 0, "zzzz")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of the overwrite: %v", err)
	}
	eventually(t, "the overwritten record to leave the index", func() bool { return len(cr.ix.Retained(f)) == 0 })

	cr.set.Release(seg)
	cr.tickUntil(t, "the hold to be let go", func() bool { return !cr.holdOf(seg).held })
	if got := cr.awaitResolved(t); got != seg {
		t.Fatalf("resolved segment %d, want %d", got, seg)
	}
	if got := l.all(); len(got) != 1 || got[0].c.Seq != rs[1].c.Seq {
		t.Fatalf("re-appended %+v, want only g's record", got)
	}
	cr.wantRetirable(t, seg)
	l.wantNoDomain(t)
	cr.wantNoLoss(t)
}

// TestRepackLetsGoWorkerRetriesAndReappends covers the retry once the repack
// lets go: the worker's tick takes the claim, repoints each owed record to its
// copy, re-appends it and removes it, and the segment is announced resolved
// only when the set is empty.
func TestRepackLetsGoWorkerRetriesAndReappends(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(true)
	cr := newCorruptRig(t, l.install)
	t.Cleanup(l.close)
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	seg := rs[0].orig.Segment
	if !cr.set.Claim(seg) {
		t.Fatal("the repack could not claim the segment")
	}
	cr.useCopy(t, rs[0])
	cr.useCopy(t, rs[1])

	cr.set.Release(seg)
	first := l.nextTicking(t, cr)
	if v := cr.holdOf(seg); len(v.seqs) != 2 || !v.claimed || v.owed {
		t.Fatalf("hold at the first re-append = %+v, want both records under the claim", v)
	}
	cr.noResolved(t)
	l.land(nil)

	second := l.next(t)
	if second.Seq == first.Seq {
		t.Fatalf("seq %d was re-appended twice", first.Seq)
	}
	if v := cr.holdOf(seg); !slices.Equal(v.seqs, []core.Seq{second.Seq}) || !v.claimed {
		t.Fatalf("hold after the first landing = %+v, want only seq %d, claimed", v, second.Seq)
	}
	cr.noResolved(t)
	l.land(nil)

	if got := cr.awaitResolved(t); got != seg {
		t.Fatalf("resolved segment %d, want %d", got, seg)
	}
	for _, c := range l.all() {
		if c.atRecord || !c.atCopy {
			t.Fatalf("seq %d was re-appended with the index naming the record %v and the copy %v, want only the copy", c.c.Seq, c.atRecord, c.atCopy)
		}
	}
	// Each record was repointed to its copy before its re-append, which then
	// moved it on.
	for _, r := range rs {
		if got := cr.placedAt(t, r.c.File, 0); got == copyAt(r) || got != l.movedTo(r.c.Seq) {
			t.Fatalf("seq %d is served from %+v, want the place its re-append moved it to %+v, off its copy %+v", r.c.Seq, got, l.movedTo(r.c.Seq), copyAt(r))
		}
	}
	cr.wantRetirable(t, seg)
	l.wantNoDomain(t)
	cr.wantNoLoss(t)
}

// TestRetirementWinsBeforeRetryLosesEveryOwedRecord covers a retirement that
// takes the segment before the worker's retry: every owed record goes to Lose
// as corrupt, its extents dropped and its loss record written.
func TestRetirementWinsBeforeRetryLosesEveryOwedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	cr := newCorruptRig(t, l.install)
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	seg := rs[0].orig.Segment
	if _, _, err := cr.set.ClaimRetirement(seg); err != nil {
		t.Fatalf("ClaimRetirement: %v", err)
	}
	cr.useCopy(t, rs[0])
	cr.useCopy(t, rs[1])
	if v := cr.holdOf(seg); len(v.seqs) != 2 || !v.owed {
		t.Fatalf("hold = %+v, want both records owed", v)
	}

	if _, err := cr.set.Retire(seg); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	cr.d.Retired(seg)
	cr.tickUntil(t, "the hold to be let go", func() bool { return !cr.holdOf(seg).held })

	for _, r := range rs {
		if rec := cr.awaitRecord(t, r.c.Seq); rec.Reason != format.LossCorrupt {
			t.Fatalf("loss record of seq %d = %+v, want corrupt", r.c.Seq, rec)
		}
		if p, ok := cr.ix.Lookup(r.c.File, 0); ok {
			t.Fatalf("seq %d's extent is served from %+v, want it dropped", r.c.Seq, p)
		}
		if evs := cr.lossesOf(r.c.File); len(evs) != 1 || evs[0].Reason != core.LossCorrupt {
			t.Fatalf("events of seq %d = %+v, want one corrupt loss", r.c.Seq, evs)
		}
	}
	if got := l.all(); len(got) != 0 {
		t.Fatalf("re-appended %+v from a retired segment, want nothing", got)
	}
	cr.noResolved(t)
	cr.d.dmu.Lock()
	defer cr.d.dmu.Unlock()
	if len(cr.d.unproduced) != 0 {
		t.Fatalf("unproduced = %v once the hold is gone, want none", cr.d.unproduced)
	}
}

// TestCopyFromResolvedSegmentUnresolvesAndClaims covers a copy used from a
// segment nothing held: a retirement that read it resolved, started while a
// read of another file's record takes the claim, is refused; and a copy whose
// segment the set reports retired is a loss.
//
// It does not cover a failed segment that was announced resolved, nor a
// retirement racing the read: the copy segment here is one sealed and never
// failed, standing for one with no hold, and the steps run in sequence. A
// failed segment holding registered copies is made only by the per-record
// re-append, which is when this test should move to one. f's write is only
// the segment's other file; only g's record is read.
func TestCopyFromResolvedSegmentUnresolvesAndClaims(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	seg := rs[0].orig.Segment

	// The retirer reads the segment resolved: nothing holds it.
	if v := cr.holdOf(seg); v.held {
		t.Fatalf("hold before any read = %+v, want none", v)
	}
	cr.useCopy(t, rs[1])
	if v := cr.holdOf(seg); !v.held || !v.claimed {
		t.Fatalf("hold = %+v, want the segment unresolved and claimed", v)
	}
	if _, _, err := cr.set.ClaimRetirement(seg); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("ClaimRetirement = %v, want ErrClaimed", err)
	}

	t.Run("a retired copy's segment", func(t *testing.T) {
		h := file(3)
		r := cr.reappend(t, h, 0, "hhhh")
		cr.register(r)
		if _, _, err := cr.set.ClaimRetirement(r.orig.Segment); err != nil {
			t.Fatalf("ClaimRetirement: %v", err)
		}
		if _, err := cr.set.Retire(r.orig.Segment); err != nil {
			t.Fatalf("Retire: %v", err)
		}
		cr.corrupt(t, r.c)
		if v := cr.holdOf(r.orig.Segment); v.held {
			t.Fatalf("hold on the retired segment = %+v, want none", v)
		}
		if p, ok := cr.ix.Lookup(h, 0); ok {
			t.Fatalf("the extent is served from %+v, want it dropped", p)
		}
		cr.awaitRecord(t, r.c.Seq)
	})
}

// TestReadOnlyCopyStaysPut covers a read-only journal: a copy is served with
// no hold and no claim, and a hold made before the journal turned read-only
// takes no claim and re-appends nothing on the worker's retries.
func TestReadOnlyCopyStaysPut(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	p := &passes{}
	cr := newCorruptRig(t, func(cr *corruptRig) {
		l.install(cr)
		cr.d.afterHolds = p.count
	})
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	// Registering the copies is in memory only; what matters is that the
	// records they serve for are synced before the journal enters read-only,
	// because the read-only pass fails every unsynced record and leaves a
	// synced one.
	if err := cr.d.Sync(context.Background(), file(1), file(2)); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	eventually(t, "the synced entries to complete", func() bool { return cr.ledgerLen(0) == 0 })
	seg := rs[0].orig.Segment
	if !cr.set.Claim(seg) {
		t.Fatal("the repack could not claim the segment")
	}
	cr.useCopy(t, rs[0])
	cr.d.Enter(errors.New("the journal is read-only"))

	cr.useCopy(t, rs[1])
	if v := cr.holdOf(seg); !slices.Equal(v.seqs, []core.Seq{rs[0].c.Seq}) || !v.owed {
		t.Fatalf("hold = %+v, want only the record held before the journal turned read-only", v)
	}
	if got := cr.placedAt(t, rs[1].c.File, 0); got != copyAt(rs[1]) {
		t.Fatalf("the read-only read is served from %+v, want its copy %+v", got, copyAt(rs[1]))
	}

	cr.set.Release(seg)
	p.afterWaking(t, cr)
	if v := cr.holdOf(seg); !v.owed || v.claimed {
		t.Fatalf("hold after retries = %+v, want it still owed", v)
	}
	if !cr.set.Claim(seg) {
		t.Fatal("the layer took the segment's claim in a read-only journal")
	}
	cr.set.Release(seg)
	if got := l.all(); len(got) != 0 {
		t.Fatalf("re-appended %+v in a read-only journal, want nothing", got)
	}
	cr.noResolved(t)
}

// TestUnholdReturnsSegmentToPriorResolvedState covers a hold taken back
// because the repoint moved nothing: the claim is released, and the segment
// is announced resolved only when a resolution was withheld while it was
// held, never for one already announced.
func TestUnholdReturnsSegmentToPriorResolvedState(t *testing.T) {
	for _, withheld := range []bool{false, true} {
		name := "already resolved"
		if withheld {
			name = "resolved while held"
		}
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			cr := newCorruptRig(t, nil)
			f := file(1)
			r := cr.reappend(t, f, 0, "aaaa")
			seg := r.orig.Segment
			st := cr.d.made[0]
			oc := ownedCopy{File: f, Seq: r.c.Seq, Record: r.c.Record, Copy: r.orig, Share: r.c.Share, Version: r.c.Version, Extent: r.c.Extent}

			unlock := cr.domains.Lock(f)
			v, added := cr.d.holdCopy(st, oc)
			unlock()
			if v != holdRepoint || !added {
				t.Fatalf("holdCopy = %v, %v, want a repoint and the record added", v, added)
			}
			if withheld {
				cr.d.resolve(st, &window{id: 1 << 40, seg: seg})
				if h := cr.holdOf(seg); !h.withheld {
					t.Fatalf("hold = %+v, want the resolution withheld", h)
				}
			}
			cr.noResolved(t)

			unlock = cr.domains.Lock(f)
			cr.d.unholdCopy(st, oc)
			unlock()
			if h := cr.holdOf(seg); h.held {
				t.Fatalf("hold after unhold = %+v, want none", h)
			}
			cr.wantRetirable(t, seg)
			if withheld {
				if got := cr.awaitResolved(t); got != seg {
					t.Fatalf("resolved segment %d, want %d", got, seg)
				}
			}
			cr.noResolved(t)
		})
	}
}

// wantNoDomain fails unless every re-append ran with its record's file append
// domain free: a re-append appends and syncs, and takes the domain only for
// its own final repoint.
func (l *landing) wantNoDomain(t *testing.T) {
	t.Helper()
	got := l.all()
	if len(got) == 0 {
		t.Fatal("nothing was re-appended")
	}
	for _, c := range got {
		if c.domainHeld {
			t.Fatalf("seq %d was re-appended with its file's append domain held", c.c.Seq)
		}
	}
}

// TestLateReadOfHeldRecordKeepsTheFirstReadsHold covers a read that adds no
// record to the hold, the record already held by an earlier read, and whose
// repoint then moves nothing: it does not take back the earlier read's hold,
// so the segment stays claimed. The file's domain is held throughout, so the
// worker cannot move the hold meanwhile.
func TestLateReadOfHeldRecordKeepsTheFirstReadsHold(t *testing.T) {
	prof.Watch(t, prof.Options{})
	h := &holds{}
	cr := newCorruptRig(t, func(cr *corruptRig) { h.watch(cr.d) })
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.register(r)
	seg := r.orig.Segment
	cr.useCopy(t, r)

	unlock := cr.domains.Lock(f)
	defer unlock()
	// The index stops naming both the corrupt record and the copy, as a later
	// move of the record would leave it, so the late read's repoint moves
	// nothing and finds the copy not served either.
	elsewhere := index.RecordAt{Segment: 999, Offset: 0}
	if moved, _, err := cr.ix.RepointRecord(f, copyAt(r), elsewhere); err != nil || moved == 0 {
		t.Fatalf("RepointRecord = %d, %v, want the record moved", moved, err)
	}
	late := r.c
	late.DomainHeld = true
	if _, err := cr.d.Corrupt(context.Background(), late); err != nil {
		t.Fatalf("Corrupt: %v", err)
	}
	if v := cr.holdOf(seg); !slices.Equal(v.seqs, []core.Seq{r.c.Seq}) || !v.claimed {
		t.Fatalf("hold after the late read = %+v, want the first read's record, claimed", v)
	}
	if _, _, err := cr.set.ClaimRetirement(seg); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("ClaimRetirement = %v, want ErrClaimed", err)
	}
	if got := h.released(); len(got) != 0 {
		t.Fatalf("unholdCopy was handed %+v, want nothing", got)
	}
}

// TestReadAfterAnotherReadsRepointServesTheCopy covers two reads of one
// corrupt record, the second reaching Corrupt after the first repointed the
// index to the copy: the second is served the copy too, and is no loss, so no
// loss record, no event, no mark, and the record stays in the ledger.
func TestReadAfterAnotherReadsRepointServesTheCopy(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.register(r)

	cr.useCopy(t, r)
	cr.useCopy(t, r)
	if got := cr.placedAt(t, f, 0); got != copyAt(r) {
		t.Fatalf("the extent is served from %+v, want the copy %+v", got, copyAt(r))
	}
	if !cr.inLedger(r.again) {
		t.Fatal("the record left the ledger")
	}
	if n := cr.marksOf(f); n != 0 {
		t.Fatalf("%d marks for the file, want none", n)
	}
	if v := cr.holdOf(r.orig.Segment); !slices.Equal(v.seqs, []core.Seq{r.c.Seq}) || !v.claimed {
		t.Fatalf("hold = %+v, want the one record, claimed", v)
	}
	cr.wantNoLoss(t)
}

// TestHeldRecordIsReappendedAndHoldsEnd covers a record served from a copy
// with the layer's own re-append installed: the worker appends the record
// again under its own number and version, the index names that place and not
// the copy, the hold ends and releases the segment's claim, the segment is
// announced resolved once, and the record that was not read stays where it is.
func TestHeldRecordIsReappendedAndHoldsEnd(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, func(cr *corruptRig) { cr.d.reappend = cr.d.reappendHeld })
	rs := cr.reappendTogether(t, file(1), file(2))
	cr.register(rs...)
	seg := rs[0].orig.Segment

	cr.useCopy(t, rs[0])
	eventually(t, "the hold to end", func() bool { return !cr.holdOf(seg).held })
	if got := cr.awaitResolved(t); got != seg {
		t.Fatalf("resolved segment %d, want %d", got, seg)
	}
	cr.wantRetirable(t, seg)

	f := rs[0].c.File
	got := cr.placedAt(t, f, 0)
	if got == copyAt(rs[0]) || got == placeAt(rs[0].again) {
		t.Fatalf("the extent is served from %+v, want a new place outside the copy and the corrupt record", got)
	}
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	hs, _ := cr.durableHeaders(t)
	if !slices.ContainsFunc(hs, func(h format.RecordHeader) bool {
		return h.Seq == rs[0].c.Seq && h.Version == rs[0].c.Version && h.File == f && placeOf(h) == got
	}) {
		t.Errorf("no durable record of seq %d, version %+v sits at %+v", rs[0].c.Seq, rs[0].c.Version, got)
	}
	if got := cr.placedAt(t, rs[1].c.File, 0); got != placeAt(rs[1].again) {
		t.Errorf("the record nothing read is served from %+v, want %+v", got, placeAt(rs[1].again))
	}
	if ev := cr.lossesOf(f); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
	cr.noResolved(t)
}

// heldMove is a failRig with one record re-appended out of a failed segment,
// its new place read corrupt and served from the original, and the worker
// parked after the step over the holds that made the hold's re-append: that
// re-append's Append has returned, and no sync has run since.
type heldMove struct {
	fr      *failRig
	f       core.FileID
	orig    segment.Location // the original, the copy the hold serves from
	bad     index.RecordAt   // the window's re-append, read corrupt
	seq     core.Seq
	v       core.Version
	placed  segment.Location // the hold's re-append
	release func()

	mu sync.Mutex
	// counted is what the index backed in each segment when it was announced
	// resolved, and heldAt whether the original's segment was held then.
	counted map[core.SegmentID]index.SegmentBytes
	heldAt  map[core.SegmentID]holdView
	// unsynced is whether a place of the record was still pending when each
	// segment was announced resolved.
	unsynced map[core.SegmentID]bool
}

// seqPending reports whether any ledger or window entry of the record is still
// pending.
func (m *heldMove) seqPending() bool {
	st := m.fr.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.ledger {
		if e.w.Seq == m.seq && st.pending(e.w.Loc) {
			return true
		}
	}
	for _, w := range st.windows {
		for _, e := range w.entries {
			if e.w.Seq == m.seq {
				return true
			}
		}
	}
	return false
}

func newHeldMove(t *testing.T) *heldMove {
	t.Helper()
	m := &heldMove{counted: map[core.SegmentID]index.SegmentBytes{}, heldAt: map[core.SegmentID]holdView{}, unsynced: map[core.SegmentID]bool{}}
	hook, reached, release := gate(t)
	var appends atomic.Int32
	var holdAppended atomic.Bool
	m.fr = newFailRig(t, func(fr *failRig) {
		fr.d.reappend = fr.d.reappendHeld
		// The first append is the failed window's re-append, the second the
		// hold's.
		fr.d.afterAppend = func() {
			if appends.Add(1) == 2 {
				holdAppended.Store(true)
			}
		}
		fr.d.afterHolds = func() {
			if holdAppended.Load() {
				hook()
			}
		}
		fr.d.resolved = func(_ uint32, seg core.SegmentID) {
			m.mu.Lock()
			m.counted[seg] = fr.ix.SegmentBytes(seg)
			m.heldAt[seg] = fr.holdOf(m.orig.Segment)
			m.unsynced[seg] = m.seqPending()
			m.mu.Unlock()
			fr.resolved <- seg
		}
	})
	m.release = release
	t.Cleanup(release)
	fr := m.fr
	m.f = file(1)
	var a segment.Location
	a, m.v = fr.publish(t, m.f, 0, "aaaa")
	m.mu.Lock()
	m.orig = a
	m.mu.Unlock()
	m.seq = core.Seq(fr.lastSeq.Load())
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), m.f)); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	h, ok := fr.writesOutside(t, a.Segment)[m.seq]
	if !ok {
		t.Fatalf("seq %d was not re-appended", m.seq)
	}
	m.bad = placeOf(h)
	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: m.f, Record: m.bad, Seq: m.seq, Version: m.v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != a {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy of the original %+v", out, err, a)
	}
	awaitGate(t, "the step over the holds that re-appended the held record", reached)
	st := fr.d.made[0]
	st.mu.Lock()
	for _, e := range st.ledger {
		if e.w.Seq == m.seq {
			m.placed = e.w.Loc
		}
	}
	st.mu.Unlock()
	if m.placed == (segment.Location{}) {
		t.Fatalf("no ledger entry for the hold's re-append of seq %d", m.seq)
	}
	if got := fr.placedAt(t, m.f, 0); got != placeAt(m.placed) {
		t.Fatalf("the extent is served from %+v, want the hold's re-append %+v", got, placeAt(m.placed))
	}
	return m
}

// wantStillHeld fails unless the original's segment is held for the record,
// under the hold's claim, so no retirement can take it.
func (m *heldMove) wantStillHeld(t *testing.T, when string) {
	t.Helper()
	seg := m.orig.Segment
	if v := m.fr.holdOf(seg); !v.held || !v.claimed || !slices.Equal(v.seqs, []core.Seq{m.seq}) {
		t.Fatalf("hold %s = %+v, want seq %d held under the claim", when, v, m.seq)
	}
	if _, _, err := m.fr.set.ClaimRetirement(seg); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("ClaimRetirement %s = %v, want ErrClaimed", when, err)
	}
}

// announced returns what the index backed in seg when it was announced
// resolved, and the original's hold then.
func (m *heldMove) announced(seg core.SegmentID) (index.SegmentBytes, holdView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counted[seg], m.heldAt[seg]
}

// TestAHeldRecordStaysHeldUntilItsNewPlaceCompletes covers the hold's end: a
// held record whose re-append's Append has returned stays held, its copy's
// segment claimed and unannounced, until the new place's sync completes; the
// hold then ends, the claim is released and the segment is announced resolved
// once, with the record durable at its new place and nothing in the segment
// backed by the index.
func TestAHeldRecordStaysHeldUntilItsNewPlaceCompletes(t *testing.T) {
	prof.Watch(t, prof.Options{})
	m := newHeldMove(t)
	fr := m.fr
	if fr.d.Durable(m.placed) {
		t.Fatalf("the hold's re-append at %+v is durable before any sync of it", m.placed)
	}
	m.wantStillHeld(t, "after the re-append's Append returned")
	fr.wantNotResolved(t, "before the held record's new place synced")
	m.release()

	if got := fr.awaitResolved(t); got != m.orig.Segment {
		t.Fatalf("resolved segment %d, want %d", got, m.orig.Segment)
	}
	if !fr.d.Durable(m.placed) {
		t.Fatalf("the segment was announced with the new place %+v not durable", m.placed)
	}
	counted, held := m.announced(m.orig.Segment)
	if counted != (index.SegmentBytes{}) {
		t.Errorf("segment %d backed %+v when it was announced resolved, want nothing", m.orig.Segment, counted)
	}
	if held.held {
		t.Errorf("hold at the announcement = %+v, want it ended", held)
	}
	fr.wantRetirable(t, m.orig.Segment)
	if got := fr.placedAt(t, m.f, 0); got != placeAt(m.placed) {
		t.Errorf("the extent is served from %+v, want the hold's re-append %+v", got, placeAt(m.placed))
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
	mark := fr.passes.Load()
	fr.d.made[0].wake()
	fr.awaitPassEnd(t, mark)
	fr.wantNotResolved(t, "twice")
}

// TestAHeldRecordFollowsItsNewPlaceThroughAFailedSync covers a held record
// whose new place's sync fails while its window's re-read verifies: the window
// moves the record on, so the new place stops being pending while the record's
// next place is still unsynced, and the hold must not end before that place
// completes.
func TestAHeldRecordFollowsItsNewPlaceThroughAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	m := newHeldMove(t)
	fr := m.fr
	fr.failSyncKeepingPages(t, m.placed.Segment, errors.New("sync boom"))
	m.release()

	// The new place's window resolves first, then the hold's end announces the
	// original's segment.
	if got := fr.awaitResolved(t); got != m.placed.Segment {
		t.Fatalf("resolved segment %d, want %d", got, m.placed.Segment)
	}
	if got := fr.awaitResolved(t); got != m.orig.Segment {
		t.Fatalf("resolved segment %d, want %d", got, m.orig.Segment)
	}
	m.mu.Lock()
	unsynced := m.unsynced[m.orig.Segment]
	m.mu.Unlock()
	if unsynced {
		t.Errorf("segment %d was announced resolved while a place of seq %d was still pending", m.orig.Segment, m.seq)
	}
	if got := fr.placedAt(t, m.f, 0); got == placeAt(m.placed) {
		t.Errorf("the extent is still served from %+v, want the record moved on", got)
	}
}

// TestAHeldRecordStaysClaimedWhenTheJournalTurnsReadOnlyBeforeItsNewPlaceSyncs
// covers a journal that turns read-only after a held record's re-append and
// before the new place syncs: the read-only pass fails the new place, and the
// copy's segment stays claimed and unannounced, as nothing is re-appended or
// released in a read-only journal.
func TestAHeldRecordStaysClaimedWhenTheJournalTurnsReadOnlyBeforeItsNewPlaceSyncs(t *testing.T) {
	prof.Watch(t, prof.Options{})
	m := newHeldMove(t)
	fr := m.fr
	fr.d.Enter(errors.New("the journal is read-only"))
	m.release()

	st := fr.d.made[0]
	eventually(t, "the read-only pass to fail the new place", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		_, lost := st.lost[m.placed]
		return lost
	})
	for range 3 {
		mark := fr.passes.Load()
		st.wake()
		fr.awaitPassEnd(t, mark)
	}
	m.wantStillHeld(t, "in the read-only journal")
	fr.wantNotResolved(t, "in the read-only journal")
}

// TestACorruptNewPlaceOfAHeldRecordIsReappendedAgainUnderTheHold covers a read
// that finds the hold's re-append corrupt before it syncs: the read is served
// from the copy again, the hold does not end when that place completes, the
// record is re-appended once more, and the copy's segment is announced
// resolved only once the index names the last place and nothing in the
// segment.
func TestACorruptNewPlaceOfAHeldRecordIsReappendedAgainUnderTheHold(t *testing.T) {
	prof.Watch(t, prof.Options{})
	m := newHeldMove(t)
	fr := m.fr
	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: m.f, Record: placeAt(m.placed), Seq: m.seq, Version: m.v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != m.orig {
		t.Fatalf("Corrupt of the new place = %+v, %v, want UseCopy of the original %+v", out, err, m.orig)
	}
	m.release()

	if got := fr.awaitResolved(t); got != m.orig.Segment {
		t.Fatalf("resolved segment %d, want %d", got, m.orig.Segment)
	}
	counted, _ := m.announced(m.orig.Segment)
	if counted != (index.SegmentBytes{}) {
		t.Errorf("segment %d backed %+v when it was announced resolved, want nothing", m.orig.Segment, counted)
	}
	p, ok := fr.ix.Lookup(m.f, 0)
	if !ok || p.Version != m.v || p.Record == placeAt(m.orig) || p.Record == m.bad || p.Record == placeAt(m.placed) {
		t.Fatalf("Lookup = %+v, %v, want version %+v at a place after the corrupt ones", p, ok, m.v)
	}
	// The window's re-append, the hold's and the hold's second each placed
	// the record once.
	if got, want := fr.d.Stats().ReappendedBytes, 3*uint64(m.orig.Len); got != want {
		t.Errorf("ReappendedBytes = %d, want %d", got, want)
	}
	fr.wantRetirable(t, m.orig.Segment)
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
}
