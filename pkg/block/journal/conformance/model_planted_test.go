//go:build journal_conformance && journal_plant_split

package conformance

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
)

// The bounds of the planted-run sweep: seedBound sequences of plantedSteps
// steps, drawn as the data-path test draws its own, are opened, run and closed
// each on a journal of its own. The planted build's index splits a range the
// held set shares wrongly, and any write sequence over a split range shows it
// in the answers the comparison reads after the step, so the first diverging
// seed is expected well inside the bound; eight was the starting bound, and
// the first seed tripped at step 39, so no widening of the bound or of the
// steps was needed.
const (
	seedBound    = 8
	plantedSteps = model.DefaultSteps
)

// TestModelDataPathFindsPlantedSupersession runs the data-path model over
// bounded generated sequences on a build whose index carries a known bug in
// the held set's three-way split, and requires one of them to diverge: a
// journal that drops a write's remainder where a newer write splits its range
// answers as though the newer record superseded the whole older one, and
// cannot agree with the model over every seed. The sequences run
// without shrinking, each opening a journal of its own, so the sweep costs one
// run per seed and reports the seed that trips; that seed is the one to run
// through the checking run when its shrunk sequence is wanted. A misuse of the
// model, a stub the journal has not written or a fixture failure fails this
// test itself: a driver bug must not make it pass vacuously, exactly as the
// planted fuzz's own test refuses one.
func TestModelDataPathFindsPlantedSupersession(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, _ := journaltest.DataPathTargets(t)
	for seed := uint64(1); seed <= seedBound; seed++ {
		steps := model.GenerateWithin(seed, plantedSteps, model.DataPathOps)
		tgt, done, err := open()
		if err != nil {
			t.Fatalf("seed %d: opening the journal: %v", seed, err)
		}
		err = model.RunDataPath(t.Context(), tgt, steps)
		done()
		switch {
		case err == nil:
			continue
		case errors.Is(err, model.ErrDiverged):
			t.Logf("seed %d diverged: %v", seed, err)
			return
		default:
			// Anything else is a driver bug or the journal failing its own
			// fixture, neither of them the planted bug: report it as this
			// test's failure, not as a find.
			t.Fatalf("seed %d failed the run, not the planted bug; redraw with model.GenerateWithin(%d, %d, model.DataPathOps):\n%v", seed, seed, plantedSteps, err)
		}
	}
	t.Fatalf("no divergence in %d sequences of %d steps each, drawn as the data-path test draws them: the planted build's index cannot agree with the model over every seed", seedBound, plantedSteps)
}
