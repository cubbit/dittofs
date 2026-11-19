//go:build journal_conformance && journal_model

package conformance

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
)

// modelOpsWithoutOffload is the operation set the many-seed model draws from:
// every operation the generator's weights table names except the offload and
// the release, the two calls the 9b data path owns, and the repack. A
// sequence over it still makes reclaimable space as the epic's scope does
// without the 9b pair: overwrites, deletes and truncates — and the crashes
// and reopens around all of it.
//
// decision: OpOffload and OpRelease stay out of the alphabet until the 9b
// epic lands them, which is the change that removes the exclusion by
// restoring the two operations here; a fill idles with no offload to report,
// so no other operation reaches a 9b-only call path. The two weights stay in
// the table, so GenerateWithin renormalises the rest exactly as the full
// alphabet will draw them once 9b restores the pair.
//
// decision: OpRepack stays out too, because the repack pass's per-unit work
// grows with the segment count — its copy reads a full snapshot of the
// segment set per copied unit and its retire scan walks the ledger once per
// tick, both ceilings the pass's own markers name. Over the model rig's
// segment caps a 200-step draw with a repack in it makes the journal mint
// thousands of empty 2 KiB segments and the pass then does not answer in
// minutes, well past the suite's budget on most seeds drawn, so the nightly
// sweep cannot carry the operation. Restore it the day the pass's scans stop
// growing with the segment count, which the repack test's own marker names
// as the trigger that widens that test's seed bound with it.
var modelOpsWithoutOffload = []model.Op{
	model.OpWrite, model.OpSync, model.OpRead, model.OpFill, model.OpStaleFill,
	model.OpTruncate, model.OpDeallocate, model.OpDelete, model.OpSettle,
	model.OpCrash, model.OpReopen, model.OpForget,
}

// modelSeeds is how many seeds TestModelManySeeds draws: DITTOFS_MODEL_SEEDS
// when it is set — the nightly model suite sets it to a hundred — and one
// otherwise, so the change's own runs draw one useful sequence.
//
// decision: the count rises and never gates the run: a seed that fails is a
// divergence to fix, not one to be skipped around by drawing fewer, so the
// sweep runs every seed the count names whatever the count is.
func modelSeeds(t *testing.T) int {
	t.Helper()

	s, ok := os.LookupEnv("DITTOFS_MODEL_SEEDS")
	if !ok {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("DITTOFS_MODEL_SEEDS=%q: %v", s, err)
	}
	if n < 1 {
		t.Fatalf("DITTOFS_MODEL_SEEDS=%q: %d is no seed count", s, n)
	}
	return n
}

// TestModelManySeeds runs the reference model over the seeds 0..n-1, each a
// fresh timeline of the same operations — the alphabet modelOpsWithoutOffload
// names — over the two shares and four files of one journal, comparing after
// every step. The nightly suite raises n through DITTOFS_MODEL_SEEDS, so the
// rare timeline a change's one draw misses is a matter of which seed trips
// there. A failing seed's shrunk sequence is written to the artifact
// directory, to be committed to testdata/model with the fix.
func TestModelManySeeds(t *testing.T) {
	prof.Watch(t, prof.Options{})

	open, reached := journaltest.ModelTargets(t)
	for s := 0; s < modelSeeds(t); s++ {
		name := fmt.Sprintf("seed-%d.txt", s)
		steps := model.GenerateWithin(uint64(s), model.DefaultSteps, modelOpsWithoutOffload)
		before := t.Failed()
		model.Check(t, name, steps, open,
			fmt.Sprintf("drawn from seed %d over the alphabet without offload and release, %d steps", s, model.DefaultSteps),
			fmt.Sprintf("reproduce: DITTOFS_MODEL_SEEDS=%d running TestModelManySeeds, which reruns seeds 0..%d; or commit the shrunk sequence as testdata/model/%s, which TestModel's corpus runs first", s+1, s, name))
		if !t.Failed() || before {
			continue
		}
		// The first failing seed ends the sweep: Check has already shrunk it
		// and written its sequence, so the seeds after it would only spend
		// the run's budget re-reporting what the failing journal cannot pass.
		t.Errorf("seed %d diverged: rerun the sweep to it with DITTOFS_MODEL_SEEDS=%d (seeds 0..%d, the same draws, the alphabet above); to replay the finding itself, commit the shrunk sequence above as testdata/model/%s, which TestModel's corpus runs", s, s+1, s, name)
		return
	}

	// What the seeds reached of what only the journal chooses: the shares and
	// files their calls named, and the segment rolls their writes make over
	// the rig's small caps. Over one seed the assertions pin the fixed draw;
	// over the nightly counts they pin the alphabet.
	//
	// decision: the append streams a finished sequence's live records sit on
	// are not asserted: a single draw's end state leaves whichever streams
	// its removals emptied bare, so one seed cannot pin two live append
	// streams at once and the two-stream assertion stays with the corpus run
	// TestModel makes over committed sequences; and the bytes repacks freed
	// are not in this alphabet, so no free is asserted either.
	if r := reached(); !t.Failed() {
		if r.Files < 2 {
			t.Errorf("reached: Files = %d, want at least 2: the draws name four files", r.Files)
		}
		if r.Shares < 2 {
			t.Errorf("reached: Shares = %d, want at least 2: the draws call through both shares", r.Shares)
		}
		if r.Sealed == 0 {
			t.Errorf("reached: Sealed = %d, want segment rolls: the draws write enough to fill the rig's small segments", r.Sealed)
		}
	}
}
