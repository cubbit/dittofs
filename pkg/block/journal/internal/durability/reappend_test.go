package durability

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
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
	return newRereadRigWith(t, nil)
}

// newRereadRigWith is newRereadRig with pre run on the rig before it starts.
func newRereadRigWith(t *testing.T, pre func(*failRig)) (*failRig, *rereadSeam) {
	t.Helper()
	s := &rereadSeam{}
	var fr *failRig
	s.st = func() *stream { return fr.d.made[0] }
	fr = newFailRig(t, func(fr *failRig) {
		fr.d.readCopyAt = s.read
		if pre != nil {
			pre(fr)
		}
	})
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

// TestReappendRefusesAHeaderThatNamesAnotherVersion covers a re-read whose
// header and payload verify but carry another version than the ledger kept:
// the record is failed, not re-appended under the wrong version.
func TestReappendRefusesAHeaderThatNamesAnotherVersion(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr, seam := newRereadRig(t)
	f := file(1)
	a, _ := fr.publish(t, f, 0, "aaaa")
	seam.set(a, func(p []byte) (int, error) {
		real := make([]byte, len(p))
		_, _ = fr.d.made[0].s.ReadAt(a.Segment, real, a.Off)
		h, err := format.DecodeRecordHeader(real[:format.RecordHeaderSize], rigIdentity, a.Segment, a.Off)
		if err != nil {
			t.Errorf("DecodeRecordHeader: %v", err)
		}
		h.Version.Counter++
		return copy(p, format.AppendRecord(nil, rigIdentity, h, format.RecordBody{Payload: []byte("aaaa"), MTime: 1})), nil
	})
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), a.Segment, f)
	fr.awaitResolved(t)
	if out := fr.writesOutside(t, a.Segment); len(out) != 0 {
		t.Errorf("a record with another version was re-appended: %+v", out)
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
		t.Fatalf("Sync of the re-appended file = %v, want nil", err)
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
	// real reads the record's true bytes, so each case differs from a good
	// read only in the fault it adds.
	cases := map[string]func(p []byte, real func([]byte) (int, error)) (int, error){
		"EIO": func(p []byte, real func([]byte) (int, error)) (int, error) {
			_, _ = real(p)
			return 0, syscall.EIO
		},
		"short read": func(p []byte, real func([]byte) (int, error)) (int, error) {
			_, _ = real(p)
			return len(p) / 2, nil
		},
		"gone": func([]byte, func([]byte) (int, error)) (int, error) { return 0, segment.ErrSegmentGone },
	}
	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			fr, seam := newRereadRig(t)
			f := file(1)
			a, _ := fr.publish(t, f, 0, "aaaa")
			seam.set(a, func(p []byte) (int, error) {
				return read(p, func(q []byte) (int, error) { return fr.d.made[0].s.ReadAt(a.Segment, q, a.Off) })
			})
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

// placeOf returns where the index places the durable record h.
func placeOf(h format.RecordHeader) index.RecordAt {
	return index.RecordAt{Segment: h.Segment, Offset: uint32(h.Offset)}
}

// placeAt returns where the index places the record at loc.
func placeAt(loc segment.Location) index.RecordAt {
	return index.RecordAt{Segment: loc.Segment, Offset: uint32(loc.Off)}
}

// ledgerHolds reports whether the stream's ledger holds a record of f with
// the sequence number seq.
func (r *rig) ledgerHolds(f core.FileID, seq core.Seq) bool {
	st := r.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.ContainsFunc(st.ledger, func(e entry) bool { return e.w.File == f && e.w.Seq == seq })
}

// awaitGate waits for a gate's hook to be reached.
func awaitGate(t *testing.T, what string, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was never reached", what)
	}
}

// wantNotResolved fails the test if a segment was announced resolved.
func (fr *failRig) wantNotResolved(t *testing.T, when string) {
	t.Helper()
	select {
	case seg := <-fr.resolved:
		t.Fatalf("segment %d was announced resolved %s", seg, when)
	default:
	}
}

// TestReappendRepointsTheIndexAndRegistersTheOriginal covers a re-appended
// content record: the index names the new place and no longer the original
// once the copy is appended, the copy table names nothing for the record
// while the new place's sync has not completed, and names the original with
// the checksum the ledger kept once it has, by the time a Sync returns on the
// record; a corrupt new copy read through Corrupt then is served from the
// original.
func TestReappendRepointsTheIndexAndRegistersTheOriginal(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	kept := fr.keptChecksum(t, a)
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	fr.awaitResolved(t)
	if got := fr.placedAt(t, f, 0); got.Segment == a.Segment {
		t.Fatalf("the extent is served from %+v, want the new place once the window resolved", got)
	}
	if cp, ok := fr.d.copyOf(seq); ok {
		t.Fatalf("copy table = %+v before the new place synced, want nothing", cp)
	}
	awaitGate(t, "the new segment's sync", hold.Reached())
	if cp, ok := fr.d.copyOf(seq); ok {
		t.Fatalf("copy table = %+v while the new place's sync is in flight, want nothing", cp)
	}
	hold.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}

	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok {
		t.Fatalf("seq %d was not re-appended", seq)
	}
	at := placeOf(h)
	if !fr.ix.Names(f, at) {
		t.Errorf("the index does not name the re-appended record at %+v", at)
	}
	if fr.ix.Names(f, placeAt(a)) {
		t.Errorf("the index still names the original at %+v", a)
	}
	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Record != at || p.Version != v {
		t.Errorf("Lookup = %+v, %v, want version %+v at %+v", p, ok, v, at)
	}
	if cp, ok := fr.d.copyOf(seq); !ok || cp.loc != a || cp.checksum != kept {
		t.Fatalf("copy table = %+v, %v, want the original at %+v with checksum %#x", cp, ok, a, kept)
	}

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: at, Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != a {
		t.Fatalf("Corrupt of the new copy = %+v, %v, want UseCopy of the original %+v", out, err, a)
	}
}

