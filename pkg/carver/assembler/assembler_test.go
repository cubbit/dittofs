package assembler_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
)

// A zero BlockTarget is below carver.MinBlockTarget, so New refuses it whether
// the body is the stub or the implementation.
func TestNewRefusesZeroBlockTarget(t *testing.T) {
	if f, err := assembler.New(carver.AssemblerConfig{}); err == nil || f != nil {
		t.Fatalf("New(zero config): got (%v, %v), want (nil, an error)", f, err)
	}
}
