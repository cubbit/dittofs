package params_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

func TestZeroSettingsReadsZero(t *testing.T) {
	var s params.Settings
	if s.Target() != 0 || s.Min() != 0 || s.Max() != 0 {
		t.Fatalf("zero Settings: got Target %d, Min %d, Max %d, want 0, 0, 0", s.Target(), s.Min(), s.Max())
	}
}
