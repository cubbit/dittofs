package chunker

import (
	"bytes"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// eachTarget runs f at every supported Target, with exp its exponent.
func eachTarget(t *testing.T, f func(t *testing.T, target, exp int)) {
	for _, target := range carvetest.Targets {
		t.Run(fmt.Sprintf("target=%d", target), func(t *testing.T) { f(t, target, bits.TrailingZeros(uint(target))) })
	}
}

// Every golden boundary, unkeyed and keyed, at every profile, is reproduced
// end for end through a window of Max bytes, as the carver's Cut streams.
func TestGoldenBoundaries(t *testing.T) {
	v := carvetest.LoadVectors(t)
	seen := map[[2]int]bool{}

	for _, g := range carvetest.LoadBoundaries(t) {
		gear, keyed := &v.GearUnkeyed, 0
		if g.Keyed {
			gear, keyed = &v.GearKeyed, 1
		}

		seen[[2]int{g.Target, keyed}] = true
		s := params.New(g.Target, gear)

		got := Ends(s, carvetest.Input(carvetest.Random, g.Seed, int64(g.Length)), s.Max())
		if !slices.Equal(got, g.Ends) {
			t.Errorf("target %d, keyed %v, seed %d, %d bytes: %d ends, first difference at chunk %s; want %d ends",
				g.Target, g.Keyed, g.Seed, g.Length, len(got), firstDiff(got, g.Ends), len(g.Ends))
		}
	}

	for _, target := range carvetest.Targets {
		for keyed := range 2 {
			if !seen[[2]int{target, keyed}] {
				t.Errorf("no golden at target %d, keyed %v; want one at every profile under both tables", target, keyed == 1)
			}
		}
	}
}

// firstDiff names the first chunk end got and want disagree on, with both
// values, or a missing one as -1.
func firstDiff(got, want []int) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}

	at := func(s []int) int {
		if i < len(s) {
			return s[i]
		}

		return -1
	}

	return fmt.Sprintf("%d (got %d, want %d)", i, at(got), at(want))
}

// table returns a gear table whose every entry is v, then sets pairs of
// (byte, value) over it.
func table(v uint64, set ...uint64) *[256]uint64 {
	var g [256]uint64
	for i := range g {
		g[i] = v
	}

	for i := 0; i < len(set); i += 2 {
		g[set[i]] = set[i+1]
	}

	return &g
}

// filled returns n bytes of b.
func filled(n int, b byte) []byte { return bytes.Repeat([]byte{b}, n) }

// With every table entry zero every fingerprint is zero, so every chunk ends
// at Min, the first byte tested; with every entry 2⁶³ the top bit is always
// set, so every chunk ends at Max. The last chunk of the stream is the rest.
func TestTablesThatAlwaysOrNeverPassCutAtMinOrMax(t *testing.T) {
	eachTarget(t, func(t *testing.T, target, _ int) {
		for _, tc := range []struct {
			name  string
			gear  *[256]uint64
			chunk func(s params.Settings) int
		}{
			{"zero table", table(0), params.Settings.Min},
			{"top-bit table", table(1 << 63), params.Settings.Max},
		} {
			s := params.New(target, tc.gear)
			n := 2*s.Max() + 12345
			got := Ends(s, bytes.NewReader(make([]byte, n)), s.Max())

			want := []int{}
			for e := tc.chunk(s); e < n; e += tc.chunk(s) {
				want = append(want, e)
			}

			want = append(want, n)

			if !slices.Equal(got, want) {
				t.Errorf("%s over %d bytes: %d ends, first difference at chunk %s; want %d ends",
					tc.name, n, len(got), firstDiff(got, want), len(want))
			}
		}
	})
}

// The first byte tested is at index Min−1: under a table where byte 1 always
// sets the top bit and byte 0 adds nothing, a 0 in a run of 1s zeroes the
// fingerprint, which ends the chunk just after it at Min−1, and is never
// tested at Min−2, where the next byte sets the top bit again.
func TestTheFirstByteTestedIsAtMinMinusOne(t *testing.T) {
	eachTarget(t, func(t *testing.T, target, _ int) {
		s := params.New(target, table(0, 1, 1<<63))

		for _, tc := range []struct {
			at, want int
		}{
			{s.Min() - 1, s.Min()},
			{s.Min() - 2, s.Max()},
		} {
			buf := filled(s.Max(), 1)
			buf[tc.at] = 0

			if got := NextBoundary(s, buf, false); got != tc.want {
				t.Errorf("a 0 at index %d (Min %d): NextBoundary = %d, want %d", tc.at, s.Min(), got, tc.want)
			}
		}
	})
}

