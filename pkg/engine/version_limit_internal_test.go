package engine

import (
	"testing"
	"time"
)

// The peak is the highest count of any one second of the last 60: a second
// 59 seconds ago still counts, one 60 seconds ago no longer does, and a
// bucket reused for a new second starts from zero.
func TestPeakOver60Seconds(t *testing.T) {
	var r rateBuckets

	for range 100 {
		r.add(0)
	}

	for sec := uint64(1); sec < 59; sec++ {
		r.add(sec)
	}

	if got := r.add(59); got != 100 {
		t.Errorf("peak at second 59 = %d, want 100 from second 0", got)
	}

	if got := r.peak(60); got != 1 {
		t.Errorf("peak read at second 60 = %d, want 1: second 0 has left the window", got)
	}

	if got := r.add(60); got != 1 {
		t.Errorf("peak at second 60 = %d, want 1: its bucket starts from zero", got)
	}
}

// The versions left after v are the ones from v+1 to limit-1, and the raise
// is due only when they are fewer than the trigger.
func TestHeadroomBoundary(t *testing.T) {
	const block = 16 // half a block, 8, is the trigger when no version was drawn

	for _, c := range []struct {
		v    uint64
		left uint64
		due  bool
	}{
		{991, 8, false},
		{992, 7, true},
		{999, 0, true},
		{1000, 0, true},
	} {
		left := headroom(1000, c.v)

		if left != c.left {
			t.Errorf("headroom(1000, %d) = %d, want %d", c.v, left, c.left)
		}

		if got := due(left, 0, 32*time.Second, block); got != c.due {
			t.Errorf("due with %d left = %v, want %v", left, got, c.due)
		}
	}
}
