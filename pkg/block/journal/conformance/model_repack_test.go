//go:build journal_conformance

package conformance

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
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