// TestACorruptReadOfAnUnsyncedReappendedRecordIsNotServedFromTheOriginal
// covers a read that finds the new place of a re-appended record corrupt
// before that place's sync completes: no copy is registered yet, so the read
// is not served from the original in the failed segment, the record leaves
// the ledger unsynced, and nothing is registered for it afterwards either.
func TestACorruptReadOfAnUnsyncedReappendedRecordIsNotServedFromTheOriginal(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	fr.awaitResolved(t)
	at := fr.placedAt(t, f, 0)
	if at.Segment == a.Segment {
		t.Fatalf("the extent is served from %+v, want the new place once the window resolved", at)
	}

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: at, Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind == CorruptUseCopy {
		t.Fatalf("Corrupt of the unsynced new place = %+v, %v, want it not served from a copy", out, err)
	}
	if out.Kind != CorruptDropped {
		t.Errorf("Corrupt of the unsynced new place = %+v, want Dropped", out)
	}
	hold.Release()
	// The dropped record's answer is Corrupt's to decide, not this test's.
	_ = mustReturn(t, "Sync", ch)
	if err := mustReturn(t, "second Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("the second Sync = %v, want nil", err)
	}
	if cp, ok := fr.d.copyOf(seq); ok {
		t.Errorf("copy table = %+v for a record whose new place never completed, want nothing", cp)
	}
}

// TestAReappendWhoseSyncFailsRegistersNothing covers a re-append whose new
// segment's sync then fails and whose new place does not verify on re-read:
// the record is failed at the new place, and its original is never registered
// as a copy. A record of the same window whose new place an earlier sync of
// that segment covered is registered once it completes.
func TestAReappendWhoseSyncFailsRegistersNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var appends atomic.Int32
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.afterAppend = func() {
			if appends.Add(1) == 1 {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f, g := file(1), file(2)
	a1, _ := fr.publish(t, f, 0, "aaaa")
	seq1 := core.Seq(fr.lastSeq.Load())
	kept1 := fr.keptChecksum(t, a1)
	fr.publish(t, g, 0, "bbbb")
	seq2 := core.Seq(fr.lastSeq.Load())
	b := a1.Segment + 1
	// Only the first segment's pages read back: the second record's copy in b
	// does not verify once b fails. b's first sync passes, so that its failure
	// is not the second in a row, which stops the stream.
	fr.keptPages.Store(a1.Segment, struct{}{})
	if err := fr.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(a1.Segment), 1, errors.New("first boom")),
		kit.FailingFrom(kit.OpSync, format.SegmentName(b), 2, 1, errors.New("second boom")),
	); err != nil {
		t.Fatal(err)
	}

	ch := fr.syncAsync(context.Background(), g)
	awaitGate(t, "the first record's append", reached)
	// As in the chain test, the roll out of the failing segment needs its
	// spare ready.
	fr.settlePool(t)
	if err := fr.streams.Stream(0).Sync(); err != nil {
		t.Fatalf("Sync of the new segment = %v, want it to pass", err)
	}
	release()
	wantSyncError(t, mustReturn(t, "Sync", ch), b, g)
	if got := []core.SegmentID{fr.awaitResolved(t), fr.awaitResolved(t)}; got[0] != a1.Segment || got[1] != b {
		t.Fatalf("resolved segments %v, want %d then %d", got, a1.Segment, b)
	}
	if cp, ok := fr.d.copyOf(seq2); ok {
		t.Errorf("copy table = %+v for a re-append whose sync failed, want nothing", cp)
	}
	eventually(t, "the first record's copy to complete", func() bool {
		cp, ok := fr.d.copyOf(seq1)
		return ok && cp.loc == a1 && cp.checksum == kept1
	})
}

// TestACopyInASegmentRetiredBeforeTheNewPlaceSyncsIsNotRegistered covers a
// failed segment retired once its window resolved and while the re-appended
// record's new place is still syncing: the completion that follows registers
// nothing in the retired segment, since Retired, which runs once per segment,
// would never remove it.
func TestACopyInASegmentRetiredBeforeTheNewPlaceSyncsIsNotRegistered(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	a, _ := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	awaitGate(t, "the new segment's sync", hold.Reached())
	if _, _, err := fr.set.ClaimRetirement(a.Segment); err != nil {
		t.Fatalf("ClaimRetirement: %v", err)
	}
	if _, err := fr.set.Retire(a.Segment); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	fr.d.Retired(a.Segment)
	hold.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if cp, ok := fr.d.copyOf(seq); ok {
		t.Errorf("copy table = %+v, a copy in the retired segment %d", cp, a.Segment)
	}
}

// TestAWriteBetweenTheCopyAndItsRepointKeepsTheOverwrite covers a write over
// the record made after its copy is appended and before the repoint: the
// overwrite stays what the index serves, and the record's retained extent,
// which named the failed segment, is repointed to the copy.
func TestAWriteBetweenTheCopyAndItsRepointKeepsTheOverwrite(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	fr := newFailRig(t, func(fr *failRig) { fr.d.afterAppend = hook })
	t.Cleanup(release)
	f := file(1)
	a, v10 := fr.publish(t, f, 0, "aaaa")
	// The overwrite stays unsynced, and its superseded piece retained, until
	// the checks below are done.
	hold := fr.holdNewSegmentSync(t, a.Segment)
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	awaitGate(t, "the copy's append", reached)
	_, v11 := fr.publish(t, f, 0, "bbbb")
	release()
	fr.awaitResolved(t)

	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Version != v11 {
		t.Fatalf("Lookup = %+v, %v, want the overwrite v11", p, ok)
	}
	rs := fr.retainedBy(f, v11)
	if len(rs) != 1 || rs[0].Version != v10 || rs[0].Record.Segment == a.Segment {
		t.Fatalf("retained under v11 = %+v, want v10 at its copy, outside the failed segment %d", rs, a.Segment)
	}
	copyAt := rs[0].Record
	hold.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	hs, _ := fr.durableHeaders(t)
	if !slices.ContainsFunc(hs, func(h format.RecordHeader) bool { return h.Version == v10 && placeOf(h) == copyAt }) {
		t.Fatalf("no durable copy of v10 sits at %+v, where the retained piece names it", copyAt)
	}
}

