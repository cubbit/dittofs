package durability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// syncAnswer is one Sync call's marks and error.
type syncAnswer struct {
	marks []core.SyncMark
	err   error
}

// syncMarksAsync runs Sync in a goroutine and returns the channel its answer
// lands on.
func (r *rig) syncMarksAsync(ctx context.Context, ids ...core.FileID) <-chan syncAnswer {
	ch := make(chan syncAnswer, 1)
	go func() {
		marks, err := r.d.Sync(ctx, ids...)
		ch <- syncAnswer{marks: marks, err: err}
	}()
	return ch
}

// mustSyncReturn waits for the goroutine's Sync to return and hands its
// answer back, failing the test if it does not.
func mustSyncReturn(t *testing.T, what string, ch <-chan syncAnswer) syncAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return syncAnswer{}
	}
}

// setLatest hands the layer the Latest the datapath wires over the core: the
// file's newest record the rig's index names, its place read from the
// record's header. A rig without it answers as the layer did before the
// fallback existed.
func (r *rig) setLatest() {
	r.d.latest = func(id core.FileID) (segment.Location, core.Seq, bool) {
		var best index.RecordAt
		for _, e := range r.ix.FileEntries(id) {
			if e.Record == (index.RecordAt{}) {
				continue // a marker names no record
			}
			if best == (index.RecordAt{}) || e.Record.Segment > best.Segment ||
				(e.Record.Segment == best.Segment && e.Record.Offset > best.Offset) {
				best = e.Record
			}
		}
		if best == (index.RecordAt{}) {
			return segment.Location{}, 0, false
		}
		head := make([]byte, format.RecordHeaderSize)
		if _, err := r.streams.For(id).ReadAt(best.Segment, head, int64(best.Offset)); err != nil {
			return segment.Location{}, 0, false
		}
		h, err := format.DecodeRecordHeader(head, rigIdentity, best.Segment, int64(best.Offset))
		if err != nil {
			return segment.Location{}, 0, false
		}
		return segment.Location{Segment: best.Segment, Off: int64(best.Offset), Len: h.Size()}, h.Seq, true
	}
}

// wantMark fails the test unless m is exactly the mark of the record at loc:
// the segment, the record's end, its sequence number as the placed record's
// header carries it, and a synced-through offset that covers the record and
// claims nothing past the durable bytes of its segment.
func (r *rig) wantMark(t *testing.T, what string, m core.SyncMark, loc segment.Location) {
	t.Helper()
	raw, err := r.seam.Durable(format.SegmentName(loc.Segment))
	if err != nil || loc.Off+format.RecordHeaderSize > int64(len(raw)) {
		t.Fatalf("%s: the durable bytes of segment %d do not hold the record at %+v (%v)", what, loc.Segment, loc, err)
	}
	h, err := format.DecodeRecordHeader(raw[loc.Off:loc.Off+format.RecordHeaderSize], rigIdentity, loc.Segment, loc.Off)
	if err != nil {
		t.Fatalf("%s: decoding the record at %+v: %v", what, loc, err)
	}
	if m.Segment != uint64(loc.Segment) {
		t.Errorf("%s: mark.Segment = %d, want the record's segment %d", what, m.Segment, loc.Segment)
	}
	if want := loc.Off + loc.Len; m.Offset != want {
		t.Errorf("%s: mark.Offset = %d, want the record's end %d", what, m.Offset, want)
	}
	if m.Sequence != uint64(h.Seq) {
		t.Errorf("%s: mark.Sequence = %d, want the record's %d", what, m.Sequence, h.Seq)
	}
	if m.Through < m.Offset {
		t.Errorf("%s: mark.Through = %d, want at least the record's end %d", what, m.Through, m.Offset)
	}
	if dur := int64(len(raw)); m.Through > dur {
		t.Errorf("%s: mark.Through = %d, want at most the segment's durable length %d: a synced-through offset proves only durable bytes", what, m.Through, dur)
	}
}

