package assembler_test

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
)

// The generator's bounds: what the chunker can cut, and keys per chunk well
// inside any budget a commit is given.
const (
	maxChunkLen  = 16 << 20 // the largest chunker Max RFC 2 §3.7 accepts
	maxChunkKeys = 2048
	maxStream    = 10_000
)

// FuzzFold folds a random pass and asserts checkPlans over it: the plans' refs
// tile the input, A1 to A3, A5 to A7 and the key budget. A fuzzed byte slice
// rarely forms a valid stream, so the fuzzer drives the generator's parameters
// instead, and the stream is drawn from them with a seeded ChaCha8.
func FuzzFold(f *testing.F) {
	// The arguments: seed, chunks, files, zero %, repeat %, the least length
	// and its spread, the least keys and their spread, the target above
	// MinBlockTarget, the count cap less one, K's slack over the largest
	// chunk's keys, block keys, file keys, MaxAge in seconds. Each is reduced
	// into its range, never skipped.
	// An all-zero pass.
	f.Add(uint64(1), uint16(500), uint16(7), uint8(100), uint8(0), uint32(1<<10), uint32(64<<10), uint16(1), uint16(1), uint32(0), uint16(1023), uint16(40), uint8(5), uint8(1), uint16(60))
	// Closed by the count cap.
	f.Add(uint64(2), uint16(1000), uint16(3), uint8(0), uint8(0), uint32(1<<10), uint32(0), uint16(1), uint16(0), uint32(0), uint16(7), uint16(60000), uint8(3), uint8(1), uint16(60))
	// Closed by K at every block, zero refs carrying keys.
	f.Add(uint64(3), uint16(2000), uint16(40), uint8(40), uint8(10), uint32(1<<10), uint32(0), uint16(50), uint16(50), uint32(63<<20), uint16(1023), uint16(0), uint8(5), uint8(1), uint16(60))
	// A single chunk at exactly K.
	f.Add(uint64(4), uint16(1), uint16(1), uint8(0), uint8(0), uint32(1<<20), uint32(0), uint16(100), uint16(0), uint32(3<<20), uint16(1023), uint16(0), uint8(3), uint8(1), uint16(60))
	// A mixed pass.
	f.Add(uint64(5), uint16(10000), uint16(300), uint8(20), uint8(20), uint32(16<<10), uint32(1<<20), uint16(0), uint16(8), uint32(3<<20), uint16(1023), uint16(500), uint8(3), uint8(1), uint16(30))
	f.Fuzz(func(t *testing.T, seed uint64, chunks, files uint16, zeros, repeats uint8, minLen, lenSpread uint32, minKeys, keySpread uint16, target uint32, maxChunks, slack uint16, blockKeys, fileKeys uint8, maxAge uint16) {
		n := int(chunks) % (maxStream + 1)
		lo := 1 + int32(minLen%maxChunkLen)
		hi := lo + int32(lenSpread%uint32(maxChunkLen-lo+1))
		kLo := int(minKeys) % (maxChunkKeys + 1)
		kHi := kLo + int(keySpread)%(maxChunkKeys-kLo+1)
		cfg := carver.AssemblerConfig{
			BlockTarget: carver.MinBlockTarget + int64(target)%(carver.MaxBlockTarget-carver.MinBlockTarget+1),
			MaxChunks:   1 + int(maxChunks)%1024,
			BlockKeys:   int(blockKeys),
			FileKeys:    int(fileKeys),
			MaxAge:      time.Duration(maxAge) * time.Second,
		}
		// Every chunk fits K alone: a lone chunk over K is a caller fault, and
		// its plan is the assembler tests' to check.
		cfg.KeyBudget = cfg.BlockKeys + cfg.FileKeys + kHi + int(slack)

		var key [32]byte
		binary.LittleEndian.PutUint64(key[:], seed)
		rng := rand.New(rand.NewChaCha8(key))
		s := randomStream(rng, n, 1+int(files)%max(n, 1), int(zeros)%101, int(repeats)%101, lo, hi, kLo, kHi, cfg.MaxAge)

		r := fold(t, cfg, s, t0)
		checkPlans(t, cfg, s, r.plans, t0, r.held)
	})
}

// randomStream draws n chunks over files files, one after another, through
// stream: zeroPct of them all zeros, repeatPct a repeat of one of the last 64
// chunks that are not, lengths in lo..hi, Keys in kLo..kHi and Oldest up to
// twice maxAge before t0.
func randomStream(rng *rand.Rand, n, files, zeroPct, repeatPct int, lo, hi int32, kLo, kHi int, maxAge time.Duration) []carver.ChunkInfo {
	length := make([]int32, n)
	of := make([]int, n) // the earlier chunk a repeat copies, -1 if none
	isZero := make([]bool, n)
	var recent []int
	for i := range n {
		of[i] = -1
		length[i] = lo + rng.Int32N(hi-lo+1)
		switch {
		case rng.IntN(100) < zeroPct:
			isZero[i] = true
			continue
		case len(recent) > 0 && rng.IntN(100) < repeatPct:
			of[i] = recent[rng.IntN(len(recent))]
			length[i] = length[of[i]]
		}
		if recent = append(recent, i); len(recent) > 64 {
			recent = recent[1:]
		}
	}

	byFile := make([][]int32, files)
	for i := range n {
		f := i * files / n
		byFile[f] = append(byFile[f], length[i])
	}
	s := stream(rng.Int64N(1<<20), byFile...)
	for i := range s {
		s[i].Keys = kLo + rng.IntN(kHi-kLo+1)
		s[i].Oldest = t0.Add(-time.Duration(rng.Int64N(int64(2*maxAge) + 1)))
		switch {
		case isZero[i]:
			zero(s, i, i+1)
		case of[i] >= 0:
			s[i].Hash = s[of[i]].Hash
		}
	}
	return s
}
