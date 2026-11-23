package engine

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A copy's chunks cover each extent in order, in offset order, none longer
// than the smaller of the copy chunk and clone_max_len.
func TestCopyChunks(t *testing.T) {
	const m = 1 << 20
	x := func(src, dst, n int64) vfserr.Extent { return vfserr.Extent{SrcOff: src, DstOff: dst, Len: n} }
	for _, tc := range []struct {
		name   string
		xs     []vfserr.Extent
		maxLen int64
		want   []vfserr.Extent
	}{
		{"one byte", []vfserr.Extent{x(5, 7, 1)}, 1 << 30, []vfserr.Extent{x(5, 7, 1)}},
		{"exactly a chunk", []vfserr.Extent{x(0, m, 64*m)}, 1 << 30, []vfserr.Extent{x(0, m, 64*m)}},
		{"200 MiB", []vfserr.Extent{x(m, 0, 200*m)}, 1 << 30, []vfserr.Extent{
			x(m, 0, 64*m), x(65*m, 64*m, 64*m), x(129*m, 128*m, 64*m), x(193*m, 192*m, 8*m)}},
		{"two extents in their order", []vfserr.Extent{x(100*m, 0, 70*m), x(0, 100*m, 10)}, 1 << 30, []vfserr.Extent{
			x(100*m, 0, 64*m), x(164*m, 64*m, 6*m), x(0, 100*m, 10)}},
		{"clone_max_len below the chunk", []vfserr.Extent{x(0, 0, 50*m)}, 20 * m, []vfserr.Extent{
			x(0, 0, 20*m), x(20*m, 20*m, 20*m), x(40*m, 40*m, 10*m)}},
	} {
		if got := slices.Collect(chunks(tc.xs, tc.maxLen)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: chunks %v, want %v", tc.name, got, tc.want)
		}
	}
}