// v10UnderV11 publishes v10, fails its segment and, before the window is
// taken, publishes v11 over it into the next segment, after a synced record
// of another file, and v10 is then re-appended there. Once v10's segment is announced resolved, and before the worker
// syncs the next segment, it runs between, then fails that sync: v11 and v10's
// copy are both in that window, v11's re-read fails, so v11 is failed and
// v10's copy re-appended again. It returns the rig, v10's original place,
// v11's place, v10's version, what the index counted in v10's segment when it
// was announced resolved, and the Sync of the file that led the first failure.
func v10UnderV11(t *testing.T, between func(fr *failRig, a segment.Location)) (fr *failRig, a, b segment.Location, v10 core.Version, atResolve index.SegmentBytes, led <-chan error) {
	t.Helper()
	take, taking, released := gate(t)
	pause, paused, resume := gate(t)
	var armed atomic.Bool
	var mu sync.Mutex
	counted := map[core.SegmentID]index.SegmentBytes{}
	fr, seam := newRereadRigWith(t, func(fr *failRig) {
		fr.d.beforeTakeWindow = take
		fr.d.afterHolds = func() {
			if armed.Load() {
				pause()
			}
		}
		fr.d.resolved = func(_ uint32, seg core.SegmentID) {
			mu.Lock()
			counted[seg] = fr.ix.SegmentBytes(seg)
			mu.Unlock()
			fr.resolved <- seg
		}
	})
	t.Cleanup(released)
	t.Cleanup(resume)
	f := file(1)
	a, v10 = fr.publish(t, f, 0, "aaaa")
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	led = fr.syncAsync(context.Background(), f)
	awaitGate(t, "the window's take", taking)
	// A record of another file opens the next segment and is synced there, so
	// the later failure of that segment is not the second in a row, which
	// would stop the stream.
	g, _ := fr.publish(t, file(2), 0, "gggg")
	fr.syncStream(t, file(2))
	b, _ = fr.publish(t, f, 0, "bbbb")
	if b.Segment == a.Segment || b.Segment != g.Segment {
		t.Fatalf("v11 landed in segment %d, want the one after the failed %d", b.Segment, a.Segment)
	}
	seam.set(b, func([]byte) (int, error) { return 0, syscall.EIO })
	armed.Store(true)
	released()
	awaitGate(t, "the end of the window's pass", paused)
	select {
	case got := <-fr.resolved:
		if got != a.Segment {
			t.Fatalf("resolved segment %d, want %d", got, a.Segment)
		}
	default:
		t.Fatalf("segment %d was not announced resolved in the pass that re-appended its record", a.Segment)
	}
	mu.Lock()
	atResolve = counted[a.Segment]
	mu.Unlock()
	if between != nil {
		between(fr, a)
	}
	// The re-append out of v11's failed segment rolls into a spare, and a
	// refusal for a missing one is retried only at a tick of the clock, which
	// this test does not drive.
	fr.settlePool(t)
	fr.failSyncOf(t, b.Segment, errors.New("sync boom"))
	resume()
	return fr, a, b, v10, atResolve, led
}

// failV11 waits for v11's window to fail, which the Sync that led the first
// failure answers, and checks that the index serves v10 again from a copy
// outside both failed segments, whose bytes read back as v10's.
func failV11(t *testing.T, fr *failRig, a, b segment.Location, v10 core.Version, led <-chan error) {
	t.Helper()
	f := file(1)
	wantSyncError(t, mustReturn(t, "Sync", led), b.Segment, f)
	if got := fr.awaitResolved(t); got != b.Segment {
		t.Fatalf("resolved segment %d, want v11's %d", got, b.Segment)
	}
	if err := mustReturn(t, "second Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("the second Sync = %v, want nil", err)
	}
	p, ok := fr.ix.Lookup(f, 0)
	if !ok || p.Version != v10 {
		t.Fatalf("Lookup = %+v, %v, want v10 held again", p, ok)
	}
	if p.Record.Segment == a.Segment || p.Record.Segment == b.Segment {
		t.Fatalf("v10 is held at %+v, in a failed segment (%d or %d)", p.Record, a.Segment, b.Segment)
	}
	head := make([]byte, format.RecordHeaderSize)
	if _, err := fr.d.made[0].s.ReadAt(p.Record.Segment, head, int64(p.Record.Offset)); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading v10's header where the index holds it: %v", err)
	}
	h, err := format.DecodeRecordHeader(head, rigIdentity, p.Record.Segment, int64(p.Record.Offset))
	if err != nil || h.Version != v10 {
		t.Fatalf("the record the index holds v10 at decodes as %+v, %v", h, err)
	}
	buf := make([]byte, h.Size())
	if _, err := fr.d.made[0].s.ReadAt(p.Record.Segment, buf, int64(p.Record.Offset)); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading v10 where the index holds it: %v", err)
	}
	body, err := format.VerifyRecordBody(buf[format.RecordHeaderSize:format.RecordHeaderSize+h.BodySize()], rigIdentity, h)
	if err != nil || string(body.Payload) != "aaaa" {
		t.Fatalf("the record the index holds v10 at carries %q, %v, want v10's bytes", body.Payload, err)
	}
}

// TestV10ReappendedThenV11WindowFailedReadsV10FromTheCopy covers v10 in a
// failed window, v11 written over it and not yet synced, v10 re-appended and
// then v11's window failed: v10's failed segment is announced resolved backing
// no extent, and the read serves v10 from its copy.
func TestV10ReappendedThenV11WindowFailedReadsV10FromTheCopy(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr, a, b, v10, atResolve, led := v10UnderV11(t, nil)
	if atResolve != (index.SegmentBytes{}) {
		t.Errorf("segment %d backed %+v when it was announced resolved, want nothing", a.Segment, atResolve)
	}
	failV11(t, fr, a, b, v10, led)
}

