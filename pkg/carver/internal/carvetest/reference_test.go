package carvetest

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"slices"
	"testing"

	"lukechampine.com/blake3"
)

var update = flag.Bool("update", false, "rewrite testdata/boundaries.json from the reference")

// table is a gear table with every word w but those set in sets.
func table(w uint64, sets map[byte]uint64) *[256]uint64 {
	var g [256]uint64
	for i := range g {
		g[i] = w
	}
	for b, v := range sets {
		g[b] = v
	}

	return &g
}

// Every G[b] zero passes every test, so each chunk ends at its first tested
// byte, index Min − 1, and is exactly Min; the last is the rest.
func TestReferenceZeroTableCutsAtMin(t *testing.T) {
	for _, target := range Targets {
		minLen := target / 4
		n := 3*minLen + minLen/2

		got := Boundaries(table(0, nil), target, Bytes(Random, 1, n))
		if want := []int{minLen, 2 * minLen, 3 * minLen, n}; !slices.Equal(got, want) {
			t.Errorf("Target %d, every G[b] zero: ends %v, want %v", target, got, want)
		}
	}
}

// Every G[b] 2⁶³ keeps fp's top bit set, so no byte passes and each chunk
// ends at Max.
func TestReferenceTopBitTableCutsAtMax(t *testing.T) {
	for _, target := range Targets {
		maxLen := 4 * target
		n := 2*maxLen + 7

		got := Boundaries(table(1<<63, nil), target, Bytes(Random, 1, n))
		if want := []int{maxLen, 2 * maxLen, n}; !slices.Equal(got, want) {
			t.Errorf("Target %d, every G[b] 2^63: ends %v, want %v", target, got, want)
		}
	}
}

// With G[1] = 2⁶³ a run of byte 1 leaves fp = 2⁶³ after every byte, failing
// every test. One byte x at index k makes fp = G[x] there, so G[x] alone
// decides whether the chunk ends at k, which pins m on each side of Target.
// When it does not, x's term reaches bit 63 after 63 − log2(G[x]) more bytes,
// where it carries out against the run's 2⁶³ and leaves fp zero, a boundary.
// Every later chunk is a run of 1 and ends at Max.
func TestReferencePinsTheTestEitherSideOfTarget(t *testing.T) {
	const x = 2

	for _, target := range Targets {
		tt := bits.TrailingZeros(uint(target))
		maxLen := 4 * target

		cases := []struct {
			name string
			k    int // x's index
			gx   uint64
			end  int // the first chunk's end
		}{
			// Before Target m is t + 1: fp passes when under 2^(63−t).
			{"before Target, under 2^(63-t)", target - 1, 1 << (62 - tt), target},
			{"before Target, at 2^(63-t)", target - 1, 1 << (63 - tt), target - 1 + tt + 1},
			// From Target on m is t − 3: fp passes when under 2^(67−t).
			{"from Target, under 2^(67-t)", target, 1 << (66 - tt), target + 1},
			{"from Target, at 2^(67-t)", target, 1 << (67 - tt), target + (tt - 4) + 1},
		}
		for _, c := range cases {
			p := make([]byte, 2*maxLen)
			for i := range p {
				p[i] = 1
			}
			p[c.k] = x

			got := Boundaries(table(0, map[byte]uint64{1: 1 << 63, x: c.gx}), target, p)
			if want := []int{c.end, c.end + maxLen, len(p)}; !slices.Equal(got, want) {
				t.Errorf("Target %d, %s (G[x] = %#x at index %d): ends %v, want %v", target, c.name, c.gx, c.k, got, want)
			}
		}
	}
}

// goldenSeed is the seed of a profile's input: one per Target, the same for
// both tables.
func goldenSeed(target int) uint64 { return uint64(target) }

// cut is the reference's answer for one profile: its ends and each chunk's
// keyed hash under key.
func cut(g *[256]uint64, key [32]byte, target int, p []byte) ([]int, [][32]byte) {
	ends := Boundaries(g, target, p)
	hashes := make([][32]byte, len(ends))

	start := 0
	for i, end := range ends {
		h := blake3.New(32, key[:])
		_, _ = h.Write(p[start:end])
		h.Sum(hashes[i][:0])
		start = end
	}

	return ends, hashes
}

