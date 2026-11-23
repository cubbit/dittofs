package datapath

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
)

// TestHeadroomSparesCoverEveryStreamsLastSegment checks the headroom's spare
// target is the segments its records fill plus one for every stream past the
// first: each stream can leave one headroom segment partly filled, so
// records that fill two segments over four streams need five, not four.
func TestHeadroomSparesCoverEveryStreamsLastSegment(t *testing.T) {
	const segCap = 64 << 10
	// Records filling just over one segment's cap, so k is 2 whatever the
	// seal reserve adds.
	two := int(segCap/capacity.RecordCharge) + 1
	for _, c := range []struct {
		records, streams, want int
	}{
		{1, 1, 1},
		{1, 4, 4},
		{two, 1, 2},
		{two, 4, 5},
		{two, 16, 17},
	} {
		if got := HeadroomSpares(c.records, c.streams, segCap); got != c.want {
			t.Errorf("HeadroomSpares(%d records, %d streams) = %d, want %d", c.records, c.streams, got, c.want)
		}
	}
}
