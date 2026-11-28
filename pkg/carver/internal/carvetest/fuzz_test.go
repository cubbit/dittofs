package carvetest

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

// tiles reports the first way ends fails to tile [0, length) within RFC 2 §8
// C4's bounds for target: every chunk longer than zero and at most Max, and
// every one but the last at least Min.
func tiles(target, length int, ends []int) error {
	start := 0
	for i, end := range ends {
		n := end - start
		last := i == len(ends)-1
		if n > 4*target || n <= 0 || (!last && n < target/4) {
			return fmt.Errorf("chunk %d is %d bytes, want %d to %d (the last may be shorter)", i, n, target/4, 4*target)
		}
		start = end
	}
	if start != length {
		return fmt.Errorf("chunks end at %d, want the length %d", start, length)
	}

	return nil
}

// The reference's chunks tile every input within C4's bounds, and cutting
// again from any chunk's end gives the same later ends, so no state crosses a
// boundary. A fuzzed byte slice rarely grows past Min, so the target fuzzes
// the generator's parameters instead: the input's seed, kind and length, up
// to 4 × Max, the Target, and the seed of a random gear table, which reaches
// content-defined cuts at every profile.
func FuzzReferenceBounds(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed uint64, kind uint8, length uint32, target uint8, tableSeed uint64) {
		tg := Targets[int(target)%len(Targets)]
		k := Kind(int(kind) % (int(RandomRepeat) + 1))
		n := int(length % uint32(16*tg+1))

		var g [256]uint64
		words := Bytes(Random, tableSeed, 8*len(g))
		for i := range g {
			g[i] = binary.LittleEndian.Uint64(words[8*i:])
		}

		p := Bytes(k, seed, n)
		ends := Boundaries(&g, tg, p)
		if err := tiles(tg, n, ends); err != nil {
			t.Fatalf("Target %d, kind %d, %d bytes: %v (ends %v)", tg, k, n, err, ends)
		}

		for i, e := range ends {
			again := Boundaries(&g, tg, p[e:])
			for j := range again {
				again[j] += e
			}
			if want := ends[i+1:]; !slices.Equal(again, want) {
				t.Fatalf("Target %d, kind %d, %d bytes: cut again from chunk %d's end %d gives ends %v, want %v", tg, k, n, i, e, again, want)
			}
		}
	})
}