// TestV11WindowFailedAfterTheSegmentWasRetired covers the same, with v10's
// failed segment retired once it was announced resolved and before v11's
// window fails: v11's failure holds v10 again at its copy, which reads back,
// never in the retired segment.
func TestV11WindowFailedAfterTheSegmentWasRetired(t *testing.T) {
	prof.Watch(t, prof.Options{})
	retire := func(fr *failRig, a segment.Location) {
		if _, _, err := fr.set.ClaimRetirement(a.Segment); err != nil {
			t.Fatalf("ClaimRetirement: %v", err)
		}
		if _, err := fr.set.Retire(a.Segment); err != nil {
			t.Fatalf("Retire: %v", err)
		}
		fr.d.Retired(a.Segment)
	}
	fr, a, b, v10, _, led := v10UnderV11(t, retire)
	failV11(t, fr, a, b, v10, led)
}

// TestAFailedSegmentIsAnnouncedOnlyAfterTheLastRecord covers the order of a
// window's resolution: while its last record's copy is appended and not yet
// repointed, or while its unverified record is being failed, the segment is
// not announced; once announced, the index names nothing in it.
func TestAFailedSegmentIsAnnouncedOnlyAfterTheLastRecord(t *testing.T) {
	t.Run("reappended", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		hook, reached, release := gate(t)
		var appends atomic.Int32
		var mu sync.Mutex
		var counted index.SegmentBytes
		fr := newFailRig(t, func(fr *failRig) {
			fr.d.afterAppend = func() {
				if appends.Add(1) == 2 {
					hook()
				}
			}
			fr.d.resolved = func(_ uint32, seg core.SegmentID) {
				mu.Lock()
				counted = fr.ix.SegmentBytes(seg)
				mu.Unlock()
				fr.resolved <- seg
			}
		})
		t.Cleanup(release)
		f, g := file(1), file(2)
		a, _ := fr.publish(t, f, 0, "aaaa")
		fr.publish(t, g, 0, "bbbb")
		fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
		ch := fr.syncAsync(context.Background(), f, g)
		awaitGate(t, "the last record's append", reached)
		fr.wantNotResolved(t, "before its last record was repointed")
		if ws := fr.windowsOf(0); len(ws) != 1 || len(ws[0].locs) != 1 {
			t.Fatalf("windows = %+v, want the last record still pending", ws)
		}
		release()
		if err := mustReturn(t, "Sync", ch); err != nil {
			t.Fatalf("Sync = %v, want nil", err)
		}
		if got := fr.awaitResolved(t); got != a.Segment {
			t.Fatalf("resolved segment %d, want %d", got, a.Segment)
		}
		mu.Lock()
		defer mu.Unlock()
		if counted != (index.SegmentBytes{}) {
			t.Fatalf("segment %d backed %+v when it was announced resolved, want nothing", a.Segment, counted)
		}
	})
	t.Run("failed", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		hook, reached, release := gate(t)
		fr, seam := newRereadRigWith(t, func(fr *failRig) { fr.d.beforeFail = func(*window) { hook() } })
		t.Cleanup(release)
		f, g := file(1), file(2)
		a, _ := fr.publish(t, f, 0, "aaaa")
		b, _ := fr.publish(t, g, 0, "bbbb")
		seam.set(b, func([]byte) (int, error) { return 0, syscall.EIO })
		fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
		ch := fr.syncAsync(context.Background(), g)
		awaitGate(t, "the unverified record's failure", reached)
		fr.wantNotResolved(t, "before its unverified record was failed")
		release()
		wantSyncError(t, mustReturn(t, "Sync", ch), a.Segment, g)
		if got := fr.awaitResolved(t); got != a.Segment {
			t.Fatalf("resolved segment %d, want %d", got, a.Segment)
		}
	})
}

// TestReappendHoldsNoDomainAcrossAnAppendOrASync covers the lock order over a
// window of many files: no file domain is held when a copy's append returns or
// while the new segment's fsync is in flight, each file's domain is taken once
// before that fsync, for its repoint, in the window's order, and never two at
// once.
func TestReappendHoldsNoDomainAcrossAnAppendOrASync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	var mu sync.Mutex
	var atAppend []int
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.afterAppend = func() {
			mu.Lock()
			defer mu.Unlock()
			atAppend = append(atAppend, fr.domains.heldNow())
		}
	})
	var files []core.FileID
	var first segment.Location
	for i := range 6 {
		f := file(byte(1 + i))
		files = append(files, f)
		loc, _ := fr.publish(t, f, 0, "aaaa")
		if i == 0 {
			first = loc
		}
	}
	hold := fr.holdNewSegmentSync(t, first.Segment)
	fr.domains.mu.Lock()
	start := len(fr.domains.order)
	fr.domains.mu.Unlock()
	fr.failSyncKeepingPages(t, first.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), files...)
	select {
	case <-hold.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the new segment was never synced")
	}
	if n := fr.domains.heldNow(); n != 0 {
		t.Errorf("%d file domains held during the new segment's fsync, want 0", n)
	}
	// The domains the re-append took, before the completion of the copies,
	// which takes each file's domain again once the sync returns.
	fr.domains.mu.Lock()
	taken := slices.Clone(fr.domains.order[start:])
	fr.domains.mu.Unlock()
	hold.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	fr.awaitResolved(t)

	mu.Lock()
	if len(atAppend) != 6 || slices.ContainsFunc(atAppend, func(n int) bool { return n != 0 }) {
		t.Errorf("domains held as each copy's append returned: %v, want six zeros", atAppend)
	}
	mu.Unlock()
	fr.domains.mu.Lock()
	defer fr.domains.mu.Unlock()
	if fr.domains.mostHeld != 1 {
		t.Errorf("%d file domains were held at once, want 1", fr.domains.mostHeld)
	}
	if !slices.Equal(taken, files) {
		t.Errorf("domains taken in order %v, want each file once in the window's order %v", taken, files)
	}
}

