package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
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
	for part := range waits {
		if part == "stuck" {
			break
		}
	}
	clock.Advance(stopBound)
	err := <-halted
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
