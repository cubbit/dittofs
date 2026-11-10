package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// errStoreDown is a store's failure of the whole call.
var errStoreDown = errors.New("the store is down")

// newClockFence is a fence over a test clock that has run for an idle minute.
func newClockFence() (*fence, *sim.TestClock) {
	clock := sim.NewTestClock(time.Time{})
	clock.Advance(time.Minute)
	return &fence{clock: clock, logger: slog.New(slog.DiscardHandler)}, clock
}

// A node with nothing outstanding or failing never fences, however long it
// stays idle.
func TestFenceIdleNeverHolds(t *testing.T) {
	f, clock := newClockFence()
	clock.Advance(time.Hour)
	if f.holds() {
		t.Error("an idle node fenced")
	}
}

// A transaction outstanding since t0 fences at t0+fenceAfter exactly, and not
// a nanosecond before; the fence counts once.
func TestFenceHoldsAtTheBound(t *testing.T) {
	f, clock := newClockFence()
	f.begin()
	clock.Advance(fenceAfter - 1)
	if f.holds() {
		t.Fatal("fenced a nanosecond before the bound")
	}
	clock.Advance(1)
	if !f.holds() {
		t.Fatal("not fenced at the bound")
	}
	f.holds()
	if n := f.fences.Load(); n != 1 {
		t.Errorf("fences %d, want 1", n)
	}
}

// A commit beside an outstanding transaction restarts the bound from the
// commit.
func TestFenceCommitRestarts(t *testing.T) {
	f, clock := newClockFence()
	f.begin()
	clock.Advance(20 * time.Second)
	f.begin()
	f.end(context.Background(), nil)
	clock.Advance(fenceAfter - 1)
	if f.holds() {
		t.Fatal("fenced before the bound counted from the commit")
	}
	clock.Advance(1)
	if !f.holds() {
		t.Fatal("not fenced at the bound counted from the commit")
	}
}

// A retrying owner failing holds the node busy with no transaction
// outstanding: it fences fenceAfter after its first attempt began, a retry
// between attempts not restarting the bound.
func TestFenceFailingWhileIdle(t *testing.T) {
	f, clock := newClockFence()
	ctx := withRetryingOwner(context.Background())
	f.begin()
	f.end(ctx, errStoreDown)
	clock.Advance(10 * time.Second)
	f.begin()
	f.end(ctx, errStoreDown)
	if f.outstanding.Load() != 0 {
		t.Fatal("a transaction still outstanding")
	}
	clock.Advance(fenceAfter - 10*time.Second)
	if !f.holds() {
		t.Error("a retrying owner failing for the bound did not fence")
	}
}

// A transaction starting after an idle minute, raced against gate reads:
// none fences, each start beginning a fresh bound. A start that raises the
// count before it stores since is seen busy with the last spell's since.
func TestFenceBeginRacesAReader(t *testing.T) {
	clock := sim.NewTestClock(time.Time{})
	f := &fence{clock: clock, logger: slog.New(slog.DiscardHandler)}
	stop := make(chan struct{})
	var seen atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if f.holds() {
				seen.Add(1)
			}
		}
	})
	oneShot := errors.New("a one-shot call's failure")
	for range 20000 {
		clock.Advance(time.Minute)
		f.begin()
		f.end(context.Background(), oneShot) // neither a commit nor a failure
	}
	close(stop)
	wg.Wait()
	if n := seen.Load(); n > 0 {
		t.Errorf("%d gate reads fenced a transaction just begun", n)
	}
}

// A retrying owner's failure that its own commit followed is not the cause
// of a stall an idle minute later: node_fenced names errNoCommit.
func TestFenceCauseOfAStallAfterARecoveredFailure(t *testing.T) {
	f, clock := newClockFence()
	owner := withRetryingOwner(context.Background())
	f.begin()
	f.end(owner, errStoreDown)
	f.begin()
	f.end(owner, nil)
	clock.Advance(time.Minute)
	f.begin()
	clock.Advance(fenceAfter)
	c, ok := f.condition()
	if !ok || c.Cause != errNoCommit {
		t.Errorf("node_fenced %v naming %v, want errNoCommit", ok, c.Cause)
	}
}

