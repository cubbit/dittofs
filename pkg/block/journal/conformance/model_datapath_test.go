//go:build journal_conformance

package conformance

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
)

// The journal agrees with its reference model over the data path alone:
// writes, reads, fills that carry their own bytes, truncates, deallocates,
// deletes and settles, over four files of two shares on one journal, with
// the comparison reading nothing but the answers the handles give — its
// views carry no index, so it needs no inspection. Every sequence kept in
// testdata/model-datapath runs first, then the seeded draws — the seed the
// environment names and four more beside it — so a journal that answers one
// draw wrongly cannot pass by drawing the same sequence every run.
func TestModel_DataPath(t *testing.T) {
	prof.Watch(t, prof.Options{})

	corpus, err := model.ReadCorpus("testdata/model-datapath")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	open, reached := journaltest.DataPathTargets(t)
	for _, seq := range corpus {
		model.CheckDataPath(t, seq.Name, seq.Steps, open, "kept as "+seq.Name)
	}

	// The model takes a data-path fill's version on trust — the caller
	// fetched the bytes, so the version is the ref's, not one the journal
	// issued — so a journal drawing a version for a fill again would pass
	// whatever sequence the one seeded draw happens to miss. Several fixed
	// seeds beside it make that a matter of which one trips.
	seed := modelSeed(t)
	model.CheckDataPath(t, fmt.Sprintf("seed-%d.txt", seed), model.GenerateWithin(seed, model.DefaultSteps, model.DataPathOps), open,
		fmt.Sprintf("drawn from seed %d, %d steps", seed, model.DefaultSteps))
	for _, s := range []uint64{2, 3, 4, 5} {
		if s == seed {
			continue // already drawn above
		}
		model.CheckDataPath(t, fmt.Sprintf("seed-%d.txt", s), model.GenerateWithin(s, model.DefaultSteps, model.DataPathOps), open,
			fmt.Sprintf("drawn from seed %d, %d steps", s, model.DefaultSteps))
	}

	if r := reached(); !t.Failed() {
		// decision: only the files and the shares are asserted, not the
		// streams the records sit on. The fixture's journals append over
		// two streams, and a file's stream is Streams.For's per-file hash,
		// so the driven files sit on both by construction; reading which
		// streams a finished sequence's records reached waits for the
		// inspection read-out that recovery builds, and TestModel already
		// carries that assertion, waiting there on the same read-out.
		if r.Files < 2 {
			t.Errorf("reached: Files = %d, want at least 2: the generated draws name more than one file", r.Files)
		}
		if r.Shares < 2 {
			t.Errorf("reached: Shares = %d, want at least 2: the generated draws call through both shares", r.Shares)
		}
	}
}

// fuzzStepBytes is how many input bytes one step decodes from, and
// maxFuzzSteps is the most steps one input decodes to, so an input however
// long costs at most one bounded sequence.
const (
	fuzzStepBytes = 9
	maxFuzzSteps  = 64
)

// decodeDataPathSteps decodes a fuzz input into a data-path sequence: per
// step, its operation drawn from model.DataPathOps by the first byte's
// remainder, and the eight that follow its share, over the journal's two,
// its file, over the four, its pick, the byte its writes and fills are made
// of, and the low 16 bits each of its offset and its length. An offset of
// 0xffff decodes as the region at the end of the largest file, which the
// generated draws reach; any other offset beyond 16 bits has no encoding,
// and none is drawn. A short input is a short sequence, never an error, and
// bytes past the step cap are ignored.
func decodeDataPathSteps(data []byte) []model.Step {
	var steps []model.Step
	for ; len(data) >= fuzzStepBytes && len(steps) < maxFuzzSteps; data = data[fuzzStepBytes:] {
		d := data[:fuzzStepBytes]
		s := model.Step{
			Op:    model.DataPathOps[int(d[0])%len(model.DataPathOps)],
			Share: int(d[1] % 2), // a share past the two would not run
			File:  int(d[2] % 4), // nor a file past the four
			Pick:  d[3],
			Byte:  d[4],
			Off:   int64(d[5]) | int64(d[6])<<8,
			Len:   int64(d[7]) | int64(d[8])<<8,
		}
		if s.Off == math.MaxUint16 {
			s.Off = math.MaxInt64 - 64 - s.Len
		}
		steps = append(steps, s)
	}
	return steps
}

// encodeDataPathSteps writes steps as a fuzz input of the form
// decodeDataPathSteps reads: one byte per step field, the operation's place
// in model.DataPathOps first, an offset beyond 16 bits as the far-window
// byte pair the decoder widens by the step's length.
func encodeDataPathSteps(steps []model.Step) []byte {
	out := make([]byte, 0, len(steps)*fuzzStepBytes)
	for _, s := range steps {
		off := s.Off
		if off > math.MaxUint16 {
			off = math.MaxUint16
		}
		out = append(out,
			byte(slices.Index(model.DataPathOps, s.Op)),
			byte(s.Share), byte(s.File), s.Pick, s.Byte,
			byte(off), byte(off>>8), byte(s.Len), byte(s.Len>>8))
	}
	return out
}

// FuzzModelDataPath drives byte inputs through the same run as
// TestModel_DataPath: each step is decoded straight from the input, so the
// fuzzer steers the sequence itself, not a generator's seed. It is a driver
// of steps, not a parser of journal bytes, so its property is the run's own:
// no input panics the journal, and every decoded sequence either agrees
// with the model or reports its divergence.
func FuzzModelDataPath(f *testing.F) {
	corpus, err := model.ReadCorpus("testdata/model-datapath")
	if err != nil {
		f.Fatalf("read the corpus: %v", err)
	}
	for _, seq := range corpus {
		f.Add(encodeDataPathSteps(seq.Steps))
	}
	f.Add(encodeDataPathSteps(model.GenerateWithin(7, 16, model.DataPathOps)))
	f.Add(encodeDataPathSteps(model.GenerateWithin(11, 16, model.DataPathOps)))

	f.Fuzz(func(t *testing.T, data []byte) {
		prof.Watch(t, prof.Options{})
		open, _ := journaltest.DataPathTargets(t)
		model.CheckDataPath(t, "fuzz.txt", decodeDataPathSteps(data), open, "decoded from the fuzz input")
	})
}
