package durability

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// durableHeaders returns the headers of every record the segments would hold
// after a crash now, with each loss record's body, in segment then file order.
func (r *rig) durableHeaders(t *testing.T) ([]format.RecordHeader, []format.RecordBody) {
	t.Helper()
	var hs []format.RecordHeader
	var bs []format.RecordBody
	for id := core.SegmentID(1); id < 32; id++ {
		raw, err := r.seam.Durable(format.SegmentName(id))
		if err != nil {
			continue
		}
		off := int64(format.SegmentHeaderSize)
		for off+format.RecordHeaderSize <= int64(len(raw)) {
			h, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], rigIdentity, id, off)
			if err != nil {
				break
			}
			end := off + format.RecordHeaderSize + h.BodySize()
			if end > int64(len(raw)) {
				t.Fatalf("record at %d of segment %d runs past the durable bytes", off, id)
			}
			body, err := format.VerifyRecordBody(raw[off+format.RecordHeaderSize:end], rigIdentity, h)
			if err != nil {
				t.Fatalf("record at %d of segment %d: %v", off, id, err)
			}
			hs = append(hs, h)
			bs = append(bs, body)
			off += h.Size()
		}
	}
	return hs, bs
}

// appendHeaderOnly appends one header-only record of f with the given kind,
// sequence number and version, and returns where it landed.
func (r *rig) appendHeaderOnly(t *testing.T, f core.FileID, k format.Kind, ext core.Extent, seq core.Seq, v core.Version, body format.RecordBody) segment.Location {
	t.Helper()
	h := format.RecordHeader{Kind: k, File: f, Extent: ext, Seq: seq, Version: v, Tag: 1}
	p, err := r.streams.For(f).Append(h, body)
	if err != nil {
		t.Fatalf("Append of %v: %v", k, err)
	}
	return p.Loc
}

// movedLen returns how many re-appended records the stream still maps.
func (r *rig) movedLen(no int) int {
	st := r.d.made[uint32(no)]
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.moved)
}

// TestReappendHeaderOnlyWindow covers a window of a removal, a release and an
// unmark: it is re-appended whole, each record keeping its sequence number and
// version at a new place, with no loss, no event and no generation raised, and
// the window resolved.
func TestReappendHeaderOnlyWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	type rec struct {
		k   format.Kind
		ext core.Extent
		seq core.Seq
		v   core.Version
	}
	want := []rec{
		{format.KindDelete, core.Extent{}, 101, core.Version{Counter: 21}},
		{format.KindRelease, core.Extent{Off: 8, Len: 4}, 102, core.Version{Counter: 22}},
		{format.KindUnmark, core.Extent{Off: 16, Len: 4}, 103, core.Version{Counter: 23}},
	}
	var first segment.Location
	for i, w := range want {
		loc := fr.appendHeaderOnly(t, f, w.k, w.ext, w.seq, w.v, format.RecordBody{})
		if i == 0 {
			first = loc
		}
	}
	gen := fr.d.Generation()
	fr.failSyncOf(t, first.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v, want nil: the records were re-appended", err)
	}
	if got := fr.awaitResolved(t); got != first.Segment {
		t.Fatalf("resolved segment %d, want %d", got, first.Segment)
	}

	hs, _ := fr.durableHeaders(t)
	if len(hs) != len(want) {
		t.Fatalf("%d durable records, want %d: %+v", len(hs), len(want), hs)
	}
	for i, w := range want {
		h := hs[i]
		if h.Kind != w.k || h.Seq != w.seq || h.Version != w.v || h.File != f || h.Extent != w.ext {
			t.Errorf("durable record %d = %+v, want kind %v seq %d version %+v", i, h, w.k, w.seq, w.v)
		}
		if h.Segment == first.Segment {
			t.Errorf("durable record %d is still in the failed segment %d", i, h.Segment)
		}
	}
	if got := fr.d.Generation(); got != gen {
		t.Errorf("the loss generation moved from %d to %d, a re-append raises none", gen, got)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("%d loss events published, want none: %+v", len(ev), ev)
	}
	if ls := fr.lossRecordsOnDisk(t); len(ls) != 0 {
		t.Errorf("%d loss records written, want none", len(ls))
	}
	if ws := fr.windowsOf(0); len(ws) != 0 {
		t.Errorf("windows still pending: %+v", ws)
	}
}

