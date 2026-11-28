//go:build jdemo

package jdemo

import "time"

// realClock is the journal's clock on wall time: the demo has no injected
// clock, so the journal reads the system's.
type realClock struct{}

var startInstant = time.Now()

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Elapsed() time.Duration { return time.Since(startInstant) }

func (realClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}