// filesOnEachStream returns two files the rig places on different streams.
func filesOnEachStream(t *testing.T, r *rig) (core.FileID, core.FileID) {
	t.Helper()
	f := file(1)
	for b := byte(2); b != 0; b++ {
		if g := file(b); r.streams.IndexFor(g) != r.streams.IndexFor(f) {
			return f, g
		}
	}
	t.Fatal("no file lands on a stream other than the first file's")
	return f, f
}

// TestSyncReturnsAMarkPerStreamCoveringTheNewestNamedRecord covers: two files
// on two streams, one Sync naming both: one mark per stream carrying the
// stream, the segment, the end and the sequence number of the named record the
// stream holds, ordered by stream number. A stream that holds no named record
// returns no mark, and neither does an empty call or a file with nothing
// unsynced.
func TestSyncReturnsAMarkPerStreamCoveringTheNewestNamedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 2)
	f, g := filesOnEachStream(t, r)
	a := r.appendWrite(t, f, 0, "ffff")
	b := r.appendWrite(t, g, 0, "gggg")

	marks, err := r.d.Sync(context.Background(), f, g)
	if err != nil {
		t.Fatalf("Sync(f, g) = %v, want nil", err)
	}
	if len(marks) != 2 {
		t.Fatalf("Sync of two streams returned %d marks, want one per stream: %+v", len(marks), marks)
	}
	locs := map[uint32]segment.Location{
		uint32(r.streams.IndexFor(f)): a,
		uint32(r.streams.IndexFor(g)): b,
	}
	for i, m := range marks {
		if int(m.Stream) != i {
			t.Errorf("mark %d names stream %d: the marks are not ordered by stream number (%+v)", i, m.Stream, marks)
		}
		loc, ok := locs[m.Stream]
		if !ok {
			t.Errorf("mark %+v names stream %d, which holds no named record", m, m.Stream)
			continue
		}
		r.wantMark(t, fmt.Sprintf("the mark of stream %d", m.Stream), m, loc)
	}
}

// TestSyncOmitsAStreamHoldingNoNamedRecord covers: a Sync naming one file of
// two written on two streams returns the file's mark alone; the other stream
// holds no named record, so it returns no mark however unsynced its own
// records are.
func TestSyncOmitsAStreamHoldingNoNamedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 2)
	f, g := filesOnEachStream(t, r)
	a := r.appendWrite(t, f, 0, "ffff")
	r.appendWrite(t, g, 0, "gggg")

	marks, err := r.d.Sync(context.Background(), f)
	if err != nil {
		t.Fatalf("Sync(f) = %v, want nil", err)
	}
	if len(marks) != 1 {
		t.Fatalf("Sync of one file returned %d marks, want the one of its stream: %+v", len(marks), marks)
	}
	if want := uint32(r.streams.IndexFor(f)); marks[0].Stream != want {
		t.Fatalf("the mark names stream %d, want the named file's %d", marks[0].Stream, want)
	}
	r.wantMark(t, "the mark of the named file", marks[0], a)
}

