package resources

import (
	"context"
	"net/netip"
	"testing"
)

// A pooled reply's attach and release allocates nothing per round: the
// parts' lists keep their capacity past its release and the tail continues
// past the ended part in the reply's own charged bytes (epic 4's rule 4,
// no allocation per segment).
func TestAttachAllocatesNothingPerSegment(t *testing.T) {
	if underRace {
		t.Skip("under -race sync.Pool keeps allocating")
	}
	budget := NewWaitBudget("alloc", int64(^uint64(0)>>1))
	p := NewBufPool(budget)
	ctx := context.Background()
	var reply *Buf
	round := func() {
		reply, _ = p.Get(ctx, netip.Addr{}, 64)
		seg, ok := p.TryGet(netip.Addr{}, 4096)
		if !ok {
			t.Fatal("TryGet refused the segment")
		}
		reply.Attach(seg)
		reply.B = append(reply.B, 't')
		seg.Release()
		reply.Release()
	}
	for range 3 {
		round() // warm the pool's and the lists' capacity
	}
	_ = testing.AllocsPerRun(10, round) // settle
	if n := testing.AllocsPerRun(100, round); n != 0 {
		t.Errorf("a pooled reply's attach and release allocated %v per round, want 0", n)
	}
}
