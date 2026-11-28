package integration

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"math/bits"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The figures the target sweep reports and the rule that picks the default
// Target from them (RFC 2 Appendix C). Every function here is a pure function
// of sizes, counts and bytes: none takes a duration, so speed can never decide
// the default (RFC 2 §9.1).

// The workloads of the sweep. The selection decides on the 256 pages and the
// sustained overwrites alone; the other three are reported and decide nothing.
const (
	workloadRelease   = "next release"
	workloadPages     = "256 pages"
	workloadInserts   = "64 inserts"
	workloadAppend    = "streaming append"
	workloadSustained = "sustained overwrites"
)

// proposedTarget is the proposal a candidate has to beat (RFC 2 §3.2). It is
// part of the rule, fixed before the run, and never read from the settings
// section, whose default the run's selection sets.
const proposedTarget = 256 << 10

// candidates are the Targets the sweep measures: the four rows of RFC 2
// Appendix C's whole-file baseline.
var candidates = []int64{64 << 10, proposedTarget, 1 << 20, 4 << 20}

// histogramFloor is the lower edge of the histogram's first power-of-two
// bucket; one bucket below it holds the short chunks at stretch ends.
const histogramFloor = 4 << 10

// chunkFigures summarises a file's live chunk sizes under one Target.
type chunkFigures struct {
	Records   int     // non-zero refs
	PerGiB    float64 // records per GiB of live data, reported
	Ceiling   int     // ⌈live / Min⌉ + holes + 1, the bound the sweep checks
	ColdFetch float64 // Σs²/Σs, bytes
	Buckets   []int   // [0, 4 KiB), then [2^k, 2^(k+1)) from 4 KiB up to Max
	AtMax     int     // chunks of exactly Max, counted apart from Buckets
}

// summarise takes the sizes of a file's live non-zero refs, the number of its
// zero refs, each counted as a hole, and its Target. Min and Max are Target/4 and 4 × Target (RFC 2
// §3.2). The ceiling allows a chunk per Min of live bytes, rounded up, one per
// hole, and one for the file's last chunk, which may fall under Min (RFC 2
// §3.2, "The ceiling under random overwrite"); it is an integer so a file at
// the bound passes and one record more fails. A size of zero or past Max is
// no chunk the carver cuts, and panics: a caller checks its sizes first if a
// panic would cost it more than the one test.
func summarise(sizes []int64, holes int, target int64) chunkFigures {
	lo, hi := target/4, 4*target
	f := chunkFigures{Records: len(sizes), Buckets: make([]int, bits.Len64(uint64(hi/histogramFloor)))}

	var live, squares int64
	for _, s := range sizes {
		if s <= 0 || s > hi {
			panic(fmt.Sprintf("summarise: a chunk of %d bytes, outside (0, Max %d]", s, hi))
		}

		live += s
		squares += s * s

		switch {
		case s == hi:
			f.AtMax++
		case s < histogramFloor:
			f.Buckets[0]++
		default:
			f.Buckets[bits.Len64(uint64(s/histogramFloor))]++
		}
	}

	f.Ceiling = int((live+lo-1)/lo) + holes + 1
	if live > 0 {
		f.PerGiB = float64(len(sizes)) * (1 << 30) / float64(live)
		f.ColdFetch = float64(squares) / float64(live)
	}

	return f
}

// blockFigures summarises the blocks phase 1 put.
type blockFigures struct {
	Blocks, Passes, Short int
	PerPass               float64
}

// summariseBlocks takes the carried bytes of each block phase 1 put, the
// passes that put them and the block target; a block carrying less than the
// target is short (RFC 2 §5.4). Blocks put by no pass are a contradiction,
// and panic.
func summariseBlocks(carried []int64, passes int, blockTarget int64) blockFigures {
	if len(carried) > 0 && passes <= 0 {
		panic(fmt.Sprintf("summariseBlocks: %d blocks put by %d passes", len(carried), passes))
	}

	f := blockFigures{Blocks: len(carried), Passes: passes}
	for _, c := range carried {
		if c < blockTarget {
			f.Short++
		}
	}

	if passes > 0 {
		f.PerPass = float64(f.Blocks) / float64(passes)
	}

	return f
}

