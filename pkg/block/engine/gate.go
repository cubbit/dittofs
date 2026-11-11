package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// gate is one binding's frame: it admits units of work, counts them until
// they are withdrawn, and, once shut, admits none and lets one caller wait
// until the count is zero. The zero value is an open gate.
//
// ponytail: every content call on a share does a compare and swap on the one
// state word, so a busy share on many cores contends on a single cache line.
// Use a sharded count when that shows in a profile.
type gate struct {
	st   atomic.Int64
	mu   sync.Mutex
	idle chan struct{}
	down chan struct{} // closed once the gate is shut; made by shut or shutdown
}

// shutBit marks a shut gate in its state word; the bits below it count the
// units entered and not withdrawn.
const shutBit = 1 << 62

// The joining seam's text for what Unbind waits for.
const (
	whatWorkInFlight = "work in flight"
	whatRemovalBatch = "a removal batch"
	whatGroupBatch   = "a group commit batch"
)

// enter admits one unit, and reports false once the gate is shut. A single
// compare and swap checks the shut bit and adds the unit, so no unit gets in
// after the shut.
func (g *gate) enter() bool {
	for {
		v := g.st.Load()

		if v&shutBit != 0 {
			return false
		}

		if g.st.CompareAndSwap(v, v+1) {
			return true
		}
	}
}

// leave withdraws one unit. The withdrawal that takes a shut gate to zero
// closes the idle channel; that happens once, as nothing enters after the
// shut, so the count never rises again.
func (g *gate) leave() {
	if g.st.Add(-1) != shutBit {
		return
	}

	g.mu.Lock()
	close(g.idle)
	g.mu.Unlock()
}

// shut refuses every later enter. It makes the idle channel before it sets
// the shut bit, so a leave that takes the count to zero always finds a channel
// to close. It closes the shutdown channel after the bit is set, so a call
// woken by it finds the gate shut. A second shut changes nothing.
func (g *gate) shut() {
	g.mu.Lock()

	if g.idle == nil {
		g.idle = make(chan struct{})
	}

	if g.down == nil {
		g.down = make(chan struct{})
	}

	g.mu.Unlock()
	g.st.Or(shutBit)

	g.mu.Lock()

	select {
	case <-g.down:
	default:
		close(g.down)
	}

	g.mu.Unlock()
}

// shutdown is a channel that closes once the gate is shut, so a call waiting
// inside the gate for something that may never come can leave it and let the
// shut gate drain.
func (g *gate) shutdown() <-chan struct{} {
	g.mu.Lock()

	defer g.mu.Unlock()

	if g.down == nil {
		g.down = make(chan struct{})

		if g.st.Load()&shutBit != 0 {
			close(g.down)
		}
	}

	return g.down
}

// drained reports whether the gate is shut and holds no unit.
func (g *gate) drained() bool {
	return g.st.Load() == shutBit
}

// isShut reports whether the gate is shut, without a lock.
func (g *gate) isShut() bool {
	return g.st.Load()&shutBit != 0
}

// wait returns nil once the shut gate holds no unit, errClosed once stop is
// closed, and ctx's own error if ctx ends first; a later wait joins the units
// still entered. A nil stop never ends the wait. It is called only after shut.
func (g *gate) wait(ctx context.Context, stop <-chan struct{}) error {
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()

	if g.st.Load() == shutBit {
		return nil
	}

	select {
	case <-idle:
		return nil
	case <-stop:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leaveOnce is the withdrawal of work that outlives the call that entered
// it: the function it returns withdraws one unit from each gate, once
// however often it is called and from whichever goroutine.
func leaveOnce(gs ...*gate) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			for _, g := range gs {
				g.leave()
			}
		})
	}
}

// joinWork waits until share gate g, already shut, holds no unit, and fires
// the joining seam when it has to wait. A Close that begins ends the wait
// with errClosed: Close's drain of the lifecycle calls waits for the Unbind
// that waits here, while what it waits for, a rebuilt clone waiting for its
// source's shard, ends only once Close stops the work after that drain.
func (e *Engine) joinWork(ctx context.Context, g *gate) error {
	if g.st.Load() != shutBit {
		if f := e.seams.joining.Load(); f != nil {
			(*f)(whatWorkInFlight)
		}
	}

	return g.wait(ctx, e.stopping)
}
