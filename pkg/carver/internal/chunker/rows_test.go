package chunker_test

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/internal/chunker"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// RFC 2 §9's chunker rows for size, keyed tables, repetitive input and edit
// stability, cut through the settings the carver's constructor builds. Every
// row runs at every supported Target, and the bounds are restated here from
// Target, Min = Target/4 and Max = 4 × Target, never read back from the value.
// Random input and repetitive input are separate tests, so no size check
// rests on repetitive input alone. Each input comes from a fixed seed, so a
// row passes or fails the same way on every run.

// gearTable is one way a namespace cuts: unkeyed, or keyed under a chunking key.
type gearTable struct {
	name       string
	encrypting bool
	key        [32]byte
}

// gearTables is how the rows cut: unkeyed, keyed under the vectors' chunking
// key, and keyed under that key with its last byte flipped, so a derivation
// that drops key bytes gives the two keyed tables the same boundaries.
type gearTables struct{ unkeyed, keyed, second gearTable }

func tables(v carvetest.Vectors) gearTables {
	second := v.ChunkingKey
	second[31] ^= 1

	return gearTables{
		unkeyed: gearTable{name: "unkeyed"},
		keyed:   gearTable{name: "keyed", encrypting: true, key: v.ChunkingKey},
		second:  gearTable{name: "keyed-second", encrypting: true, key: second},
	}
}

// cutter builds the settings a namespace at target cuts under with gt.
func cutter(t *testing.T, v carvetest.Vectors, target int, gt gearTable) params.Settings {
	t.Helper()

	s, err := carve.NewSettings(target, gt.encrypting, gt.key, v.ChunkIDKey)
	if err != nil {
		t.Fatalf("NewSettings(%d, %s): %v", target, gt.name, err)
	}

	return s
}

// eachTarget runs f at every supported Target, in parallel.
func eachTarget(t *testing.T, f func(t *testing.T, target int)) {
	for _, target := range carvetest.Targets {
		t.Run(fmt.Sprintf("target=%d", target), func(t *testing.T) {
			t.Parallel()
			f(t, target)
		})
	}
}

// lengths is each chunk's length from its ends.
func lengths(ends []int) []int {
	ls := make([]int, len(ends))
	for i, e := range ends {
		ls[i] = e
		if i > 0 {
			ls[i] -= ends[i-1]
		}
	}

	return ls
}

// misfits is a check over many chunks: it names the first chunk that fails,
// and how many do, in one error.
type misfits struct {
	n     int
	first string
}

func (m *misfits) add(format string, args ...any) {
	if m.n == 0 {
		m.first = fmt.Sprintf(format, args...)
	}
	m.n++
}

func (m *misfits) report(t *testing.T) {
	t.Helper()

	if m.n > 0 {
		t.Errorf("%s (%d chunks fail in all)", m.first, m.n)
	}
}

// checkBounds fails a chunk over Max, or under Min but the last.
func checkBounds(t *testing.T, what string, target int, ends []int) {
	t.Helper()

	var bad misfits
	lo, hi := target/4, 4*target
	for i, l := range lengths(ends) {
		if l > hi || (l < lo && i < len(ends)-1) {
			bad.add("target %d, %s: chunk %d of %d is %d bytes; want within Min %d and Max %d, the last only at most Max",
				target, what, i, len(ends), l, lo, hi)
		}
	}
	bad.report(t)
}

// Over 256 MiB of seeded random input the mean chunk, the Min skip included,
// is within 10 % of Target under the unkeyed table and under two chunking
// keys (RFC 2 §3.2, §9 "target is real", §10.4).
func TestTheAverageChunkIsTarget(t *testing.T) {
	const n = 256 << 20
	v := carvetest.LoadVectors(t)
	gt := tables(v)

	eachTarget(t, func(t *testing.T, target int) {
		for _, gt := range []gearTable{gt.unkeyed, gt.keyed, gt.second} {
			s := cutter(t, v, target, gt)
			ends := chunker.Ends(s, carvetest.Input(carvetest.Random, 1, n), s.Max())

			mean := float64(n) / float64(len(ends))
			t.Logf("target %d, %s: %d chunks, mean %.0f bytes, %.4f × Target", target, gt.name, len(ends), mean, mean/float64(target))
			if mean < 0.9*float64(target) || mean > 1.1*float64(target) {
				t.Errorf("target %d, %s: mean chunk %.0f bytes over %d chunks, %.4f × Target; want within 10%% of Target",
					target, gt.name, mean, len(ends), mean/float64(target))
			}
		}
	})
}