// TestSyncReturnsNoMarkForASealedSegment covers: a Sync that returns over a
// record a seal already covered, in a segment the seal closed, returns no mark
// for it -- the seal is the proof, so a mark would be redundant -- while a
// record a later Sync places outside the sealed segment is marked.
func TestSyncReturnsNoMarkForASealedSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var set atomic.Pointer[segment.Set]
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Sealed = func(id core.SegmentID) bool {
			if s := set.Load(); s != nil {
				st, err := s.State(id)
				return err != nil || st == segment.Sealed
			}
			return false
		}
	})
	set.Store(r.set)
	hook, reached, release := gate(t)
	captured := make(chan struct{})
	var capturedOnce sync.Once
	r.d.beforeWait = func(ws []work) {
		for _, w := range ws {
			if len(w.cands) > 0 {
				capturedOnce.Do(func() { close(captured) })
				return
			}
		}
	}
	var armed atomic.Bool
	// Installed before the workers start, as every test hook is, so the
	// worker never races the field it reads each pass.
	r.d.afterHolds = func() {
		if armed.CompareAndSwap(true, false) {
			hook()
		}
	}
	r.d.Start(r.streams)
	// Registered after the rig's own cleanups, so a failing test reports
	// before the layer's Stop tries to join the parked worker.
	t.Cleanup(release)

	f := file(1)
	// The park is armed before the append, so the pass the append itself
	// kicks is the parked one: nothing can complete the record first, and
	// the sealed segment's entry is still a target when the Sync below
	// captures its candidates, so the mark can only be missing because the
	// sealed check removed it.
	armed.Store(true)
	r.appendWrite(t, f, 0, "ssss")
	awaitGate(t, "the worker's pause", reached)
	if _, err := r.streams.For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	sync := r.syncMarksAsync(context.Background(), f)
	// The parked worker is released only once the waiter holds its
	// candidates, as the applicability test releases it, so the empty answer
	// below can only come from the sealed check.
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("the Sync never parked on the candidate it captured")
	}
	release()
	answered := mustSyncReturn(t, "Sync over the sealed segment", sync)
	marks, err := answered.marks, answered.err
	if err != nil {
		t.Fatalf("Sync over a sealed segment = %v, want nil", err)
	}
	if len(marks) != 0 {
		t.Errorf("Sync over a sealed segment returned marks %+v, want none: a seal is the record's proof", marks)
	}

	// The pre-seal path: a record the roll after the seal placed in the new
	// segment is marked when a Sync names it, beside the healed file.
	g := file(2)
	w := r.appendWrite(t, g, 0, "hhhh")
	marks, err = r.d.Sync(context.Background(), f, g)
	if err != nil {
		t.Fatalf("Sync after the seal = %v, want nil", err)
	}
	if len(marks) != 1 || marks[0].Stream != 0 {
		t.Fatalf("Sync after the seal returned %+v, want one mark of the stream", marks)
	}
	r.wantMark(t, "the mark of the new segment", marks[0], w)
}

// TestSyncReturnsNoMarksOnAFailedWindow covers: a Sync the failed window
// answers returns no marks with its error, and a later successful Sync of the
// healed file returns its marks again.
func TestSyncReturnsNoMarksOnAFailedWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)
	a := r.appendWrite(t, f, 0, "ffff")
	// The default rig fails the re-read of a pending window's segment, so the
	// record is failed and its file marked, as a segment whose pages the
	// failed sync left unreliable.
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("sync boom"))); err != nil {
		t.Fatal(err)
	}

	marks, err := r.d.Sync(context.Background(), f)
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync over a failed window = %v, want a *SyncError", err)
	}
	if !slices.Contains(se.Files, f) {
		t.Errorf("the SyncError names files %v, want %v", se.Files, f)
	}
	if marks != nil {
		t.Errorf("Sync over a failed window returned marks %+v, want none: a mark would prove records the sync lost", marks)
	}

	// The report taken, a later Sync of the file succeeds and returns the
	// marks of what it makes durable.
	w := r.appendWrite(t, f, 8, "hhhh")
	marks, err = r.d.Sync(context.Background(), f)
	if err != nil {
		t.Fatalf("the Sync after the report = %v, want nil: the failure is reported once", err)
	}
	if len(marks) != 1 {
		t.Fatalf("the Sync after the report returned %d marks, want one: %+v", len(marks), marks)
	}
	r.wantMark(t, "the mark after the failure", marks[0], w)
}

