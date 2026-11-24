package format

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// TestLossReasonOfMapsEveryReason pins the one mapping the loss writers share.
func TestLossReasonOfMapsEveryReason(t *testing.T) {
	for _, tc := range []struct {
		in   core.LossReason
		want LossReason
	}{
		{core.LossCorrupt, LossCorrupt},
		{core.LossStale, LossStale},
		{core.LossFailedWindow, LossFailedSync},
		{core.LossSuperseded, LossSuperseded},
		{core.LossAcknowledged, LossAcknowledged},
	} {
		if got := LossReasonOf(tc.in); got != tc.want {
			t.Errorf("LossReasonOf(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
