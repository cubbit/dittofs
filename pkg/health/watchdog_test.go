package health_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Liveness fails only when a watched loop has neither turned nor reported idle
// within 60 s, on the monotonic clock, advanced and never slept.
func TestWatchdogLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(c *sim.TestClock, l *health.Loop)
		want bool
	}{
		{"silent 60 s", func(c *sim.TestClock, _ *health.Loop) { c.Advance(60 * time.Second) }, true},
		{"silent 60 s and 1 ns", func(c *sim.TestClock, _ *health.Loop) { c.Advance(60*time.Second + time.Nanosecond) }, false},
		{"closed, then silent an hour", func(c *sim.TestClock, l *health.Loop) {
			l.Close()
			c.Advance(time.Hour)
		}, true},
		{"idle every 30 s for an hour", func(c *sim.TestClock, l *health.Loop) {
			for range 120 {
				c.Advance(health.IdleEvery)
				l.Idle()
			}
		}, true},
		{"a turn 59 s ago", func(c *sim.TestClock, l *health.Loop) {
			c.Advance(50 * time.Second)
			l.Turn()
			c.Advance(59 * time.Second)
		}, true},
		{"a wall-clock step of a day", func(c *sim.TestClock, _ *health.Loop) { c.Jump(24 * time.Hour) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sim.NewTestClock(time.Unix(0, 0))
			w := health.NewWatchdog(c)
			tc.run(c, w.Loop("reconcile"))
			w.Loop("runner").Turn() // a second loop, always fresh
			if got := w.Live(); got != tc.want {
				t.Errorf("Live: got %v, want %v", got, tc.want)
			}
		})
	}
}

// A loop asked for twice by name is one loop: its report counts for both.
func TestWatchdogLoopByName(t *testing.T) {
	c := sim.NewTestClock(time.Unix(0, 0))
	w := health.NewWatchdog(c)
	w.Loop("sweep")
	c.Advance(61 * time.Second)
	w.Loop("sweep").Idle()
	if !w.Live() {
		t.Error("Live after the named loop reported again: got false, want true")
	}
}

// Closing a loop whose name a new loop has since taken leaves the new one
// watched.
func TestWatchdogCloseOldLoop(t *testing.T) {
	c := sim.NewTestClock(time.Unix(0, 0))
	w := health.NewWatchdog(c)
	old := w.Loop("runner")
	old.Close()
	w.Loop("runner")
	old.Close()
	c.Advance(61 * time.Second)
	if w.Live() {
		t.Error("Live with the new runner loop silent 61 s: got true, want false")
	}
}

type conditions []health.Condition

// Conditions reads the slice through its pointer, so a condition the test
// raises shows at the next read.
func (c *conditions) Conditions(dst []health.Condition) []health.Condition {
	return append(dst, *c...)
}

// The evaluator reads every source it holds when asked, and a removed
// source's conditions are gone.
func TestEvaluatorSources(t *testing.T) {
	a := &conditions{{Name: "journal_full"}}
	b := &conditions{{Name: "node_fenced"}, {Name: "journal_lost"}}
	var e health.Evaluator
	e.Add(a)
	e.Add(b)
	if got := names(e.Conditions(nil)); got != "journal_full,node_fenced,journal_lost" {
		t.Errorf("Conditions with both sources: got %s, want journal_full,node_fenced,journal_lost", got)
	}
	*a = append(*a, health.Condition{Name: "journal_read_only"})
	if got := names(e.Conditions(nil)); got != "journal_full,journal_read_only,node_fenced,journal_lost" {
		t.Errorf("Conditions after a source raised one: got %s, want it read again", got)
	}
	e.Remove(a)
	if got := names(e.Conditions(nil)); got != "node_fenced,journal_lost" {
		t.Errorf("Conditions after Remove: got %s, want node_fenced,journal_lost", got)
	}
}

func names(cs []health.Condition) string {
	var s string
	for i, c := range cs {
		if i > 0 {
			s += ","
		}
		s += c.Name
	}
	return s
}

// Each scope reads as RFC 25's name for it, and one it does not define as
// its number.
func TestScopeString(t *testing.T) {
	for s, want := range map[health.Scope]string{health.ScopeStore: "store", health.ScopeNamespace: "namespace",
		health.ScopeJournal: "journal", health.ScopeShare: "share", health.ScopeNode: "node",
		health.ScopeInstallation: "installation", 0: "scope(0)", 7: "scope(7)"} {
		if got := s.String(); got != want {
			t.Errorf("Scope(%d).String(): got %q, want %q", uint8(s), got, want)
		}
	}
}