// twinChunkIDKey is the chunk-ID key the whole-file twin cuts under. Any
// non-zero key gives the same boundaries; it only names the chunks, and
// before and after are named under the same one.
var twinChunkIDKey = [32]byte{1}

// restoredWholeFile is the whole-file twin: the bytes of after's chunks whose
// chunk IDs before's chunks do not hold, both cut whole by carve.Cut under one
// Target in a namespace that does not encrypt. A chunk of after that repeats
// within after is counted each time, as the real path, which has no
// deduplication, stores it each time.
//
// decision: a chunk of after that is all zeros is not counted. The real path
// commits such a chunk as a zero ref, which no block carries, and its
// re-stored bytes count only live non-zero refs, so a twin that counted it
// would differ from the real path on a control workload by bytes neither
// stores. Count it if the engine ever carries zero chunks.
func restoredWholeFile(before, after io.Reader, target int) (int64, error) {
	s, err := carve.NewSettings(target, false, [32]byte{}, twinChunkIDKey)
	if err != nil {
		return 0, err
	}

	buf := make([]byte, s.Max())

	held := map[entity.ChunkHash]struct{}{}
	if _, err := carve.Cut(before, 0, s, buf, true, func(c carver.Chunk, _ []byte) error {
		held[c.Hash] = struct{}{}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("cutting the file before: %w", err)
	}

	zeros := make([]byte, s.Max())

	var restored int64
	if _, err := carve.Cut(after, 0, s, buf, true, func(c carver.Chunk, p []byte) error {
		if _, ok := held[c.Hash]; !ok && !bytes.Equal(p, zeros[:len(p)]) {
			restored += int64(c.Length)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("cutting the file after: %w", err)
	}

	return restored, nil
}

// row is one candidate's figures on one workload. For the sustained overwrites
// Restored and Twin are summed over every round, and Chunks is the round whose
// records stand furthest above, or least below, its ceiling, so the ceiling
// is checked on the round that binds.
type row struct {
	Workload                          string
	Target, Written, Stored, Restored int64 // Restored: the real path's re-stored bytes
	Twin                              int64 // restoredWholeFile on the same files
	Chunks                            chunkFigures
	Blocks                            blockFigures
}

// selectDefault applies the selection rule of RFC 2 §11 to a report's rows,
// deciding on the 256 pages and the sustained overwrites alone; it takes no
// time. On each deciding workload a candidate's real ratio is its real
// re-stored bytes over 256 KiB's, and its baseline ratio its twin's over 256
// KiB's twin's. A candidate qualifies if, on both, its real ratio is below 1
// and below its baseline ratio and its chunk records stay at or under their
// ceiling. Of those that qualify, the larger Σ (baseline − real) / baseline
// wins, then the smaller Target; with none, 256 KiB stays. The ratios are
// exact rationals, so a tie is a tie and no rounding picks the winner.
//
// It panics on a report that is not one row for each candidate on each
// deciding workload, on a repeated row, and on 256 KiB rows with no re-stored
// bytes, over which no ratio can be taken: a half-run report decides nothing.
func selectDefault(rows []row) int64 {
	type key struct {
		workload string
		target   int64
	}

	deciding := []string{workloadPages, workloadSustained}

	by := map[key]row{}
	for _, r := range rows {
		k := key{r.Workload, r.Target}
		if _, dup := by[k]; dup {
			panic(fmt.Sprintf("selectDefault: two rows for %q at Target %d", r.Workload, r.Target))
		}
		by[k] = r
	}

	for _, w := range deciding {
		for _, t := range candidates {
			if _, ok := by[key{w, t}]; !ok {
				panic(fmt.Sprintf("selectDefault: no row for %q at Target %d", w, t))
			}
		}
		if b := by[key{w, proposedTarget}]; b.Restored <= 0 || b.Twin <= 0 {
			panic(fmt.Sprintf("selectDefault: 256 KiB re-stores %d bytes on %q, its twin %d", b.Restored, w, b.Twin))
		}
	}

	ratio := func(a, b int64) *big.Rat { return big.NewRat(a, b) }
	one := big.NewRat(1, 1)

	best, bestMargin := int64(proposedTarget), new(big.Rat)
	for _, t := range candidates {
		if t == proposedTarget {
			continue
		}

		margin, ok := new(big.Rat), true
		for _, w := range deciding {
			r, b := by[key{w, t}], by[key{w, proposedTarget}]
			realRatio, baseline := ratio(r.Restored, b.Restored), ratio(r.Twin, b.Twin)
			if realRatio.Cmp(one) >= 0 || realRatio.Cmp(baseline) >= 0 || r.Chunks.Records > r.Chunks.Ceiling {
				ok = false
				break
			}

			gain := new(big.Rat).Sub(baseline, realRatio)
			margin.Add(margin, gain.Quo(gain, baseline))
		}

		if c := margin.Cmp(bestMargin); ok && (c > 0 || c == 0 && t < best) {
			best, bestMargin = t, margin
		}
	}

	return best
}

// The cold-read fetch is Σs²/Σs and records per GiB count live bytes alone:
// 16, 16 and 64 KiB give 4,608 KiB² over 96 KiB, 48 KiB, and 3 records in 96
// KiB, 32,768 per GiB. A file with no live chunk reports zeros, not NaN.
func TestSummariseColdFetchAndPerGiB(t *testing.T) {
	f := summarise([]int64{16 << 10, 16 << 10, 64 << 10}, 2, 64<<10)
	if f.ColdFetch != 48<<10 || f.PerGiB != 32768 || f.Records != 3 {
		t.Errorf("16, 16 and 64 KiB: got ColdFetch %v, PerGiB %v, Records %d, want %v, 32768, 3", f.ColdFetch, f.PerGiB, f.Records, 48<<10)
	}

	if e := summarise(nil, 2, 64<<10); e.ColdFetch != 0 || e.PerGiB != 0 || e.Ceiling != 3 {
		t.Errorf("no live chunk, two holes: got ColdFetch %v, PerGiB %v, Ceiling %d, want 0, 0, 3", e.ColdFetch, e.PerGiB, e.Ceiling)
	}
}

// The ceiling is ⌈live / Min⌉ + holes + 1 at every candidate's Min.
func TestSummariseCeilingAtEachCandidatesMin(t *testing.T) {
	for _, target := range candidates {
		lo := target / 4

		// Five chunks of Min and one byte: ⌈(5 Min + 1) / Min⌉ = 6, three
		// holes, one for the last chunk.
		if got := summarise([]int64{lo, lo, lo, lo, lo, 1}, 3, target).Ceiling; got != 10 {
			t.Errorf("Target %d: Ceiling of five Min chunks, one byte and three holes: got %d, want 10", target, got)
		}

		// Four chunks of Min/2 are 2 Min of live bytes, a ceiling of 3: one
		// record over it.
		over := summarise([]int64{lo / 2, lo / 2, lo / 2, lo / 2}, 0, target)
		if over.Records != over.Ceiling+1 {
			t.Errorf("Target %d: four chunks of Min/2: got %d records against a ceiling of %d, want one over", target, over.Records, over.Ceiling)
		}

		// A thousand holes and 1,002 one-byte chunks sit exactly at the bound:
		// each hole keeps its whole allowance however few live bytes there are.
		if at := summarise(slices.Repeat([]int64{1}, 1002), 1000, target); at.Ceiling != at.Records {
			t.Errorf("Target %d: 1,002 one-byte chunks and 1,000 holes: got a ceiling of %d, want %d", target, at.Ceiling, at.Records)
		}

		// 300 chunks of Max, past a GiB of live bytes: each hole still adds
		// one, never one per GiB.
		if got := summarise(slices.Repeat([]int64{4 * target}, 300), 50, target).Ceiling; got != 300*16+51 {
			t.Errorf("Target %d: 300 chunks of Max and 50 holes: got a ceiling of %d, want %d", target, got, 300*16+51)
		}
	}
}

// Every bucket edge falls on a power of two: under 4 KiB in the first bucket,
// [2^k, 2^(k+1)) above, Min and Target on their own edges, exactly Max apart.
func TestSummariseBucketEdges(t *testing.T) {
	f := summarise([]int64{4<<10 - 1, 4 << 10, 8<<10 - 1, 8 << 10, 16 << 10, 64 << 10, 256<<10 - 1, 256 << 10}, 0, 64<<10)
	if want := []int{1, 2, 1, 1, 0, 1, 1}; !slices.Equal(f.Buckets, want) || f.AtMax != 1 {
		t.Errorf("Target 64 KiB: got Buckets %v and AtMax %d, want %v and 1", f.Buckets, f.AtMax, want)
	}

	for _, c := range []struct {
		target            int64
		buckets, min, tgt int // the bucket count, and Min's and Target's bucket
	}{
		{64 << 10, 7, 3, 5},
		{256 << 10, 9, 5, 7},
		{1 << 20, 11, 7, 9},
		{4 << 20, 13, 9, 11},
	} {
		f := summarise([]int64{c.target / 4, c.target, 4*c.target - 1, 4 * c.target}, 0, c.target)
		want := make([]int, c.buckets)
		want[c.min]++
		want[c.tgt]++
		want[c.buckets-1]++
		if !slices.Equal(f.Buckets, want) || f.AtMax != 1 {
			t.Errorf("Target %d: Min, Target, Max − 1 and Max: got Buckets %v and AtMax %d, want %v and 1", c.target, f.Buckets, f.AtMax, want)
		}
	}
}

// panics says whether f panics.
func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}

// A size the carver never cuts is refused, not bucketed.
func TestSummariseRefusesAChunkPastMax(t *testing.T) {
	for _, s := range []int64{0, 256<<10 + 1} {
		if !panics(func() { summarise([]int64{s}, 0, 64<<10) }) {
			t.Errorf("summarise of a %d-byte chunk at Target 64 KiB: got no panic, want one", s)
		}
	}
}

// A block carrying less than the target is short; one carrying exactly the
// target is not; blocks with no pass are refused.
func TestSummariseBlocks(t *testing.T) {
	for _, c := range []struct {
		carried []int64
		passes  int
		want    blockFigures
	}{
		{[]int64{4 << 20, 4 << 20, 1 << 20}, 2, blockFigures{Blocks: 3, Passes: 2, Short: 1, PerPass: 1.5}},
		{[]int64{4 << 20, 4<<20 + 1}, 1, blockFigures{Blocks: 2, Passes: 1, PerPass: 2}},
		{nil, 0, blockFigures{}},
	} {
		if got := summariseBlocks(c.carried, c.passes, 4<<20); got != c.want {
			t.Errorf("summariseBlocks(%v, %d): got %+v, want %+v", c.carried, c.passes, got, c.want)
		}
	}

	if !panics(func() { summariseBlocks([]int64{1}, 0, 4<<20) }) {
		t.Errorf("summariseBlocks of one block and no pass: got no panic, want one")
	}
}

// twinSeededBytes returns n bytes of the ChaCha8 stream under seed.
func twinSeededBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

// The twin re-stores nothing of a file against itself. Before's chunks but
// its last are after's too, so from the last one's start after is new: all of
// it counts when seeded bytes are appended, a repeated block each time it
// recurs, and of appended zeros only the chunks that hold before's bytes.
func TestRestoredWholeFile(t *testing.T) {
	const target = 64 << 10
	before := twinSeededBytes(1, 4<<20)

	if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(before), target); err != nil || got != 0 {
		t.Errorf("a file against itself: got %d, %v, want 0, nil", got, err)
	}

	s, err := carve.NewSettings(target, false, [32]byte{}, twinChunkIDKey)
	if err != nil {
		t.Fatalf("NewSettings(%d): %v", target, err)
	}
	cut := func(b []byte) (chunks []carver.Chunk) {
		if _, err := carve.Cut(bytes.NewReader(b), 0, s, make([]byte, s.Max()), true, func(c carver.Chunk, _ []byte) error {
			chunks = append(chunks, c)
			return nil
		}); err != nil {
			t.Fatalf("cutting %d bytes: %v", len(b), err)
		}
		return chunks
	}
	last := cut(before)[len(cut(before))-1].Offset

	block := twinSeededBytes(2, 1<<20)
	for _, c := range []struct {
		name  string
		after []byte
	}{
		{"1 MiB appended", slices.Concat(before, block)},
		{"one 1 MiB block appended twice", slices.Concat(before, block, block)},
	} {
		want := int64(len(c.after)) - last
		if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(c.after), target); err != nil || got != want {
			t.Errorf("%s: got %d, %v, want %d, nil", c.name, got, err, want)
		}
	}

	zeroed := slices.Concat(before, make([]byte, 2<<20))
	var want int64
	for _, c := range cut(zeroed) {
		if c.Offset >= last && c.Offset < int64(len(before)) {
			want += int64(c.Length)
		}
	}
	if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(zeroed), target); err != nil || got != want {
		t.Errorf("2 MiB of zeros appended: got %d, %v, want %d, nil", got, err, want)
	}
}

