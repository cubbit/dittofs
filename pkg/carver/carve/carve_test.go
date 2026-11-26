package carve_test

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
)

// Each call below is given an input its contract refuses, so it must fail
// whether the body is the stub or the implementation.
func TestRefusesWhatItsContractRefuses(t *testing.T) {
	if err := carve.CheckTarget(0); err == nil {
		t.Error("CheckTarget(0): got nil error, want a refusal")
	}
	if _, err := carve.NewSettings(1<<20, false, [32]byte{}, [32]byte{}); err == nil {
		t.Error("NewSettings with a zero chunk-ID key: got nil error, want a refusal")
	}
	emit := func(carver.Chunk, []byte) error { return nil }
	if _, err := carve.Cut(bytes.NewReader([]byte("x")), 0, carver.Settings{}, make([]byte, 1), true, emit); err == nil {
		t.Error("Cut with zero Settings: got nil error, want a refusal")
	}
}
