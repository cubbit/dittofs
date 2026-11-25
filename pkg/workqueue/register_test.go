package workqueue_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

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
// cannot honour, and never substitutes a default; it numbers the sources it
// accepts in registration order.
func TestRegisterRefusesLimitsItCannotHonour(t *testing.T) {
	for _, c := range []struct {
		field string
		bad   func(*workqueue.Limits)
	}{
		{"Batch", func(l *workqueue.Limits) { l.Batch = 0 }},
		{"Concurrency", func(l *workqueue.Limits) { l.Concurrency = -1 }},
		{"Rate", func(l *workqueue.Limits) { l.Rate = 10 }},
		{"Rate", func(l *workqueue.Limits) { l.Rate = math.NaN() }},
		{"Weight", func(l *workqueue.Limits) { l.Weight = 0 }},
		{"Total", func(l *workqueue.Limits) { l.Total = 0 }},
		{"RunTimeout", func(l *workqueue.Limits) { l.RunTimeout = 0 }},
		{"Deadline", func(l *workqueue.Limits) { l.Deadline = -time.Second }},
	} {
		l := validLimits()
		c.bad(&l)

		got := panicOf(func() { workqueue.New(nil, nil).Register(noWork{}, l) })
		if !strings.Contains(got, "Limits."+c.field+" ") {
			t.Errorf("Register(%+v) panic = %q, want one naming Limits.%s", l, got, c.field)
		}
	}

	s := workqueue.New(nil, nil)
	l := validLimits()
	l.Deadline = time.Minute

	for want := range 2 {
		var id workqueue.SourceID

		if got := panicOf(func() { id = s.Register(noWork{}, l) }); got != "" {
			t.Fatalf("Register(%+v) panicked: %s", l, got)
		}

		if id != workqueue.SourceID(want) {
			t.Errorf("registration %d: SourceID = %d, want %d", want, id, want)
		}
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
