package health

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Stall is how long a watched loop may go without a turn or an idle
	// report before liveness fails.
	Stall = 60 * time.Second
	// IdleEvery is how often a waiting loop wakes to report Idle at least.
	IdleEvery = 30 * time.Second
)

// Clock is the watchdog's time: the monotonic part of the clock every
// component declares, so a wall-clock step neither fails nor saves a loop.
type Clock interface {
	Elapsed() time.Duration
}

// Watchdog answers liveness from the loops it watches. It reads nothing but
// the time each loop last reported: no condition, store, network or disk.
type Watchdog struct {
	clock Clock
	mu    sync.Mutex
	loops map[string]*Loop
}

// NewWatchdog returns a watchdog on clock, watching no loop.
func NewWatchdog(clock Clock) *Watchdog {
	return &Watchdog{clock: clock, loops: map[string]*Loop{}}
}

// Loop returns the watched loop named name, counted as reported now when it
// is new. A second call with the same name returns the same loop, until it is
// closed.
func (w *Watchdog) Loop(name string) *Loop {
	w.mu.Lock()
	defer w.mu.Unlock()
	l, ok := w.loops[name]
	if !ok {
		l = &Loop{w: w, name: name}
		l.Turn()
		w.loops[name] = l
	}
	return l
}

// Live reports whether every watched loop completed a turn or reported idle
// within Stall.
func (w *Watchdog) Live() bool {
	now := w.clock.Elapsed()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.loops {
		if now-time.Duration(l.last.Load()) > Stall {
			return false
		}
	}
	return true
}

// Loop is one watched loop.
type Loop struct {
	w    *Watchdog
	name string
	last atomic.Int64 // the clock's Elapsed at the last report
}

// Turn reports a turn completed.
func (l *Loop) Turn() { l.last.Store(int64(l.w.clock.Elapsed())) }

// Idle reports the loop waiting with nothing to do. A waiting loop repeats it
// at least every IdleEvery, so a loop that wakes and stalls before its turn
// misses its next report.
func (l *Loop) Idle() { l.Turn() }

// Close reports the loop gone: liveness no longer counts it. Closing it twice,
// or after another loop took its name, does nothing.
func (l *Loop) Close() {
	l.w.mu.Lock()
	defer l.w.mu.Unlock()
	if l.w.loops[l.name] == l {
		delete(l.w.loops, l.name)
	}
}