// TestSyncReturnsNoMarksForAnyStreamOnAFailedWindow covers the decision's
// scope: a Sync whose window failed on one stream returns no marks for its
// OTHER stream either, though that stream's named file lost nothing -- the
// no-marks-on-error rule is the call's, not the lost record's, so a healthy
// record never shares a Sync with a failed one in the marks it hands back.
func TestSyncReturnsNoMarksForAnyStreamOnAFailedWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 2)
	f, g := filesOnEachStream(t, r)
	a := r.appendWrite(t, f, 0, "ffff")
	r.appendWrite(t, g, 0, "gggg")
	// The default rig fails the re-read of a pending window's segment, so the
	// record is failed and its file marked, as a segment whose pages the
	// failed sync left unreliable.
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("sync boom"))); err != nil {
		t.Fatal(err)
	}

	marks, err := r.d.Sync(context.Background(), f, g)
	var se *core.SyncError
	if !errors.As(err, &se) {
		t.Fatalf("Sync across the failed window = %v, want a *SyncError", err)
	}
	if !slices.Contains(se.Files, f) {
		t.Errorf("the SyncError names files %v, want %v among them", se.Files, f)
	}
	if marks != nil {
		t.Errorf("Sync across a failed window on one stream returned marks %+v, want none on every stream: the rule is the call's, not the lost record's", marks)
	}
}

// TestSyncMarkFollowsAReAppendedRecord covers: a Sync that waits through a
// re-append returns the mark of the copy -- the new segment and the copy's
// end -- never the ended segment the record was first placed in. The worker
// is parked after the copy's sync and released only once the waiter's checks
// have followed the record to the copy, so the mark is built from the place
// the wait resolved, which completing the record forgets (see cand).
func TestSyncMarkFollowsAReAppendedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	t.Cleanup(release)
	var syncs atomic.Int32
	var origSeg atomic.Uint64
	saw := make(chan struct{})
	var sawOnce sync.Once
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.afterSync = func() {
			if syncs.Add(1) == 2 {
				hook()
			}
		}
		fr.d.beforeWait = func(ws []work) {
			orig := core.SegmentID(origSeg.Load())
			if orig == 0 {
				return
			}
			for _, w := range ws {
				for _, c := range w.cands {
					if c.final.Segment != orig && c.along >= c.final.Off+c.final.Len {
						sawOnce.Do(func() { close(saw) })
						return
					}
				}
			}
		}
	})

	f := file(1)
	a, _ := fr.publish(t, f, 0, "aaaa")
	origSeg.Store(uint64(a.Segment))
	seq := core.Seq(fr.lastSeq.Load())
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))

	syncing := fr.syncMarksAsync(context.Background(), f)
	awaitGate(t, "the park after the copy's sync", reached)
	select {
	case <-saw:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never followed the record to the copy")
	}
	release()
	answered := mustSyncReturn(t, "Sync", syncing)
	if answered.err != nil {
		t.Fatalf("Sync = %v, want nil: the record was re-appended", answered.err)
	}

	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok {
		t.Fatalf("seq %d was never re-appended", seq)
	}
	want := segment.Location{Segment: h.Segment, Off: h.Offset, Len: h.Size()}
	if len(answered.marks) != 1 {
		t.Fatalf("Sync returned %d marks, want the one of the stream: %+v", len(answered.marks), answered.marks)
	}
	fr.wantMark(t, "the mark of the copy", answered.marks[0], want)
	if answered.marks[0].Segment == uint64(a.Segment) {
		t.Errorf("the mark names the ended segment %d, want the copy's %d", a.Segment, h.Segment)
	}
}

// TestSyncMarksAFileSyncedEarlier covers: a Sync naming a file with nothing
// unsynced -- the timer or an earlier call already made its record durable --
// returns its mark all the same, from the newest record the index names
// (RFC 1 §3.1: the clause carries no unsynced qualifier), issuing no sync of
// its own. A Sync naming no file at all returns no marks.
func TestSyncMarksAFileSyncedEarlier(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigConfigured(t, 1, func(*Config) {})
	r.setLatest()
	r.d.Start(r.streams)
	f := file(1)
	a, _ := r.publish(t, f, 0, "ffff")
	if marks, err := r.d.Sync(context.Background(), f); err != nil || len(marks) != 1 {
		t.Fatalf("the first Sync = %v, %d marks, want nil error and one", err, len(marks))
	}

	marks, err := r.d.Sync(context.Background(), f)
	if err != nil {
		t.Fatalf("the second Sync of %v = %v, want nil: nothing unsynced, so no sync, but the mark still comes", f, err)
	}
	if len(marks) != 1 {
		t.Fatalf("the second Sync of a synced file returned %d marks, want the record's: %+v", len(marks), marks)
	}
	r.wantMark(t, "the mark of the already-synced record", marks[0], a)
	if marks, err = r.d.Sync(context.Background()); err != nil || len(marks) != 0 {
		t.Fatalf("Sync naming no file = %v, %d marks, want nil error and none", err, len(marks))
	}
}