// The warm-up covers the 64 bytes ending at index Min−1, so a byte at Min−64
// is in the first fingerprint tested: with G[1] = 1 it reaches bit 63 there,
// fails the test at Min−1, and leaves at Min, which ends the chunk at Min+1.
// A warm-up over 63 bytes would end it at Min.
func TestTheWarmUpCoversSixtyFourBytes(t *testing.T) {
	eachTarget(t, func(t *testing.T, target, _ int) {
		s := params.New(target, table(0, 1, 1))
		buf := make([]byte, s.Max())
		buf[s.Min()-window] = 1

		if got, want := NextBoundary(s, buf, false), s.Min()+1; got != want {
			t.Errorf("byte 1 at index Min−64 = %d: NextBoundary = %d, want %d", s.Min()-window, got, want)
		}
	})
}

// For a Target of 2^t the test reads the top t+1 bits while the chunk's
// length before the byte under test is under Target, and the top t−3 from
// Target on. In a run of byte 1, whose 2⁶³ leaves the fingerprint after one
// shift, a byte 2 at index i makes the fingerprint G[2] exactly, tested at i:
// it passes, ending the chunk at i+1, exactly when G[2] < 2^(64−m). The final
// buffer ends one byte later, before a later carry out of bit 63 could zero
// the fingerprint, so a failing test ends the chunk at the buffer's end.
func TestTheBitCountSwitchesAtTarget(t *testing.T) {
	eachTarget(t, func(t *testing.T, target, exp int) {
		before, after := exp+1, exp-3
		pass := func(m int) uint64 { return 1<<(64-m) - 1 }
		fail := func(m int) uint64 { return 1 << (64 - m) }

		for _, tc := range []struct {
			name string
			at   int
			g2   uint64
			pass bool
		}{
			{"under Target, largest passing", target - 1, pass(before), true},
			{"under Target, smallest failing", target - 1, fail(before), false},
			{"at Target, largest passing", target, pass(after), true},
			{"at Target, smallest failing", target, fail(after), false},
			{"at Target, failing under Target's count", target, fail(before), true},
		} {
			s := params.New(target, table(0, 1, 1<<63, 2, tc.g2))
			buf := filled(tc.at+2, 1)
			buf[tc.at] = 2

			want := len(buf)
			if tc.pass {
				want = tc.at + 1
			}

			if got := NextBoundary(s, buf, true); got != want {
				t.Errorf("%s: byte 2 = %#x at index %d: NextBoundary = %d, want %d", tc.name, tc.g2, tc.at, got, want)
			}
		}
	})
}

// A buffer of Max bytes with no boundary ends at Max; a final buffer ends at
// its length, or at Max when it is longer, and a non-empty one never at 0.
func TestBufferEdges(t *testing.T) {
	eachTarget(t, func(t *testing.T, target, _ int) {
		s := params.New(target, table(1<<63))

		for _, tc := range []struct {
			n     int
			final bool
			want  int
		}{
			{s.Max(), false, s.Max()},
			{s.Max() + 1, false, s.Max()},
			{0, true, 0},
			{1, true, 1},
			{s.Min() - 1, true, s.Min() - 1},
			{s.Min(), true, s.Min()},
			{s.Max(), true, s.Max()},
			{s.Max() + 1, true, s.Max()},
		} {
			if got := NextBoundary(s, make([]byte, tc.n), tc.final); got != tc.want {
				t.Errorf("%d bytes, final %v: NextBoundary = %d, want %d", tc.n, tc.final, got, tc.want)
			}
		}
	})
}

// There is no "not yet" answer: a buffer under Max that is not final panics,
// and so does the zero settings value, whose bit counts of 0 would pass every
// byte and cut a chunk of one byte.
func TestNextBoundaryRefusesWhatHasNoAnswer(t *testing.T) {
	s := params.New(64<<10, table(1<<63))

	for _, tc := range []struct {
		name  string
		s     params.Settings
		n     int
		final bool
		want  string
	}{
		{"Max−1 bytes, not final", s, s.Max() - 1, false, "not final"},
		{"the zero value", params.Settings{}, s.Max(), true, "zero settings value"},
		{"the zero value, empty", params.Settings{}, 0, true, "zero settings value"},
	} {
		func() {
			defer func() {
				if got, _ := recover().(string); !strings.Contains(got, tc.want) {
					t.Errorf("%s: NextBoundary panicked with %q, want a panic naming %q", tc.name, got, tc.want)
				}
			}()

			NextBoundary(tc.s, make([]byte, tc.n), tc.final)
		}()
	}
}

// NextBoundary allocates nothing, over a buffer of Max bytes whose search
// runs to Max.
func TestAllocsNextBoundary(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector allocates")
	}

	s := params.New(256<<10, table(1<<63))
	buf := make([]byte, s.Max())

	if got := testing.AllocsPerRun(10, func() { NextBoundary(s, buf, false) }); got != 0 {
		t.Errorf("NextBoundary over %d bytes: %v allocations, want 0", len(buf), got)
	}
}