// TestSyncOfAFileWithAHeaderOnlyRecordInAFailedWindowSucceeds covers the
// Sync: it returns nil once the new segment syncs, never a SyncError, and the
// next Sync of the file returns nil too.
func TestSyncOfAFileWithAHeaderOnlyRecordInAFailedWindowSucceeds(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a := fr.appendKind(t, f, format.KindDelete, format.RecordBody{})
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		var se *core.SyncError
		if errors.As(err, &se) {
			t.Fatalf("Sync answered the failure of segment %d, want nil", se.Segment)
		}
		t.Fatalf("Sync = %v, want nil", err)
	}
	if err := mustReturn(t, "second Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("the second Sync = %v, want nil: no wedge", err)
	}
}

// holdNewSegmentSync makes the sync of the segment after seg wait at the seam,
// where a re-appended record lands.
func (r *rig) holdNewSegmentSync(t *testing.T, seg core.SegmentID) *kit.Hold {
	t.Helper()
	return r.seam.Hold(t, kit.OpSync, format.SegmentName(seg+1))
}

// TestWaitDurableFollowsTheReappendedRecord covers a durability wait on the
// old place: it returns nil only after the new segment syncs, the old place
// reads not durable until then, and the stream forgets the move afterwards.
func TestWaitDurableFollowsTheReappendedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a := fr.appendKind(t, f, format.KindDelete, format.RecordBody{})
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))

	done := make(chan error, 1)
	go func() { done <- fr.d.WaitDurable(a) }()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the new segment was never synced")
	}
	mustBlock(t, "WaitDurable", done)
	if fr.d.Durable(a) {
		t.Error("the old place reads durable before the new segment synced")
	}
	hold.Release()
	if err := mustReturn(t, "WaitDurable", done); err != nil {
		t.Fatalf("WaitDurable = %v, want nil", err)
	}
	eventually(t, "the old place to read durable", func() bool { return fr.d.Durable(a) })
	eventually(t, "the move to be forgotten", func() bool { return fr.movedLen(0) == 0 })
}

// TestWaitDurableReturnsErrClosedOnStop covers a wait on a re-appended record
// whose new segment has not synced: Stop ends it with core.ErrClosed.
func TestWaitDurableReturnsErrClosedOnStop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a := fr.appendKind(t, f, format.KindDelete, format.RecordBody{})
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))

	done := make(chan error, 1)
	go func() { done <- fr.d.WaitDurable(a) }()
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the new segment was never synced")
	}
	stopped := make(chan struct{})
	go func() { fr.d.Stop(); close(stopped) }()
	if err := mustReturn(t, "WaitDurable", done); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("WaitDurable = %v, want core.ErrClosed", err)
	}
	hold.Release()
	<-stopped
}

// TestReappendIntoAStoppedStreamFailsTheRecordReadOnly covers a re-append
// whose new segment fails too: the second segment's first sync fails, which
// stops the stream and so turns the journal read-only. The record the first
// window moved is then in a second window, and the read-only pass fails it,
// undoing it, rather than keeping it for a re-append the stopped stream would
// refuse: the Sync waiting on it returns the refusal and the record never
// reads durable at either place.
func TestReappendIntoAStoppedStreamFailsTheRecordReadOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a := fr.appendKind(t, f, format.KindDelete, format.RecordBody{})
	if err := fr.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("boom 1")),
		kit.Failing(kit.OpSync, format.SegmentName(a.Segment+1), 1, errors.New("boom 2")),
	); err != nil {
		t.Fatal(err)
	}
	ch := fr.syncAsync(context.Background(), f)

	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want the first window's %d", got, a.Segment)
	}
	if err := mustReturn(t, "Sync", ch); !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("Sync = %v, want core.ErrJournalReadOnly", err)
	}
	eventually(t, "the second window to be failed", func() bool { return len(fr.windowsOf(0)) == 0 })
	st := fr.d.made[0]
	st.mu.Lock()
	to := st.followLocked(a)
	l, lost := st.lost[to]
	st.mu.Unlock()
	if to.Segment != a.Segment+1 {
		t.Errorf("the record leads to segment %d, want the second segment %d", to.Segment, a.Segment+1)
	}
	if !lost || !l.undone {
		t.Errorf("the record at its new place is lost = %v, undone = %v, want it undone by the read-only pass", lost, l.undone)
	}
	if fr.d.Durable(a) || fr.d.Durable(to) {
		t.Error("the record reads durable though no segment holds it synced")
	}
}