// The twin's re-stored bytes on Appendix C's whole-file baseline, in KiB, as
// a hand-built report's figures.
var baselineTwin = map[string]map[int64]int64{
	workloadRelease:   {64 << 10: 166912, 256 << 10: 198656, 1 << 20: 216064, 4 << 20: 225280},
	workloadPages:     {64 << 10: 24576, 256 << 10: 71680, 1 << 20: 168960, 4 << 20: 219136},
	workloadInserts:   {64 << 10: 4915, 256 << 10: 19456, 1 << 20: 79872, 4 << 20: 179200},
	workloadSustained: {64 << 10: 24576, 256 << 10: 71680, 1 << 20: 168960, 4 << 20: 219136},
}

// edit changes one row of a report: its real and twin re-stored bytes and
// its chunk records, each where non-zero.
type edit struct {
	w                      string
	target, restored, twin int64
	records                int
}

// report is a report whose real re-stored bytes equal the twin's on every
// row, every row's chunk records exactly at its ceiling, the largest Target
// first so a tie meets the larger Target already held, with edits applied.
func report(edits ...edit) []row {
	var rows []row
	for _, target := range slices.Backward(candidates) {
		for _, w := range []string{workloadRelease, workloadPages, workloadInserts, workloadSustained} {
			b := baselineTwin[w][target]
			rows = append(rows, row{Workload: w, Target: target, Restored: b, Twin: b, Chunks: chunkFigures{Records: 2, Ceiling: 2}})
		}
	}

	for _, e := range edits {
		i := slices.IndexFunc(rows, func(r row) bool { return r.Workload == e.w && r.Target == e.target })
		if e.restored != 0 {
			rows[i].Restored = e.restored
		}
		if e.twin != 0 {
			rows[i].Twin = e.twin
		}
		if e.records != 0 {
			rows[i].Chunks.Records = e.records
		}
	}

	return rows
}

