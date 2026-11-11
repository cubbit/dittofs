package engine_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// lateServed is open state's CloneHold whose Served, once its context ended,
// returns only when gate closes: a rebuilt clone's wait slow to see the stop.
type lateServed struct {
	engine.CloneHold
	gate <-chan struct{}
}

func (h lateServed) Served(ctx context.Context, s entity.ShardID) error {
	err := h.CloneHold.Served(ctx, s)
	if ctx.Err() != nil {
		<-h.gate
	}

	return err
}

// cloneSource is the ID of src's clone-kind open record in the store.
func (r *rig) cloneSource(t *testing.T, src entity.FileID) entity.OpenID {
	t.Helper()

	recs, err := r.fs.Store.Opens().Of(t.Context(), r.one.ID, src)
	if err != nil {
		t.Fatal(err)
	}

	i := slices.IndexFunc(recs, func(o entity.OpenRecord) bool { return o.Kind == entity.OpenCloneSource })
	if i < 0 {
		t.Fatal("the source holds no clone-kind open record")
	}

	return recs[i].ID
}

// cloneSourceHeld reports whether src holds a clone-kind open record in the
// store.
func (r *rig) cloneSourceHeld(t *testing.T, src entity.FileID) bool {
	t.Helper()

	recs, err := r.fs.Store.Opens().Of(t.Context(), r.one.ID, src)
	if err != nil {
		t.Fatal(err)
	}

	return slices.ContainsFunc(recs, func(o entity.OpenRecord) bool { return o.Kind == entity.OpenCloneSource })
}

// A clone rebuilt from its removal record, waiting for its source's shard
// when Close begins, is joined by Close: with the wait slow to see the stop,
// Close reports the clone goroutine still running at the end of the stop
// bound, and a Close once the wait has ended answers nil with the clone
// ended, so nothing of the clone runs after Close returned.
func TestCloseJoinsARebuiltCloneWaitingForItsSourcesShard(t *testing.T) {
	m := newRemovalCrash(t, false)

	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)

	deps := m.deps
	deps.Holds = lateServed{CloneHold: m.deps.Holds, gate: gate}

	e, err := engine.New(deps)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(e.CloseGroupCommits)

	if err := e.Apply(enginetest.Settings); err != nil {
		t.Fatal(err)
	}

	m.e = e
	m.hold(t, e)
	m.begin(t)
	m.run()

	waiting := joinOf(m.e, "a clone goroutine")
	closed := closing(m.e, context.Background())

	reached(t, waiting, closed)
	stays(t, closed, &events{}) // Close is still in the clone join
	m.clock.Advance(engine.StopBoundDefault)

	if err := <-closed; !unavailable(err) || !strings.Contains(err.Error(), "clone goroutine") {
		t.Fatalf("Close = %v, want ErrUnavailable naming the clone goroutine", err)
	}

	open()

	if err := m.e.Close(t.Context()); err != nil {
		t.Fatalf("Close once the wait ended = %v, want nil", err)
	}

	s, _ := m.e.Registrations(m.src)
	_, d := m.e.Registrations(m.dst)

	if s != 0 || d != 0 {
		t.Errorf("after Close the rebuilt clone holds %d source and %d destination registrations, want it ended", s, d)
	}
}

// A Close that stops a clone's loop before its last batch takes no Release
// of the source: the clone-kind open record stays in the store, and the next
// start rebuilds the clone from its removal record, holds the source from
// that record and finishes it, the destination reading the source.
func TestACloseBeforeTheLastBatchLeavesTheSourceHeldForTheRestart(t *testing.T) {
	r := newRig(t, withOpenState())
	r.serve(t)
	r.begin(t)
	r.run()
	r.e.FakeCloneDone()

	src, dst := r.newFile2(t, "src"), r.newFile2(t, "dst")
	want := fill(runLen, 's')

	if _, err := r.e.Write(t.Context(), r.one.ID, src, 0, want, engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}

	// Phase 2 fails until the restart, the hold taken: the clone is
	// removal_stalled only after its Hold answered and five batches failed.
	r.content.freeze(true)

	errc := make(chan error, 1)

	go func() {
		errc <- r.e.Clone(context.Background(), r.one.ID, src, r.one.ID, dst, vfserr.Extent{Len: runLen})
	}()

	r.until(t, "the clone's phase 2 removal_stalled", func() bool { return r.e.RemovalStalled(dst) })

	if err := r.e.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	if err := <-errc; !unavailable(err) {
		t.Errorf("Clone = %v, want ErrUnavailable: Close ended its loop", err)
	}

	// The probe closes the open the clone's Hold took, still held only if
	// no Release followed the cancelled phase 2; the restart's table is a new
	// one over the store, which keeps the record either way.
	hold := r.cloneSource(t, src)
	if err := r.tb.Opens().CloneClose(t.Context(), hold); err != nil {
		t.Fatalf("closing the clone's open after Close: %v, want it still held: a cancelled phase 2 was followed by a Release", err)
	}

	r.restart(t)
	r.content.freeze(false)
	r.serve(t)
	r.begin(t)
	r.run()
	r.cloneDone(t, src, dst)

	if got := r.readIn(t, r.one.ID, dst, 0, runLen); !bytes.Equal(got, want) {
		t.Error("the clone the restart finished does not read the source")
	}

	r.until(t, "the hold released once the clone is done", func() bool { return !r.cloneSourceHeld(t, src) })
}
