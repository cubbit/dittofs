//go:build journal_conformance && journal_model && journal_plant_truncate

package conformance

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
)

// plantedSeedBound bounds the planted sweep: sequences of the many-seed
// model's own alphabet and length, one seed each, on a build whose Truncate
// carries a known bug. A write before a truncate over its bytes shows the
// bug in the answer the comparison reads right after the truncate, so the
// first diverging seed is expected well inside the bound; the first seed
// itself was where it was looked for first, and no widening of the bound
// was needed once it tripped there.
const plantedSeedBound = 8

// plantedTB is the testing.TB the planted leg hands model.Check in place of
// its own: a divergence of the planted build is the leg's finding, not its
// failure, so the report Check would fail the test with is captured here for
// the leg to assert on instead. Everything else stays the real test's: the
// fatal failures of the rig itself, the context the run takes, the deadline
// shrinking keeps inside and the artifact directory the shrunk sequence is
// written to, which is where the leg reads it back from. The capture keeps
// only the first report, which shrink names once at the end.
type plantedTB struct {
	*testing.T
	reported string
}

func (p *plantedTB) Errorf(format string, args ...any) {
	if p.reported == "" {
		p.reported = fmt.Sprintf(format, args...)
	}
}

// TestModelManySeedsFindsPlantedTruncate runs the many-seed model's own
// alphabet over bounded seeds on a build whose Truncate plants a known bug in
// its removal marker — the marker reaches one byte past the size the caller
// named, so the byte at the size stays held — and requires the seeds to find
// it: some seed must fail with a shrunk sequence, written to the artifact
// directory, shorter than the drawn one. A report from a run whose shrunk
// sequence never landed in the artifact directory cannot be the planted bug —
// a divergence is shrunk and written, while a misuse of the model is reported
// alone — so the leg fails on it rather than counting it as a find. The
// planted build breaks the package's other tests of this build by design, so
// only this test runs on it. The leg mirrors TestModelManySeeds exactly —
// its alphabet, its step count, its rig — so what the seeds must find here is
// what the nightly sweep would find if the bug were real.
func TestModelManySeedsFindsPlantedTruncate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, _ := journaltest.ModelTargets(t)
	for s := 0; s < plantedSeedBound; s++ {
		name := fmt.Sprintf("planted-seed-%d.txt", s)
		steps := model.GenerateWithin(uint64(s), model.DefaultSteps, modelOpsWithoutOffload)
		p := &plantedTB{T: t}
		model.Check(p, name, steps, open,
			fmt.Sprintf("drawn from seed %d over the alphabet without offload and release, %d steps", s, model.DefaultSteps))
		if p.reported == "" {
			continue
		}
		// The seed reported something. A divergence shrinks and writes; read
		// the written sequence back and check the shrink really removed
		// steps, and take the missing file as the leg's own failure: what
		// reported there is not the planted bug.
		corpus, err := model.ReadCorpus(t.ArtifactDir())
		if err != nil {
			t.Fatalf("seed %d reported a divergence but its shrunk sequence could not be read back: %v;\n%s", s, err, p.reported)
		}
		shrunk := -1
		for _, seq := range corpus {
			if seq.Name == name {
				shrunk = len(seq.Steps)
			}
		}
		if shrunk < 0 {
			t.Fatalf("seed %d reported with no shrunk sequence written, a divergence never does; the report may be a misuse of the model rather than the planted truncate:\n%s", s, p.reported)
		}
		if shrunk >= len(steps) {
			t.Fatalf("seed %d found the planted truncate but its shrunken sequence kept all %d steps of the drawn one, want fewer:\n%s", s, len(steps), p.reported)
		}
		t.Logf("seed %d found the planted truncate, its sequence shrunk from %d to %d steps:\n%s", s, len(steps), shrunk, p.reported)
		return
	}
	t.Fatalf("no seed found the planted truncate in %d sequences of %d steps each, drawn as TestModelManySeeds draws them: the planted build cannot agree with the model over every seed", plantedSeedBound, model.DefaultSteps)
}
