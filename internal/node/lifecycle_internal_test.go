package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/sim"
)

// A part whose stop misses its bound leaves open the parts it uses, and only
// those: a part started before it that it does not use still stops. A later
// halt, once the stuck stop ended, stops what the first one left.
func TestHaltLeavesOpenWhatAStuckPartUses(t *testing.T) {
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(time.Unix(0, 0))
	n := &Node{cfg: Config{Clock: clock}}
	var mu sync.Mutex
	var stopped []string
	stop := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, name)
			return nil
		}
	}
	release := make(chan struct{})
	waits := make(chan string, 8)
	n.life.stepped = func(step string) {
		if part, ok := strings.CutPrefix(step, "wait "); ok {
			waits <- part
		}
	}
	n.started("used", stop("used"))
	n.started("unrelated", stop("unrelated"))
	n.started("stuck", func(ctx context.Context) error { <-release; return stop("stuck")(ctx) }, "used")
	n.started("last", stop("last"))

	halted := make(chan error, 1)
	go func() { halted <- n.halt(t.Context(), 0) }()
	backstop := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case part := <-waits:
			waiting = part != "stuck"
		case <-backstop:
			t.Fatal("halt never waited on the stuck part's stop")
		}
	}
	clock.Advance(stopBound)
	var err error
	select {
	case err = <-halted:
	case <-time.After(10 * time.Second):
		t.Fatal("halt still running 10 s after the stuck part's bound")
	}
	if err == nil || !strings.Contains(err.Error(), "stop stuck: not stopped within") || !strings.Contains(err.Error(), "stop used: left open") {
		t.Errorf("halt with a stuck part: got %v, want it named past its bound and the part it uses left open", err)
	}
	mu.Lock()
	got := slices.Clone(stopped)
	mu.Unlock()
	if want := []string{"last", "unrelated"}; !slices.Equal(got, want) {
		t.Errorf("parts stopped while one was stuck: got %v, want %v", got, want)
	}

	close(release)
	if err := n.halt(t.Context(), 0); err != nil {
		t.Errorf("a second halt once the stuck stop ended: got %v, want nil", err)
	}
	if want := []string{"last", "unrelated", "stuck", "used"}; !slices.Equal(stopped, want) {
		t.Errorf("parts stopped after the second halt: got %v, want %v", stopped, want)
	}
	if len(n.life.parts) != 0 {
		t.Errorf("parts left after the second halt: got %d, want none", len(n.life.parts))
	}
}

// A part whose stop fails is kept, like one past its bound: the parts it uses
// and the store stay open, and a later Stop runs its stop again, then closes
// the store.
func TestStopRetriesAPartWhoseStopFailed(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{cfg: Config{Clock: sim.NewTestClock(time.Unix(0, 0)), KV: db}}
	var used, engineStops int
	n.started("used", func(context.Context) error { used++; return nil })
	n.started("engine", func(context.Context) error {
		engineStops++
		if engineStops == 1 {
			return errors.New("injected")
		}
		return nil
	}, "used")

	err = n.Stop(t.Context())
	for _, want := range []string{"stop engine: injected", "stop used: left open", "stop store: left open"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stop with the engine's stop failing: got %v, want it to say %q", err, want)
		}
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err != nil {
		t.Errorf("the KV after a Stop whose engine stop failed: %v, want it open", err)
	}
	if used != 0 {
		t.Errorf("stops of the part the engine uses, its stop failed: got %d, want 0", used)
	}

	if err := n.Stop(t.Context()); err != nil {
		t.Errorf("a second Stop: got %v, want nil", err)
	}
	if engineStops != 2 || used != 1 {
		t.Errorf("stops after the second Stop: engine %d, used %d; want engine 2, used 1", engineStops, used)
	}
	if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err == nil {
		t.Error("the KV after the second Stop: a View succeeded, want it closed")
	}
}

// A part carrying its own bound is waited on for that bound, not stopBound:
// one of 120 s whose stop holds is still waited on past 30 s, and is named
// not stopped within its 120 s once the clock has moved that far.
func TestHaltWaitsAPartForItsOwnBound(t *testing.T) {
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(time.Unix(0, 0))
	n := &Node{cfg: Config{Clock: clock}}
	waits := make(chan string, 1)
	n.life.stepped = func(step string) {
		if part, ok := strings.CutPrefix(step, "wait "); ok {
			waits <- part
		}
	}
	release := make(chan struct{})
	n.life.parts = []*part{{name: "drain", bound: func() time.Duration { return 120 * time.Second },
		stop: func(context.Context) error { <-release; return nil }}}
	defer close(release)

	halted := make(chan error, 1)
	go func() { halted <- n.halt(t.Context(), 0) }()
	select {
	case <-waits:
	case <-time.After(10 * time.Second):
		t.Fatal("halt never waited on the part")
	}
	clock.Advance(120 * time.Second)
	select {
	case err := <-halted:
		if err == nil || !strings.Contains(err.Error(), "stop drain: not stopped within 2m0s") {
			t.Errorf("halt past the part's own bound: got %v, want it named not stopped within 2m0s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("halt still running 10 s after the part's bound")
	}
}
