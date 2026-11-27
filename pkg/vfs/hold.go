package vfs

import (
	"context"
	"time"
)

// holdKey keys the hold a call carries. A struct type, never a string, so no
// other package's key can collide with it.
type holdKey struct{}

// WithHold bounds the service's one wait inside a call: behind a running clone
// or copy chunk. Zero, or no WithHold at all, answers ErrDelay(DelayClone) at
// once. A later WithHold on the same context replaces an earlier one, so the
// innermost bound is the one the call carries.
func WithHold(ctx context.Context, max time.Duration) context.Context {
	return context.WithValue(ctx, holdKey{}, max)
}

// Hold returns the bound WithHold set on ctx and whether the call carries one.
// It is false on a bare context, and a bound of zero is carried like any other.
func Hold(ctx context.Context) (max time.Duration, ok bool) {
	max, ok = ctx.Value(holdKey{}).(time.Duration)
	return max, ok
}
