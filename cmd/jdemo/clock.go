package main

import "time"

// realClock is the journal's clock on wall time: the demo daemon has no
// injected clock, so the journal reads the system's.
type realClock struct{}

var startInstant = time.Now()

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Elapsed() time.Duration { return time.Since(startInstant) }

func (realClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		timer := time.NewTimer(d)
		<-timer.C
		close(ch)
	}()
	return ch
}