func TestSelectDefault(t *testing.T) {
	const k64, k256, m1 = 64 << 10, 256 << 10, 1 << 20

	for _, c := range []struct {
		name string
		rows []row
		want int64
	}{
		{"the real ratios equal the baseline's", report(), k256},
		{
			"64 KiB below its twin on the 256 pages and the sustained overwrites",
			report(edit{w: workloadPages, target: k64, restored: 20000}, edit{w: workloadSustained, target: k64, restored: 20000}),
			k64,
		},
		{"64 KiB below its twin on the 256 pages alone", report(edit{w: workloadPages, target: k64, restored: 20000}), k256},
		{
			"64 KiB below its twin on both, its chunk records past the ceiling on the 256 pages",
			report(edit{w: workloadPages, target: k64, restored: 20000, records: 3}, edit{w: workloadSustained, target: k64, restored: 20000}),
			k256,
		},
		{
			"1 MiB below its twin on both, above 256 KiB's bytes",
			report(edit{w: workloadPages, target: m1, restored: 100000}, edit{w: workloadSustained, target: m1, restored: 100000}),
			k256,
		},
		{
			"64 KiB below its twin on both controls alone",
			report(edit{w: workloadRelease, target: k64, restored: 1000}, edit{w: workloadInserts, target: k64, restored: 100}),
			k256,
		},
		{
			// Correct: baseline 24576/71680 ≈ 0.343 and real 30000/143360 ≈
			// 0.209, so 64 KiB qualifies. A baseline over 256 KiB's real bytes,
			// 24576/143360 ≈ 0.171, or a real ratio over its twin's,
			// 30000/71680 ≈ 0.419, would not.
			"256 KiB's real bytes twice its twin's: each ratio over its own denominator",
			report(edit{w: workloadPages, target: k256, restored: 143360}, edit{w: workloadSustained, target: k256, restored: 143360},
				edit{w: workloadPages, target: k64, restored: 30000}, edit{w: workloadSustained, target: k64, restored: 30000}),
			k64,
		},
		{
			// 1 MiB: baseline 2.357, real 0.9, an absolute margin of 1.457 and a
			// relative one of 0.618 a workload; 64 KiB: baseline 0.343, real
			// 0.05, 0.293 and 0.854.
			"64 KiB and 1 MiB qualify, 1 MiB by the absolute margin, 64 KiB by the relative one",
			report(edit{w: workloadPages, target: k64, restored: 3584}, edit{w: workloadSustained, target: k64, restored: 3584},
				edit{w: workloadPages, target: m1, restored: 64512}, edit{w: workloadSustained, target: m1, restored: 64512}),
			k64,
		},
		{
			"64 KiB and 1 MiB tie on the margin",
			report(edit{w: workloadPages, target: k64, restored: 12288}, edit{w: workloadSustained, target: k64, restored: 12288},
				edit{w: workloadPages, target: m1, restored: 12288, twin: 24576}, edit{w: workloadSustained, target: m1, restored: 12288, twin: 24576}),
			k64,
		},
		{
			// 1 MiB's figures are six times 64 KiB's, so every ratio and the
			// margin are the same; summed as floats they differ in the last bit.
			"64 KiB and 1 MiB tie on margins whose float sums differ",
			report(edit{w: workloadPages, target: k256, restored: 70001}, edit{w: workloadSustained, target: k256, restored: 1150013, twin: 1146880},
				edit{w: workloadPages, target: k64, restored: 2972, twin: 11838}, edit{w: workloadSustained, target: k64, restored: 51, twin: 13467},
				edit{w: workloadPages, target: m1, restored: 6 * 2972, twin: 6 * 11838}, edit{w: workloadSustained, target: m1, restored: 6 * 51, twin: 6 * 13467}),
			k64,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := selectDefault(c.rows); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// A report that is not one row per candidate on each deciding workload, or
// whose 256 KiB rows re-store nothing, decides nothing and is refused.
func TestSelectDefaultRefusesAnIncompleteReport(t *testing.T) {
	missing := slices.DeleteFunc(report(), func(r row) bool { return r.Workload == workloadSustained && r.Target == 64<<10 })

	for _, c := range []struct {
		name string
		rows []row
	}{
		{"no 64 KiB row on the sustained overwrites", missing},
		{"two 64 KiB rows on the sustained overwrites", append(report(), row{Workload: workloadSustained, Target: 64 << 10, Restored: 1, Twin: 1})},
		{"256 KiB re-stores nothing on the 256 pages", report(edit{w: workloadPages, target: 256 << 10, restored: -1})},
	} {
		if !panics(func() { selectDefault(c.rows) }) {
			t.Errorf("%s: got no panic, want one", c.name)
		}
	}
}
