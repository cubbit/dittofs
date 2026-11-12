package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// heldPhase1 delays the first clone phase 1 in the store's call until let
// is called: entered is closed once it is held there. A later phase 1 runs
// on, the first one held or not.
type heldPhase1 struct {
	entered chan struct{}
	hold    chan struct{}
	first   atomic.Bool
	let     func()
}

func newHeldPhase1(t *testing.T) *heldPhase1 {
	h := &heldPhase1{entered: make(chan struct{}), hold: make(chan struct{})}
	h.let = sync.OnceFunc(func() { close(h.hold) })
	t.Cleanup(h.let)
	return h
}

func (h *heldPhase1) clone(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	if h.first.CompareAndSwap(false, true) {
		close(h.entered)
		<-h.hold
	}
	return next.Clone(ctx, op, pending)
}

// waitsForTheClone fails the test unless the Unbind answering unbound waits
// for work in flight, the clone, before it answers.
func waitsForTheClone(t *testing.T, joined <-chan string, unbound <-chan error) {
	t.Helper()
	select {
	case what := <-joined:
		if what != "work in flight" {
			t.Fatalf("Unbind waits for %q first, want the clone, work in flight", what)
		}
	case err := <-unbound:
		t.Fatalf("Unbind = %v with the clone running, want it to wait for the clone", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Unbind neither waited nor answered")
	}
}

// startCloneHeld runs a clone of the first MiB of src onto dst of share
// dstShare whose phase 1 h holds, and waits until it is held there and its
// call has answered DelayClone at its context's end, so only the clone's
// own work is left in the engine.
func (u *unit) startCloneHeld(t *testing.T, h *heldPhase1, src entity.FileID, dstShare entity.ShareID, dst entity.FileID) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan error, 1)
	go func() { answer <- u.e.Clone(ctx, u.share.ID, src, dstShare, dst, vfserr.Extent{Len: mib}) }()
	select {
	case <-h.entered:
	case err := <-answer:
		t.Fatalf("the clone answered %v before its phase 1", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the clone never reached its phase 1")
	}
	cancel()
	if err := u.answered(t, "the clone's call", answer); !isDelay(err, vfserr.DelayClone) {
		t.Fatalf("the clone's call at its context's end = %v, want DelayClone", err)
	}
}

// An Unbind of the destination's share waits for a clone whose call has
// answered and which runs on, on its own goroutine, refusing a new clone onto
// the share meanwhile, and returns once the clone is done, nothing of it left
// registered (catches a clone the share's gate does not count, which Unbind
// passes while it still writes the destination).
func TestUnbindOfTheDestinationsShareWaitsForItsLiveClone(t *testing.T) {
	h := newHeldPhase1(t)
	u := clonedUnit(t, onClone(h.clone))
	u.e.SetConflictBound(time.Minute) // the held phase 1 outlasts no bound
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	other := u.create(t, "other")
	u.startCloneHeld(t, h, src, u.share.ID, dst)

	joined := joins(u.e)
	unbound := u.unbinding(t)
	waitsForTheClone(t, joined, unbound)

	if err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, other, vfserr.Extent{Len: mib}); !isStale(err) {
		t.Errorf("a new clone onto the share once Unbind began = %v, want exactly ErrStale", err)
	}

	h.let()

	if err := u.answered(t, "Unbind", unbound); err != nil {
		t.Fatalf("Unbind once the clone is done = %v, want nil", err)
	}

	s, _ := u.e.Registrations(src)
	_, d := u.e.Registrations(dst)

	if s != 0 || d != 0 {
		t.Errorf("Unbind returned with %d source and %d destination registrations of the clone, want it done", s, d)
	}

	if _, ok := u.cloneRecord(t, src, dst); ok {
		t.Error("Unbind returned with the source's clone-kind open record still in the store: the clone's phase 2 had not run")
	}
}

// An Unbind of the source's share waits for a clone into another share
// until the clone ends, here at its phase 1, the destination being a file
// the store does not hold (catches a clone counted on its destination's share
// alone, whose Unbind of the source's share returns while the clone still
// reads the source).
func TestUnbindOfTheSourcesShareWaitsForTheClone(t *testing.T) {
	h := newHeldPhase1(t)
	u := clonedUnit(t, onClone(h.clone))
	u.e.SetConflictBound(time.Minute)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := entity.NewFileID()
	u.startCloneHeld(t, h, src, other, dst)

	joined := joins(u.e)
	unbound := u.unbinding(t)
	waitsForTheClone(t, joined, unbound)

	h.let()

	if err := u.answered(t, "Unbind", unbound); err != nil {
		t.Fatalf("Unbind once the clone ended = %v, want nil", err)
	}
}

