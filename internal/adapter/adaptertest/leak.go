// Package adaptertest holds what the adapters' tests share: the leak check,
// and below it the test-only protocol the core is proven on.
package adaptertest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/resources"
)

// CheckLeaks, called before the test starts anything, fails it at its end
// when a goroutine or descriptor opened since is still open, the live heap
// grew past prof's default slack (checked only without the race detector), a
// budget of b (nil: none) still holds bytes, or its connection limit still
// counts a connection. Its checks are process-wide, as prof.Watch's are: a
// test that calls it runs beside no parallel test, and its subtests do not
// call it.
func CheckLeaks(t testing.TB, b *resources.Budgets) {
	t.Helper()

	// Cleanups run last-in first-out: the budgets are read after the watch has
	// waited for the test's goroutines to return, so a release one of them
	// makes on its way out is counted.
	t.Cleanup(func() {
		t.Helper()

		if err := budgetsHeld(b); err != nil {
			t.Errorf("adaptertest: %v", err)
		}
	})
	prof.Watch(t, prof.Options{})
}

// budgetsHeld names each budget of b that still holds bytes, and the
// connection limit while it counts a connection, nil when none does or b is
// nil.
func budgetsHeld(b *resources.Budgets) error {
	if b == nil {
		return nil
	}

	var held []error
	note := func(name string, reserved int64) {
		if reserved > 0 {
			held = append(held, fmt.Errorf("the %s budget still holds %d bytes", name, reserved))
		}
	}
	if b.Receive != nil {
		note("Receive", b.Receive.Reserved())
	}
	if b.Reply != nil {
		note("Reply", b.Reply.Reserved())
	}
	if b.Connection != nil {
		note("Connection", b.Connection.Reserved())
	}
	if b.Hold != nil {
		note("Hold", b.Hold.Reserved())
	}
	if b.Conns != nil {
		if n := b.Conns.InUse(); n != 0 {
			held = append(held, fmt.Errorf("the connection limit still counts %d connections", n))
		}
	}

	return errors.Join(held...)
}