// The same 64 MiB of random input cut under two chunking keys and under none
// gives three different sets of boundaries, and a second cut under each the
// same set (RFC 2 §6, §9 "keyed boundaries", §3.5). Different means most ends
// differ: two independent tables share an end only where a boundary under one
// happens to fall at a boundary under the other, so a tenth shared is a table
// that ignores most of its key.
func TestKeyedBoundariesDifferPerKey(t *testing.T) {
	v := carvetest.LoadVectors(t)
	in := carvetest.Bytes(carvetest.Random, 2, 64<<20)
	gt := tables(v)
	gts := []gearTable{gt.unkeyed, gt.keyed, gt.second}

	eachTarget(t, func(t *testing.T, target int) {
		ends := make([][]int, len(gts))

		for i, gt := range gts {
			s := cutter(t, v, target, gt)
			ends[i] = chunker.Ends(s, bytes.NewReader(in), s.Max())

			if again := chunker.Ends(cutter(t, v, target, gt), bytes.NewReader(in), s.Max()); !slices.Equal(again, ends[i]) {
				t.Errorf("target %d, %s: a second cut gave %d ends, the first %d; want the same ends", target, gt.name, len(again), len(ends[i]))
			}
		}

		for i := range gts {
			for j := i + 1; j < len(gts); j++ {
				shared := 0 // the input's own end, which every cut shares, left out
				for _, e := range ends[i][:len(ends[i])-1] {
					if _, ok := slices.BinarySearch(ends[j], e); ok {
						shared++
					}
				}
				t.Logf("target %d: %s and %s share %d of %d and %d ends", target, gts[i].name, gts[j].name, shared, len(ends[i]), len(ends[j]))
				if 10*shared >= min(len(ends[i]), len(ends[j])) {
					t.Errorf("target %d: %s and %s share %d of %d and %d ends; want under a tenth shared, different boundaries under each table",
						target, gts[i].name, gts[j].name, shared, len(ends[i]), len(ends[j]))
				}
			}
		}
	})
}

// Zeros under the unkeyed table never pass the test, so every chunk of 64 MiB
// is exactly Max; a 64-byte pattern, keyed and unkeyed, ends with no chunk
// over Max (RFC 2 §3.8, §9 "repetitive input still terminates").
func TestRepetitiveInputEndsAtMax(t *testing.T) {
	const n = 64 << 20
	v := carvetest.LoadVectors(t)
	gt := tables(v)

	eachTarget(t, func(t *testing.T, target int) {
		s := cutter(t, v, target, gt.unkeyed)
		ends := chunker.Ends(s, carvetest.Input(carvetest.Zeros, 3, n), s.Max())
		var bad misfits
		for i, l := range lengths(ends) {
			if l != 4*target {
				bad.add("target %d, zeros: chunk %d of %d is %d bytes; want exactly Max %d", target, i, len(ends), l, 4*target)
			}
		}
		bad.report(t)

		for _, gt := range []gearTable{gt.unkeyed, gt.keyed} {
			s := cutter(t, v, target, gt)
			checkBounds(t, "Period64 "+gt.name, target, chunker.Ends(s, carvetest.Input(carvetest.Period64, 3, n), s.Max()))
		}
	})
}

// period4KSeeds is, per Target, a Period4K seed whose pattern has a phase
// passing the test from Target on, under the unkeyed table and under the keyed
// one, so its chunks end at a boundary of the pattern, not only at Max. Most
// seeds have none above the smallest Target: 4,096 fingerprints against a
// test that passes one in 2^(t−3). Seeds found by computing each pattern's
// 4,096 fingerprints; a change to the gear tables means finding them again.
var period4KSeeds = map[int][2]uint64{
	64 << 10:  {3, 3},
	128 << 10: {5, 9},
	256 << 10: {5, 29},
	512 << 10: {6, 29},
	1 << 20:   {13, 29},
	2 << 20:   {216, 79},
	4 << 20:   {519, 453},
}

