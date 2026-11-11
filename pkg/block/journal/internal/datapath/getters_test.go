package datapath

import "testing"

// TestGettersCore checks the core hands out the pieces it owns and the
// maximum it was built with.
func TestGettersCore(t *testing.T) {
	const max = 1 << 24
	c, _ := newTestCore(t, capacityCoreOption(max))
	if c.Set() == nil || c.Pool() == nil || c.Descriptors() == nil || c.Headroom() == nil {
		t.Fatalf("a getter returned nil: set %v pool %v descriptors %v headroom %v", c.Set(), c.Pool(), c.Descriptors(), c.Headroom())
	}
	if got := c.MaxBytes(); got != max {
		t.Fatalf("MaxBytes = %d, want %d", got, max)
	}
}
