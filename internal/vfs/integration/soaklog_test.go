package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The soak's fault log lives here, without the soak's build tags, so the
// unit suite, which runs every untagged test under the race detector, checks
// it under concurrent use.

// firedFault is one fault a schedule fired: where, on what, and the client
// call in flight, 0 for none.
type firedFault struct {
	side, op, key string
	tear          int
	err           error
	call          int
	desc          string
}

// faultLog records every fault the seed's schedules fire, in order, with the
// client call in flight when each fired. The writer sets the call; the
// engine's goroutines consult the schedules, so every field is under mu.
type faultLog struct {
	mu     sync.Mutex
	call   int
	desc   string
	paused bool
	fired  []firedFault
}

// loggedFault is a schedule whose every non-zero answer goes to log.
type loggedFault struct {
	log   *faultLog
	side  string
	inner sim.Fault
}

func (l loggedFault) At(op string, key []byte) (time.Duration, int, error) {
	// One lock covers the pause, the draw and the record, so a fault drawn
	// while paused is never logged, and one drawn while running is logged
	// with the call in flight when it was drawn.
	//
	// decision: the lock is held across inner.At because no soak rule
	// delays: sim.Schedule.At waits out a rule's Delay on the clock, which
	// would block every faulted call and the writer's set and since while it
	// waits. Overturned by a soak rule with a Delay, which needs the wait
	// taken outside the lock.
	l.log.mu.Lock()
	defer l.log.mu.Unlock()

	// A fault while the fixture restarts fails the fixture, not the engine.
	if l.log.paused {
		return 0, 0, nil
	}

	d, tear, err := l.inner.At(op, key)
	if tear != 0 || err != nil {
		l.log.fired = append(l.log.fired, firedFault{
			side: l.side,
			op:   op,
			key:  string(key),
			tear: tear,
			err:  err,
			call: l.log.call,
			desc: l.log.desc,
		})
	}

	return d, tear, err
}

func (l *faultLog) set(call int, desc string, paused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.call, l.desc, l.paused = call, desc, paused
}

// since is every fault fired after the first n.
func (l *faultLog) since(n int) []firedFault {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]firedFault(nil), l.fired[n:]...)
}

func (l *faultLog) String() string {
	var b strings.Builder

	for i, f := range l.since(0) {
		in := "no client call in flight"
		if f.call != 0 {
			in = fmt.Sprintf("in call %d, %s", f.call, f.desc)
		}

		fmt.Fprintf(
			&b,
			"  %d. %s %s %s: tear %d, err %v, %s\n",
			i+1,
			f.side,
			f.op,
			f.key,
			f.tear,
			f.err,
			in,
		)
	}

	return b.String()
}

// lostRecord reports whether f made a journal record not durable: a lost
// write, or one torn with no error.
func (f firedFault) lostRecord() bool {
	return f.side == "journal" && f.op == kit.OpWrite && (f.tear < 0 || f.tear > 0 && f.err == nil)
}

// TestFaultLogUnderConcurrentCalls has many goroutines consult one logged
// schedule while the writer moves the call in flight and reads the log, as
// the engine's goroutines and the soak's writer do: each fault is recorded
// once, and the race detector, which the unit suite runs, sees no unguarded
// access.
func TestFaultLogUnderConcurrentCalls(t *testing.T) {
	sched, err := sim.NewSchedule(1, nil, sim.Rule{
		Op:     kit.OpWrite,
		Key:    "*",
		Action: sim.FaultAction{TearAfter: sim.Lose},
	})
	if err != nil {
		t.Fatal(err)
	}

	log := &faultLog{}
	f := loggedFault{log: log, side: "journal", inner: sched}

	const goroutines, calls = 8, 100

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range calls {
				_, _, _ = f.At(kit.OpWrite, []byte("0001.seg"))
			}
		})
	}

	for k := range calls {
		log.set(k+1, "a call", false)
		_ = log.since(0)
	}

	wg.Wait()

	got := log.since(0)
	if len(got) != goroutines*calls || !got[0].lostRecord() {
		t.Fatalf("the log holds %d faults, the first a lost record %v; want %d, true",
			len(got), len(got) > 0 && got[0].lostRecord(), goroutines*calls)
	}

	if s := log.String(); !strings.Contains(s, "journal write 0001.seg") {
		t.Errorf("the log prints %q, want each fault named", s[:min(len(s), 80)])
	}

	log.set(0, "", true)

	if _, tear, _ := f.At(kit.OpWrite, []byte("0001.seg")); tear != 0 {
		t.Errorf("a paused log answered a tear of %d, want none", tear)
	}

	if n := len(log.since(0)); n != goroutines*calls {
		t.Errorf("a paused log recorded a fault: %d faults, want %d", n, goroutines*calls)
	}
}
