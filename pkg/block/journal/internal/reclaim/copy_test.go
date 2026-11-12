package reclaim

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

const runKib = int64(4 << 10)

// runVer returns a hard-coded version with counter n and no epoch.
func runVer(n uint64) core.Version { return core.Version{Counter: n} }

// spec is one live piece with the facts the merge rule reads, spelled out for
// the table: where it sits in the file, how long it is, what record its bytes
// read from and where in that record's payload, and its version, kind, mtime
// and offloaded bit.
type spec struct {
	off, n  int64
	rec     index.RecordAt
	skip    uint32
	version core.Version
	kind    format.Kind
	mtime   int64
	offload bool
}

// input turns the specs, in file order, into the grouping input.
func input(specs ...spec) []pieceInfo {
	out := make([]pieceInfo, len(specs))
	for i, s := range specs {
		out[i] = pieceInfo{
			Piece: index.Piece{
				Extent:    core.Extent{Off: s.off, Len: s.n},
				Record:    s.rec,
				Skip:      s.skip,
				Version:   s.version,
				Offloaded: s.offload,
			},
			kind:  s.kind,
			mtime: s.mtime,
		}
	}
	return out
}

// TestPlanRuns pins the coalescing rule (R30): adjacent pieces merge when
// they share version, kind, offloaded bit and — for writes — mtime, and a gap
// splits the copy (R29). Kinds never mix. The kinds, mtimes and versions are
// hard-coded; no fixture takes part.
func TestPlanRuns(t *testing.T) {
	recA := index.RecordAt{Segment: 1, Offset: 0}
	recB := index.RecordAt{Segment: 2, Offset: 0}
	const (
		write = format.KindWrite
		fill  = format.KindFill
	)
	cases := []struct {
		name   string
		specs  []spec
		want   []core.Extent // the copy extents, in file order
		pieces []int         // per run, how many pieces it stitches
	}{
		{
			name: "adjacent pieces of one record at one version merge",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, skip: 0, version: runVer(1), kind: write, mtime: 7},
				{off: runKib, n: runKib, rec: recA, skip: uint32(runKib), version: runVer(1), kind: write, mtime: 7},
			},
			want:   []core.Extent{{Off: 0, Len: 2 * runKib}},
			pieces: []int{2},
		},
		{
			name: "a superseded middle splits the copy",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, skip: 0, version: runVer(1), kind: write, mtime: 7},
				{off: 2 * runKib, n: runKib, rec: recA, skip: 2 * uint32(runKib), version: runVer(1), kind: write, mtime: 7},
			},
			want:   []core.Extent{{Off: 0, Len: runKib}, {Off: 2 * runKib, Len: runKib}},
			pieces: []int{1, 1},
		},
		{
			name: "different versions never merge",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, version: runVer(1), kind: write, mtime: 7},
				{off: runKib, n: runKib, rec: recA, version: runVer(2), kind: write, mtime: 7},
			},
			want:   []core.Extent{{Off: 0, Len: runKib}, {Off: runKib, Len: runKib}},
			pieces: []int{1, 1},
		},
		{
			name: "writes at one version with different mtime stay separate",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, version: runVer(1), kind: write, mtime: 7},
				{off: runKib, n: runKib, rec: recA, version: runVer(1), kind: write, mtime: 8},
			},
			want:   []core.Extent{{Off: 0, Len: runKib}, {Off: runKib, Len: runKib}},
			pieces: []int{1, 1},
		},
		{
			name: "a fill beside a write never merges",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, version: runVer(1), kind: fill, offload: true},
				{off: runKib, n: runKib, rec: recA, version: runVer(1), kind: write, mtime: 7},
			},
			want:   []core.Extent{{Off: 0, Len: runKib}, {Off: runKib, Len: runKib}},
			pieces: []int{1, 1},
		},
		{
			name: "pieces of two records at one version merge",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, version: runVer(1), kind: write, mtime: 7},
				{off: runKib, n: runKib, rec: recB, version: runVer(1), kind: write, mtime: 7},
			},
			want:   []core.Extent{{Off: 0, Len: 2 * runKib}},
			pieces: []int{2},
		},
		{
			name: "fills at one version merge",
			specs: []spec{
				{off: 0, n: runKib, rec: recA, version: runVer(1), kind: fill, offload: true},
				{off: runKib, n: runKib, rec: recB, version: runVer(1), kind: fill, offload: true},
			},
			want:   []core.Extent{{Off: 0, Len: 2 * runKib}},
			pieces: []int{2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runs := planRuns(input(c.specs...))
			got := make([]core.Extent, len(runs))
			for i, r := range runs {
				got[i] = r.extent
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("planRuns made copy extents %v, want %v", got, c.want)
			}
			for i, r := range runs {
				if len(r.pieces) != c.pieces[i] {
					t.Fatalf("run %d stitches %d pieces, want %d", i, len(r.pieces), c.pieces[i])
				}
			}
		})
	}
}
