package durability_test

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestPartReportsTheProbesFacts covers the report after Enter, after a probe
// whose check fails, and after a probe that made a spare.
func TestPartReportsTheProbesFacts(t *testing.T) {
	r := newProbeRig(t)
	if got := r.d.Stats().ReadOnly; got != (core.ReadOnlyState{}) {
		t.Fatalf("Stats().ReadOnly on a fresh layer = %+v, want zero", got)
	}
	r.setCheck(errNoFlush)
	cause := errors.New("read-only")
	r.d.Enter(cause)
	if got := r.d.Stats().ReadOnly; !got.ReadOnly || got.Err != cause || got.SpareMade || got.Check != nil {
		t.Fatalf("Stats().ReadOnly after Enter = %+v, want read-only with the cause alone", got)
	}
	r.tick(t, 1)
	eventually(t, "the check failure in the report", func() bool { return r.d.Stats().ReadOnly.Check != nil })
	if got := r.d.Stats().ReadOnly; !errors.Is(got.Check, errNoFlush) || !got.ReadOnly || got.SpareMade {
		t.Fatalf("Stats().ReadOnly after the failed check = %+v, want the check named and no spare", got)
	}
	r.setCheck(nil)
	r.tick(t, 2)
	eventually(t, "the spare in the report", func() bool { return r.d.Stats().ReadOnly.SpareMade })
	if got := r.d.Stats().ReadOnly; !got.ReadOnly || got.Check != nil || got.Err != cause {
		t.Fatalf("Stats().ReadOnly after the spare = %+v, want read-only, check cleared, cause kept", got)
	}
}
