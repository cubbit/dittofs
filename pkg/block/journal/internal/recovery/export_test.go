package recovery

import (
	"math/rand/v2"
)

// SetRebuildOrder installs the reorder seam, the one hook the rebuild's
// order check drives; nil keeps the attach pass's listing order. Both the
// in-package tests and the external model test drive it.
func SetRebuildOrder(f func([]SegmentInfo)) { rebuildOrder = f }

// setOrder is the in-package shorthand of the seam.
func setOrder(f func([]SegmentInfo)) { rebuildOrder = f }

// shuffleOrder returns a reorder that permutes the segments deterministically
// from seed, never keeping the listing's order, so a check that plants it
// feeds the rebuild in an order the directory did not hand over.
func shuffleOrder(seed uint64) func([]SegmentInfo) {
	return func(own []SegmentInfo) {
		r := rand.New(rand.NewPCG(seed, seed*2+1))
		r.Shuffle(len(own), func(i, j int) { own[i], own[j] = own[j], own[i] })
	}
}
