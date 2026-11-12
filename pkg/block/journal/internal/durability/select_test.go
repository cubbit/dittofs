package durability

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
)

// candidatesOf is what a selector over the rig's segments, index and
// durability offers, with no pins and nothing unread.
func (r *rig) candidatesOf() []core.SegmentID {
	s := reclaim.NewSelector(r.set, r.ix, unread.New(nil), r.d, nil)
	var out []core.SegmentID
	for _, c := range s.Select(reclaim.Scope{}, 100) {
		out = append(out, c.ID)
	}
	return out
}

// TestFailedSegmentAfterResolution covers a segment a failed sync ended: no
// selection offers it while its window is open, and one offers it as soon as
// the window's resolved announcement is made.
func TestFailedSegmentAfterResolution(t *testing.T) {
	prof.Watch(t, prof.Options{})
	atFail := make(chan struct{}, 1)
	release, open := releaser(t)
	fr := newFailRig(t, func(fr *failRig) {
		t.Cleanup(open)
		fr.d.beforeFail = func(*window) {
			select {
			case atFail <- struct{}{}:
			default:
			}
			<-release
		}
	})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")
	seg := loc.Segment
	fr.failSyncOf(t, seg, errors.New("sync boom"))
	answer := fr.syncAsync(context.Background(), f)
	select {
	case <-atFail:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the window")
	}
	if got := fr.candidatesOf(); slices.Contains(got, seg) {
		t.Fatalf("candidates = %v while segment %d's window is open, want it absent", got, seg)
	}
	open()
	fr.awaitResolved(t)
	_ = mustReturn(t, "Sync", answer)
	if got := fr.candidatesOf(); !slices.Contains(got, seg) {
		t.Fatalf("candidates = %v after the window resolved, want segment %d present", got, seg)
	}
}

// TestDamagedSegmentAfterDrop covers a damaged segment whose corrupt record
// the index still names, served from a copy whose repoint is owed: no
// selection offers the segment until the repoint drops the record, and one
// offers it then.
func TestDamagedSegmentAfterDrop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	l := newLanding(false)
	cr := newCorruptRig(t, l.install)
	f := file(1)
	r := cr.reappend(t, f, 0, "aaaa")
	if err := cr.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of the re-append: %v", err)
	}
	cr.register(r)
	copySeg, damaged := r.orig.Segment, r.c.Record.Segment
	// The damaged segment is the stream's active one: seal it, so state alone
	// does not keep it from a selection.
	cr.settlePool(t)
	if _, err := cr.streams.For(f).Seal(false); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !cr.set.Claim(copySeg) {
		t.Fatal("the repack could not claim the copy's segment")
	}
	cr.useCopy(t, r)
	if !cr.d.ReclaimBlocked(damaged) {
		t.Fatalf("ReclaimBlocked(%d) = false, want true while the corrupt record is named", damaged)
	}
	if got := cr.candidatesOf(); slices.Contains(got, damaged) {
		t.Fatalf("candidates = %v while segment %d blocks reclamation, want it absent", got, damaged)
	}

	cr.set.Release(copySeg)
	cr.tickUntil(t, "the hold to be let go", func() bool { return !cr.holdOf(copySeg).held })
	if got := cr.candidatesOf(); !slices.Contains(got, damaged) {
		t.Fatalf("candidates = %v after the corrupt record left the index, want segment %d present", got, damaged)
	}
}
