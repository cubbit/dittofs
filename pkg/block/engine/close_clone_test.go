package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
