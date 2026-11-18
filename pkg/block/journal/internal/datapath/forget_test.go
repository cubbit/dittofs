package datapath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// forgetTag is the share tag the forget tests write under; the strand tests'
// otherTag names the second share a cross-share check reaches through.
const forgetTag core.ShareTag = 0x40

// forgetDeadline bounds the polls the forget tests run against state only
// the journal's workers move: a bounded wait on the real condition, with one
// generous backstop.
const forgetDeadline = 5 * time.Second

// holdOrForgot is the choreography guard the forget test's holds run through:
// before the forget under test reaches its own append, it must not return —
// a forget that comes back early leaves the held write unreached and the
// test hanging, so the guard fails the test on the early return instead.
func holdOrForgot(t *testing.T, hold <-chan struct{}, forgot chan error) {
	t.Helper()
	select {
	case <-hold:
		return
	case err := <-forgot:
		t.Fatalf("the Forget returned %v before its record reached the held append: the choreography broke", err)
	}
}

// settledWithin reports whether cond holds before the deadline, polling the
// condition itself.
func settledWithin(cond func() bool) bool {
	for deadline := time.Now().Add(forgetDeadline); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// publishUnder plants one write of payload at off to f under tag, modelled on
// publish: appended through the core's own stream, published through the
// index and accounted, under the file's append domain.
func publishUnder(t *testing.T, c *Core, tag core.ShareTag, f core.FileID, off int64, payload []byte) core.Version {
	t.Helper()
	unlock := c.domains.Lock(f)
	defer unlock()
	v, err := c.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	x := core.Extent{Off: off, Len: int64(len(payload))}
	// Whole, like an unreserved write whose roll takes a content spare: each
	// test piece lands in one segment, so the segment a placement names is
	// the piece's own and the rolls are countable.
	p, err := c.Streams().For(f).AppendRoll(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: x, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{Payload: payload, MTime: 1}, segment.Roll{Whole: true})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	hd, err := c.Index().WriteRetain(tag, f, index.Placed{
		Extent:  x,
		Record:  index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)},
		Version: v,
	}, seq)
	if err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	c.Account(hd)
	return v
}

// rollingPieces writes three pieces of just under one segment to f, so the
// writes roll the stream twice and leave two sealed segments holding only the
// tag's records beside the active one, and returns their ids in write order.
// The pieces are synced, so the leaves retirement waits on are durable and
// the forgetting below is gated by its own record alone.
func rollingPieces(t *testing.T, c *Core, tag core.ShareTag, f core.FileID) []core.SegmentID {
	t.Helper()
	piece := testSegmentCap - 2048
	var segs []core.SegmentID
	for i := range 3 {
		off := int64(i) * piece
		publishUnder(t, c, tag, f, off, make([]byte, piece))
		p, ok := c.index.Lookup(f, off)
		if !ok {
			t.Fatalf("the write at %d left no placement to read its segment from", off)
		}
		if len(segs) == 0 || segs[len(segs)-1] != p.Record.Segment {
			segs = append(segs, p.Record.Segment)
		}
	}
	syncFiles(t, c, f)
	if len(segs) != 3 {
		t.Fatalf("the three rolling writes used %d segments, want 3", len(segs))
	}
	for i, seg := range segs {
		state, err := c.set.State(seg)
		if err != nil {
			t.Fatalf("segment %d: %v", seg, err)
		}
		if i < len(segs)-1 && state == segment.Active {
			t.Fatalf("segment %d is still active after the writes rolled past it", seg)
		}
	}
	return segs
}

