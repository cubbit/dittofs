package datapath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
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
