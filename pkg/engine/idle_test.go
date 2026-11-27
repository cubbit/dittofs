package engine_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Idle does not report the group commit idle while a file is due, once the
// clock passed the file's age, or while a batch is in flight, and returns once
// the batch did. A context already ended asks Idle whether the loops are idle
// now, without waiting.
func TestIdleReturnsOnlyWhenNoBatchIsDue(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})

	var commits atomic.Int32

	u := newUnit(t, onCommit(func(
		ctx context.Context,
		next engine.Existence,
		p []kvstore.PendingExistence,
		marks *kvstore.MarksWrite,
	) (map[entity.FileID]error, error) {
		entered <- struct{}{}

		<-release // each batch stays in flight until the test lets it go
		commits.Add(1)

		return next.Commit(ctx, p, marks)
	}))
	t.Cleanup(func() { close(release) }) // runs before the unit's close, so a held batch ends
	now, cancel := context.WithCancel(t.Context())
	cancel()

	a := u.create(t, "a")
	u.write(t, a, 0, []byte("a"))

	if err := u.e.Idle(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Idle before the file's age: %v, want nil", err)
	}

	u.clock.Advance(enginetest.Settings.ExistenceAge)

	if err := u.e.Idle(now, u.share.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Idle with a's age passed and its batch held: %v, want context.Canceled", err)
	}

	<-entered // a's batch, so the wait below is for b's
	release <- struct{}{}

	if err := u.e.Idle(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Idle after a's batch: %v, want nil", err)
	}

	if got := commits.Load(); got != 1 {
		t.Errorf("existence commits once idle: %d, want 1", got)
	}

	if u.e.HasEntry(a) {
		t.Error("a still holds pending existence once idle, want it committed")
	}

	b := u.create(t, "b")
	u.write(t, b, 0, []byte("b"))

	asked := make(chan error, 1)
	go func() { asked <- u.e.CommitExistence(t.Context(), u.share.ID, b) }()

	<-entered

	if err := u.e.Idle(now, u.share.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Idle with b's batch in flight: %v, want context.Canceled", err)
	}

	release <- struct{}{}

	if err := <-asked; err != nil {
		t.Fatalf("CommitExistence(b): %v", err)
	}

	if err := u.e.Idle(t.Context(), u.share.ID); err != nil {
		t.Fatalf("Idle after b's batch: %v, want nil", err)
	}

	if got := commits.Load(); got != 2 {
		t.Errorf("existence commits once idle again: %d, want 2", got)
	}
}

// Idle does not report the share idle while its removal loop has not parked,
// as when the loop is held with a removal posted or runs a removal's step, and
// returns once the loop ran the removal and parked.
func TestIdleWaitsForTheRemovalLoop(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, r.one.ID, f, 8192)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)
	r.wantPosted(t, 1)
	now, cancel := context.WithCancel(t.Context())
	cancel()

	if err := r.e.Idle(now, r.one.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Idle with the removal loop held: %v, want context.Canceled", err)
	}

	entered, release := r.content.holdPrunes(t)
	r.run()
	<-entered

	if err := r.e.Idle(now, r.one.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Idle with the removal's prune in flight: %v, want context.Canceled", err)
	}

	release()

	if err := r.e.Idle(t.Context(), r.one.ID); err != nil {
		t.Fatalf("Idle once the removal loop runs: %v, want nil", err)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{jver(v2)}) {
		t.Errorf("pruned at %v once idle, want exactly the truncate's version %v", p, jver(v2))
	}

	r.wantRemovals(t, r.file(t, h), nil)
}
