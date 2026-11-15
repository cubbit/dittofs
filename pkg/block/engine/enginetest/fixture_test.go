package enginetest

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Once the process crashed, its engine's store views never reach the store,
// so a closing engine commits nothing that only recovery may. An existence
// commit answers done, so the closing engine's last group commit ends; every
// other change is refused. The embedded views are nil: a call reaching them
// panics.
func TestACrashedProcessChangesNothingInTheStore(t *testing.T) {
	crashed := new(atomic.Bool)
	crashed.Store(true)
	x, c := fencedExistence{crashed: crashed}, fencedContent{crashed: crashed}
	ctx := t.Context()
	var share entity.ShareID
	var file entity.FileID
	pending := []kvstore.PendingExistence{{Share: share, File: file}}
	if failed, err := x.Commit(ctx, pending, nil); err != nil || len(failed) != 0 {
		t.Errorf("Commit after the crash: %v, %v; want done, with nothing written", failed, err)
	}
	if err := x.Remove(ctx, share, file, 0, kvstore.RemoveOp{}, nil); !errors.Is(err, errCrashed) {
		t.Errorf("Remove after the crash: %v, want errCrashed", err)
	}
	if err := x.Clone(ctx, kvstore.CloneOp{}, nil); !errors.Is(err, errCrashed) {
		t.Errorf("Clone after the crash: %v, want errCrashed", err)
	}
	if _, err := c.Resume(ctx, share, file); !errors.Is(err, errCrashed) {
		t.Errorf("Resume after the crash: %v, want errCrashed", err)
	}
	if err := c.PruneRemovals(ctx, share, file, entity.JournalVersion{}); !errors.Is(err, errCrashed) {
		t.Errorf("PruneRemovals after the crash: %v, want errCrashed", err)
	}
}

// Under HoldNextWrite's rule, a second write transaction decided while the
// held one is still arming its hold does not close the channel: only the
// held write does, once its hold is armed on the clock.
func TestOnlyTheHeldWriteClosesHoldNextWritesChannel(t *testing.T) {
	clock := &armGate{Waiter: sim.NewTestClock(time.Unix(0, 0)), arming: make(chan struct{}), release: make(chan struct{})}
	e, err := newEntered(clock, []sim.Rule{{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _, _ = e.At(kvtest.OpUpdate, []byte("a")) }()
	<-clock.arming
	if d, _, _ := e.At(kvtest.OpUpdate, []byte("b")); d != 0 {
		t.Fatalf("the second write was held %v, want 0: the rule holds one", d)
	}
	select {
	case <-e.ch:
		t.Error("the channel closed on the unheld write, before the held write's hold was armed")
	default:
	}
	close(clock.release)
	select {
	case <-e.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("after 5 s: the channel still open once the held write's hold was armed")
	}
}

// armGate is a clock that stops a hold just before it is armed, telling the
// test it got there, until the test releases it.
type armGate struct {
	sim.Waiter
	arming, release chan struct{}
}

func (c *armGate) After(d time.Duration) <-chan struct{} {
	close(c.arming)
	<-c.release
	return c.Waiter.After(d)
}