// TestACorruptDuringTheRereadAppendsNoCopy covers a record Corrupt takes out
// of the window while the worker reads it again: the worker appends no copy,
// so the only durable trace of the number outside the failed segment is
// Corrupt's loss record.
func TestACorruptDuringTheRereadAppendsNoCopy(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	fr, seam := newRereadRig(t)
	t.Cleanup(release)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	seam.set(a, func(p []byte) (int, error) {
		hook()
		return fr.d.made[0].s.ReadAt(a.Segment, p, a.Off)
	})
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	awaitGate(t, "the re-read", reached)

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: placeAt(a), Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want Dropped", out, err)
	}
	release()
	wantSyncError(t, mustReturn(t, "Sync", ch), a.Segment, f)
	fr.awaitResolved(t)
	if fr.ledgerHolds(f, seq) {
		t.Errorf("the ledger tracks a copy of seq %d", seq)
	}
	fr.d.Stop()
	if h, ok := fr.writesOutside(t, a.Segment)[seq]; ok {
		t.Errorf("a copy of seq %d was appended at %+v after Corrupt took it", seq, placeOf(h))
	}
	n := 0
	for _, l := range fr.lossRecordsOnDisk(t) {
		if l.Dropped == seq {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d loss records name seq %d, want Corrupt's one", n, seq)
	}
}

// TestACorruptAfterTheCopysAppendLeavesItUntracked covers a record Corrupt
// takes out of the window after its copy was appended and before the repoint:
// the copy is not repointed to, and leaves the ledger as a record never
// durable, so no wait and no later window ever handles it.
func TestACorruptAfterTheCopysAppendLeavesItUntracked(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	fr := newFailRig(t, func(fr *failRig) { fr.d.afterAppend = hook })
	t.Cleanup(release)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	awaitGate(t, "the copy's append", reached)

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: placeAt(a), Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want Dropped", out, err)
	}
	release()
	wantSyncError(t, mustReturn(t, "Sync", ch), a.Segment, f)
	fr.awaitResolved(t)

	// Corrupt's loss record follows the copy in the new segment, so once it
	// is durable the copy is too, and a completion pass has had its chance.
	var dup format.RecordHeader
	eventually(t, "Corrupt's loss record", func() bool {
		for _, l := range fr.lossRecordsOnDisk(t) {
			if l.Dropped == seq {
				return true
			}
		}
		return false
	})
	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok {
		t.Fatalf("the copy of seq %d is not durable", seq)
	}
	dup = h
	mark := fr.passes.Load()
	fr.d.made[0].wake()
	fr.awaitPassEnd(t, mark)
	if fr.ledgerHolds(f, seq) {
		t.Errorf("the ledger still tracks the copy of seq %d", seq)
	}
	if fr.ix.Names(f, placeOf(dup)) {
		t.Errorf("the index names the copy at %+v of a record Corrupt dropped", placeOf(dup))
	}
	if fr.d.Durable(segment.Location{Segment: dup.Segment, Off: dup.Offset, Len: dup.Size()}) {
		t.Errorf("the copy of a record Corrupt dropped reads durable")
	}
	if err := mustReturn(t, "second Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("the second Sync = %v, want nil", err)
	}
}

// TestACopyServedRecordInTheWindowIsRepointedFromTheCopy covers a record of a
// pending window that a read served from another copy, which repointed the
// index to that copy, and whose re-read then verifies: it is re-appended, the
// index is repointed from the copy to the new place, the copy table names the
// window's original, and a wait on the old place follows the record.
func TestACopyServedRecordInTheWindowIsRepointedFromTheCopy(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var window atomic.Uint32
	cr := newCorruptRig(t, func(cr *corruptRig) {
		cr.d.readCopyAt = func(seg core.SegmentID, p []byte, off int64) (int, error) {
			if uint32(seg) == window.Load() {
				hook()
			}
			return cr.streams.For(file(1)).ReadAt(seg, p, off)
		}
	})
	t.Cleanup(release)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.RegisterCopy(r.c.Seq, r.orig, r.kept)
	window.Store(uint32(r.again.Segment))
	cr.failSyncOf(t, r.again.Segment, errors.New("sync boom"))
	ch := cr.syncAsync(context.Background(), f)
	awaitGate(t, "the window's re-read", reached)

	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
	}
	if got := cr.placedAt(t, f, 0); got != placeAt(r.orig) {
		t.Fatalf("the extent is served from %+v, want the copy %+v", got, placeAt(r.orig))
	}
	release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil: the record was re-appended", err)
	}
	if got := cr.awaitResolved(t); got != r.again.Segment {
		t.Fatalf("resolved segment %d, want %d", got, r.again.Segment)
	}
	if err := mustReturn(t, "WaitDurable", waitDurableAsync(cr.d, r.again)); err != nil {
		t.Fatalf("WaitDurable on the old place = %v, want nil", err)
	}
	got := cr.placedAt(t, f, 0)
	if got.Segment == r.orig.Segment || got.Segment == r.again.Segment {
		t.Fatalf("the extent is served from %+v, want the new place", got)
	}
	hs, _ := cr.durableHeaders(t)
	if !slices.ContainsFunc(hs, func(h format.RecordHeader) bool { return h.Seq == r.c.Seq && placeOf(h) == got }) {
		t.Errorf("the index serves %+v, where no durable record of seq %d sits", got, r.c.Seq)
	}
	if cp, ok := cr.d.copyOf(r.c.Seq); !ok || cp.loc != r.again {
		t.Errorf("copy table = %+v, %v, want the window's original %+v", cp, ok, r.again)
	}
}

// waitDurableAsync runs WaitDurable on loc and hands its answer back.
func waitDurableAsync(d *Durability, loc segment.Location) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- d.WaitDurable(loc) }()
	return ch
}

// TestReappendReadsTheFailedSegmentThroughTheStream covers the production
// re-read, with no seam: the failed segment is read through the stream, and
// its records, which the storage still returns, are re-appended and served
// from their new place.
func TestReappendReadsTheFailedSegmentThroughTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, func(fr *failRig) { fr.d.readCopyAt = nil })
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	fr.failSyncOf(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v, want nil: the record was re-appended", err)
	}
	fr.awaitResolved(t)
	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok || h.Version != v {
		t.Fatalf("seq %d re-appended as %+v, %v, want version %+v", seq, h, ok, v)
	}
	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Record != placeOf(h) {
		t.Fatalf("Lookup = %+v, %v, want the re-appended record at %+v", p, ok, placeOf(h))
	}
}