// The reference reproduces the committed boundaries and hashes of every
// supported Target, under both tables; -update rewrites the file from it.
func TestGoldenBoundaries(t *testing.T) {
	v := LoadVectors(t)

	if *update {
		var fs []goldenFile
		for _, target := range Targets {
			p := Bytes(Random, goldenSeed(target), 16*target)
			for _, keyed := range []bool{false, true} {
				g := &v.GearUnkeyed
				if keyed {
					g = &v.GearKeyed
				}

				ends, hashes := cut(g, v.ChunkIDKey, target, p)
				f := goldenFile{Target: target, Keyed: keyed, Seed: goldenSeed(target), Length: len(p), Ends: ends}
				for _, h := range hashes {
					f.Hashes = append(f.Hashes, hex.EncodeToString(h[:]))
				}
				fs = append(fs, f)
			}
		}

		raw, err := json.MarshalIndent(fs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/boundaries.json", append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote testdata/boundaries.json; run again without -update to check it")

		return
	}

	for _, gd := range LoadBoundaries(t) {
		name := fmt.Sprintf("Target %d keyed %t", gd.Target, gd.Keyed)
		g := &v.GearUnkeyed
		if gd.Keyed {
			g = &v.GearKeyed
		}

		ends, hashes := cut(g, v.ChunkIDKey, gd.Target, Bytes(Random, gd.Seed, gd.Length))
		if !slices.Equal(ends, gd.Ends) {
			t.Errorf("%s: ends %v, want the committed %v", name, ends, gd.Ends)
			continue
		}
		for i := range hashes {
			if hashes[i] != gd.Hashes[i] {
				t.Errorf("%s: chunk %d hash %x, want the committed %x", name, i, hashes[i], gd.Hashes[i])
			}
		}
	}
}

// The committed file covers every supported Target under both tables, and
// every profile's chunks tile its length within RFC 2 §8 C4's bounds, so a
// file written by a broken reference fails here even before the reference is
// run against it.
func TestGoldenBoundariesHoldTheBounds(t *testing.T) {
	type profile struct {
		target int
		keyed  bool
	}

	seen := map[profile]bool{}
	for _, gd := range LoadBoundaries(t) {
		name := fmt.Sprintf("Target %d keyed %t", gd.Target, gd.Keyed)
		if !slices.Contains(Targets[:], gd.Target) {
			t.Errorf("%s: not a supported Target %v", name, Targets)
		}
		if seen[profile{gd.Target, gd.Keyed}] {
			t.Errorf("%s: committed twice", name)
		}
		seen[profile{gd.Target, gd.Keyed}] = true

		if want := 16 * gd.Target; gd.Length != want {
			t.Errorf("%s: length %d, want 16 × Target = %d", name, gd.Length, want)
		}

		start := 0
		for i, end := range gd.Ends {
			n := end - start
			last := i == len(gd.Ends)-1
			if n > 4*gd.Target || n <= 0 || (!last && n < gd.Target/4) {
				t.Errorf("%s: chunk %d is %d bytes, want %d to %d (the last may be shorter)", name, i, n, gd.Target/4, 4*gd.Target)
			}
			start = end
		}
		if start != gd.Length {
			t.Errorf("%s: chunks end at %d, want the length %d", name, start, gd.Length)
		}
	}

	for _, target := range Targets {
		for _, keyed := range []bool{false, true} {
			if !seen[profile{target, keyed}] {
				t.Errorf("Target %d keyed %t: no committed profile", target, keyed)
			}
		}
	}
}

// parseBoundaries refuses a file LoadBoundaries' callers could not trust.
func TestParseBoundariesRefuses(t *testing.T) {
	hash := `"` + hex.EncodeToString(make([]byte, 32)) + `"`
	for _, c := range []struct{ name, raw string }{
		{"not JSON", `{`},
		{"no profile", `[]`},
		{"no chunks", `[{"target":65536,"length":0,"ends":[],"hashes":[]}]`},
		{"fewer hashes than ends", `[{"target":65536,"length":2,"ends":[1,2],"hashes":[` + hash + `]}]`},
		{"a short hash", `[{"target":65536,"length":1,"ends":[1],"hashes":["00"]}]`},
		{"a hash not hex", `[{"target":65536,"length":1,"ends":[1],"hashes":["zz"]}]`},
	} {
		if _, err := parseBoundaries([]byte(c.raw)); err == nil {
			t.Errorf("%s: parseBoundaries accepted it, want an error", c.name)
		}
	}

	if gs, err := parseBoundaries([]byte(`[{"target":65536,"length":1,"ends":[1],"hashes":[` + hash + `]}]`)); err != nil || len(gs) != 1 || gs[0].Ends[0] != 1 {
		t.Errorf("a well-formed profile: got %v, %v, want one profile ending at 1", gs, err)
	}
}
