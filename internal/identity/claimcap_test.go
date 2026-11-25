package identity

import (
	"log/slog"
	"net/netip"
	"testing"
	"time"
)

// A full lookup-cap table makes room for a new unit by evicting an idle
// unit, one whose newest charge is a minute old, before a unit charging
// within the minute: a unit at its cap stays refused when a new unit
// arrives. Catches a live ring reset while an idle one is held.
func TestLookupCapEvictsAnIdleUnitFirst(t *testing.T) {
	c := newLookupCap(2, slog.New(slog.DiscardHandler))
	idle, busy, fresh := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("192.0.2.3")
	t0 := time.Unix(1_000_000, 0)
	if !c.charge(idle, t0) {
		t.Fatal("the idle unit's first charge refused")
	}
	t1 := t0.Add(30 * time.Second)
	for i := range claimLimit {
		if !c.charge(busy, t1) {
			t.Fatalf("the busy unit's charge %d refused", i+1)
		}
	}
	t2 := t0.Add(61 * time.Second)
	if !c.charge(fresh, t2) {
		t.Fatal("a new unit in a full table refused")
	}
	if c.charge(busy, t2) {
		t.Error("the busy unit charged again within its minute after a new unit arrived, want its ring kept, refused")
	}
	if _, held := c.units[throttleUnit(idle)]; held {
		t.Error("the idle unit is still held, want it evicted for the new one")
	}
}
