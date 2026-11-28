package integration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// The workloads the selection decides on; every other workload is reported and
// decides nothing.
const (
	workloadPages     = "256 pages"
	workloadSustained = "sustained overwrites"
)

// defaultTarget is the proposed default a candidate has to beat (RFC 2 §3.2).
const defaultTarget = 256 << 10

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
// zero refs and its Target. Min and Max are Target/4 and 4 × Target (RFC 2
// §3.2). The ceiling allows a chunk per Min of live bytes, rounded up, one per
// hole, and one for the file's last chunk, which may fall under Min (RFC 2
// §3.2, "The ceiling under random overwrite"); it is an integer so a file at
// the bound passes and one record more fails. A size of zero or past Max is
// no chunk the carver cuts, and panics.
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
// target is short (RFC 2 §5.4).
func summariseBlocks(carried []int64, passes int, blockTarget int64) blockFigures {
	f := blockFigures{Blocks: len(carried), Passes: passes}
	for _, c := range carried {
		if c < blockTarget {
			f.Short++
		}
	}

	if passes > 0 {
		f.PerPass = float64(len(carried)) / float64(passes)
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

	buf := make([]byte, 2*s.Max())

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
// Restored and Twin are summed over every round.
type row struct {
	Workload                          string
	Target, Written, Stored, Restored int64 // Restored: the real path's re-stored bytes
	Twin                              int64 // restoredWholeFile on the same files
	Chunks                            chunkFigures
	Blocks                            blockFigures
}

// selectDefault applies the selection rule of RFC 2 §11 to a report's rows,
// deciding on the 256 pages and the sustained overwrites alone; it takes no
// time. On each deciding workload a candidate's baseline ratio is its twin's
// re-stored bytes over 256 KiB's, and its real ratio its real re-stored bytes
// over 256 KiB's. A candidate qualifies if, on both, its real ratio is below
// 1 and below its baseline ratio, and none of its rows passes its chunk
// ceiling. Of those that qualify, the larger total relative margin,
// Σ (baseline − real) / baseline, wins, then the smaller Target; with none,
// 256 KiB stays. It panics on a report with no 256 KiB row for a deciding
// workload, or one whose 256 KiB re-stored bytes are zero, since no ratio can
// be taken over it.
func selectDefault(rows []row) int64 {
	type key struct {
		workload string
		target   int64
	}

	by := map[key]row{}
	var targets []int64
	for _, r := range rows {
		if !slices.Contains(targets, r.Target) {
			targets = append(targets, r.Target)
		}
		by[key{r.Workload, r.Target}] = r
	}

	deciding := []string{workloadPages, workloadSustained}
	for _, w := range deciding {
		b, ok := by[key{w, defaultTarget}]
		if !ok || b.Restored <= 0 || b.Twin <= 0 {
			panic(fmt.Sprintf("selectDefault: no 256 KiB row with re-stored bytes for %q", w))
		}
	}

	best, bestMargin := int64(defaultTarget), 0.0
	for _, t := range targets {
		if t == defaultTarget || !underCeiling(rows, t) {
			continue
		}

		margin, ok := 0.0, true
		for _, w := range deciding {
			r, found := by[key{w, t}]
			if !found {
				ok = false
				break
			}

			b := by[key{w, defaultTarget}]
			realRatio := float64(r.Restored) / float64(b.Restored)
			baseline := float64(r.Twin) / float64(b.Twin)
			if realRatio >= 1 || realRatio >= baseline {
				ok = false
				break
			}

			margin += (baseline - realRatio) / baseline
		}

		if ok && (best == defaultTarget || margin > bestMargin || margin == bestMargin && t < best) {
			best, bestMargin = t, margin
		}
	}

	return best
}

// underCeiling says whether every row of target keeps its chunk records at or
// under its ceiling.
func underCeiling(rows []row, target int64) bool {
	for _, r := range rows {
		if r.Target == target && r.Chunks.Records > r.Chunks.Ceiling {
			return false
		}
	}

	return true
}

// The cold-read fetch is Σs²/Σs and records per GiB count live bytes alone:
// 16, 16 and 64 KiB give 4,608 KiB² over 96 KiB, 48 KiB, and 3 records in 96
// KiB, 32,768 per GiB.
func TestSummariseColdFetchAndPerGiB(t *testing.T) {
	f := summarise([]int64{16 << 10, 16 << 10, 64 << 10}, 2, 64<<10)

	if f.ColdFetch != 48<<10 {
		t.Errorf("ColdFetch: got %v, want %v", f.ColdFetch, 48<<10)
	}
	if f.PerGiB != 32768 {
		t.Errorf("PerGiB: got %v, want 32768", f.PerGiB)
	}
	if f.Records != 3 {
		t.Errorf("Records: got %d, want 3", f.Records)
	}
}

// The ceiling is ⌈live / Min⌉ + holes + 1 at every candidate's Min.
func TestSummariseCeilingAtEachCandidatesMin(t *testing.T) {
	for _, target := range []int64{64 << 10, 256 << 10, 1 << 20, 4 << 20} {
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
		sizes := make([]int64, 1002)
		for i := range sizes {
			sizes[i] = 1
		}
		if at := summarise(sizes, 1000, target); at.Ceiling != at.Records {
			t.Errorf("Target %d: 1,002 one-byte chunks and 1,000 holes: got a ceiling of %d, want %d", target, at.Ceiling, at.Records)
		}
	}
}

// Every bucket edge falls where D6 puts it: under 4 KiB in the first bucket,
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

// A size the carver never cuts is refused, not bucketed.
func TestSummariseRefusesAChunkPastMax(t *testing.T) {
	for _, s := range []int64{0, 256<<10 + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("summarise of a %d-byte chunk at Target 64 KiB: got no panic, want one", s)
				}
			}()
			summarise([]int64{s}, 0, 64<<10)
		}()
	}
}