// TestReappendOfARecordThatCannotBeAppendedIsRetriedByTheNextPass covers a
// record whose re-append fails for a reason the next pass may not meet: it
// stays in its window pass after pass, with no announcement and no move, and
// lands once the record can be appended.
func TestReappendOfARecordThatCannotBeAppendedIsRetriedByTheNextPass(t *testing.T) {
	prof.Watch(t, prof.Options{})
	// No worker runs, so the test takes the window and resolves it itself.
	r := newRigUnstarted(t, 1)
	st := r.d.made[0]
	// Start binds the stream; the test does the same without the worker.
	st.s = r.streams.Stream(0)
	r.streams.OnEnd(r.d.onEnd)
	var announced []core.SegmentID
	r.d.resolved = func(_ uint32, seg core.SegmentID) { announced = append(announced, seg) }
	f := file(1)
	a := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	eventually(t, "the end to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) == 1
	})
	r.d.takeWindows(st)

	// The kept bytes no longer verify, as a record the stream cannot place.
	st.mu.Lock()
	good := st.windows[0].entries[0].w.Bytes
	bad := append([]byte(nil), good...)
	bad[format.RecordHeaderSize-1] ^= 0xff
	st.windows[0].entries[0].w.Bytes = bad
	st.mu.Unlock()
	for range 2 {
		r.d.resolveWindows(st)
		if ws := r.windowsOf(0); len(ws) != 1 || len(ws[0].locs) != 1 {
			t.Fatalf("windows = %+v after a refused pass, want the record pending", ws)
		}
		if len(announced) != 0 || r.movedLen(0) != 0 {
			t.Fatalf("a refused record was announced %v or moved (%d)", announced, r.movedLen(0))
		}
		if r.d.arm(st) == nil {
			t.Fatal("no tick armed for a window with a record still pending")
		}
	}

	st.mu.Lock()
	st.windows[0].entries[0].w.Bytes = good
	st.mu.Unlock()
	r.d.resolveWindows(st)
	if ws := r.windowsOf(0); len(ws) != 0 {
		t.Fatalf("windows = %+v after the retry, want none", ws)
	}
	if len(announced) != 1 || announced[0] != a.Segment {
		t.Errorf("announced %v, want the failed segment %d once", announced, a.Segment)
	}
	if n := r.ledgerLen(0); n != 1 {
		t.Errorf("ledger holds %d entries, want the re-appended record", n)
	}
	if r.d.Durable(a) {
		t.Error("the old place reads durable before the new segment synced")
	}
}

// TestALossRecordInAFailedWindowIsReappendedWithoutAnEvent covers a loss
// record caught in a failed window: it is written again, with the record it
// names, and its re-append is no new loss, so no event is published and the
// generation stays.
func TestALossRecordInAFailedWindowIsReappendedWithoutAnEvent(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	body := format.RecordBody{Loss: format.Loss{Dropped: 77, Synced: true, Reason: format.LossCorrupt}}
	a := fr.appendHeaderOnly(t, f, format.KindLoss, core.Extent{Off: 4, Len: 2}, 120, core.Version{Counter: 5}, body)
	gen := fr.d.Generation()
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	fr.awaitResolved(t)

	hs, bs := fr.durableHeaders(t)
	if len(hs) != 1 {
		t.Fatalf("%d durable records, want the loss record alone: %+v", len(hs), hs)
	}
	if h := hs[0]; h.Kind != format.KindLoss || h.Seq != 120 || h.Segment == a.Segment {
		t.Errorf("durable record = %+v, want the loss record, seq 120, in a new segment", h)
	}
	if bs[0].Loss != body.Loss {
		t.Errorf("loss body = %+v, want %+v", bs[0].Loss, body.Loss)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("%d loss events published, want none: %+v", len(ev), ev)
	}
	if got := fr.d.Generation(); got != gen {
		t.Errorf("the loss generation moved from %d to %d", gen, got)
	}
}

