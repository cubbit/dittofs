package engine_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// loopItem is a removal item a test posts, counting its runs and ends.
type loopItem struct {
	ran, ended atomic.Int32
	entered    chan struct{}
	release    chan struct{}
}

// post puts an item of kind on share s's loop. A held item stays in its run
// until release is closed.
func (u *unit) post(s entity.ShareID, kind entity.RemovalKind, held bool) *loopItem {
	return u.postItem(s, kind, held, false)
}

// postItem is post, the item asking to run again after its run when retry.
func (u *unit) postItem(s entity.ShareID, kind entity.RemovalKind, held, retry bool) *loopItem {
	it := &loopItem{entered: make(chan struct{}), release: make(chan struct{})}

	u.e.EnqueueKind(s, kind, retry, func(context.Context) {
		it.ran.Add(1)
		close(it.entered)

		if held {
			<-it.release
		}
	}, func() {
		it.ended.Add(1)
	})

	return it
}

// unbindHeldByAnItem returns a unit whose loop is running a held item of its
// share, and the Unbind that waits for it.
func unbindHeldByAnItem(t *testing.T) (*unit, *loopItem, <-chan error) {
	t.Helper()

	u := newUnit(t)
	held := u.post(u.share.ID, entity.RemoveTruncate, true)

	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})

	<-held.entered

	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case what := <-joined:
		if what != "a removal batch" {
			t.Errorf("Unbind waits for %q, want %q", what, "a removal batch")
		}
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a removal batch running, want it to wait", err)
	}

	return u, held, unbound
}

// An Unbind waits for the removal batch in flight, and while it waits the
// share refuses a call and a clone, and parks a new removal item, which never
// runs.
func TestUnbindJoinsTheBatchInFlightAndRefusesWhileItWaits(t *testing.T) {
	u, held, unbound := unbindHeldByAnItem(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	f := u.create(t, "f")

	if err := u.writeTo(t, f); !isStale(err) {
		t.Errorf("Write while Unbind waits = %v, want exactly ErrStale", err)
	}

	err := u.e.Clone(
		t.Context(),
		other,
		entity.NewFileID(),
		u.share.ID,
		entity.NewFileID(),
		vfserr.Extent{Len: 1},
	)
	if !isStale(err) {
		t.Errorf("Clone naming the share while Unbind waits = %v, want exactly ErrStale", err)
	}

	posted := u.post(u.share.ID, entity.RemoveTruncate, false)

	if got := posted.ran.Load(); got != 0 {
		t.Errorf("a removal item posted during Unbind ran %d times, want 0", got)
	}

	if got := posted.ended.Load(); got != 0 {
		t.Errorf("a removal item posted during Unbind ended %d times, want 0: it is parked", got)
	}

	close(held.release)

	if err := <-unbound; err != nil {
		t.Errorf("Unbind = %v, want nil", err)
	}

	if got := held.ran.Load(); got != 1 {
		t.Errorf("the item in flight ran %d times, want 1", got)
	}
}

// An Unbind with ten thousand pending writes and a removal item in flight
// returns only once the item has, and touches no write: every one is still
// pending, and none has had its Done.
func TestUnbindWithTenThousandPendingWritesAndARemovalItemInFlightJoinsOnlyTheItem(t *testing.T) {
	const (
		files  = 100
		writes = 10000
	)

	u := newUnit(t)
	res := enginetest.NewReservations(t)
	ids := make([]entity.FileID, files)

	for i := range ids {
		ids[i] = u.create(t, string(rune('a'+i%26))+string(rune('a'+i/26)))
	}

	for i := range writes {
		o, answered := res.Opts()
		_, err := u.e.Write(t.Context(), u.share.ID, ids[i%files], int64(i/files), []byte("x"), o)
		answered(err)

		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	held := u.post(u.share.ID, entity.RemoveTruncate, true)

	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})

	<-held.entered

	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case <-joined:
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a removal item running, want it to wait", err)
	}

	close(held.release)

	if err := <-unbound; err != nil {
		t.Fatalf("Unbind = %v, want nil", err)
	}

	pending := 0

	for _, f := range ids {
		pending += len(u.e.Pending(f))
	}

	if pending != writes {
		t.Errorf("pending operations after Unbind = %d, want %d", pending, writes)
	}

	if got := res.Outstanding(); got != writes {
		t.Errorf("writes awaiting their Done after Unbind = %d, want %d", got, writes)
	}
}

// The items queued for a share when it is unbound are parked, never run, but
// the items of a clone still run.
func TestAnUnboundSharesQueuedItemsAreParkedAndOnlyClonesRun(t *testing.T) {
	u := newUnit(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	block := u.post(other, entity.RemoveTruncate, true)

	t.Cleanup(func() {
		select {
		case <-block.release:
		default:
			close(block.release)
		}
	})

	<-block.entered

	parked := u.post(u.share.ID, entity.RemoveRelease, false)
	clone := u.post(u.share.ID, entity.RemoveClone, false)

	u.unbind(t)
	close(block.release)
	<-clone.entered

	if got := clone.ran.Load(); got != 1 {
		t.Errorf("the clone's item ran %d times, want 1", got)
	}

	if got := parked.ran.Load(); got != 0 {
		t.Errorf("the parked item ran %d times, want 0", got)
	}

	if got := parked.ended.Load(); got != 0 {
		t.Errorf("the parked item ended %d times before any Drop, want 0", got)
	}
}
