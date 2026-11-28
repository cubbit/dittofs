package carve_test

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/internal/blake3"
	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
)

// benchBytes is how much each benchmark cuts per iteration.
const benchBytes = 64 << 20

// BenchmarkCut is RFC 2 §9.1's K2 and K3 at every profile: Random is Cut over
// seeded random bytes, hashing included, and BLAKE3 the same bytes through the
// keyed hash alone in Target-sized pieces, K2's baseline; Zeros and Period64
// are Cut over repetitive input. The names, BenchmarkCut/<Target>/<kind>, are
// read by the checker that holds them to RFC 2 §10.5.
func BenchmarkCut(b *testing.B) {
	key := [32]byte{0: 1}
	inputs := map[string][]byte{
		"Random":   carvetest.Bytes(carvetest.Random, 1, benchBytes),
		"Zeros":    carvetest.Bytes(carvetest.Zeros, 1, benchBytes),
		"Period64": carvetest.Bytes(carvetest.Period64, 1, benchBytes),
	}

	for _, target := range carvetest.Targets {
		s, err := carve.NewSettings(target, false, [32]byte{}, key)
		if err != nil {
			b.Fatalf("NewSettings(%d): %v", target, err)
		}
		buf := make([]byte, 2*s.Max())

		b.Run(strconv.Itoa(target), func(b *testing.B) {
			for _, name := range []string{"Random", "BLAKE3", "Zeros", "Period64"} {
				b.Run(name, func(b *testing.B) {
					if name == "BLAKE3" {
						hashPieces(b, &key, inputs["Random"], target)
						return
					}

					data := inputs[name]
					r := bytes.NewReader(data)
					emit := func(carver.Chunk, []byte) error { return nil }

					b.SetBytes(int64(len(data)))
					b.ReportAllocs()

					for b.Loop() {
						r.Reset(data)
						if _, err := carve.Cut(r, 0, s, buf, true, emit); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// hashPieces times data hashed under key in pieces of n bytes, as Cut hashes
// chunks of Target bytes on average.
func hashPieces(b *testing.B, key *[32]byte, data []byte, n int) {
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()

	for b.Loop() {
		for p := data; len(p) > 0; p = p[min(n, len(p)):] {
			var h blake3.Hasher
			h.InitKeyed(key)
			h.Write(p[:min(n, len(p))])
			h.Sum256()
		}
	}
}