// TestACorruptOfTheOldPlaceAfterTheRepointLiftsNothing covers a read that
// found the original corrupt and reaches Corrupt after the record was
// re-appended and repointed, while its copy is not yet synced: the old place
// is not read as a synced record, so what the record superseded stays
// retained, and the index goes on naming the copy.
func TestACorruptOfTheOldPlaceAfterTheRepointLiftsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	f := file(1)
	loc, v1, v2, seq2 := fr.syncedThenOverwritten(t, f, "aaaa", "bbbb")
	hold := fr.holdNewSegmentSync(t, loc.Segment)
	fr.failSyncKeepingPages(t, loc.Segment, errors.New("sync boom"))
	ch := fr.syncAsync(context.Background(), f)
	fr.awaitResolved(t)
	copyAt := fr.placedAt(t, f, 0)
	if copyAt.Segment == loc.Segment {
		t.Fatalf("v2 is still served from the failed segment %d", loc.Segment)
	}

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: placeAt(loc), Seq: seq2, Version: v2, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptDropped {
		t.Fatalf("Corrupt = %+v, %v, want Dropped with nothing to drop", out, err)
	}
	if got := fr.placedAt(t, f, 0); got != copyAt {
		t.Errorf("the extent is served from %+v, want the copy %+v", got, copyAt)
	}
	if rs := fr.retainedBy(f, v2); len(rs) != 1 || rs[0].Version != v1 {
		t.Errorf("retained under v2 = %+v, want v1 kept until v2's copy syncs", rs)
	}
	hold.Release()
	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
}

// TestAReappendWhoseSyncFailsStartsANewWindow covers a re-append whose own
// segment then fails to sync, after an earlier sync of it succeeded: the
// record's copy is a record of a new window, which is re-appended under the
// same rules into the next segment, each failed segment is announced resolved
// once, and a wait on the first place reaches the last one through the chain
// and returns nil once that syncs. The record keeps its sequence number and
// version throughout, and no loss is reported. A record of the same window
// whose copy that earlier sync covered stays where it landed.
func TestAReappendWhoseSyncFailsStartsANewWindow(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var appends atomic.Int32
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.afterAppend = func() {
			if appends.Add(1) == 1 {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f, g := file(1), file(2)
	a1, _ := fr.publish(t, f, 0, "aaaa")
	seq1 := core.Seq(fr.lastSeq.Load())
	kept1 := fr.keptChecksum(t, a1)
	a2, v2 := fr.publish(t, g, 0, "bbbb")
	seq2 := core.Seq(fr.lastSeq.Load())
	kept2 := fr.keptChecksum(t, a2)
	b, c := a1.Segment+1, a1.Segment+2
	// One schedule fails both segments: a second one would replace the first.
	// The new segment's first sync passes, so that its failure is not the
	// second in a row, which stops the stream.
	fr.keptPages.Store(a1.Segment, struct{}{})
	fr.keptPages.Store(b, struct{}{})
	if err := fr.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(a1.Segment), 1, errors.New("first boom")),
		kit.FailingFrom(kit.OpSync, format.SegmentName(b), 2, 1, errors.New("second boom")),
	); err != nil {
		t.Fatal(err)
	}

	wait := waitDurableAsync(fr.d, a2)
	awaitGate(t, "the first record's append", reached)
	// The roll out of the second failing segment needs a spare ready: the
	// refusal of a missing one is retried only at the worker's next tick.
	fr.settlePool(t)
	if err := fr.streams.Stream(0).Sync(); err != nil {
		t.Fatalf("Sync of the new segment = %v, want it to pass", err)
	}
	release()
	if err := mustReturn(t, "WaitDurable on the first place", wait); err != nil {
		t.Fatalf("WaitDurable = %v, want nil: the record reached the third segment", err)
	}
	// The wait returned only once the final segment synced: its copy is
	// durable now, whatever else syncs later.
	if hs, _ := fr.durableHeaders(t); !slices.ContainsFunc(hs, func(h format.RecordHeader) bool {
		return h.Seq == seq2 && h.File == g
	}) {
		t.Fatalf("WaitDurable returned before a record of seq %d was durable", seq2)
	}
	if h, ok := fr.writesOutside(t, b)[seq2]; !ok || placeOf(h).Segment != c {
		t.Fatalf("at WaitDurable's return seq %d is durable as %+v, %v, want segment %d", seq2, h, ok, c)
	}
	if got := []core.SegmentID{fr.awaitResolved(t), fr.awaitResolved(t)}; got[0] != a1.Segment || got[1] != b {
		t.Fatalf("resolved segments %v, want %d then %d", got, a1.Segment, b)
	}
	outside := fr.writesOutside(t, a1.Segment)
	h1, ok := outside[seq1]
	if !ok || placeOf(h1).Segment != b {
		t.Fatalf("seq %d is durable as %+v, %v, want segment %d", seq1, h1, ok, b)
	}
	h2, ok := outside[seq2]
	if !ok || h2.Version != v2 || placeOf(h2).Segment != c {
		t.Fatalf("seq %d is durable as %+v, %v, want version %+v in segment %d", seq2, h2, ok, v2, c)
	}
	if p, ok := fr.ix.Lookup(g, 0); !ok || p.Record != placeOf(h2) || p.Version != v2 {
		t.Fatalf("Lookup = %+v, %v, want version %+v at %+v", p, ok, v2, placeOf(h2))
	}
	if fr.ix.Names(g, placeAt(a2)) {
		t.Errorf("the index still names the first place %+v", a2)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
	if recs := fr.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Errorf("durable loss records = %+v, want none", recs)
	}
	eventually(t, "the chain to be forgotten", func() bool { return fr.movedLen(0) == 0 })
	// Each record's copy is registered once its last place completed: the
	// first record's original, and for the second the place in b its final
	// re-append was read from, never the first place in a1.
	if cp, ok := fr.d.copyOf(seq1); !ok || cp.loc != a1 || cp.checksum != kept1 {
		t.Errorf("copy table for seq %d = %+v, %v, want the original %+v with checksum %#x", seq1, cp, ok, a1, kept1)
	}
	if cp, ok := fr.d.copyOf(seq2); !ok || cp.loc.Segment != b || cp.checksum != kept2 {
		t.Errorf("copy table for seq %d = %+v, %v, want its place in segment %d with checksum %#x", seq2, cp, ok, b, kept2)
	}
}

