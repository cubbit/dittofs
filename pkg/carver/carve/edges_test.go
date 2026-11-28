package carve_test

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
)

// edgeProfile is one settings value with the gear table the reference cuts
// under.
type edgeProfile struct {
	name  string
	s     carver.Settings
	gear  *[256]uint64
	keyed bool
}

// referenceCut is the reference chunker's cut of p, a stretch with a real end
// at base, under gear and s, as the chunks Cut must emit.
func referenceCut(gear *[256]uint64, s carver.Settings, base int64, p []byte) []carver.Chunk {
	var out []carver.Chunk
	start := 0
	for _, end := range carvetest.Boundaries(gear, s.Target(), p) {
		out = append(out, carver.Chunk{Offset: base + int64(start), Length: int32(end - start), Hash: carve.ChunkID(s, p[start:end])})
		start = end
	}

	return out
}

// edgeProfiles is the smallest and largest Target, each unkeyed and keyed.
// Under -short only the smallest runs: the largest cuts hundreds of MiB, and
// a refill or a short tail sits at the same place relative to Max at both.
func edgeProfiles(t *testing.T) []edgeProfile {
	t.Helper()

	v := carvetest.LoadVectors(t)
	targets := []int{carvetest.Targets[0], carvetest.Targets[len(carvetest.Targets)-1]}
	if testing.Short() {
		targets = targets[:1]
	}

	var out []edgeProfile
	for _, target := range targets {
		for _, keyed := range []bool{false, true} {
			chunking, gear := [32]byte{}, &v.GearUnkeyed
			if keyed {
				chunking, gear = v.ChunkingKey, &v.GearKeyed
			}

			s, err := carve.NewSettings(target, keyed, chunking, v.ChunkIDKey)
			if err != nil {
				t.Fatalf("NewSettings(%d, keyed %v): %v", target, keyed, err)
			}
			out = append(out, edgeProfile{fmt.Sprintf("target %d, keyed %v", target, keyed), s, gear, keyed})
		}
	}

	return out
}

// The stretch shapes of RFC 2 §10.2, each cut as the reference cuts it: an
// empty stretch emits nothing and succeeds with either end; 1, Min - 1, Min,
// Max and Max + 1 bytes keep their last chunk at whatever length is left,
// never padded to Min; 3 × Max zeros end on a chunk of exactly Max under the
// unkeyed table, every chunk the buffer's length; and bases 0, 1 and
// MaxInt64 - 4 × Max give offsets that tile from base, the last stretch
// ending with its next byte at MaxInt64.
func TestCutStretchShapes(t *testing.T) {
	for _, e := range edgeProfiles(t) {
		t.Run(e.name, func(t *testing.T) {
			s := e.s

			for _, realEnd := range []bool{false, true} {
				if got, consumed := offer(t, nil, 0, s, make([]byte, s.Max()), realEnd); len(got) != 0 || consumed != 0 {
					t.Errorf("an empty stretch, real end %v: %d chunks and consumed %d, want none and 0", realEnd, len(got), consumed)
				}
			}

			data := carvetest.Bytes(carvetest.Random, uint64(s.Target()), 4*s.Max())
			for _, n := range []int{1, s.Min() - 1, s.Min(), s.Max(), s.Max() + 1} {
				checkClean(t, fmt.Sprintf("%d bytes of Random", n), mustCut(t, bytes.NewReader(data[:n]), 0, s, s.Max()), referenceCut(e.gear, s, 0, data[:n]))
			}

			zeros := carvetest.Bytes(carvetest.Zeros, 1, 3*s.Max())
			got := mustCut(t, bytes.NewReader(zeros), 0, s, s.Max())
			checkClean(t, "3 × Max zeros", got, referenceCut(e.gear, s, 0, zeros))
			if !e.keyed {
				for i, c := range got {
					if int(c.Length) != s.Max() {
						t.Errorf("3 × Max zeros, chunk %d: %d bytes, want Max %d, the buffer's length", i, c.Length, s.Max())
					}
				}
			}

			ref := referenceCut(e.gear, s, 0, data)
			for _, base := range []int64{0, 1, math.MaxInt64 - int64(len(data))} {
				want := slices.Clone(ref)
				for i := range want {
					want[i].Offset += base
				}
				checkClean(t, fmt.Sprintf("%d bytes at base %d", len(data), base), mustCut(t, bytes.NewReader(data), base, s, s.Max()), want)
			}
		})
	}
}

// The buffer edges of RFC 2 §10.2, through a buffer of Max and of Max + 1
// bytes, each cut as the reference cuts it: a boundary exactly where the
// buffer is first refilled, one byte before and one byte after; Max zeros
// with a real end; and a real end whose tail, the last NextBoundary call with
// final set, is shorter than Min. The boundary cases are cut from one long
// seeded input, no byte planted: for each reference end b, the stretch starts
// len(buf) + d bytes before b, and is used once its own reference cut ends a
// chunk there, since a cut from a new start need not meet the old ends at
// once. The input is 4 × Max, not more: every case finds an end in it at the
// smallest and the largest Target under both tables, and a case that finds
// none fails, naming itself, rather than falling back to a plain match.
func TestCutBufferEdges(t *testing.T) {
	for _, e := range edgeProfiles(t) {
		t.Run(e.name, func(t *testing.T) {
			s := e.s
			long := carvetest.Bytes(carvetest.Random, uint64(s.Target())+1, 4*s.Max())
			ends := carvetest.Boundaries(e.gear, s.Target(), long)

			for _, n := range []int{s.Max(), s.Max() + 1} {
				for _, d := range []int{-1, 0, 1} {
					refill := n + d // the stretch offset of the boundary
					found := false
					for _, b := range ends {
						start := b - refill
						if start < 0 || start+2*s.Max() > len(long) {
							continue
						}

						stretch := long[start : start+2*s.Max()]
						if !slices.Contains(carvetest.Boundaries(e.gear, s.Target(), stretch), refill) {
							continue
						}

						name := fmt.Sprintf("buffer %d: a boundary at %d, the refill point %+d", n, refill, d)
						checkClean(t, name, mustCut(t, bytes.NewReader(stretch), 0, s, n), referenceCut(e.gear, s, 0, stretch))
						found = true

						break
					}
					if !found {
						t.Fatalf("buffer %d: no end of %d bytes of Random cuts a stretch at the refill point %+d", n, len(long), d)
					}
				}

				zeros := carvetest.Bytes(carvetest.Zeros, 1, s.Max())
				checkClean(t, fmt.Sprintf("buffer %d: Max zeros", n), mustCut(t, bytes.NewReader(zeros), 0, s, n), referenceCut(e.gear, s, 0, zeros))

				// The first end past the buffer, so the short tail comes after
				// a refill; the last end is the input's, with no room after it.
				i := slices.IndexFunc(ends, func(b int) bool { return b > n })
				if i < 0 || i == len(ends)-1 {
					t.Fatalf("buffer %d: no end of %d bytes of Random past the buffer leaves room for a tail", n, len(long))
				}
				tail := long[:ends[i]+s.Min()/2]
				ref := referenceCut(e.gear, s, 0, tail)
				if last := ref[len(ref)-1]; int(last.Length) != s.Min()/2 {
					t.Fatalf("buffer %d: the reference's last chunk is %d bytes, want the %d-byte tail", n, last.Length, s.Min()/2)
				}
				checkClean(t, fmt.Sprintf("buffer %d: a %d-byte tail", n, s.Min()/2), mustCut(t, bytes.NewReader(tail), 0, s, n), ref)
			}
		})
	}
}
