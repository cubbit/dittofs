package commands

import "time"

// systemClock is the node's clock in production: the wall clock, elapsed
// time from a monotonic origin, and timers on the runtime's.
type systemClock struct{ origin time.Time }

func newSystemClock() systemClock { return systemClock{origin: time.Now()} }

func (systemClock) Now() time.Time { return time.Now() }

func (c systemClock) Elapsed() time.Duration { return time.Since(c.origin) }

func (systemClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}
