package chunker

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// The chunker, warmed over the 64 bytes ending at each first tested byte,
// emits the reference's chunks, warmed from each chunk's start, over the same
// bytes, table and Target, through a window of Max, Max+1 or 2×Max bytes; and
// every chunk is within RFC 2 §8 C4's bounds and the chunks tile the input. A
// fuzzed byte slice rarely grows past Min, so the target fuzzes the
// generator's parameters: the input's seed, kind and length, up to 4 × Max, an
// index into Targets, a table, and the window. Table 0 is Vectors' unkeyed
// table, 1 its keyed one, and any other value the seed of a random table.
func FuzzNextBoundary(f *testing.F) {
	const (
		unkeyed, keyed = 0, 1
		maxWin         = 0 // a window of Max bytes
		maxWin1        = 1 // Max + 1
		twiceMax       = 2 // 2 × Max
	)

	// Index into carvetest.Targets: 64 KiB, 256 KiB, 1 MiB, 4 MiB.
	const p64K, p256K, p1M, p4M = 0, 2, 4, 6

	seeds := []struct {
		seed   uint64
		kind   carvetest.Kind
		length uint32
		target uint8
		table  uint64
		window uint8
	}{
		{1, carvetest.Random, 1 << 20, p64K, unkeyed, maxWin},
		{2, carvetest.Random, 1 << 20, p64K, keyed, maxWin1},
		{3, carvetest.Random, 4 << 20, p256K, 7, twiceMax},
		{4, carvetest.Random, 8 << 20, p1M, keyed, maxWin},
		{5, carvetest.Random, 20 << 20, p4M, unkeyed, maxWin1}, // past Max: a full window, then a final one
		{6, carvetest.Random, 0, p256K, unkeyed, maxWin},       // empty
		{7, carvetest.Random, 1, p1M, keyed, twiceMax},         // one byte
		{8, carvetest.Random, 64<<10 - 1, p256K, 9, maxWin},    // Min − 1: one short chunk
		{9, carvetest.Zeros, 1<<20 + 17, p64K, unkeyed, maxWin1},
		{10, carvetest.Zeros, 2 << 20, p256K, keyed, twiceMax},
		{11, carvetest.Period64, 1 << 20, p64K, keyed, maxWin},
		{12, carvetest.Period64, 4 << 20, p256K, unkeyed, maxWin1},
		{13, carvetest.Text44, 1 << 20, p64K, unkeyed, twiceMax},
		{14, carvetest.Text44, 4 << 20, p1M, keyed, maxWin},
		{15, carvetest.Period4K, 1 << 20, p64K, 11, maxWin},
		{16, carvetest.Period4K, 4 << 20, p256K, keyed, twiceMax},
		{17, carvetest.RandomRepeat, 1 << 20, p64K, unkeyed, maxWin1},
		{18, carvetest.RandomRepeat, 8 << 20, p1M, keyed, maxWin},
	}
	for _, s := range seeds {
		f.Add(s.seed, uint8(s.kind), s.length, s.target, s.table, s.window)
	}

	v := carvetest.LoadVectors(f)

	f.Fuzz(func(t *testing.T, seed uint64, kind uint8, length uint32, target uint8, table uint64, window uint8) {
		tg := carvetest.Targets[int(target)%len(carvetest.Targets)]
		k := carvetest.Kind(int(kind) % (int(carvetest.RandomRepeat) + 1))
		n := int(length % uint32(16*tg+1))

		var g *[256]uint64
		switch table {
		case unkeyed:
			g = &v.GearUnkeyed
		case keyed:
			g = &v.GearKeyed
		default:
			g = randomTable(table)
		}

		s := params.New(tg, g)
		buf := [...]int{s.Max(), s.Max() + 1, 2 * s.Max()}[int(window)%3]
		p := carvetest.Bytes(k, seed, n)
		name := fmt.Sprintf("Target %d, kind %d, %d bytes, table %d, window %d", tg, k, n, table, buf)

		got := Ends(s, bytes.NewReader(p), buf)
		if want := carvetest.Boundaries(g, tg, p); !slices.Equal(got, want) {
			t.Fatalf("%s: %d ends, first difference at chunk %s; want the reference's %d ends", name, len(got), firstDiff(got, want), len(want))
		}

		if err := tiles(s, n, got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	})
}

// randomTable is a gear table of seeded random words.
func randomTable(seed uint64) *[256]uint64 {
	var g [256]uint64
	words := carvetest.Bytes(carvetest.Random, seed, 8*len(g))
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(words[8*i:])
	}

	return &g
}

// tiles reports the first way ends fails to tile [0, n) within RFC 2 §8 C4's
// bounds under s: every chunk longer than zero and at most Max, and every one
// but the last at least Min.
func tiles(s params.Settings, n int, ends []int) error {
	start := 0
	for i, end := range ends {
		l := end - start
		if l <= 0 || l > s.Max() || (i < len(ends)-1 && l < s.Min()) {
			return fmt.Errorf("chunk %d is %d bytes, want %d to %d (the last may be shorter)", i, l, s.Min(), s.Max())
		}

		start = end
	}

	if start != n {
		return fmt.Errorf("chunks end at %d, want the length %d", start, n)
	}

	return nil
}
