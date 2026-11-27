package chunker

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// BenchmarkNextBoundary cuts 64 MiB of seeded random bytes whole per
// iteration under the unkeyed table, at every supported Target (RFC 2 §9.1
// K1): its rate is the chunker's, its allocations NextBoundary's.
func BenchmarkNextBoundary(b *testing.B) {
	v := carvetest.LoadVectors(b)
	p := carvetest.Bytes(carvetest.Random, 1, 64<<20)

	for _, target := range carvetest.Targets {
		b.Run(fmt.Sprintf("target=%d", target), func(b *testing.B) {
			s := params.New(target, &v.GearUnkeyed)

			b.SetBytes(int64(len(p)))
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				for rest := p; len(rest) > 0; {
					rest = rest[NextBoundary(s, rest, len(rest) < s.Max()):]
				}
			}
		})
	}
}