// A block carrying less than the target is short; one carrying exactly the
// target is not.
func TestSummariseBlocks(t *testing.T) {
	for _, c := range []struct {
		carried []int64
		passes  int
		want    blockFigures
	}{
		{[]int64{4 << 20, 4 << 20, 1 << 20}, 2, blockFigures{Blocks: 3, Passes: 2, Short: 1, PerPass: 1.5}},
		{[]int64{4 << 20, 4<<20 + 1}, 1, blockFigures{Blocks: 2, Passes: 1, PerPass: 2}},
	} {
		if got := summariseBlocks(c.carried, c.passes, 4<<20); got != c.want {
			t.Errorf("summariseBlocks(%v, %d): got %+v, want %+v", c.carried, c.passes, got, c.want)
		}
	}
}

// seededBytes returns n bytes of the ChaCha8 stream under seed.
func seededBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

// The twin re-stores nothing of a file against itself, only the re-cut tail
// and the appended bytes of a file grown by 1 MiB, and nothing of zeros.
func TestRestoredWholeFile(t *testing.T) {
	const target = 64 << 10
	before := seededBytes(1, 4<<20)

	if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(before), target); err != nil || got != 0 {
		t.Errorf("a file against itself: got %d, %v, want 0, nil", got, err)
	}

	// Before's chunks but its last are after's too: from the last one's
	// start, after is cut anew.
	s, err := carve.NewSettings(target, false, [32]byte{}, twinChunkIDKey)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	if _, err := carve.Cut(bytes.NewReader(before), 0, s, make([]byte, s.Max()), true, func(c carver.Chunk, _ []byte) error {
		last = c.Offset
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	grown := append(slices.Clone(before), seededBytes(2, 1<<20)...)
	if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(grown), target); err != nil || got != int64(len(grown))-last {
		t.Errorf("1 MiB appended: got %d, %v, want %d, nil", got, err, int64(len(grown))-last)
	}

	// 2 MiB of zeros appended: only the chunk that holds before's tail holds
	// a non-zero byte, and it is at most Max.
	zeroed := append(slices.Clone(before), make([]byte, 2<<20)...)
	if got, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(zeroed), target); err != nil || got <= 0 || got > int64(s.Max()) {
		t.Errorf("2 MiB of zeros appended: got %d, %v, want 1 to %d bytes, nil", got, err, s.Max())
	}

	if _, err := restoredWholeFile(bytes.NewReader(before), bytes.NewReader(before), target+1); !errors.Is(err, carver.ErrInvalidSettings) {
		t.Errorf("a Target that is not a power of two: got %v, want carver.ErrInvalidSettings", err)
	}
}