// TestAWindowOfBothKindsFailsTheContentAndReappendsTheRest covers a window
// holding a write and a removal of one file: the write is failed and named,
// the removal is re-appended, and the file's Sync answers the write's loss
// once, then returns nil.
func TestAWindowOfBothKindsFailsTheContentAndReappendsTheRest(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	w, _ := fr.publish(t, f, 0, "aaaa")
	fr.appendHeaderOnly(t, f, format.KindDelete, core.Extent{}, 130, core.Version{Counter: 40}, format.RecordBody{})
	fr.failSyncOf(t, w.Segment, errors.New("sync boom"))
	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), w.Segment, f)
	if got := fr.awaitResolved(t); got != w.Segment {
		t.Fatalf("resolved segment %d, want %d", got, w.Segment)
	}
	if err := mustReturn(t, "second Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("the second Sync = %v, want nil", err)
	}
	if ev := fr.events(); len(ev) != 1 {
		t.Errorf("%d loss events, want the write's alone: %+v", len(ev), ev)
	}
	hs, _ := fr.durableHeaders(t)
	deletes := 0
	for _, h := range hs {
		if h.Kind == format.KindDelete && h.Seq == 130 && h.Segment != w.Segment {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("%d durable removals at a new place, want 1: %+v", deletes, hs)
	}
}

// keptByItsFailedWindow makes the header-only record at loc, of file f, one a
// failed window keeps: its kept bytes are cut to a bare header, so no pass can
// re-append it, and its segment's sync is failed by a Sync of f, which the
// helper returns still waiting, as the record it targets stays pending.
func (r *rig) keptByItsFailedWindow(t *testing.T, f core.FileID, loc segment.Location) <-chan error {
	t.Helper()
	st := r.d.made[0]
	st.mu.Lock()
	i := slices.IndexFunc(st.ledger, func(e entry) bool { return e.w.Loc == loc })
	if i < 0 {
		st.mu.Unlock()
		t.Fatalf("the record at %+v is not in the ledger", loc)
	}
	st.ledger[i].w.Bytes = make([]byte, format.RecordHeaderSize)
	st.mu.Unlock()
	r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	sync := r.syncAsync(context.Background(), f)
	eventually(t, "the failed window to keep the record", func() bool {
		ws := r.windowsOf(0)
		return len(ws) == 1 && slices.Contains(ws[0].locs, loc)
	})
	mustBlock(t, "Sync of a record a failed window keeps", sync)
	return sync
}

// rereadSeam is a test seam for the re-read of a failed segment: a place with
// a verdict gets that verdict's bytes or error, every other read goes to the
// stream.
type rereadSeam struct {
	mu   sync.Mutex
	st   func() *stream
	over map[segment.Location]func(p []byte) (int, error)
}

