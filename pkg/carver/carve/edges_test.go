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
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
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
// bytes, each cut as the reference cuts it. With a buffer that small Cut
// refills after every chunk, so a chunk starting after a refill sees exactly
// the buffer's length of bytes: the refill point is that many bytes past the
// chunk's start. A boundary is put one byte before it, at it and one byte after
// it by refillStretch; past Max the chunk is cut at Max whatever lies beyond,
// so at Max + 1 bytes only "one byte before" has a content boundary in view.
// Max zeros with a real end, and a real end whose tail, the last NextBoundary
// call with final set, is shorter than Min, follow.
func TestCutBufferEdges(t *testing.T) {
	for _, e := range edgeProfiles(t) {
		t.Run(e.name, func(t *testing.T) {
			s := e.s
			long := carvetest.Bytes(carvetest.Random, uint64(s.Target())+1, 4*s.Max())
			ends := carvetest.Boundaries(e.gear, s.Target(), long)
			w := 0
			if !e.keyed {
				w = refillWindow(t, s, long)
			}

			for _, n := range []int{s.Max(), s.Max() + 1} {
				if !e.keyed {
					for _, d := range []int{-1, 0, 1} {
						stretch, end := refillStretch(s, long, w, n+d)
						ref := referenceCut(e.gear, s, 0, stretch)
						if len(ref) < 2 || int(ref[0].Length) != s.Max() || ref[1].Offset+int64(ref[1].Length) != int64(end) {
							t.Fatalf("buffer %d, refill point %+d: the reference cuts %v, want Max zeros then a chunk ending at %d", n, d, ref[:min(2, len(ref))], end)
						}
						checkClean(t, fmt.Sprintf("buffer %d: a boundary at the refill point %+d", n, d), mustCut(t, bytes.NewReader(stretch), 0, s, n), ref)
					}
				}

				// A chunk exactly the buffer's length is the Max-byte chunk
				// zeros give under the unkeyed table, in the Max-byte buffer;
				// in the Max + 1 one no chunk is that long.
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

// refillWindow is where in long the 64 bytes end that, after zeros, pass the
// boundary test first at their last byte, read with the bit count from Target
// on: the fingerprint forgets a byte 64 bytes on, so zeros then this window
// end a chunk at the window's end once the chunk is Target bytes or longer,
// and no earlier.
func refillWindow(t *testing.T, s carver.Settings, long []byte) int {
	t.Helper()

	g := params.Gear(s)
	_, m := params.Bits(s)
	var zfp uint64 // the fingerprint over 64 zeros
	for range 64 {
		zfp = zfp<<1 + g[0]
	}

	for w := 64; w <= len(long); w++ {
		fp, pass := zfp, 0
		for _, b := range long[w-64 : w] {
			if fp = fp<<1 + g[b]; fp>>(64-m) == 0 {
				pass++
			}
		}
		if pass == 1 && fp>>(64-m) == 0 {
			return w
		}
	}
	t.Fatalf("no 64 bytes of %d bytes of Random pass the boundary test at their last byte alone", len(long))

	return 0
}

// refillStretch is a stretch under the unkeyed table whose second chunk
// starts after a refill and would end, by content, l bytes in: Max zeros, the
// first chunk; zeros up to long's window ending at w (refillWindow); then the
// Max bytes of long after it. It returns the stretch and where its second
// chunk ends: at Max + l, or Max + Max once l passes Max. Only the unkeyed
// table promises zeros no boundary (RFC 2 §3.8), so a chunk runs out to Max
// with no byte planted.
func refillStretch(s carver.Settings, long []byte, w, l int) ([]byte, int) {
	rest := long[w:min(w+s.Max(), len(long))]

	return slices.Concat(make([]byte, s.Max()+l-64), long[w-64:w], rest), s.Max() + min(l, s.Max())
}
