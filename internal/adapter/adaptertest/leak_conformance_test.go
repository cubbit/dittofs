//go:build adapter_conformance

package adaptertest

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/resources"
)

// Every test named TestConformance in the adapters' tree is a specification
// an epic builds to. One that still fails on a stub is listed in
// test/adapter/known.yaml until the change that fills the stub.

// budgets returns four budgets with room for a test's reservations.
func budgets() *resources.Budgets {
	return &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 1<<20),
		Reply:      resources.NewWaitBudget("reply", 1<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}
}

// reserve charges n bytes to the budget of b that name picks.
func reserve(ctx context.Context, b *resources.Budgets, name string, n int64) (resources.Reservation, error) {
	client := netip.MustParseAddr("127.0.0.1")
	switch name {
	case "Receive":
		return b.Receive.Reserve(ctx, client, n)
	case "Reply":
		return b.Reply.Reserve(ctx, client, n)
	case "Connection":
		return b.Connection.Reserve(n)
	default:
		return b.Hold.Reserve(n)
	}
}

// budgetsHeld names the one budget that holds bytes, and no other.
func TestConformanceBudgetsHeldNamesEachCharged(t *testing.T) {
	names := []string{"Receive", "Reply", "Connection", "Hold"}
	for _, charged := range names {
		t.Run(charged, func(t *testing.T) {
			b := budgets()
			r, err := reserve(t.Context(), b, charged, 512)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Release()

			held := budgetsHeld(b)
			if held == nil {
				t.Fatalf("budgetsHeld reports nothing with %s charged", charged)
			}
			for _, name := range names {
				if got := strings.Contains(held.Error(), "the "+name+" budget"); got != (name == charged) {
					t.Errorf("with %s charged, budgetsHeld names %s: %v; its error: %v", charged, name, got, held)
				}
			}
		})
	}
}

// CheckLeaks reads the budgets at the test's end, not at its call: a
// reservation still held then fails it, and one released by then does not.
func TestConformanceCheckLeaksReadsBudgets(t *testing.T) {
	t.Run("Held", func(t *testing.T) {
		b := budgets()
		r, err := reserve(t.Context(), b, "Hold", 512)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Release()
		rec := newRecorder(t)

		CheckLeaks(rec, b)
		rec.finish()

		if !rec.Failed() || !strings.Contains(rec.failure(), "the Hold budget") {
			t.Fatalf("a reservation held at the test's end did not fail CheckLeaks naming Hold:\n%s", rec.failure())
		}
	})
	t.Run("Released", func(t *testing.T) {
		b := budgets()
		r, err := reserve(t.Context(), b, "Hold", 512)
		if err != nil {
			t.Fatal(err)
		}
		rec := newRecorder(t)

		CheckLeaks(rec, b)
		r.Release()
		rec.finish()

		if rec.Failed() {
			t.Fatalf("a reservation released before the test's end failed CheckLeaks:\n%s", rec.failure())
		}
	})
}