// Forget drops the tag whole: every file entry, marker and extent of the tag
// leaves the running index once the forget record is durable, the tag leaves
// Stats().Shares while its records still sit on disk, and a second forget of
// the same tag succeeds and appends nothing.
func TestForgetDropsTheTagWhole(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f, g := testFile(1), testFile(2)

	publishUnder(t, c, forgetTag, f, 0, []byte("one"))
	publishUnder(t, c, forgetTag, g, 0, []byte("two"))
	// An unsettled deletion marker of the tag: the forget clears it, where a
	// per-file removal would leave it for a settle that never comes.
	if _, err := c.Delete(context.Background(), s, g); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.index.RemovalMarkers(); got != 1 {
		t.Fatalf("RemovalMarkers after the delete = %d, want 1", got)
	}
	if _, ok := c.Stats().Shares[forgetTag]; !ok {
		t.Fatalf("the tag with held content is not listed in Stats().Shares before the forget")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := len(c.index.Files(forgetTag)); got != 0 {
		t.Fatalf("the index still lists %d files of the forgotten tag, want none", got)
	}
	if got := c.index.RemovalMarkers(); got != 0 {
		t.Fatalf("RemovalMarkers after the forget = %d, want the marker cleared with the tag", got)
	}
	if _, ok := c.index.Owner(f); ok {
		t.Fatalf("file %v still has an owner after the tag was forgotten", f)
	}
	if _, ok := c.Stats().Shares[forgetTag]; ok {
		t.Fatalf("the forgotten tag is still listed in Stats().Shares")
	}

	// The second forget appends nothing: count the writes to the segment the
	// forget record lands in, the zero file's stream's active segment, which
	// no other record takes.
	zero, ok := c.Streams().For(core.FileID{}).OpenSegment()
	if !ok {
		t.Fatalf("the zero file's stream has no open segment")
	}
	name := format.SegmentName(zero)
	writes := seam.CallsFor(kit.OpWrite, name)
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("the second Forget: %v", err)
	}
	if got := seam.CallsFor(kit.OpWrite, name); got != writes {
		t.Fatalf("the second Forget made %d writes to the forget record's segment %s, want the %d there were", got, name, writes)
	}

	// A file the forgotten tag claimed is claimable by another share: a write
	// through the other handle lands.
	u := mustShare(t, c, otherTag)
	if _, err := c.WriteAt(context.Background(), u, f, 4096, []byte("fresh"), 1); err != nil {
		t.Fatalf("the write through another tag after the forget: %v", err)
	}
}

// Forget is refused while a handle of the tag is open, having changed
// nothing: the marker an unsettled deletion left still stands and the forget
// answer is ErrHandleOpen.
func TestForgetRefusedWhileAHandleIsOpen(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	g := testFile(1)
	publishUnder(t, c, forgetTag, g, 0, []byte("one"))
	if _, err := c.Delete(context.Background(), s, g); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	err := c.Forget(context.Background(), forgetTag)
	if !errors.Is(err, core.ErrHandleOpen) {
		t.Fatalf("Forget with the handle open = %v, want ErrHandleOpen", err)
	}
	if got := c.index.RemovalMarkers(); got != 1 {
		t.Fatalf("the refused Forget changed RemovalMarkers to %d, want the marker left at 1", got)
	}
	if got := len(c.index.Files(forgetTag)); got != 1 {
		t.Fatalf("the refused Forget left %d files of the tag listed, want 1", got)
	}
}

