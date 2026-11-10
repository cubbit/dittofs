package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// gate is one binding's frame: it admits units of work, counts them until
// they are withdrawn, and, once shut, admits none and lets one caller wait
// until the count is zero. The zero value is an open gate.
type gate struct {
	st   atomic.Int64
	mu   sync.Mutex
	idle chan struct{}
}

// shutBit marks a shut gate in its state word; the bits below it count the
// units entered and not withdrawn.
const shutBit = 1 << 62

// whatWorkInFlight names the join of a share's gate for the joining seam.
const whatWorkInFlight = "work in flight"

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
// to close. A second shut changes nothing.
func (g *gate) shut() {
	g.mu.Lock()

	if g.idle == nil {
		g.idle = make(chan struct{})
	}

	g.mu.Unlock()
	g.st.Or(shutBit)
}

// isShut reports whether the gate is shut, without a lock.
func (g *gate) isShut() bool {
	return g.st.Load()&shutBit != 0
}

// wait returns nil once the shut gate holds no unit, and ctx's own error if
// ctx ends first; a later wait joins the units still entered. It is called
// only after shut.
func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()

	if g.st.Load() == shutBit {
		return nil
	}

	select {
	case <-idle:
		return nil
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
// the joining seam when it has to wait.
func (e *Engine) joinWork(ctx context.Context, g *gate) error {
	if g.st.Load() != shutBit {
		if f := e.seams.joining.Load(); f != nil {
			(*f)(whatWorkInFlight)
		}
	}

	return g.wait(ctx)
}
