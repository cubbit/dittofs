package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// heldPass is a unit whose share's pass is held in its scope sync, with
// unreclaimed bytes for the pass to repack once the sync returns, the
// journal whose Repack calls it counts, and the function that lets the sync go.
func heldPass(t *testing.T) (*unit, *repackSyncHold, *repackFault, entity.FileID, func()) {
	t.Helper()

	hold := &repackSyncHold{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	wrap := func(h engine.Handle) engine.Handle {
		hold.Handle = h

		return hold
	}

	var fault *repackFault

	u := newUnitWith(t, unitConfig{
		wrap: wrap,
		deps: []func(*engine.Deps){trackedRepack(nil, false, &fault)},
	})
	u.j.SegmentCap(segBytes, 0)
	u.j.AddEnded(u.share.Tag, 30)

	f := u.create(t, "f")
	u.write(t, f, 0, []byte("x"))

	release := sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(release)

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitShare)); err != nil {
		t.Fatal(err)
	}

	return u, hold, fault, f, release
}

// A caller whose context ends while the share's pass is held in its scope sync
// answers retry-later, and the pass runs on under the engine's lifetime: once
// the sync is let go it repacks.
func TestASharePassOutlivesItsCallersCancel(t *testing.T) {
	u, hold, fault, f, release := heldPass(t)

	ctx, cancel := context.WithCancel(bounded(t))
	defer cancel()

	got := make(chan error, 1)

	go func() {
		_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		got <- err
	}()

	within(t, "the pass reaching its scope sync", hold.entered)
	cancel()

	err := within(t, "the cancelled write's answer", got)
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a context ended in the scope sync: %v, want ErrDelay(DelayCapacity)", err)
	}

	release()
	u.e.AwaitSharePassesForTest()

	if n := fault.calls.Load(); n != 1 {
		t.Errorf("%d Repack calls once the sync was let go, want 1: the pass ended with its caller", n)
	}
}

// A refusal waits for a held pass a tenth of its deadline by the engine's
// clock, then answers retry-later, and the pass, left running, still repacks
// once its sync is let go.
func TestARefusalWaitsATenthOfItsDeadlineForThePass(t *testing.T) {
	u, hold, fault, f, release := heldPass(t)

	ctx := &endsAtCtx{Context: t.Context(), now: u.clock.Now(), done: make(chan struct{})}
	armed := u.clock.Armed(time.Hour / 10)
	got := make(chan error, 1)

	go func() {
		_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		got <- err
	}()

	within(t, "the pass reaching its scope sync", hold.entered)
	// The wait armed at a tenth of the deadline is the assertion on its length.
	within(t, "the refusal's wait for the pass", armed)
	u.clock.Advance(time.Hour / 10)

	if err := within(t, "the refusal's answer at its bound", got); !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a refusal past a tenth of its deadline: %v, want ErrDelay(DelayCapacity)", err)
	}

	release()
	u.e.AwaitSharePassesForTest()

	if n := fault.calls.Load(); n != 1 {
		t.Errorf("%d Repack calls once the sync was let go, want 1: the pass ended with the refusal's wait", n)
	}
}

// Close cancels a share pass held in its scope sync and joins it: Close ends
// clean once the pass has left its sync, the pass repacks nothing after its
// cancelled sync, and a refusal waiting for it answers the journal closed.
func TestCloseStopsASharePass(t *testing.T) {
	u, hold, fault, f, _ := heldPass(t)

	ctx := &endsAtCtx{Context: t.Context(), now: u.clock.Now(), done: make(chan struct{})}
	armed := u.clock.Armed(time.Hour / 10)
	got := make(chan error, 1)

	go func() {
		_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		got <- err
	}()

	within(t, "the pass reaching its scope sync", hold.entered)
	within(t, "the refusal's wait for the pass", armed)

	closed := make(chan error, 1)

	go func() { closed <- u.e.Close(bounded(t)) }()

	if err := within(t, "Close", closed); err != nil {
		t.Errorf("Close with a share pass held: %v, want nil", err)
	}

	select {
	case <-hold.returned:
	default:
		t.Error("Close answered while the share's pass was still in its sync, want it joined")
	}

	if n := fault.calls.Load(); n != 0 {
		t.Errorf("%d Repack calls after Close, want 0: Close did not stop the pass", n)
	}

	if err := within(t, "the waiting write's answer", got); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a refusal waiting for the pass Close stopped: %v, want ErrUnavailable", err)
	}
}

// A refusal whose context has no deadline waits for a held pass 3 s by the
// engine's clock, a tenth of the 30 s default request deadline, then answers
// retry-later.
func TestARefusalWithNoDeadlineWaitsThreeSeconds(t *testing.T) {
	u, hold, _, f, _ := heldPass(t)

	armed := u.clock.Armed(3 * time.Second)
	got := make(chan error, 1)

	go func() {
		_, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		got <- err
	}()

	within(t, "the pass reaching its scope sync", hold.entered)
	within(t, "the refusal's wait for the pass", armed)
	u.clock.Advance(3 * time.Second)

	if err := within(t, "the refusal's answer", got); !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a refusal with no deadline past 3 s: %v, want ErrDelay(DelayCapacity)", err)
	}
}