// Share of a tag whose forget is running is refused with ErrHandleOpen wrapped
// to name the forget, and the refusal ends when the forget returns.
func TestForgetRefusesSharesWhileItRuns(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	publishUnder(t, c, forgetTag, f, 0, []byte("one"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The spare pool is settled before the hold: a background spare write
	// taking the held storage first would reach the choreography before the
	// forget's own record does.
	awaitSpares(t, c, 0)

	hold := seam.Hold(t, kit.OpWrite, "*.seg")
	done := make(chan error, 1)
	go func() { done <- c.Forget(context.Background(), forgetTag) }()
	holdOrForgot(t, hold.Reached(), done)
	_, err := c.Share(forgetTag, 0, core.Version{})
	if !errors.Is(err, core.ErrHandleOpen) {
		t.Fatalf("Share of the forgetting tag = %v, want the wrapped ErrHandleOpen", err)
	}
	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("the Forget the choreography ran: %v", err)
	}
	// The mark went with the call: the tag attaches again.
	if s, err := c.Share(forgetTag, 0, core.Version{}); err != nil {
		t.Fatalf("Share of the tag after the forget = %v, want the attach", err)
	} else if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A context already done is answered before anything changes: no record is
// appended and no mark is left behind — a later Share and Forget of the tag
// both work.
func TestForgetDoneContextAppendsNothing(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	publishUnder(t, c, forgetTag, f, 0, []byte("one"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writes := seam.Calls(kit.OpWrite)
	if err := c.Forget(ctx, forgetTag); !errors.Is(err, context.Canceled) {
		t.Fatalf("Forget with a done context = %v, want the context's error", err)
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("the refused Forget made %d segment writes, want the %d there were", got, writes)
	}
	// The refused call leaves the tag usable: the handle it refused is
	// attachable and a later forget of the same tag succeeds.
	if s, err := c.Share(forgetTag, 0, core.Version{}); err != nil {
		t.Fatalf("Share after the refused Forget = %v, want the refused mark released", err)
	} else if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget after the refused call = %v, want the tag usable", err)
	}
}

// A tag nothing holds — no record on disk, no file in the index — is answered
// without appending anything, and the answer takes the tag out of the
// stranded set too.
func TestForgetOfATagNothingHoldsAppendsNothing(t *testing.T) {
	c, seam := newTestCore(t)
	if err := c.Strand(forgetTag); err != nil {
		t.Fatalf("Strand: %v", err)
	}

	writes := seam.Calls(kit.OpWrite)
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget of a tag nothing holds: %v", err)
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("the empty Forget made %d segment writes, want the %d there were", got, writes)
	}
	if c.Stranded(forgetTag) {
		t.Fatalf("the empty Forget left the tag stranded")
	}
}

// Forget takes the tag out of the stranded set, and strandings of other tags
// stand.
func TestForgetUnstrands(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	publishUnder(t, c, forgetTag, f, 0, []byte("one"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Strand(otherTag); err != nil {
		t.Fatalf("Strand: %v", err)
	}
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if c.Stranded(forgetTag) {
		t.Fatalf("the forgotten tag is still stranded")
	}
	if !c.Stranded(otherTag) {
		t.Fatalf("the forget of one tag unstranded another")
	}
}

// A core whose close began refuses the forget, and the set of a closed core
// keeps the tags the caller stranded: nothing is dropped of them by the
// refusal.
func TestForgetAfterCloseKeepsTheStrandedSet(t *testing.T) {
	c, _ := newTestCore(t)
	if err := c.Strand(forgetTag); err != nil {
		t.Fatalf("Strand: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Forget(context.Background(), forgetTag); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Forget after Close = %v, want ErrClosed", err)
	}
	if !c.Stranded(forgetTag) {
		t.Fatalf("the refused Forget unstranded the tag of a closed core")
	}
}

// The segments holding only the forgotten tag's records retire once the
// forget record is durable, and the record itself stays live while a segment
// of its snapshot still holds the tag's records — the active one the third
// piece rolled no further than.
func TestForgetRetiresTheTagsSegments(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	segs := rollingPieces(t, c, forgetTag, f)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// The two sealed segments retire: their held extents left with the drop
	// and the forget record is durable before the drop offered them. The
	// bounded wait is on the worker's own retirement, a background the test
	// cannot join.
	sealed := segs[:len(segs)-1]
	if !settledWithin(func() bool {
		for _, seg := range sealed {
			if _, err := c.set.State(seg); err == nil {
				return false
			}
		}
		return true
	}) {
		state, _ := c.set.State(sealed[0])
		t.Fatalf("the segments holding only the forgotten tag's records did not retire within %v (first still in state %d)", forgetDeadline, state)
	}
	// The forget record itself stays live: its snapshot still names the
	// stream's active segment, which holds the third piece of the tag below
	// the record and never retires in this core — no idle sealer seals it.
	// Live is the answer the record's own rule owes while that segment
	// remains, so the retirement above is the durability gate at work, not
	// the forget's liveness over.
	rec, ok := c.Streams().For(core.FileID{}).OpenSegment()
	if !ok {
		t.Fatalf("the zero file's stream has no open segment to hold the forget record")
	}
	if !c.liveness.Held(rec) {
		t.Fatalf("the forget record is not live while the active segment holding the tag's records below it remains")
	}
}

// The forget ends the tag's deferred losses: a loss whose record the copy of
// the tag's segment owes would otherwise be written by a later pass at a
// sequence number above the forget record, claiming the file again and
// escaping the recovery withhold. The forget's own drop ends them, leaving
// the pass nothing to write.
func TestForgetEndsTheTagsDeferredLosses(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	x := core.Extent{Off: 0, Len: 32}
	v := publishUnder(t, c, forgetTag, f, 0, make([]byte, x.Len))
	p, ok := c.index.Lookup(f, 0)
	if !ok {
		t.Fatalf("the planted write left no placement to read its segment from")
	}
	seg := p.Record.Segment

	// A deferred loss of the tag, raised through the durability layer the
	// way a failed window does, owing its loss record to whatever copy
	// retires seg.
	if _, err := c.Durability().Lose(context.Background(), durability.Loss{
		Share: forgetTag, File: f, Extent: x, Version: v,
		Seq: 1 << 30, Reason: core.LossStale, Deferred: true, Seg: seg,
	}); err != nil {
		t.Fatalf("the planted deferred loss: %v", err)
	}
	if got := c.Durability().DeferredAgainst(seg); len(got) != 1 {
		t.Fatalf("the planted deferred loss did not register against segment %d: %d entries", seg, len(got))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := c.Durability().DeferredAgainst(seg); len(got) != 0 {
		t.Fatalf("the forget left %d deferred losses of the tag for a later pass to write: %+v", len(got), got)
	}
}

// The forget re-tests the tag's registered removal records: the drop clears
// each marker without the settle a removal waits for, so without the re-test
// a deleted file's removal record stays counted live and keeps its segment
// held — the offer the drop made is skipped and the segment never retires.
func TestForgetRetiresTheDeletedTagsSegments(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)

	// One piece and the delete beside it in the segment a later whole piece
	// rolls past, so the sealed segment holds only T's records, the delete
	// record among them: the retirement the re-test owes is the R13 gate.
	// A near-full piece, the delete beside it, then a whole piece that rolls
	// the segment closed: one sealed segment holds the piece and the delete
	// record together, and the retirement the re-test owes is its gate.
	publishUnder(t, c, forgetTag, f, 0, make([]byte, testSegmentCap-4096))
	p, ok := c.index.Lookup(f, 0)
	if !ok {
		t.Fatalf("the tagged file's piece left no placement")
	}
	seg := p.Record.Segment
	if _, err := c.Delete(context.Background(), s, f); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	syncFiles(t, c, f)
	publishUnder(t, c, forgetTag, f, testSegmentCap-4096, make([]byte, testSegmentCap-2048))
	syncFiles(t, c, f)
	if state, serr := c.set.State(seg); serr != nil || state == segment.Active {
		t.Fatalf("the segment holding the tag's delete record is in state %d (%v), want it sealed by the roll", state, serr)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.index.RemovalMarkers(); got == 0 {
		t.Fatalf("the delete left no marker to clear")
	}

	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := c.index.RemovalMarkers(); got != 0 {
		t.Fatalf("the forget left %d removal markers of the tag", got)
	}
	// The segment holding the delete record retires — bounded, as the other
	// retirement waits are: the worker's own hand-over is a background the
	// test cannot join.
	if !settledWithin(func() bool {
		_, err := c.set.State(seg)
		return err != nil
	}) {
		state, _ := c.set.State(seg)
		t.Fatalf("the segment holding the tag's delete record did not retire within %v (state %d)", forgetDeadline, state)
	}
}

// The forget record's liveness ends once the last segment of its snapshot
// leaves the set, and a pass then drops the record's own segment with it:
// nothing of the tag keeps a segment held.
func TestForgetConnectionDropsOnceNoneRemains(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, forgetTag)
	f := testFile(1)
	segs := rollingPieces(t, c, forgetTag, f)

	// Roll the last T-byte segment closed: a whole piece of another tag on
	// another file of the same stream leaves no room for it, so the stream
	// rolls and seals the segment that holds T's third piece — every segment
	// of the tag's records is sealed now, and the snapshot the forget takes
	// names only segments a retirement can take.
	fstream := c.Streams().For(f)
	var u core.FileID
	for i := byte(2); ; i++ {
		u = testFile(i)
		if c.Streams().For(u) == fstream {
			break
		}
	}
	publishUnder(t, c, otherTag, u, 0, make([]byte, testSegmentCap-2048))
	syncFiles(t, c, u)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// the whole pieces rolled: T's segments all sealed
	if state, serr := c.set.State(segs[len(segs)-1]); serr == nil && state == segment.Active {
		t.Fatalf("the stream's active segment still holds the tag's third piece")
	}
	if err := c.Forget(context.Background(), forgetTag); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// Every snapshot segment retires; the bounded wait is the worker's own
	// hand-over again.
	if !settledWithin(func() bool {
		for _, seg := range segs {
			if _, err := c.set.State(seg); err == nil {
				return false
			}
		}
		return true
	}) {
		state, _ := c.set.State(segs[0])
		t.Fatalf("the tag's segments did not retire within %v (first in state %d)", forgetDeadline, state)
	}

	// The record itself is no longer held once the last of them left: the
	// zero file's stream's segment answers not held, the record droppable
	// the rule R13 names. It stays the stream's active segment until a pass
	// takes it: seal it closed and run one, and the unit finds nothing held
	// or registered and retires it with the record's bytes gone.
	st := c.Streams().For(core.FileID{})
	zero, ok := st.OpenSegment()
	if !ok {
		t.Fatalf("the zero file's stream has no open segment for the forget record")
	}
	if !settledWithin(func() bool { return !c.liveness.Held(zero) }) {
		t.Fatalf("the forget record is still live once every segment of its snapshot retired")
	}
	draw, err := c.headroom.Admit(capacity.Seal)
	if err != nil || !draw.Append {
		t.Fatalf("the seal marker's admission: draw %+v, err %v", draw, err)
	}
	if _, err := st.SealOpen(zero, true); err != nil {
		c.headroom.Refund(draw)
		t.Fatalf("seal the forget record's segment: %v", err)
	}
	c.headroom.Drawn(draw)
	if _, err := c.Repack(context.Background(), testSegmentCap, reclaim.Scope{Segments: []core.SegmentID{zero}}); err != nil {
		t.Fatalf("the pass over the forget record's segment: %v", err)
	}
	if _, err := c.set.State(zero); err == nil {
		t.Fatalf("the forget record's segment stayed in the set after the pass that dropped it")
	}
}
