//go:build engine_conformance

package integration

import (
	"math"
	"slices"
	"strconv"
	"strings"
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

	// decision: from burst 2 on, N stays between half and twice the N before
	// it. The limit bounds the expected k, not the real one: N sized for an
	// expected k of 4 gives 2 after one step up and 8 after one step down, and
	// the real k is about Poisson around it (41% under 2 at an expected 2, 18%
	// over 10 at an expected 8), so a well sized burst one step off is often
	// out of the window anyway. It stops one bad measurement from moving N far:
	// one stalled burst asked for about 80 times the N before it (560 to 44 896,
	// 7 doublings). The cost: a burst 1 too small by about 16 times reaches
	// the window's centre (expected k 4) only at burst 5, and by about 40 times
	// its edge (expected k 10) only there, so from somewhere between the two
	// sizes fewer than the 4 of 7 bursts B7 needs are evaluated and the run
	// fails. A larger step at burst 2 alone waits for k values
	// from the bench host. Overturned if bursts 2 and 3 stay out of 2 to 10.
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

	// The warm-up runs at N = b7Files with b7WarmWorkers workers, not at a
	// sized burst's, so its rate and latency size burst 1 and nothing after it.
	if k == 0 {
		z.rates, z.lats = nil, nil
	}

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

// burstMetrics is what B7 measured in one burst, in the units of its line in
// the per-burst file.
type burstMetrics struct {
	workers       float64
	rate          float64 // chmods a second
	chmodMs       float64
	pool          float64
	k             float64
	windowMs      float64
	running       float64
	oldestMs      float64
	writerChmods  float64
	writerChmodMs float64
	otherChmodMs  float64
	mixedTxns     float64
}

// burstLine is the benchmark line of burst n of the benchmark name, one
// iteration, in the form benchstat reads, ending in a newline. Each value is
// written in full, never with an exponent.
func burstLine(name string, n int, m burstMetrics) string {
	var line strings.Builder

	line.WriteString(name + "/burst=" + strconv.Itoa(n) + " 1")

	for _, v := range []struct {
		value float64
		unit  string
	}{
		{m.workers, "workers"},
		{m.rate, "chmods/s"},
		{m.chmodMs, "chmod-ms"},
		{m.pool, "pool"},
		{m.k, "k"},
		{m.windowMs, "window-ms"},
		{m.running, "running"},
		{m.oldestMs, "oldest-ms"},
		{m.writerChmods, "writer-chmods"},
		{m.writerChmodMs, "writer-chmod-ms"},
		{m.otherChmodMs, "other-chmod-ms"},
		{m.mixedTxns, "mixed-txns"},
	} {
		line.WriteString(" " + strconv.FormatFloat(v.value, 'f', -1, 64) + " " + v.unit)
	}

	line.WriteString("\n")

	return line.String()
}
