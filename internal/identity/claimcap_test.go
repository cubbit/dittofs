package identity

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
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

// A full table evicting live rings logs at most once a window, with the
// count of live rings reset since the last log, and a ring is allocated to
// claimLimit on its first charge, so it never grows past it. Catches a log
// line per evicted unit, an eviction left uncounted, and a ring grown by
// append's doubling.
func TestLookupCapCountsLiveEvictions(t *testing.T) {
	var logs recordedLogs
	c := newLookupCap(1, slog.New(&logs))
	t0 := time.Unix(1_000_000, 0)
	for i := range 5 {
		c.charge(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), t0.Add(time.Duration(i)*time.Second))
	}
	if got := logs.evicted(); len(got) != 1 || got[0] != 1 {
		t.Errorf("logs after four live evictions within a window = %v, want one, counting 1", got)
	}
	c.charge(netip.MustParseAddr("192.0.2.9"), t0.Add(62*time.Second)) // unit 4 charged 58 s before, the last log 61 s before
	if got := logs.evicted(); len(got) != 2 || got[1] != 4 {
		t.Errorf("logs once the window has passed = %v, want a second, counting 4", got)
	}
	u := c.units[netip.MustParseAddr("192.0.2.9")].Value.(*capUnit)
	if got := cap(u.ring.at); got != claimLimit {
		t.Errorf("a ring's capacity after its first charge = %d, want %d", got, claimLimit)
	}
}

// recordedLogs keeps the evicted count of each record logged to it.
type recordedLogs struct {
	mu sync.Mutex
	n  []int64
}

func (l *recordedLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *recordedLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *recordedLogs) WithGroup(string) slog.Handler            { return l }
func (l *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "evicted" {
			l.n = append(l.n, a.Value.Int64())
		}
		return true
	})
	return nil
}

func (l *recordedLogs) evicted() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.n...)
}
