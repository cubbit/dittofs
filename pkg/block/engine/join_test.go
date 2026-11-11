package engine_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// waitingCtx is a context that reports, once, the first time a call asks
// for its Done: a Clone that joined a running clone and waits for it.
type waitingCtx struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func newWaitingCtx() *waitingCtx {
	return &waitingCtx{Context: context.Background(), waiting: make(chan struct{})}
}

func (c *waitingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// joined calls f, a Clone or a Copy, on a goroutine of its own and returns
// once it waits on its context, its answer still to come; it fails the test
// if f answers first, as a call refused rather than joined does.
func joined(t *testing.T, f func(ctx context.Context) error) <-chan error {
	t.Helper()
	ctx := newWaitingCtx()
	answer := make(chan error, 1)
	go func() { answer <- f(ctx) }()
	select {
	case <-ctx.waiting:
	case err := <-answer:
		t.Fatalf("the retry answered %v without waiting for the running clone", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the retry neither waited nor answered")
	}
	return answer
}

// retry calls Clone of the first MiB of src onto dst, the spec
// startCloneHeld runs, and returns once it waits for the running clone.
func (u *unit) retry(t *testing.T, src entity.FileID, dstShare entity.ShareID, dst entity.FileID) <-chan error {
	t.Helper()
	return joined(t, func(ctx context.Context) error {
		return u.e.Clone(ctx, u.share.ID, src, dstShare, dst, vfserr.Extent{Len: mib})
	})
}

// heldCloneUnit is a clonedUnit whose first clone's phase 1 h holds, and a
// source of 2 MiB and a destination for it.
func heldCloneUnit(t *testing.T) (u *unit, h *heldPhase1, src, dst entity.FileID) {
	t.Helper()
	h = newHeldPhase1(t)
	u = clonedUnit(t, onClone(h.clone))
	u.e.SetConflictBound(time.Minute) // the held phase 1 outlasts no bound
	return u, h, u.committedFile(t, "src", fill(2*mib, 's')), u.create(t, "dst")
}

// A Clone with the spec of a clone running, its first caller still waiting,
// joins it: one CloneTarget, both answered once it is done, and the
// destination reads the source (catches a retry that starts a second clone,
// or one refused DelayClone by the running clone's registrations).
func TestACloneRetryJoinsTheRunningClone(t *testing.T) {
	u, h, src, dst := heldCloneUnit(t)
	first := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	select {
	case <-h.entered:
	case err := <-first:
		t.Fatalf("the clone answered %v before its phase 1", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the clone never reached its phase 1")
	}
	retry := u.retry(t, src, u.share.ID, dst)

	h.let()

	if err := u.answered(t, "the first call", first); err != nil {
		t.Errorf("the first call = %v, want nil", err)
	}
	if err := u.answered(t, "the retry", retry); err != nil {
		t.Errorf("the retry = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dst)); n != 1 {
		t.Errorf("the journal holds %d clone markers, want one clone", n)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source")
	}
}

// A retry after the first call answered DelayClone at its context's end
// joins the clone still running and is answered once it is done (catches a
// clone that leaves the running clones when its first caller stops waiting).
func TestACloneRetryAfterItsCallEndedJoinsTheClone(t *testing.T) {
	u, h, src, dst := heldCloneUnit(t)
	u.startCloneHeld(t, h, src, u.share.ID, dst)
	retry := u.retry(t, src, u.share.ID, dst)

	h.let()

	if err := u.answered(t, "the retry", retry); err != nil {
		t.Errorf("the retry = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dst)); n != 1 {
		t.Errorf("the journal holds %d clone markers, want one clone", n)
	}
}

// A clone into a file another clone is running into, with any part of the
// spec other than that clone's, answers DelayClone at once, not once the
// running clone is done (catches a spec compared on part of its fields,
// which joins a clone the call did not ask for).
func TestACloneWithAnotherSpecIsNotJoined(t *testing.T) {
	u, h, src, dst := heldCloneUnit(t)
	other := u.committedFile(t, "other", fill(2*mib, 'o'))
	u.startCloneHeld(t, h, src, u.share.ID, dst)
	same := u.bindShare(t, "same", entity.NamespaceID{})
	for _, c := range []struct {
		name  string
		share entity.ShareID
		src   entity.FileID
		x     vfserr.Extent
	}{
		{"another source file", u.share.ID, other, vfserr.Extent{Len: mib}},
		{"another source share", same, src, vfserr.Extent{Len: mib}},
		{"another source offset", u.share.ID, src, vfserr.Extent{SrcOff: 1, Len: mib}},
		{"another destination offset", u.share.ID, src, vfserr.Extent{DstOff: 1, Len: mib}},
		{"another length", u.share.ID, src, vfserr.Extent{Len: mib - 1}},
	} {
		answer := make(chan error, 1)
		go func() { answer <- u.e.Clone(context.Background(), c.share, c.src, u.share.ID, dst, c.x) }()
		select {
		case err := <-answer:
			if !isDelay(err, vfserr.DelayClone) {
				t.Errorf("%s: %v, want DelayClone", c.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no answer while the clone runs: it joined the clone", c.name)
		}
	}

	h.let()

	u.until(t, "the clone is done", func() bool {
		_, d := u.e.Registrations(dst)
		return d == 0
	})
	if n := len(u.cloneMarkers(dst)); n != 1 {
		t.Errorf("the journal holds %d clone markers, want one clone", n)
	}
}

// Once a clone is done, a Clone with its spec runs the clone again: the
// source written since is what the destination then reads (catches a done
// clone kept among the running clones, which a retry joins, answered at once
// with the old content left).
func TestACloneDoneIsNotJoined(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 'a'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	u.write(t, src, 0, fill(mib, 'b'))
	u.clone(t, src, dst, vfserr.Extent{Len: mib})

	if n := len(u.cloneMarkers(dst)); n != 2 {
		t.Errorf("the journal holds %d clone markers, want two clones", n)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 'b')) {
		t.Error("the destination does not read the source as the second clone found it")
	}
}

// An Unbind of the share waits for a clone that a retry has joined, and
// both the Unbind and the retry are answered once the clone is done
// (catches a joined retry the clone's end does not wake).
func TestUnbindJoinsACloneARetryWaitsFor(t *testing.T) {
	u, h, src, dst := heldCloneUnit(t)
	u.startCloneHeld(t, h, src, u.share.ID, dst)
	retry := u.retry(t, src, u.share.ID, dst)

	joining := joins(u.e)
	unbound := u.unbinding(t)
	waitsForTheClone(t, joining, unbound)

	h.let()

	if err := u.answered(t, "the retry", retry); err != nil {
		t.Errorf("the retry = %v, want nil", err)
	}
	if err := u.answered(t, "Unbind", unbound); err != nil {
		t.Errorf("Unbind once the clone is done = %v, want nil", err)
	}
}

// A retry of a clone rebuilt at start from its removal record, waiting for
// its source's shard, joins it and is answered once the shard is served and
// the clone is done (catches a rebuilt clone kept out of the running clones,
// whose registrations refuse the retry DelayClone).
func TestACloneRetryJoinsARebuiltClone(t *testing.T) {
	m := newRemovalCrash(t, false)
	m.begin(t)
	m.run()

	retry := joined(t, func(ctx context.Context) error {
		return m.e.Clone(ctx, m.one.ID, m.src, m.one.ID, m.dst, vfserr.Extent{Len: runLen})
	})

	m.serve(t)

	if err := within(t, "the retry", retry); err != nil {
		t.Fatalf("the retry of the rebuilt clone = %v, want nil", err)
	}
	if _, d := m.e.Registrations(m.dst); d != 0 {
		t.Errorf("the retry answered with %d destination registrations, want the clone done", d)
	}
}

// Close with a retry waiting for a rebuilt clone ends the clone and answers
// the retry ErrUnavailable, returning within its bound (catches a joined
// retry left waiting by a clone that ends at its loop's end).
func TestCloseAnswersARetryJoinedToTheClone(t *testing.T) {
	m := newRemovalCrash(t, false)
	m.begin(t)
	m.run()

	retry := joined(t, func(ctx context.Context) error {
		return m.e.Clone(ctx, m.one.ID, m.src, m.one.ID, m.dst, vfserr.Extent{Len: runLen})
	})

	cctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := m.e.Close(cctx); err != nil {
		t.Fatalf("Close with a retry waiting for the clone = %v, want nil", err)
	}
	if err := within(t, "the retry", retry); !unavailable(err) {
		t.Fatalf("the retry once Close ended the clone = %v, want ErrUnavailable", err)
	}
}

// A copy continued from the chunk its deadline met joins that chunk, still
// running, rather than copying it again: three clone markers for three
// chunks, and the destination reads the source.
func TestACopyContinuedJoinsTheRunningChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	u := copyUnit(t, onClone(func(cctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
		if op.Spec.SrcOff == chunk {
			cancel()
			<-release
		}
		return next.Clone(cctx, op, pending)
	}))
	u.e.SetConflictBound(time.Minute)
	p := pattern(3 * chunk)
	src := u.committedFile(t, "src", p)
	dst := u.create(t, "dst")
	if n, err := u.copy(ctx, src, dst, vfserr.Extent{Len: 3 * chunk}); n != chunk || err != nil {
		t.Fatalf("the copy to its deadline in the second chunk: %d, %v; want %d, nil", n, err, chunk)
	}
	var n int64
	cont := joined(t, func(ctx context.Context) (err error) {
		n, err = u.copy(ctx, src, dst, vfserr.Extent{SrcOff: chunk, DstOff: chunk, Len: 2 * chunk})
		return err
	})

	let()

	if err := u.answered(t, "the continued copy", cont); err != nil || n != 2*chunk {
		t.Fatalf("the continued copy: %d, %v; want %d, nil", n, err, 2*chunk)
	}
	if m := len(u.cloneMarkers(dst)); m != 3 {
		t.Errorf("the journal holds %d clone markers, want one per chunk, 3", m)
	}
	if got := u.read(t, dst, 0, 3*chunk); !bytes.Equal(got, p) {
		t.Error("the destination does not read the source")
	}
}