func (s *rereadSeam) set(loc segment.Location, f func(p []byte) (int, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.over == nil {
		s.over = map[segment.Location]func(p []byte) (int, error){}
	}
	s.over[loc] = f
}

func (s *rereadSeam) read(seg core.SegmentID, p []byte, off int64) (int, error) {
	s.mu.Lock()
	var f func(p []byte) (int, error)
	for loc, g := range s.over {
		if loc.Segment == seg && loc.Off == off {
			f = g
		}
	}
	s.mu.Unlock()
	if f != nil {
		return f(p)
	}
	return s.st().s.ReadAt(seg, p, off)
}

// newRereadRig is a fail rig whose re-read goes through the returned seam.
func newRereadRig(t *testing.T) (*failRig, *rereadSeam) {
	t.Helper()
	s := &rereadSeam{}
	var fr *failRig
	s.st = func() *stream { return fr.d.made[0] }
	fr = newFailRig(t, func(fr *failRig) { fr.d.readCopyAt = s.read })
	return fr, s
}

// writesOutside returns the durable write records that lie outside the
// segment seg, by sequence number.
func (r *rig) writesOutside(t *testing.T, seg core.SegmentID) map[core.Seq]format.RecordHeader {
	t.Helper()
	hs, _ := r.durableHeaders(t)
	out := map[core.Seq]format.RecordHeader{}
	for _, h := range hs {
		if h.Kind == format.KindWrite && h.Segment != seg {
			if _, dup := out[h.Seq]; dup {
				t.Errorf("seq %d is durable twice outside segment %d", h.Seq, seg)
			}
			out[h.Seq] = h
		}
	}
	return out
}

// TestReappendContentVerifiesAgainstTheKeptChecksum covers a window of two
// writes whose segment's bytes read back whole: both are re-appended with the
// bytes read back, at their original sequence numbers and versions, with no
// loss and no generation raised.
func TestReappendContentVerifiesAgainstTheKeptChecksum(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f, g := file(1), file(2)
	a, va := fr.publish(t, f, 0, "aaaa")
	seqA := core.Seq(fr.lastSeq.Load())
	b, vb := fr.publish(t, g, 4, "bbbbbb")
	seqB := core.Seq(fr.lastSeq.Load())
	if a.Segment != b.Segment {
		t.Fatal("the records did not share a segment")
	}
	gen := fr.d.Generation()
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f, g)); err != nil {
		t.Fatalf("Sync = %v, want nil: both writes were re-appended", err)
	}
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	hs, bs := fr.durableHeaders(t)
	if len(hs) != 2 {
		t.Fatalf("%d durable records, want 2: %+v", len(hs), hs)
	}
	want := []struct {
		f       core.FileID
		seq     core.Seq
		v       core.Version
		ext     core.Extent
		payload string
	}{
		{f, seqA, va, core.Extent{Off: 0, Len: 4}, "aaaa"},
		{g, seqB, vb, core.Extent{Off: 4, Len: 6}, "bbbbbb"},
	}
	for i, w := range want {
		h := hs[i]
		if h.Kind != format.KindWrite || h.File != w.f || h.Seq != w.seq || h.Version != w.v || h.Extent != w.ext || h.Segment == a.Segment {
			t.Errorf("durable record %d = %+v, want a write of %v seq %d version %+v extent %+v in a new segment", i, h, w.f, w.seq, w.v, w.ext)
		}
		if string(bs[i].Payload) != w.payload {
			t.Errorf("durable record %d payload = %q, want %q", i, bs[i].Payload, w.payload)
		}
	}
	if got := fr.d.Generation(); got != gen {
		t.Errorf("the loss generation moved from %d to %d, a re-append raises none", gen, got)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("%d loss events published, want none: %+v", len(ev), ev)
	}
}