// The content cases of RFC 2 §10.2, keyed and unkeyed, over four Max of
// input: a 44-byte text cuts no chunk over Max; a 4,096-byte period cuts
// within Min and Max, every chunk starting at one phase of the pattern the
// same length; and random input with a Period64 middle third cuts within the
// bounds crossing in and out of it.
func TestContentCases(t *testing.T) {
	v := carvetest.LoadVectors(t)
	gt := tables(v)

	eachTarget(t, func(t *testing.T, target int) {
		n := int64(16 * target)

		for k, gt := range []gearTable{gt.unkeyed, gt.keyed} {
			s := cutter(t, v, target, gt)

			var bad misfits
			for i, l := range lengths(chunker.Ends(s, carvetest.Input(carvetest.Text44, 4, n), s.Max())) {
				if l > 4*target {
					bad.add("target %d, Text44 %s: chunk %d is %d bytes; want at most Max %d", target, gt.name, i, l, 4*target)
				}
			}
			bad.report(t)

			period := chunker.Ends(s, carvetest.Input(carvetest.Period4K, period4KSeeds[target][k], n), s.Max())
			checkBounds(t, "Period4K "+gt.name, target, period)

			var phased misfits
			byPhase := map[int]int{}
			compared, start := 0, 0
			for _, e := range period[:len(period)-1] {
				l := e - start
				if prev, ok := byPhase[start%4096]; ok {
					if prev != l {
						phased.add("target %d, Period4K %s: the chunk at %d, phase %d, is %d bytes, an earlier one at that phase %d; want one length per phase",
							target, gt.name, start, start%4096, l, prev)
					}
					if l < 4*target {
						compared++
					}
				}
				byPhase[start%4096], start = l, e
			}
			phased.report(t)
			if compared == 0 {
				t.Errorf("target %d, Period4K %s: no two chunks under Max start at one phase; want some compared, boundaries of the pattern", target, gt.name)
			}

			checkBounds(t, "RandomRepeat "+gt.name, target, chunker.Ends(s, carvetest.Input(carvetest.RandomRepeat, 4, n), s.Max()))
		}
	})
}

// One byte inserted 1 KiB into 8 × Max of random input leaves every chunk
// starting more than 2 × Max past the edit as it was, one byte further on,
// keyed and unkeyed (RFC 2 §3.1 B4, §9 "B4 shift resistance", §10.4 "edit
// stability").
func TestAnEditLeavesLaterChunksUnchanged(t *testing.T) {
	const edit = 1 << 10
	v := carvetest.LoadVectors(t)
	gt := tables(v)

	eachTarget(t, func(t *testing.T, target int) {
		maxLen := 4 * target
		n := int64(8 * maxLen)

		for _, gt := range []gearTable{gt.unkeyed, gt.keyed} {
			s := cutter(t, v, target, gt)

			orig := map[int]int{} // start → end
			start := 0
			for _, e := range chunker.Ends(s, carvetest.Input(carvetest.Random, 5, n), s.Max()) {
				orig[start], start = e, e
			}

			// The edited input is the original with one byte inserted at edit, so
			// a chunk of it starting past the edit at p holds the bytes the
			// original holds from p−1: equal ends, one byte back, are equal bytes.
			r := carvetest.Input(carvetest.Random, 5, n)
			edited := io.MultiReader(io.LimitReader(r, edit), bytes.NewReader([]byte{0x5a}), r)

			var bad misfits
			checked, start := 0, 0
			for _, e := range chunker.Ends(s, edited, s.Max()) {
				if start > edit+2*maxLen {
					checked++
					switch end, ok := orig[start-1]; {
					case !ok:
						bad.add("target %d, %s: the edited input's chunk at %d, %d bytes, starts where no chunk of the original starts, at %d; want it unchanged",
							target, gt.name, start, e-start, start-1)
					case end != e-1:
						bad.add("target %d, %s: the edited input's chunk at %d is %d bytes, the original's at %d %d; want it unchanged",
							target, gt.name, start, e-start, start-1, end-start+1)
					}
				}
				start = e
			}
			bad.report(t)

			t.Logf("target %d, %s: %d chunks past 2 × Max from the edit, all unchanged unless reported", target, gt.name, checked)
			if checked == 0 {
				t.Errorf("target %d, %s: no chunk of the edited input starts more than 2 × Max past the edit; want some to check", target, gt.name)
			}
		}
	})
}