// Unbind cancels the share's pass held in its scope sync and waits for it
// before it answers, so Drop never closes the handle under a running pass.
func TestUnbindEndsTheSharePass(t *testing.T) {
	u, hold, fault, f, _ := heldPass(t)

	ctx, cancel := context.WithCancel(bounded(t))
	defer cancel()

	got := make(chan error, 1)

	go func() {
		_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		got <- err
	}()

	within(t, "the pass reaching its scope sync", hold.entered)
	cancel()

	if err := within(t, "the cancelled write's answer", got); !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a context ended in the scope sync: %v, want ErrDelay(DelayCapacity)", err)
	}

	if err := u.e.Unbind(bounded(t), u.share.ID, false); err != nil {
		t.Fatalf("Unbind with a share pass held: %v", err)
	}

	select {
	case <-hold.returned:
	default:
		t.Error("Unbind answered while the share's pass was still in its sync, want it ended")
	}

	if n := fault.calls.Load(); n != 0 {
		t.Errorf("%d Repack calls after Unbind, want 0: the pass ran on after its share was unbound", n)
	}
}

// syncCounter is a handle that counts the Sync calls reaching the journal and
// passes each through.
type syncCounter struct {
	engine.Handle
	n *atomic.Int64
}

func (h syncCounter) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	h.n.Add(1)

	return h.Handle.Sync(ctx, ids...)
}

// passUnit is a unit with share A limited to aLimit, its handles wrapped by
// wrap, and the journal whose Repack calls it counts.
func passUnit(t *testing.T, aLimit int64, wrap func(engine.Handle) engine.Handle) (*segUnit, *repackFault) {
	t.Helper()

	var fault *repackFault

	u := newUnitWith(t, unitConfig{wrap: wrap, deps: []func(*engine.Deps){trackedRepack(nil, false, &fault)}})
	u.j.SegmentCap(segBytes, 0)

	a := enginetest.AddShare(t, u.fs.Store, "a", aLimit)
	if err := u.e.Bind(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	return &segUnit{unit: u, a: a}, fault
}

// Two refusals to different files of one share, the second arriving before
// the first's pass has begun its sync, join that one pass: one sync, one
// repack, and both writes served.
func TestTwoRefusalsJoinOnePass(t *testing.T) {
	var syncs atomic.Int64

	s, fault := passUnit(t, 85, func(h engine.Handle) engine.Handle { return syncCounter{h, &syncs} })
	s.e.TrackSharePassesForTest()
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.a, entity.NewFileID()) // 80 bytes held, 40 superseded once synced

	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	s.e.OnSharePass(func() {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	})
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)

	before := syncs.Load()
	first := make(chan error, 1)

	go func() { first <- s.put(bounded(t), s.a, entity.NewFileID(), 0, 10, 'x') }()

	within(t, "the first refusal's pass starting", entered)

	armed := s.clock.Armed(time.Hour / 10)
	second := make(chan error, 1)

	go func() {
		ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
		second <- s.put(ctx, s.a, entity.NewFileID(), 0, 10, 'y')
	}()

	within(t, "the second refusal waiting for a pass", armed)
	let()

	if err := within(t, "the first write's answer", first); err != nil {
		t.Errorf("the first write: %v, want served", err)
	}

	if err := within(t, "the second write's answer", second); err != nil {
		t.Errorf("the second write: %v, want served", err)
	}

	if got := s.e.SharePassesStarted(); got != 1 {
		t.Errorf("share passes the two refusals started: %d, want 1, which the second joined", got)
	}

	if got := syncs.Load() - before; got != 1 {
		t.Errorf("syncs the two refusals made: %d, want 1", got)
	}

	if got := fault.calls.Load(); got != 1 {
		t.Errorf("repacks the two refusals made: %d, want 1", got)
	}
}

// syncedHold is a handle whose first Sync syncs, then holds until release: a
// pass past the start of its sync.
type syncedHold struct {
	engine.Handle
	entered, release chan struct{}
	once             *atomic.Bool
}

func (h syncedHold) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	marks, err := h.Handle.Sync(ctx, ids...)
	if h.once.CompareAndSwap(false, true) {
		close(h.entered)
		<-h.release
	}

	return marks, err
}

// A refusal that arrives once a pass's sync has begun does not decide from
// that sync: alice's overwrite, served after it, is unsynced, so bob, refused
// next, waits for the next pass, whose sync makes her superseded bytes
// unreclaimed and whose repack frees them, and is served rather than told no
// space.
func TestARefusalAfterThePassSyncedWaitsForTheNext(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool

	s, _ := passUnit(t, 100, func(h engine.Handle) engine.Handle {
		return syncedHold{Handle: h, entered: entered, release: release, once: &once}
	})
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)
	s.j.HoldSyncTimer(true)

	fa := entity.NewFileID()
	s.must(t, s.a, fa, 0, 20, 'a')

	carol := make(chan error, 1)

	go func() { carol <- s.put(bounded(t), s.a, entity.NewFileID(), 0, 90, 'c') }()

	within(t, "carol's pass past its sync", entered)

	s.must(t, s.a, fa, 0, 20, 'b') // alice: fits, and her superseded record is unsynced

	armed := s.clock.Armed(time.Hour / 10)
	bob := make(chan error, 1)

	go func() {
		ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
		bob <- s.put(ctx, s.a, entity.NewFileID(), 0, 70, 'b')
	}()

	within(t, "bob's refusal waiting for a pass", armed)
	let()
	_ = within(t, "carol's answer", carol) // carol's own answer is not this test's

	if err := within(t, "bob's answer", bob); err != nil {
		t.Errorf("bob's write after alice's unsynced overwrite: %v, want served by the next pass", err)
	}
}
