package carve_test

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
)

// FuzzCut fuzzes the parameters of a generated stretch, since fuzzed bytes
// rarely grow past Min: a seed, a content kind, a length up to 4 × Max, a
// profile, the table, a base, a buffer of Max, Max + 1 or 2 × Max bytes, and
// an artificial end's limit. It cuts the stretch with a real end, which must
// tile from base, keep every chunk but the last within Min and Max, and give
// the reference chunker's ends and hashes; again through the next buffer size
// at another base, which must give the same chunks moved by the base, so
// neither the buffer nor the offset reaches a cut or a hash; and up to the
// limit with an artificial end, which must emit the real cut's chunks that
// have Max bytes in view, consume their sum and hold back under Max. A
// NextBoundary call under Max bytes without final panics, which fails the
// input too.
func FuzzCut(f *testing.F) {
	v := carvetest.LoadVectors(f)

	// Every content kind at the smallest profile over several Max, so each
	// cuts several chunks and its artificial end emits some; then Random at
	// two larger profiles. Between them, both tables, both ends and the three
	// buffer sizes, each seed's length short enough for -race.
	mx := uint32(4 * carvetest.Targets[0])
	for i, k := range []carvetest.Kind{carvetest.Random, carvetest.Zeros, carvetest.Period64, carvetest.Text44, carvetest.Period4K, carvetest.RandomRepeat} {
		f.Add(uint64(i+1), uint8(k), 3*mx+12345, uint8(0), i%2 == 1, int64(i)*1_000_003, uint8(i%3), 2*mx+uint32(i))
	}
	for i, target := range []uint8{4, 6} {
		mx := uint32(4 * carvetest.Targets[target])
		f.Add(uint64(i+7), uint8(carvetest.Random), mx+1+uint32(i)*mx/2, target, i == 0, int64(i)*7, uint8(i+1), mx/2)
	}

	f.Fuzz(func(t *testing.T, seed uint64, kind uint8, length uint32, target uint8, keyed bool, base int64, buffer uint8, limit uint32) {
		tg := carvetest.Targets[int(target)%len(carvetest.Targets)]
		chunking, gear := [32]byte{}, &v.GearUnkeyed
		if keyed {
			chunking, gear = v.ChunkingKey, &v.GearKeyed
		}
		s, err := carve.NewSettings(tg, keyed, chunking, v.ChunkIDKey)
		if err != nil {
			t.Fatalf("NewSettings(%d, keyed %v): %v", tg, keyed, err)
		}

		n := int(length % uint32(4*s.Max()+1))
		k := carvetest.Kind(kind % uint8(carvetest.RandomRepeat+1))
		data := carvetest.Bytes(k, seed, n)
		base = (base & math.MaxInt64) % (math.MaxInt64 - int64(n))
		sizes := []int{s.Max(), s.Max() + 1, 2 * s.Max()}
		b1, b2 := sizes[buffer%3], sizes[(buffer+1)%3]
		name := fmt.Sprintf("kind %d, %d bytes, target %d, keyed %v, base %d", k, n, tg, keyed, base)

		got, _ := offer(t, data, base, s, make([]byte, b1), true)
		end := base
		for i, c := range got {
			if c.Offset != end {
				t.Fatalf("%s, buffer %d: chunk %d at %d, want %d, the previous end", name, b1, i, c.Offset, end)
			}
			end += int64(c.Length)
			lo := s.Min()
			if i == len(got)-1 {
				lo = 1 // the last chunk before a real end may be shorter than Min
			}
			if int(c.Length) < lo || int(c.Length) > s.Max() {
				t.Fatalf("%s, buffer %d: chunk %d of %d is %d bytes, outside [%d, %d]", name, b1, i, len(got), c.Length, lo, s.Max())
			}
		}
		if end != base+int64(n) {
			t.Fatalf("%s, buffer %d: the chunks end at %d, want %d, the stretch's end", name, b1, end, base+int64(n))
		}
		ref := referenceCut(gear, s, base, data)
		checkClean(t, fmt.Sprintf("%s, buffer %d, against the reference", name, b1), got, ref)

		base2 := math.MaxInt64 - int64(n) - base
		if base2 == base {
			base2 ^= 1 // still within [0, MaxInt64 - n]: base is below it
		}
		want := slices.Clone(got)
		for i := range want {
			want[i].Offset += base2 - base
		}
		got2, _ := offer(t, data, base2, s, make([]byte, b2), true)
		checkClean(t, fmt.Sprintf("%s, buffer %d at base %d", name, b2, base2), got2, want)

		l := int64(limit % uint32(n+1))
		part, consumed := offer(t, data[:l], base, s, make([]byte, b1), false)
		checkOffer(t, fmt.Sprintf("%s, buffer %d, an artificial end", name, b1), part, consumed, ref, 0, heldAt(ref, 0, base+l, s), base, base+l, s)
	})
}
