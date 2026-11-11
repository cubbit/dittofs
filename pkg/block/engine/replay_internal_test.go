package engine

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The journal splits a large write into several records of one version, and
// the index joins only touching pieces of one record, so Since can yield two
// touching changes of one version, each with the run clipped to its piece.
// The step they make names the run once, whole.
func TestStepsOfJoinsTheOverOfTouchingPiecesOfOneVersion(t *testing.T) {
	v := journal.Version{Counter: 7}

	steps := stepsOf([]journal.Change{
		{
			Kind:    journal.ChangeWrite,
			Extent:  journal.Extent{Off: 0, Len: 4096},
			Version: v,
			Over:    []journal.Extent{{Off: 0, Len: 4096}},
		},
		{
			Kind:    journal.ChangeWrite,
			Extent:  journal.Extent{Off: 4096, Len: 8192},
			Version: v,
			Over:    []journal.Extent{{Off: 4096, Len: 8192}},
		},
	})

	if len(steps) != 1 {
		t.Fatalf("%d steps, want 1: %+v", len(steps), steps)
	}

	want := []entity.Extent{{Offset: 0, Length: 12288}}

	if !slices.Equal(steps[0].over, want) {
		t.Errorf("step Over = %v, want %v", steps[0].over, want)
	}
}
