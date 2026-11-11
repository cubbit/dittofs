package recovery

import (
	"math/rand/v2"
)

// shuffleOrder returns a reorder that permutes the segments deterministically
// from seed, never keeping the listing's order, so a check that plants it
// feeds the rebuild in an order the directory did not hand over.
func shuffleOrder(seed uint64) func([]SegmentInfo) {
	return func(own []SegmentInfo) {
		r := rand.New(rand.NewPCG(seed, seed*2+1))
		r.Shuffle(len(own), func(i, j int) { own[i], own[j] = own[j], own[i] })
	}
}
