package vfs_test

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// A call carries the bound WithHold set on it, and a bound of zero is carried
// like any other: zero answers at once because a hold of none is a hold.
func TestHoldRoundTrip(t *testing.T) {
	for _, max := range []time.Duration{time.Second, 0, 10 * time.Second} {
		got, ok := vfs.Hold(vfs.WithHold(context.Background(), max))
		if !ok {
			t.Fatalf("Hold(WithHold(%v)) reports no hold", max)
		}
		if got != max {
			t.Fatalf("Hold(WithHold(%v)) = %v, want the bound set", max, got)
		}
	}
}

// The innermost WithHold is the one the call carries: a second bound replaces
// the first rather than being ignored.
func TestHoldNesting(t *testing.T) {
	outer := vfs.WithHold(context.Background(), 10*time.Second)
	inner := vfs.WithHold(outer, 2*time.Second)

	if got, _ := vfs.Hold(inner); got != 2*time.Second {
		t.Fatalf("Hold(inner) = %v, want the innermost 2s", got)
	}
	if got, _ := vfs.Hold(outer); got != 10*time.Second {
		t.Fatalf("Hold(outer) = %v, want the outer 10s unchanged", got)
	}
}

// A context WithHold never set carries no hold, and one carrying other values
// still carries none: the reader looks for the hold alone.
func TestHoldBareContext(t *testing.T) {
	other := context.WithValue(context.Background(), struct{ v string }{"other"}, time.Second)
	for name, ctx := range map[string]context.Context{
		"background":  context.Background(),
		"other value": other,
	} {
		if got, ok := vfs.Hold(ctx); ok {
			t.Fatalf("Hold on %s = %v, true, want no hold", name, got)
		}
	}
}