// TestSyncReturnsNoMarkForRecordsInSealedOrRetiredSegments covers the sealed
// verdict's direction: a segment gone from the registry answers sealed to the
// marks' filter -- the caller keeps each stream's latest mark, so a proofless
// Through:0 mark naming a dead segment would replace the stream's real proof
// -- and so does a genuinely sealed one. A stream whose older named record's
// segment retired, and whose newer named record's segment sealed, returns no
// mark at all.
//
// The park is armed before the record is appended, so the pass the append
// itself kicks is the parked one -- nothing can complete the record first --
// and the parked worker is released only once the waiter holds its candidates
// (beforeWait runs after the waiter's checks found something still pending,
// and the parked worker is the only thing that can complete the records), so
// the empty answer below comes from the sealed filter alone.
//
// Production ordering note: this fixture could not arise in the reverse
// direction from the stream's lifecycle. Every roll seals the segment it
// leaves, so a stream's unsealed segment is its active one, which holds its
// newest appends: production never orders a sealed segment holding a NEWER
// named record than an unsealed one's. The greatest-passing selection in
// marksOf is belt for that ordering, naming the stream's newest named record
// whose segment is not sealed should any later path -- a recycle, a reseed --
// ever produce the reverse.
func TestSyncReturnsNoMarkForRecordsInSealedOrRetiredSegments(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var set atomic.Pointer[segment.Set]
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Sealed = func(id core.SegmentID) bool {
			if s := set.Load(); s != nil {
				st, err := s.State(id)
				return err != nil || st == segment.Sealed
			}
			return false
		}
	})
	set.Store(r.set)
	hook, reached, release := gate(t)
	captured := make(chan struct{})
	var capturedOnce sync.Once
	r.d.beforeWait = func(ws []work) {
		for _, w := range ws {
			if len(w.cands) > 0 {
				capturedOnce.Do(func() { close(captured) })
				return
			}
		}
	}
	var armed atomic.Bool
	r.d.afterHolds = func() {
		if armed.CompareAndSwap(true, false) {
			hook()
		}
	}
	r.d.Start(r.streams)
	// Registered after the rig's own cleanups, so a failing test reports
	// before the layer's Stop tries to join the parked worker.
	t.Cleanup(release)

	f, g := file(1), file(2)
	armed.Store(true)
	a := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	awaitGate(t, "the worker's pause", reached)
	if _, err := r.streams.For(f).Seal(false); err != nil {
		t.Fatalf("Seal of the older record's segment: %v", err)
	}
	if st, _, err := r.set.ClaimRetirement(a.Segment); err != nil || st != segment.Sealed {
		t.Fatalf("ClaimRetirement = %v, %v, want the sealed segment claimed", st, err)
	}
	if _, err := r.set.Retire(a.Segment); err != nil {
		t.Fatalf("Retire of the older record's segment: %v", err)
	}
	r.d.Retired(a.Segment)
	b := r.appendWrite(t, g, 0, "gggg")
	if b.Segment == a.Segment {
		t.Fatalf("the roll after the seal placed the newer record in the retired segment %d", a.Segment)
	}
	if _, err := r.streams.For(g).Seal(false); err != nil {
		t.Fatalf("Seal of the newer record's segment: %v", err)
	}

	sync := r.syncMarksAsync(context.Background(), f, g)
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("the Sync never parked on the candidates it captured")
	}
	release()
	answered := mustSyncReturn(t, "Sync", sync)
	if answered.err != nil {
		t.Fatalf("Sync = %v, want nil", answered.err)
	}
	if len(answered.marks) != 0 {
		t.Errorf("Sync returned marks %+v, want none: the retired record's segment answers sealed (gone from the registry), the newer record's is sealed, so no named record can be proved", answered.marks)
	}
}