// An Unbind of the destination's share waits for a clone from another share
// until the clone ends, here at its phase 1, the destination being a file the
// store does not hold (catches a clone counted on its source's share alone,
// whose Unbind of the destination's share returns while the clone still
// writes the destination).
func TestUnbindOfTheDestinationsShareAcrossSharesWaitsForTheClone(t *testing.T) {
	h := newHeldPhase1(t)
	u := clonedUnit(t, onClone(h.clone))
	u.e.SetConflictBound(time.Minute)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	src := u.committedFile(t, "src", fill(mib, 's'))
	u.startCloneHeld(t, h, src, other, entity.NewFileID())

	joined := joins(u.e)
	unbound := make(chan error, 1)

	go func() { unbound <- u.e.Unbind(t.Context(), other, false) }()

	waitsForTheClone(t, joined, unbound)

	h.let()

	if err := u.answered(t, "Unbind", unbound); err != nil {
		t.Fatalf("Unbind once the clone ended = %v, want nil", err)
	}

	if s, _ := u.e.Registrations(src); s != 0 {
		t.Errorf("Unbind of the source's share returned with %d source registrations of the clone, want it ended", s)
	}
}

// A clone rebuilt at start from its removal record, waiting for its
// source's shard, is waited out by an Unbind of its share, which refuses a
// new clone meanwhile and returns once open state serves the shard and the
// clone is done (catches a rebuilt clone the share's gate does not count,
// whose wait no removal batch shows).
func TestUnbindWaitsForARebuiltCloneWaitingForItsSourcesShard(t *testing.T) {
	m := newRemovalCrash(t, false)
	m.begin(t)
	m.run()

	joined := joins(m.e)
	unbound := make(chan error, 1)

	go func() { unbound <- m.e.Unbind(t.Context(), m.one.ID, false) }()

	waitsForTheClone(t, joined, unbound)

	if err := m.e.Clone(t.Context(), m.one.ID, m.src, m.one.ID, entity.NewFileID(), vfserr.Extent{Len: runLen}); !isStale(err) {
		t.Errorf("a new clone onto the share once Unbind began = %v, want exactly ErrStale", err)
	}

	m.serve(t)

	if err := within(t, "Unbind", unbound); err != nil {
		t.Fatalf("Unbind once the clone is done = %v, want nil", err)
	}

	s, _ := m.e.Registrations(m.src)
	_, d := m.e.Registrations(m.dst)

	if s != 0 || d != 0 {
		t.Errorf("Unbind returned with %d source and %d destination registrations of the rebuilt clone, want it done", s, d)
	}
}

// A Close that begins while an Unbind waits for a rebuilt clone waiting for
// its source's shard ends that wait, the Unbind answering ErrUnavailable, and
// stops within its bound (catches an Unbind that keeps waiting for the clone,
// which only Close stops, so Close's wait for the Unbind ends at its bound).
func TestCloseEndsAnUnbindWaitingForARebuiltClone(t *testing.T) {
	m := newRemovalCrash(t, false)
	m.begin(t)
	m.run()

	joined := joins(m.e)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel() // ends the Unbind should Close leave it waiting
	unbound := make(chan error, 1)

	go func() { unbound <- m.e.Unbind(ctx, m.one.ID, false) }()

	waitsForTheClone(t, joined, unbound)

	cctx, ccancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer ccancel()

	if err := m.e.Close(cctx); err != nil {
		t.Fatalf("Close with an Unbind waiting for the rebuilt clone = %v, want nil", err)
	}

	if err := within(t, "Unbind", unbound); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("Unbind once Close began = %v, want ErrUnavailable", err)
	}
}

// A clone onto a share whose Unbind began after the clone's call entered,
// once both registrations are installed, is refused with ErrStale at its
// shares' gates: both registrations are undone and nothing reaches the
// journal or the store, and the Unbind, which waited for the call, then
// returns (catches a refusal there that leaves the registrations in place,
// or that lets the clone run on).
func TestACloneWhoseSharesUnbindBeganAfterItsEntryChangesNothing(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	joined := joins(u.e)
	var unbound <-chan error
	u.e.SetAfterRegister(func() error {
		unbound = u.unbinding(t)
		waitsForTheClone(t, joined, unbound) // the gate is shut, the call's entry still counted
		return nil
	})

	err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	if !isStale(err) {
		t.Errorf("a clone whose share's Unbind began after its entry = %v, want exactly ErrStale", err)
	}

	s, _ := u.e.Registrations(src)
	_, d := u.e.Registrations(dst)

	if s != 0 || d != 0 {
		t.Errorf("the refused clone left %d source and %d destination registrations, want none", s, d)
	}

	if err := u.answered(t, "Unbind", unbound); err != nil {
		t.Fatalf("Unbind once the refused clone's call answered = %v, want nil", err)
	}

	if m := u.cloneMarkers(dst); len(m) != 0 {
		t.Errorf("the refused clone left %d clone markers in the journal, want none", len(m))
	}

	if _, ok := u.cloneRecord(t, src, dst); ok {
		t.Error("the refused clone left the source's clone-kind open record in the store")
	}
}
