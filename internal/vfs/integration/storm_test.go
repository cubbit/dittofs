//go:build engine_conformance

package integration

import (
	"math"
	"slices"
	"time"
)

// The storm's sizes. B7's storm draws from the first b7Files files of the
// share's journal, the first benchWriters of which the writers overwrite.
const (
	benchWriters = 64

	b7Files = 100000
	b7Rate  = 1000

	// b7MaxWorkers is the most chmods the storm has in flight at once.
	b7MaxWorkers = 4096

	// b7WantK is the expected k the storm's pool is sized for, before each
	// burst, from the length of the burst before it.
	b7WantK = 4
)

// stormSizer sizes B7's storm before each burst from the bursts measured so
// far: how many files it draws from and how many workers it runs. It takes the
// measurements as arguments and reads no clock, so a test feeds it exact ones.
type stormSizer struct {
	rates []float64       // the last three bursts' measured rates
	lats  []time.Duration // and their chmod latencies, none of them 0
	last  time.Duration   // the length of the burst measured last
	prevN int64           // the N of the burst before, 0 before the first
}

// next is the N and the worker target for the burst about to start; the target
// is 0 while no chmod latency has been measured, and the pool then stays as it
// is.
func (z *stormSizer) next() (n, workers int64) {
	rate := float64(b7Rate)
	if len(z.rates) > 0 {
		rate = median(z.rates)
	}

	n = stormPool(z.last, rate)
	if z.prevN > 0 {
		n = min(max(n, z.prevN/2, benchWriters), 2*z.prevN, b7Files)
	}

	z.prevN = n

	// A latency of 0 means no chmod started in the window: it is left out, and
	// with none measured the target stays.
	if len(z.lats) > 0 {
		workers = workersFor(median(z.lats))
	}

	return n, workers
}

// measured records the burst k (-1 for the warm-up) that lasted last, at rate
// chmods a second with chmod latency lat.
func (z *stormSizer) measured(k int, last time.Duration, rate float64, lat time.Duration) {
	z.last = last

	if rate > 0 {
		z.rates = lastThree(append(z.rates, min(rate, b7Rate)))
	}

	if lat > 0 {
		z.lats = lastThree(append(z.lats, lat))
	}
}

// stormPool is how many of the pool's files the storm draws from for a burst
// expected to last as long as last did, with the storm running at rate chmods
// a second as the burst before measured it: enough that about b7WantK of the
// writers' files are touched.
func stormPool(last time.Duration, rate float64) int64 {
	n := int64(float64(benchWriters) * rate * last.Seconds() / b7WantK)

	return min(max(n, benchWriters), b7Files)
}

// workersFor is the pool that keeps b7Rate chmods a second in flight when
// each takes latency, at least one and at most b7MaxWorkers.
func workersFor(latency time.Duration) int64 {
	n := int64(math.Ceil(b7Rate * latency.Seconds()))

	return min(max(n, 1), b7MaxWorkers)
}

// median is the middle of xs, or the mean of the two middle values when their
// count is even; xs is not changed.
func median[T float64 | time.Duration](xs []T) T {
	sorted := slices.Sorted(slices.Values(xs))
	m := len(sorted) / 2

	if len(sorted)%2 == 0 {
		return (sorted[m-1] + sorted[m]) / 2
	}

	return sorted[m]
}

// lastThree is the last three of xs, or all of them when fewer.
func lastThree[T any](xs []T) []T {
	return xs[max(len(xs)-3, 0):]
}