// The twin's re-stored bytes on Appendix C's whole-file baseline, in KiB, as
// a hand-built report's figures.
var baselineTwin = map[string]map[int64]int64{
	"next release":    {64 << 10: 166912, 256 << 10: 198656, 1 << 20: 216064, 4 << 20: 225280},
	workloadPages:     {64 << 10: 24576, 256 << 10: 71680, 1 << 20: 168960, 4 << 20: 219136},
	"64 inserts":      {64 << 10: 4915, 256 << 10: 19456, 1 << 20: 79872, 4 << 20: 179200},
	workloadSustained: {64 << 10: 24576, 256 << 10: 71680, 1 << 20: 168960, 4 << 20: 219136},
}

// baselineReport is a report whose real re-stored bytes equal the twin's on
// every row, every row under its ceiling, the largest Target first, so a tie
// is met with the larger Target already held.
func baselineReport() []row {
	var rows []row
	for _, target := range []int64{4 << 20, 1 << 20, 256 << 10, 64 << 10} {
		for _, w := range []string{"next release", workloadPages, "64 inserts", workloadSustained} {
			b := baselineTwin[w][target]
			rows = append(rows, row{Workload: w, Target: target, Restored: b, Twin: b, Chunks: chunkFigures{Records: 1, Ceiling: 2}})
		}
	}

	return rows
}

// withRow changes one row of rows in place.
func withRow(rows []row, w string, target int64, f func(*row)) []row {
	for i := range rows {
		if rows[i].Workload == w && rows[i].Target == target {
			f(&rows[i])
		}
	}

	return rows
}

func restoredAs(n int64) func(*row) { return func(r *row) { r.Restored = n } }

func TestSelectDefault(t *testing.T) {
	const k64, k256, m1 = 64 << 10, 256 << 10, 1 << 20

	for _, c := range []struct {
		name string
		rows []row
		want int64
	}{
		{"the real ratios equal the baseline's", baselineReport(), k256},
		{
			"64 KiB below its twin on the 256 pages and the sustained overwrites",
			withRow(withRow(baselineReport(), workloadPages, k64, restoredAs(20000)), workloadSustained, k64, restoredAs(20000)),
			k64,
		},
		{"64 KiB below its twin on the 256 pages alone", withRow(baselineReport(), workloadPages, k64, restoredAs(20000)), k256},
		{
			"64 KiB below its twin on both, its chunk records past the ceiling",
			withRow(withRow(withRow(baselineReport(), workloadPages, k64, restoredAs(20000)), workloadSustained, k64, restoredAs(20000)),
				"next release", k64, func(r *row) { r.Chunks.Records = 3 }),
			k256,
		},
		{
			"1 MiB below its twin on both, above 256 KiB's bytes",
			withRow(withRow(baselineReport(), workloadPages, m1, restoredAs(100000)), workloadSustained, m1, restoredAs(100000)),
			k256,
		},
		{
			"64 KiB below its twin on both controls alone",
			withRow(withRow(baselineReport(), "next release", k64, restoredAs(1000)), "64 inserts", k64, restoredAs(100)),
			k256,
		},
		{
			// 1 MiB: baseline 2.357, real 0.9, an absolute margin of 1.457 and a
			// relative one of 0.618 a workload; 64 KiB: baseline 0.343, real
			// 0.05, 0.293 and 0.854.
			"64 KiB and 1 MiB qualify, 1 MiB by the absolute margin, 64 KiB by the relative one",
			withRow(withRow(withRow(withRow(baselineReport(),
				workloadPages, k64, restoredAs(3584)), workloadSustained, k64, restoredAs(3584)),
				workloadPages, m1, restoredAs(64512)), workloadSustained, m1, restoredAs(64512)),
			k64,
		},
		{
			"64 KiB and 1 MiB tie on the margin",
			withRow(withRow(withRow(withRow(baselineReport(),
				workloadPages, k64, restoredAs(12288)), workloadSustained, k64, restoredAs(12288)),
				workloadPages, m1, func(r *row) { r.Twin, r.Restored = 24576, 12288 }),
				workloadSustained, m1, func(r *row) { r.Twin, r.Restored = 24576, 12288 }),
			k64,
		},
	} {
		if got := selectDefault(c.rows); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
