//go:build journal_conformance

package conformance

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/model"
)

// FuzzModelRepack drives byte inputs through the same run as the data-path
// sequences, with a repack pass and the restarts around it in the mix: each
// step is decoded straight from the input, drawn from model.RepackOps, so
// the fuzzer steers where the pass lands among the writes, the removals and
// the crashes itself. It is a driver of steps, not a parser of journal
// bytes, so its property is the run's own: no input panics the journal, and
// every decoded sequence either agrees with the reference model or reports
// its divergence.
func FuzzModelRepack(f *testing.F) {
	for _, seed := range []uint64{7, 11, 21, 37} {
		f.Add(encodeSteps(model.GenerateWithin(seed, 16, model.RepackOps), model.RepackOps))
	}
	// This entry is the decoder's maxFuzzSteps slice of the seed-3 shape,
	// whose full 200-step draw once drove a repack pass that never finished:
	// the records a carry moved never counted against the pass budget, so
	// the pass re-selected the very segments its own carries sealed. Every
	// carried record counts now, so the pass over these steps is bounded;
	// the 200-step draw itself is pinned at full length by
	// TestModel_RepackHeavyDraws in this file.
	f.Add(encodeSteps(model.GenerateWithin(3, 64, model.RepackOps), model.RepackOps))
	f.Fuzz(func(t *testing.T, data []byte) {
		prof.Watch(t, prof.Options{})
		steps := decodeSteps(data, model.RepackOps)
		for i := range steps {
			if steps[i].Op == model.OpCrash {
				// The normalisation lands every decoded crash on the losing
				// kind, whatever kind the input drew — not only the torn
				// one, which the crash seam refuses when nothing unsynced
				// stands behind it; the pass's own torn-crash shapes are
				// pinned by the damage rows, whose crash points they hold.
				steps[i].Pick -= steps[i].Pick % 3
			}
		}
		open, _ := journaltest.DataPathTargets(t)
		model.CheckDataPath(t, "repack-fuzz.txt", steps, open, "decoded from the fuzz input")
	})
}

// losingCrashes lands every crash step on the losing kind, whatever kind its
// draw named; see FuzzModelRepack for why the torn kind stays out.
func losingCrashes(steps []model.Step) []model.Step {
	for i := range steps {
		if steps[i].Op == model.OpCrash {
			steps[i].Pick -= steps[i].Pick % 3
		}
	}
	return steps
}

// forgetSeeds is how many fixed seeds TestModel_RepackAndForget draws.
//
// decision: the seeds used to stop at twenty because draws 22 and 24 hung:
// their repack pass manufactured the scale itself — the records a carry
// moved never counted against the pass budget, so the pass re-selected the
// very segments its own carries sealed and never finished. Every carried
// record counts now, so the draws run bounded and stay in the count; a draw
// that ever hangs again fails the suite by its timeout rather than being
// left out of the count.
const forgetSeeds = 24

// forgetSteps is the length of each of those draws.
const forgetSteps = 64

// TestModel_RepackAndForget runs fixed seeds of the model with a repack pass
// and the forget of a share's tag in the mix, beside the writes, removals and
// restarts, and fails on the first divergence from the reference model.
func TestModel_RepackAndForget(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, _ := journaltest.DataPathTargets(t)
	for seed := uint64(1); seed <= forgetSeeds; seed++ {
		steps := losingCrashes(model.GenerateWithin(seed, forgetSteps, model.ForgetOps))
		model.CheckDataPath(t, fmt.Sprintf("forget-seed-%d.txt", seed), steps, open,
			fmt.Sprintf("drawn from seed %d, %d steps", seed, forgetSteps))
	}
}

// TestModel_RepackHeavyDraws runs the one fixed draw that outgrows the fuzz
// corpus at its full length: 200 steps from seed 3, past the decoder's
// maxFuzzSteps cap, the draw whose repack pass once never finished because the
// records a carry moved never counted against the pass budget and the pass
// re-selected the very segments its own carries sealed. Every carried record
// counts now, so the pass over this draw is bounded and the run fails only on
// a divergence from the reference model.
func TestModel_RepackHeavyDraws(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, _ := journaltest.DataPathTargets(t)
	steps := losingCrashes(model.GenerateWithin(3, 200, model.RepackOps))
	model.CheckDataPath(t, "repack-heavy-seed-3.txt", steps, open,
		"drawn from seed 3, 200 steps")
}

// FuzzModelForget is FuzzModelRepack with the forget of a share's tag among
// the draws: the fuzzer steers where a forget lands among the writes, the
// passes and the restarts, and the property is the run's own.
func FuzzModelForget(f *testing.F) {
	for _, seed := range []uint64{5, 9, 17, 29} {
		f.Add(encodeSteps(model.GenerateWithin(seed, 16, model.ForgetOps), model.ForgetOps))
	}
	f.Add(encodeSteps(model.GenerateWithin(3, 64, model.ForgetOps), model.ForgetOps))
	f.Fuzz(func(t *testing.T, data []byte) {
		prof.Watch(t, prof.Options{})
		steps := losingCrashes(decodeSteps(data, model.ForgetOps))
		open, _ := journaltest.DataPathTargets(t)
		model.CheckDataPath(t, "forget-fuzz.txt", steps, open, "decoded from the fuzz input")
	})
}
