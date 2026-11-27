package fuzz

import (
	"fmt"
	"runtime/debug"
)

// Call is a call a step started on a goroutine of its own, for a later step to
// finish. With a hook in the component that parks the call, the input chooses
// what runs while it is parked, instead of the scheduler.
//
// The step that begins a call must not return before the call is where it
// parks, or the next step races it: it waits with WaitParked. A program may end without finishing a call, so what
// releases a parked one is registered on the input's cleanup, or the leak check
// reports it.
type Call[T any] struct {
	done   chan struct{}
	result T

	// panicked is the panic of the call with its stack, empty when it did not
	// panic, and finished that a step has taken it over.
	panicked string
	finished bool
}

// Begin starts call. It runs from a step, as every use of env does.
//
// A panic of the call is held for the step that finishes it, so it fails with a
// trace as a panic of a step does. One that no step takes over fails the input
// once it has ended.
func Begin[T any](env *Env, call func() T) *Call[T] {
	c := &Call[T]{
		done: make(chan struct{}),
	}
	env.unfinished = append(env.unfinished, c.untaken)

	go func() {
		defer close(c.done)
		defer func() {
			if p := recover(); p != nil {
				c.panicked = fmt.Sprintf("in a background call: %v\n%s", p, debug.Stack())
			}
		}()

		c.result = call()
	}()

	return c
}

// Done is closed once the call has returned.
func (c *Call[T]) Done() <-chan struct{} {
	return c.done
}

// WaitParked waits until entered, the signal of a hook, is closed or the call
// has returned, and reports whether the hook was reached. It waits on no wall
// clock: a call that does neither runs into the step bound.
func (c *Call[T]) WaitParked(entered <-chan struct{}) bool {
	select {
	case <-entered:
		return true
	case <-c.done:
	}

	// A hook that does not hold the call leaves both signals ready, and select
	// picks one at random.
	select {
	case <-entered:
		return true
	default:
		return false
	}
}

// Finish waits for the call and returns what it answered, or panics as the call
// did. A call that never returns runs into the step bound.
func (c *Call[T]) Finish() T {
	<-c.done
	c.finished = true

	if c.panicked != "" {
		panic(c.panicked)
	}

	return c.result
}

// untaken is the panic of a returned call that no step finished.
func (c *Call[T]) untaken() string {
	select {
	case <-c.done:
	default:
		// Still running: the leak check reports it.
		return ""
	}

	if c.finished {
		return ""
	}

	return c.panicked
}