// probe stands in for the store behind the fenced views: each write
// transaction records the count the fence holds outstanding while it runs,
// and answers advanced and err.
type probe struct {
	f        *fence
	seen     int64
	advanced bool
	err      error
}

func (p *probe) call() error { p.seen = p.f.outstanding.Load(); return p.err }

type probeExistence struct {
	Existence
	*probe
}

func (x probeExistence) Commit(context.Context, []kvstore.PendingExistence, *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	return nil, x.call()
}

func (x probeExistence) Remove(context.Context, entity.ShareID, entity.FileID, uint64, kvstore.RemoveOp, []kvstore.PendingOp) error {
	return x.call()
}

func (x probeExistence) Clone(context.Context, kvstore.CloneOp, []kvstore.PendingOp) error {
	return x.call()
}

type probeContent struct {
	Content
	*probe
}

func (c probeContent) Resume(context.Context, entity.ShareID, entity.FileID) (bool, error) {
	return c.advanced, c.call()
}

func (c probeContent) PruneRemovals(context.Context, entity.ShareID, entity.FileID, entity.JournalVersion) error {
	return c.call()
}

type probeLifecycle struct {
	Lifecycle
	*probe
}

func (l probeLifecycle) RaiseVersionLimits(context.Context, map[entity.ShareID]entity.JournalVersion) error {
	return l.call()
}

func (l probeLifecycle) RaiseAtStart(context.Context, []entity.ShardID) (map[entity.ShardID]uint64, error) {
	return nil, l.call()
}

func (l probeLifecycle) RaiseAtAttach(context.Context, entity.ShardID) (uint64, error) {
	return 0, l.call()
}

// fencedCalls is every write transaction the fenced views wrap, each called
// once through its view.
var fencedCalls = map[string]func(ctx context.Context, p *probe) error{
	"Existence.Commit": func(ctx context.Context, p *probe) error {
		_, err := fencedExistence{probeExistence{probe: p}, p.f}.Commit(ctx, nil, nil)
		return err
	},
	"Existence.Remove": func(ctx context.Context, p *probe) error {
		return fencedExistence{probeExistence{probe: p}, p.f}.Remove(ctx, entity.ShareID{}, entity.FileID{}, 0, kvstore.RemoveOp{}, nil)
	},
	"Existence.Clone": func(ctx context.Context, p *probe) error {
		return fencedExistence{probeExistence{probe: p}, p.f}.Clone(ctx, kvstore.CloneOp{}, nil)
	},
	"Content.Resume": func(ctx context.Context, p *probe) error {
		_, err := fencedContent{probeContent{probe: p}, p.f}.Resume(ctx, entity.ShareID{}, entity.FileID{})
		return err
	},
	"Content.PruneRemovals": func(ctx context.Context, p *probe) error {
		return fencedContent{probeContent{probe: p}, p.f}.PruneRemovals(ctx, entity.ShareID{}, entity.FileID{}, entity.JournalVersion{})
	},
	"Lifecycle.RaiseVersionLimits": func(ctx context.Context, p *probe) error {
		return fencedLifecycle{probeLifecycle{probe: p}, p.f}.RaiseVersionLimits(ctx, nil)
	},
	"Lifecycle.RaiseAtStart": func(ctx context.Context, p *probe) error {
		_, err := fencedLifecycle{probeLifecycle{probe: p}, p.f}.RaiseAtStart(ctx, nil)
		return err
	},
	"Lifecycle.RaiseAtAttach": func(ctx context.Context, p *probe) error {
		_, err := fencedLifecycle{probeLifecycle{probe: p}, p.f}.RaiseAtAttach(ctx, 0)
		return err
	},
}

