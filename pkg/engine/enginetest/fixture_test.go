package enginetest

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Once the process crashed, its engine's store views never reach the store,
// so a closing engine commits nothing that only recovery may. An existence
// commit answers done, so the closing engine's last group commit ends; every
// other change is refused. The embedded views are nil: a call reaching them
// panics. The fence has no gate (a nil one passes at once), and a clone's gate
// comes before its crash check, so the clone still answers errCrashed.
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
	tc := sim.NewTestClock(time.Unix(0, 0))
	t.Cleanup(func() { tc.Advance(time.Second) }) // the held write returns
	clock := &armGate{Waiter: tc, arming: make(chan struct{}), release: make(chan struct{})}
	e, err := newEntered(clock, []sim.Rule{{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _, _ = e.At(kvtest.OpUpdate, []byte("a")) }()
	select {
	case <-clock.arming:
	case <-time.After(5 * time.Second):
		t.Fatal("after 5 s: the held write's hold not armed")
	}
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

func TestACrashKeepsTheUnsyncedWritesItsVariantKeeps(t *testing.T) {
	f := New(t, WithSegmentCap(4<<20))
	_, file := f.FS.Create(t, f.FS.Admin, f.FS.Root, "f", kvstore.Regular(0o644))
	write := func(off int64) {
		t.Helper()

		_, err := f.E.Write(t.Context(), f.Share.ID, file.ID, off, make([]byte, 512), engine.WriteOpts{})
		if err != nil {
			t.Fatal(err)
		}
	}

	write(0)
	_, _, err := f.E.Commit(t.Context(), f.Share.ID, file.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	write(512)
	write(1024)
	unsynced := f.Seam.Unsynced()

	f.Crash(kit.Keeping(1<<unsynced - 1))

	n, err := f.E.Read(t.Context(), f.Share.ID, file.ID, 0, make([]byte, 4096))
	if err != nil {
		t.Fatal(err)
	}

	if n != 1536 {
		t.Fatalf("after a crash that keeps all %d unsynced writes the file reads %d bytes, want 1536", unsynced, n)
	}
}
