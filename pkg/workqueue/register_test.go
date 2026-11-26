package workqueue_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/workqueue"
)

// noWork is a source with nothing to do: Register never calls it.
type noWork struct{}

func (noWork) Rebuild(context.Context, workqueue.TenantID, func(workqueue.Item)) error { return nil }
func (noWork) Run(context.Context, []workqueue.Item, func(int, workqueue.Result))      {}

func validLimits() workqueue.Limits {
	return workqueue.Limits{Batch: 1, Concurrency: 1, Weight: 1, Total: 1, RunTimeout: time.Second}
}

// Register refuses, with a panic naming the field, every limit the scheduler
// cannot honour, and never substitutes a default.
func TestRegisterRefusesLimitsItCannotHonour(t *testing.T) {
	for _, c := range []struct {
		field string
		bad   func(*workqueue.Limits)
	}{
		{"Batch", func(l *workqueue.Limits) { l.Batch = 0 }},
		{"Concurrency", func(l *workqueue.Limits) { l.Concurrency = 0 }},
		{"Rate", func(l *workqueue.Limits) { l.Rate = 10 }},
		{"Rate", func(l *workqueue.Limits) { l.Rate = math.NaN() }},
		{"Weight", func(l *workqueue.Limits) { l.Weight = 0 }},
		{"Total", func(l *workqueue.Limits) { l.Total = 0 }},
		{"RunTimeout", func(l *workqueue.Limits) { l.RunTimeout = 0 }},
		{"Deadline", func(l *workqueue.Limits) { l.Deadline = -time.Second }},
	} {
		l := validLimits()
		c.bad(&l)

		got := panicOf(func() { newScheduler(t).Register(noWork{}, l) })
		if !strings.Contains(got, "Limits."+c.field+" ") {
			t.Errorf("Register(%+v) panic = %q, want one naming Limits.%s", l, got, c.field)
		}
	}

}

// Register numbers the sources it accepts in registration order, a Deadline of
// 0 (retry for as long as the source asks) among them.
func TestRegisterNumbersSourcesInOrder(t *testing.T) {
	s := newScheduler(t)
	timed := validLimits()
	timed.Deadline = time.Minute

	for want, l := range []workqueue.Limits{validLimits(), timed} {
		var id workqueue.SourceID

		if got := panicOf(func() { id = s.Register(noWork{}, l) }); got != "" {
			t.Fatalf("Register(%+v) panicked: %s", l, got)
		}

		if id != workqueue.SourceID(want) {
			t.Errorf("registration %d: SourceID = %d, want %d", want, id, want)
		}
	}
}

// newScheduler builds a scheduler over the set's test clock and a watchdog loop,
// as the node hands them over.
func newScheduler(t *testing.T) *workqueue.Scheduler {
	t.Helper()

	clk := sim.NewTestClock(time.Unix(0, 0))
	loop := health.NewWatchdog(clk).Loop("workqueue")
	t.Cleanup(loop.Close)

	return workqueue.New(clk, loop)
}

// Register refuses a nil Source, which it could never run.
func TestRegisterRefusesNilSource(t *testing.T) {
	if got := panicOf(func() { newScheduler(t).Register(nil, validLimits()) }); !strings.Contains(got, "nil Source") {
		t.Errorf("Register(nil) panic = %q, want one naming the nil Source", got)
	}
}

// panicOf returns what f panics with, or "" when it returns.
func panicOf(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()

	f()

	return ""
}