// TestAReappendedRecordWhoseCopyIsCorruptedIsServedFromTheOriginalThenMoved
// covers a record re-appended out of a failed segment whose new copy then
// reads corrupt: the read is served from the original, which the hold keeps
// claimed, the hold's re-append puts the record in a healthy place the index
// then names, and only after that is the failed segment announced resolved,
// with nothing in it backed by the index.
func TestAReappendedRecordWhoseCopyIsCorruptedIsServedFromTheOriginalThenMoved(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var appends atomic.Int32
	var mu sync.Mutex
	var counted index.SegmentBytes
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.reappend = fr.d.reappendHeld
		// The first append is the failed window's re-append, the second the
		// hold's.
		fr.d.afterAppend = func() {
			if appends.Add(1) == 2 {
				hook()
			}
		}
		fr.d.resolved = func(_ uint32, seg core.SegmentID) {
			mu.Lock()
			counted = fr.ix.SegmentBytes(seg)
			mu.Unlock()
			fr.resolved <- seg
		}
	})
	t.Cleanup(release)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok {
		t.Fatalf("seq %d was not re-appended", seq)
	}
	bad := placeOf(h)

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: bad, Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != a {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy of the original %+v", out, err, a)
	}
	awaitGate(t, "the hold's append", reached)
	if got := fr.placedAt(t, f, 0); got != placeAt(a) {
		t.Fatalf("the extent is served from %+v, want the original %+v", got, placeAt(a))
	}
	fr.wantNotResolved(t, "before the held record was moved")
	release()

	eventually(t, "the index to name the held record's new place", func() bool {
		p, ok := fr.ix.Lookup(f, 0)
		return ok && p.Record != placeAt(a) && p.Record != bad
	})
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	mu.Lock()
	if counted != (index.SegmentBytes{}) {
		t.Errorf("segment %d backed %+v when it was announced resolved, want nothing", a.Segment, counted)
	}
	mu.Unlock()
	if p, ok := fr.ix.Lookup(f, 0); !ok || p.Version != v {
		t.Fatalf("Lookup = %+v, %v, want version %+v", p, ok, v)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
}

// TestAFailedSegmentIsNotAnnouncedWhileAHeldCopyIsNotReappended covers a
// failed segment whose window empties while a read holds a copy of one of its
// records, which an earlier re-append left there: the window's resolution is
// withheld until the hold's re-append lands, and the announcement then comes
// once, with the index naming nothing in the segment.
//
// A copy is registered only once its record's new place completes, which the
// worker does after the pass that re-appended it, so the window is kept
// pending past that pass by a second record the stream refuses: a header-only
// record whose kept bytes do not decode, restored only once f's copy is
// registered and held.
func TestAFailedSegmentIsNotAnnouncedWhileAHeldCopyIsNotReappended(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hookPause, atPause, releasePause := gate(t)
	hookHold, atHold, releaseHold := gate(t)
	var appends atomic.Int32
	var pausing atomic.Bool
	var mu sync.Mutex
	var counted index.SegmentBytes
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.reappend = fr.d.reappendHeld
		// The first append is f's re-append, the second g's once its bytes
		// decode again, the third the hold's.
		fr.d.afterAppend = func() {
			if appends.Add(1) == 3 {
				hookHold()
			}
		}
		fr.d.afterHolds = func() {
			if pausing.Load() {
				hookPause()
			}
		}
		fr.d.resolved = func(_ uint32, seg core.SegmentID) {
			mu.Lock()
			counted = fr.ix.SegmentBytes(seg)
			mu.Unlock()
			fr.resolved <- seg
		}
	})
	t.Cleanup(releasePause)
	t.Cleanup(releaseHold)
	f, g := file(1), file(2)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	ga := fr.appendKind(t, g, format.KindDelete, format.RecordBody{})
	st := fr.d.made[0]
	var good []byte
	st.mu.Lock()
	for i := range st.ledger {
		if e := &st.ledger[i]; e.w.Loc == ga {
			good = e.w.Bytes
			bad := append([]byte(nil), good...)
			bad[format.RecordHeaderSize-1] ^= 0xff
			e.w.Bytes = bad
		}
	}
	st.mu.Unlock()
	if good == nil {
		t.Fatalf("no ledger entry for g's record at %+v", ga)
	}
	fr.failSyncKeepingPages(t, a.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync of f", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync of f = %v, want nil: its record was re-appended", err)
	}
	if cp, ok := fr.d.copyOf(seq); !ok || cp.loc != a {
		t.Fatalf("copy table = %+v, %v, want f's original %+v once its copy completed", cp, ok, a)
	}
	if ws := fr.windowsOf(0); len(ws) != 1 {
		t.Fatalf("windows = %+v, want g's record still pending", ws)
	}
	// The worker is paused after a pass's holds and before its completion,
	// so the read below holds the copy before any pass retries g.
	pausing.Store(true)
	st.wake()
	awaitGate(t, "the worker's pause", atPause)
	pausing.Store(false)

	// f's record is already re-appended and registered; a read of its new
	// copy serves the original and holds it.
	p, ok := fr.ix.Lookup(f, 0)
	if !ok || p.Record == placeAt(a) {
		t.Fatalf("Lookup = %+v, %v, want f's record at its new place", p, ok)
	}
	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: p.Record, Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != a {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy of the original %+v", out, err, a)
	}
	st.mu.Lock()
	for _, w := range st.windows {
		for i := range w.entries {
			if w.entries[i].w.Loc == ga {
				w.entries[i].w.Bytes = good
			}
		}
	}
	st.mu.Unlock()
	ch := fr.syncAsync(context.Background(), f, g)
	releasePause()
	awaitGate(t, "the hold's append", atHold)
	if ws := fr.windowsOf(0); len(ws) != 0 {
		t.Fatalf("windows = %+v, want the window resolved and its announcement withheld", ws)
	}
	fr.wantNotResolved(t, "while a held copy was not re-appended")
	releaseHold()

	if err := mustReturn(t, "Sync", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	mu.Lock()
	if counted != (index.SegmentBytes{}) {
		t.Errorf("segment %d backed %+v when it was announced resolved, want nothing", a.Segment, counted)
	}
	mu.Unlock()
	if got := fr.placedAt(t, f, 0); got.Segment == a.Segment {
		t.Errorf("f is served from %+v, want a place outside the failed segment", got)
	}
	fr.wantNotResolved(t, "twice")
}