// TestReappendDoesNotTrustPageCache covers a re-read that returns stale bytes
// for one record, bytes whose header and checksum verify against themselves:
// the kept checksum refuses them, the record's part fails with a loss event,
// a mark and one raise of the generation, and the other record is re-appended.
func TestReappendDoesNotTrustPageCache(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr, seam := newRereadRig(t)
	f, g := file(1), file(2)
	a, _ := fr.publish(t, f, 0, "aaaa")
	seqA := core.Seq(fr.lastSeq.Load())
	fr.publish(t, g, 0, "bbbb")
	seqB := core.Seq(fr.lastSeq.Load())
	seam.set(a, func(p []byte) (int, error) {
		real := make([]byte, len(p))
		if _, err := fr.d.made[0].s.ReadAt(a.Segment, real, a.Off); err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt: %v", err)
		}
		h, err := format.DecodeRecordHeader(real[:format.RecordHeaderSize], rigIdentity, a.Segment, a.Off)
		if err != nil {
			t.Errorf("DecodeRecordHeader: %v", err)
		}
		stale := format.AppendRecord(nil, rigIdentity, h, format.RecordBody{Payload: []byte("ZZZZ"), MTime: 1})
		return copy(p, stale), nil
	})
	gen := fr.d.Generation()
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f, g)), a.Segment, f)
	fr.awaitResolved(t)
	if err := mustReturn(t, "Sync of the other file", fr.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync of the re-appended file = %v, want nil", err)
	}
	if got := fr.d.Generation(); got != gen+1 {
		t.Errorf("generation = %d, want %d: raised once", got, gen+1)
	}
	if ev := fr.events(); len(ev) != 1 || ev[0].File != f {
		t.Errorf("loss events = %+v, want the stale record's alone", ev)
	}
	if ls := fr.lossRecordsOnDisk(t); len(ls) != 1 {
		t.Errorf("%d loss records on disk, want 1", len(ls))
	}
	out := fr.writesOutside(t, a.Segment)
	if _, ok := out[seqA]; ok {
		t.Errorf("the stale record seq %d was re-appended", seqA)
	}
	if _, ok := out[seqB]; !ok {
		t.Errorf("the verified record seq %d was not re-appended: %+v", seqB, out)
	}
}

// TestAMixedWindowIsPartReappendedPartFailed covers a window of two files, one
// whose record cannot be re-read: the generation is raised once, only that
// file is marked, and the other's Sync returns nil.
func TestAMixedWindowIsPartReappendedPartFailed(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr, seam := newRereadRig(t)
	f, g := file(1), file(2)
	a, _ := fr.publish(t, f, 0, "aaaa")
	fr.publish(t, g, 0, "bbbb")
	seam.set(a, func([]byte) (int, error) { return 0, syscall.EIO })
	gen := fr.d.Generation()
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	chG := fr.syncAsync(context.Background(), g)
	wantSyncError(t, mustReturn(t, "Sync of f", fr.syncAsync(context.Background(), f)), a.Segment, f)
	if err := mustReturn(t, "Sync of g", chG); err != nil {
		var se *core.SyncError
		if errors.As(err, &se) && slices.Contains(se.Files, g) {
			t.Fatalf("g was marked failed: %v", err)
		}
	}
	fr.awaitResolved(t)
	if err := mustReturn(t, "second Sync of g", fr.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync of the re-appended file = %v, want nil", err)
	}
	if got := fr.d.Generation(); got != gen+1 {
		t.Errorf("generation = %d, want %d", got, gen+1)
	}
	if ev := fr.events(); len(ev) != 1 || ev[0].File != f {
		t.Errorf("loss events = %+v, want f's alone", ev)
	}
}

// TestReappendReadErrorFailsThePart covers a re-read that fails: an I/O error,
// a short read and a segment that is gone each leave the record unverified,
// so its part fails.
func TestReappendReadErrorFailsThePart(t *testing.T) {
	cases := map[string]func(p []byte) (int, error){
		"EIO":        func([]byte) (int, error) { return 0, syscall.EIO },
		"short read": func(p []byte) (int, error) { return len(p) / 2, nil },
		"gone":       func([]byte) (int, error) { return 0, segment.ErrSegmentGone },
	}
	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			fr, seam := newRereadRig(t)
			f := file(1)
			a, _ := fr.publish(t, f, 0, "aaaa")
			seam.set(a, read)
			gen := fr.d.Generation()
			fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
			wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), a.Segment, f)
			fr.awaitResolved(t)
			if got := fr.d.Generation(); got != gen+1 {
				t.Errorf("generation = %d, want %d", got, gen+1)
			}
			if ev := fr.events(); len(ev) != 1 {
				t.Errorf("%d loss events, want 1: %+v", len(ev), ev)
			}
			if out := fr.writesOutside(t, a.Segment); len(out) != 0 {
				t.Errorf("a record that did not verify was re-appended: %+v", out)
			}
		})
	}
}