// Every write transaction a view wraps is outstanding while it runs, and a
// retrying owner's call of it failing at the store leaves the node failing.
func TestFenceRecordsEveryWriteTransaction(t *testing.T) {
	for name, call := range fencedCalls {
		t.Run(name, func(t *testing.T) {
			f, _ := newClockFence()
			p := &probe{f: f, err: errStoreDown}
			if err := call(withRetryingOwner(context.Background()), p); !errors.Is(err, errStoreDown) {
				t.Fatalf("the call answered %v, want the store's error", err)
			}
			if p.seen != 1 {
				t.Errorf("%d outstanding while it ran, want 1", p.seen)
			}
			if n, failing := f.outstanding.Load(), f.failing.Load(); n != 0 || failing != 1 {
				t.Errorf("after it: %d outstanding, %d owners failing; want 0 and 1", n, failing)
			}
		})
	}
}

// A retrying owner's error that fails one file or one call is not the
// store's: it ends that owner's failing at the store before it, leaving the
// node not failing. A retrying owner's context is cancelled only when its
// loop stops, so context.Canceled reaches the fence only then.
func TestFenceNotFailingOnACallsOwnError(t *testing.T) {
	// The store refuses a clone of nothing before it reads a key.
	refused := kvstore.Existence{}.Clone(context.Background(), kvstore.CloneOp{}, nil)
	if !kvstore.Refused(refused) {
		t.Fatalf("a clone of nothing answered %v, want the store's refusal", refused)
	}
	for name, err := range map[string]error{
		"conflict":          kvstore.ErrConflict,
		"refused":           refused,
		"stale":             entity.ErrStale,
		"open gen moved":    kvstore.ErrOpenGenMoved,
		"context cancelled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newClockFence()
			owner := withRetryingOwner(context.Background())
			f.begin()
			f.end(owner, errStoreDown)
			f.begin()
			f.end(owner, fmt.Errorf("wrapped: %w", err))
			if n := f.failing.Load(); n != 0 {
				t.Errorf("%d owners failing after the latest attempt's own error, want 0", n)
			}
		})
	}
}

// Resume's batches that committed before a later one failed are a commit:
// they restart the bound, though the call answers the store's error.
func TestFenceResumeAdvancedThenFailedCommits(t *testing.T) {
	f, clock := newClockFence()
	clock.Advance(10 * time.Second)
	p := &probe{f: f, advanced: true, err: errStoreDown}
	if err := fencedCalls["Content.Resume"](withRetryingOwner(context.Background()), p); !errors.Is(err, errStoreDown) {
		t.Fatalf("Resume answered %v, want the store's error", err)
	}
	if got, want := time.Duration(f.lastCommit.Load()), 70*time.Second; got != want {
		t.Errorf("last commit at %v, want %v", got, want)
	}
}

// Failing is per owner: one owner's commit, its own failure or its stop
// clears its failing alone, never another's still failing at the store, so
// the node fences fenceAfter after its last commit; and an owner that stops
// while failing leaves an idle node unfenced.
func TestFenceFailingIsPerOwner(t *testing.T) {
	f, clock := newClockFence()
	failer, other := withRetryingOwner(context.Background()), withRetryingOwner(context.Background())
	f.begin()
	f.end(failer, errStoreDown)
	for _, err := range []error{kvstore.ErrConflict, nil} {
		f.begin()
		f.end(other, err)
	}
	f.release(ownerOf(other))
	if n := f.failing.Load(); n != 1 {
		t.Fatalf("%d owners failing, want the failing owner's 1", n)
	}
	clock.Advance(fenceAfter)
	if !f.holds() {
		t.Fatal("not fenced fenceAfter after the last commit, an owner failing throughout")
	}
	f.release(ownerOf(failer))
	if f.holds() {
		t.Error("the node fenced once its failing owner stopped")
	}
	if n := f.failing.Load(); n != 0 {
		t.Errorf("%d owners failing once the failing owner stopped, want 0", n)
	}
}
