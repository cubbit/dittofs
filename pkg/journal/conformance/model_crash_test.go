//go:build journal_conformance

package conformance

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/model"
)

// TestModel_DataPathCrashes runs the reference model over the data path with
// storage losses drawn in among its calls, from fixed seeds: each crash step
// is a power loss (a plain one, a torn write or a reordered subset of the
// unsynced writes) and the journal opened again on what it left must agree
// with the model on every read of every file after it.
//
// decision: the sequences draw no offload, release or repack, which the
// stubbed calls would end the run on; the crashes are the draw's only
// addition to the data path. A crash step that finds records the model cannot
// leave uncertain is skipped by the run, so each seed is held to having run
// at least one crash through the journal, not the count its draw names.
func TestModel_DataPathCrashes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, reached := journaltest.DataPathTargets(t)
	ops := append(append([]model.Op{}, model.DataPathOps...), model.OpCrash, model.OpReopen)
	for _, seed := range []uint64{1, 2, 3, 4, 5} {
		before := reached().Crashes
		model.CheckDataPath(t, fmt.Sprintf("crash-seed-%d.txt", seed), model.GenerateWithin(seed, model.DefaultSteps, ops), open,
			fmt.Sprintf("drawn from seed %d, %d steps, with crashes", seed, model.DefaultSteps))
		if got := reached().Crashes - before; got == 0 && !t.Failed() {
			t.Errorf("seed %d: no crash step reached the journal, want at least one: the run compared nothing after a storage loss", seed)
		}
	}
}
