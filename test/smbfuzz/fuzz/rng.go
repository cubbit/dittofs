// Package fuzz is the SMB differential fuzzer: plans, the generator, the
// per-user workers, the runner that executes a plan on the reference and the
// device under test in lockstep, the comparison and the minimiser.
package fuzz

import (
	"math"
	"math/rand/v2"
)

// roundSalt is the second word of every round's PCG seed. It never changes:
// changing it would renumber every test.
const roundSalt = 0x534D4246555A5A31 // "SMBFUZZ1"

// RNG draws every random value of a plan. It reads only Uint64 from PCG and
// derives everything else itself, because PCG's output for a seed is fixed by
// its algorithm while rand.Rand's helpers are not promised to be stable
// across Go releases.
type RNG struct{ src *rand.PCG }

// NewRNG returns the generator for a round.
func NewRNG(round uint64) *RNG { return &RNG{src: rand.NewPCG(round, roundSalt)} }

// newRNG2 returns a generator for a derived stream (data patterns).
func newRNG2(a, b uint64) *RNG { return &RNG{src: rand.NewPCG(a, b)} }

// Uint64 returns the next raw value.
func (r *RNG) Uint64() uint64 { return r.src.Uint64() }

// Below returns a uniform value in [0, n), by rejection.
func (r *RNG) Below(n uint64) uint64 {
	if n == 0 {
		panic("fuzz: Below(0)")
	}
	limit := math.MaxUint64 - math.MaxUint64%n
	for {
		v := r.src.Uint64()
		if v < limit {
			return v % n
		}
	}
}

// Float returns a uniform value in [0, 1) with 53 bits.
func (r *RNG) Float() float64 { return float64(r.src.Uint64()>>11) / (1 << 53) }

// Chance reports true with probability p.
func (r *RNG) Chance(p float64) bool { return r.Float() < p }

// Weighted returns an index drawn with the given non-negative weights.
func (r *RNG) Weighted(w []float64) int {
	var total float64
	for _, x := range w {
		total += x
	}
	if total <= 0 {
		panic("fuzz: Weighted with no weight")
	}
	x := r.Float() * total
	for i, v := range w {
		if x < v {
			return i
		}
		x -= v
	}
	for i := len(w) - 1; i >= 0; i-- {
		if w[i] > 0 {
			return i
		}
	}
	return 0
}

// Pick returns a uniformly drawn element.
func Pick[T any](r *RNG, xs []T) T { return xs[r.Below(uint64(len(xs)))] }

// WeightedChoice is a value with a weight.
type WeightedChoice[T any] struct {
	V T
	W float64
}

// PickW returns a value drawn by weight.
func PickW[T any](r *RNG, xs []WeightedChoice[T]) T {
	w := make([]float64, len(xs))
	for i, x := range xs {
		w[i] = x.W
	}
	return xs[r.Weighted(w)].V
}
