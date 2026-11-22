package vfs

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// benchCallers is how many callers the benchmarks admit, and benchShares how
// many shares those callers are spread over.
const (
	benchCallers = 10_000
	benchShares  = 25
)

// BenchmarkAdmissionCallers admits 10⁴ callers, each granted on one of 25
// shares, round-robin, while the identity State's serial is raised once a
// second through Apply, as a settings change anywhere does. Ticket callers
// are cached and resolve again only after a raise; Secret callers carry no
// freshness, so every call resolves, which is admission's cost without the
// cache.
//
// Beside ns/op and allocs/op it reports the store's read transactions per
// second (reads/s) and per admission (reads/op), both store-wide: the Apply
// goroutine's reads are in them. It reports the 99th percentile of one
// admission (p99-ns), the cache's misses per admission (misses/op), the serial
// raises the run saw (raises) and the misses per raise (misses/raise). Each
// call is timed with the clock, which ns/op includes.
//
// The raises keep a wall-clock cadence, as the figures it is read against
// are per second, so misses/op follows the host's speed: it is about
// 10⁴ x raises / b.N, and a faster build shows fewer with the cache no
// better. Read Ticket by misses/raise instead. A run is valid when it is
// about 10⁴, every caller missing once per raise, and raises is in the tens.
// A host that cannot admit all 10⁴ callers inside a second shows fewer, and
// its Ticket is then closer to Secret than to a warm cache: do not read it.
// Run it with a -benchtime of tens of seconds, so the 99th percentile spans
// many raises.
func BenchmarkAdmissionCallers(b *testing.B) {
	for _, proof := range []struct {
		name string
		call func(c *caching, p entity.Principal) vfs.Call
	}{
		{"proof=Ticket", func(c *caching, p entity.Principal) vfs.Call { return c.ticket(p, inLab, 0) }},
		{"proof=Secret", func(_ *caching, p entity.Principal) vfs.Call { return withSIDs(p, vfs.ProofSecret, nil, nil) }},
	} {
		b.Run(proof.name, func(b *testing.B) {
			c := newCaching(b)
			ids := []entity.ShareID{c.share}
			for i := 1; i < benchShares; i++ {
				ids = append(ids, c.addShare(b, "s"+strconv.Itoa(i), "/s"+strconv.Itoa(i)))
			}
			calls := make([]vfs.Call, benchCallers)
			shares := make([]entity.ShareID, benchCallers)
			for i := range calls {
				shares[i] = ids[i%benchShares]
				u := c.user(b, "u"+strconv.Itoa(i), strconv.Itoa(100_000+i), shares[i], entity.AccessReadWrite)
				calls[i] = proof.call(c, u.Principal)
			}
			c.raise(b)
			ctx := context.Background()
			admit := func(i int) error {
				_, release, err := c.admit(ctx, calls[i], shares[i], true, false)
				release()
				return err
			}
			// Warm: every caller admitted once, so the first timed pass finds
			// the cache full, and every share served.
			for i := range calls {
				if err := admit(i); err != nil {
					b.Fatalf("warming caller %d: %v", i, err)
				}
			}

			stop, done := make(chan struct{}), make(chan struct{})
			var applyErr error
			var raises atomic.Int64
			go func() {
				defer close(done)
				tick := time.NewTicker(time.Second)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
					}
					c.gen++
					if err := c.srv.Apply(identitysrv.State{Generation: c.gen, Settings: c.ids}); err != nil {
						applyErr = err
						return
					}
					raises.Add(1)
				}
			}()
			var once sync.Once
			endApply := func() { once.Do(func() { close(stop); <-done }) }
			b.Cleanup(endApply)

			took := make([]time.Duration, b.N)
			c.views.TakeReads()
			misses := testutil.ToFloat64(c.callers.misses)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				n := i % benchCallers
				start := time.Now()
				err := admit(n)
				took[i] = time.Since(start)
				if err != nil {
					b.Fatalf("admitting caller %d: %v", n, err)
				}
			}
			b.StopTimer()
			endApply()
			if applyErr != nil {
				b.Fatalf("raising the serial: %v", applyErr)
			}
			slices.Sort(took)
			reads, missed := float64(c.views.TakeReads()), testutil.ToFloat64(c.callers.misses)-misses
			b.ReportMetric(float64(took[len(took)*99/100]), "p99-ns")
			b.ReportMetric(reads/b.Elapsed().Seconds(), "reads/s")
			b.ReportMetric(reads/float64(b.N), "reads/op")
			b.ReportMetric(missed/float64(b.N), "misses/op")
			b.ReportMetric(float64(raises.Load()), "raises")
			if r := raises.Load(); r > 0 {
				b.ReportMetric(missed/float64(r), "misses/raise")
			}
		})
	}
}