// TestACorruptReadOfAMovedPlaceIsNotCountedDamagedThroughUseCopy covers the
// sync state useCopy reads for a record whose place the stream has moved: the
// place was never synced where it was, so serving the record from its copy
// counts no segment damaged.
//
// The state is built by hand: a record's place is in st.moved only after its
// index entry was repointed away, so a read of that place reaches useCopy
// with the index still naming it only if RepointRecord failed, which it
// cannot yet. The index here names the place, and the stream's move table is
// set directly, so the test stays red if useCopy stops asking syncedAt, or
// syncedAt stops following moves.
func TestACorruptReadOfAMovedPlaceIsNotCountedDamagedThroughUseCopy(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cr := newCorruptRig(t, nil)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	cr.d.RegisterCopy(r.c.Seq, r.orig, r.kept)
	// The second copy is synced and complete, so only the move says it was
	// ever re-appended.
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync = %v", err)
	}
	eventually(t, "the second copy to complete", func() bool { return !cr.inLedger(r.again) })
	st := cr.d.made[0]
	st.mu.Lock()
	st.moved[r.again] = segment.Location{Segment: 999, Off: 48, Len: r.again.Len}
	st.mu.Unlock()

	out, err := cr.d.Corrupt(context.Background(), r.c)
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != r.orig {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy at %+v", out, err, r.orig)
	}
	if n, rows := cr.d.damagedReport(); n != 0 || len(rows) != 0 {
		t.Errorf("damaged = %d, %+v, want no segment counted damaged for a moved place", n, rows)
	}
}

// TestAHeldReappendWhoseSyncFailsIsReappendedAgain covers a held record's
// re-append whose own segment then fails to sync, after an earlier sync of it
// passed: the hold has ended at the append, the new copy is a record of a new
// window, and it is re-appended again under the same sequence number and
// version. A wait on the held re-append's place reaches the final place and
// returns only once that syncs, and no loss is reported and no generation is
// raised.
func TestAHeldReappendWhoseSyncFailsIsReappendedAgain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	hook, reached, release := gate(t)
	var appends atomic.Int32
	fr := newFailRig(t, func(fr *failRig) {
		fr.d.reappend = fr.d.reappendHeld
		// The first append is the failed window's re-append, the second the
		// hold's.
		fr.d.afterAppend = func() {
			if appends.Add(1) == 2 {
				hook()
			}
		}
	})
	t.Cleanup(release)
	f := file(1)
	a, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	b, c := a.Segment+1, a.Segment+2
	// The first sync of b, which covers the window's copy, passes, so that the
	// failure of its second is not a second segment in a row.
	fr.keptPages.Store(a.Segment, struct{}{})
	fr.keptPages.Store(b, struct{}{})
	if err := fr.seam.Schedule(
		kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("first boom")),
		kit.FailingFrom(kit.OpSync, format.SegmentName(b), 2, 1, errors.New("second boom")),
	); err != nil {
		t.Fatal(err)
	}
	if err := mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
	if got := fr.awaitResolved(t); got != a.Segment {
		t.Fatalf("resolved segment %d, want %d", got, a.Segment)
	}
	h, ok := fr.writesOutside(t, a.Segment)[seq]
	if !ok || placeOf(h).Segment != b {
		t.Fatalf("seq %d was re-appended as %+v, %v, want segment %d", seq, h, ok, b)
	}
	before := fr.d.Generation()

	out, err := fr.d.Corrupt(context.Background(), Corruption{
		Share: 1, File: f, Record: placeOf(h), Seq: seq, Version: v, Extent: core.Extent{Off: 0, Len: 4},
	})
	if err != nil || out.Kind != CorruptUseCopy || out.Copy != a {
		t.Fatalf("Corrupt = %+v, %v, want UseCopy of the original %+v", out, err, a)
	}
	awaitGate(t, "the hold's append", reached)
	// The hold's copy is the one new ledger entry of the record.
	var first segment.Location
	st := fr.d.made[0]
	st.mu.Lock()
	for _, e := range st.ledger {
		if e.w.Seq == seq {
			first = e.w.Loc
		}
	}
	st.mu.Unlock()
	if first == (segment.Location{}) || first.Segment != b {
		t.Fatalf("the hold's copy is at %+v, want a place in segment %d", first, b)
	}
	wait := waitDurableAsync(fr.d, first)
	// As in the chain test, the roll out of the failing segment needs its
	// spare ready.
	fr.settlePool(t)
	release()

	if err := mustReturn(t, "WaitDurable on the hold's copy", wait); err != nil {
		t.Fatalf("WaitDurable = %v, want nil", err)
	}
	// The wait returned only once segment c synced: the record is durable
	// there now.
	if h, ok := fr.writesOutside(t, b)[seq]; !ok || h.Version != v || placeOf(h).Segment != c {
		t.Fatalf("at WaitDurable's return seq %d is durable as %+v, %v, want version %+v in segment %d", seq, h, ok, v, c)
	}
	p, ok := fr.ix.Lookup(f, 0)
	if !ok || p.Version != v || p.Record.Segment != c {
		t.Fatalf("Lookup = %+v, %v, want version %+v in segment %d", p, ok, v, c)
	}
	// The hold's end announces the original's segment again, then b's window.
	if got := []core.SegmentID{fr.awaitResolved(t), fr.awaitResolved(t)}; got[0] != a.Segment || got[1] != b {
		t.Fatalf("resolved segments %v, want %d then %d", got, a.Segment, b)
	}
	if got := fr.d.Generation() - before; got != 0 {
		t.Errorf("the generation rose by %d, want 0", got)
	}
	if ev := fr.events(); len(ev) != 0 {
		t.Errorf("loss events = %+v, want none", ev)
	}
	if recs := fr.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Errorf("durable loss records = %+v, want none", recs)
	}
}