// TestSyncReturnsNoMarkForARecordCorruptDropsMidWait covers: a record Corrupt
// drops while a Sync waits on it leaves no mark, since the stream no longer
// holds it; the Sync the corrupt read interrupted returns nil, its record's
// loss being the reader's to report, and a later Sync marks what it makes
// durable again.
func TestSyncReturnsNoMarkForARecordCorruptDropsMidWait(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	w := cr.write(t, f, 0, "aaaa")
	hold := cr.seam.HoldFirst(t, kit.OpSync, format.SegmentName(w.loc.Segment))
	syncing := cr.syncMarksAsync(context.Background(), f)
	<-hold.Reached()

	cr.corrupt(t, w.c)
	hold.Release()
	answered := mustSyncReturn(t, "Sync", syncing)
	if answered.err != nil {
		t.Fatalf("Sync = %v, want nil: the dropped record's loss is the read's to report", answered.err)
	}
	if len(answered.marks) != 0 {
		t.Errorf("Sync returned marks %+v, want none: the record it named is dropped", answered.marks)
	}

	g := file(2)
	v := cr.appendWrite(t, g, 0, "gggg")
	marks, err := cr.d.Sync(context.Background(), g)
	if err != nil {
		t.Fatalf("the Sync after the drop = %v, want nil", err)
	}
	if len(marks) != 1 {
		t.Fatalf("the Sync after the drop returned %d marks, want the new record's: %+v", len(marks), marks)
	}
	cr.wantMark(t, "the mark after the drop", marks[0], v)
}

// TestSyncReturnsNoMarkWhenTheIndexFallbacksCoveringOffsetWasSwept covers the
// belt on the index fallback's Through source: the worker's completion sweep
// deletes a segment's synced-through offset once no live record remains and
// the segment is not the active one, and a fallback record whose covering
// value is gone would name a live segment proofless, {Through: 0, Offset:
// end} -- the shape finding 1 closed for registry-gone segments. Such a
// candidate is skipped instead. The deletion here is made by hand: production
// cannot reach the state, because every roll seals the segment it leaves, so
// the unsealed segments are the active one, whose offset the sweep spares --
// the belt holds for an ordering some later path (a recycle, a reseed) could
// produce.
func TestSyncReturnsNoMarkWhenTheIndexFallbacksCoveringOffsetWasSwept(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigConfigured(t, 1, func(*Config) {})
	r.setLatest()
	r.d.Start(r.streams)
	f := file(1)
	a, _ := r.publish(t, f, 0, "ffff")
	if marks, err := r.d.Sync(context.Background(), f); err != nil || len(marks) != 1 {
		t.Fatalf("the first Sync = %v, %d marks, want nil error and one", err, len(marks))
	}

	st := r.d.made[0]
	st.mu.Lock()
	if _, have := st.synced[a.Segment]; !have {
		st.mu.Unlock()
		t.Fatalf("segment %d has no synced-through offset after the sync that covered the record", a.Segment)
	}
	delete(st.synced, a.Segment)
	st.mu.Unlock()

	marks, err := r.d.Sync(context.Background(), f)
	if err != nil {
		t.Fatalf("the second Sync = %v, want nil: nothing unsynced, so no sync runs", err)
	}
	if len(marks) != 0 {
		t.Errorf("the second Sync returned marks %+v, want none: the fallback record's covering synced-through offset is gone, so no honest mark can name it", marks)
	}
}
