package carve_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
)

// offer is one Cut of data at base through buf, ending as realEnd says: every
// chunk emit saw, and consumed, which must be their sum.
func offer(t *testing.T, data []byte, base int64, s carver.Settings, buf []byte, realEnd bool) ([]carver.Chunk, int64) {
	t.Helper()

	var got []carver.Chunk
	consumed, err := carve.Cut(bytes.NewReader(data), base, s, buf, realEnd, func(c carver.Chunk, p []byte) error {
		if len(p) != int(c.Length) || cap(p) != len(p) {
			t.Fatalf("chunk at %d: %d bytes of capacity %d handed to emit, want Length %d of both", c.Offset, len(p), cap(p), c.Length)
		}
		got = append(got, c)

		return nil
	})
	if err != nil {
		t.Fatalf("Cut of %d bytes at base %d, real end %v: %v", len(data), base, realEnd, err)
	}

	var sum int64
	for _, c := range got {
		sum += int64(c.Length)
	}
	if consumed != sum {
		t.Fatalf("Cut of %d bytes at base %d, real end %v: consumed %d, want %d, the bytes emitted", len(data), base, realEnd, consumed, sum)
	}

	return got, consumed
}

// heldAt is the index of the first chunk of ref an artificial end at limit
// holds back: a chunk is cut only with Max bytes in view from its start.
func heldAt(ref []carver.Chunk, from int, limit int64, s carver.Settings) int {
	for from < len(ref) && ref[from].Offset+int64(s.Max()) <= limit {
		from++
	}

	return from
}

// checkOffer fails t unless got is ref[from:want] and the bytes left past
// consumed, up to limit, are fewer than Max.
func checkOffer(t *testing.T, name string, got []carver.Chunk, consumed int64, ref []carver.Chunk, from, want int, pos, limit int64, s carver.Settings) {
	t.Helper()

	if len(got) != want-from {
		t.Fatalf("%s: offer [%d, %d) emitted %d chunks, want %d, those with Max bytes in view", name, pos, limit, len(got), want-from)
	}
	for i, c := range got {
		if c != ref[from+i] {
			t.Fatalf("%s: offer [%d, %d), chunk %d: got %+v, want %+v as one stretch cuts it", name, pos, limit, i, c, ref[from+i])
		}
	}
	if held := limit - pos - consumed; held >= int64(s.Max()) {
		t.Fatalf("%s: offer [%d, %d) held %d bytes back, want fewer than Max %d", name, pos, limit, held, s.Max())
	}
}

// Offers ending at seeded limits, each re-offered from the previous consumed
// and the last with a real end, emit exactly the chunks one real-end stretch
// emits, under both tables at every profile: a tail cut at a limit would move
// every later chunk's start. The unkeyed table cuts through a buffer of Max,
// the keyed one through 2 × Max, where the reader's end can arrive with Max
// bytes or more still uncut. Under -short only the profiles up to 256 KiB
// run: the larger ones hold up to 512 MiB and cut nothing the smaller don't.
func TestCutArtificialEndsCutAsOneStretch(t *testing.T) {
	v := carvetest.LoadVectors(t)

	for _, target := range carvetest.Targets {
		if testing.Short() && target > 256<<10 {
			continue
		}

		var data []byte
		for _, keyed := range []bool{false, true} {
			var chunking [32]byte
			if keyed {
				chunking = v.ChunkingKey
			}

			s, err := carve.NewSettings(target, keyed, chunking, v.ChunkIDKey)
			if err != nil {
				t.Fatalf("NewSettings(%d, keyed %v): %v", target, keyed, err)
			}

			name := fmt.Sprintf("target %d, keyed %v", target, keyed)
			if data == nil {
				data = carvetest.Bytes(carvetest.Random, uint64(target), 32*s.Max())
			}
			buf := make([]byte, s.Max())
			if keyed {
				buf = make([]byte, 2*s.Max())
			}
			ref, _ := offer(t, data, 0, s, buf, true)

			rng := rand.New(rand.NewPCG(uint64(target), 1))
			var pos, limit int64
			next, offers := 0, 0
			for realEnd := false; !realEnd; offers++ {
				limit += 1 + rng.Int64N(2*int64(s.Max()))
				if limit >= int64(len(data)) {
					limit, realEnd = int64(len(data)), true
				}

				got, consumed := offer(t, data[pos:limit], pos, s, buf, realEnd)
				want := len(ref)
				if !realEnd {
					want = heldAt(ref, next, limit, s)
				}
				checkOffer(t, name, got, consumed, ref, next, want, pos, limit, s)
				next, pos = want, pos+consumed
			}

			if offers < 16 {
				t.Errorf("%s: %d offers, want at least 16", name, offers)
			}
		}
	}
}

// An artificial end at every byte of one chunk, at the smallest profile, cuts
// exactly the chunks with Max bytes in view and consumes their sum, so
// consumed never counts the held tail nor stops a chunk short. The offers
// start on a boundary j whose Max-byte view ends inside chunk k, so the limits
// cross the byte where chunk j is first cut; k is the shortest such chunk,
// which keeps the offers few.
func TestCutArtificialEndAtEveryLimit(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 7, 8*s.Max())
	buf := make([]byte, s.Max())
	ref, _ := offer(t, data, 0, s, buf, true)

	j, k := -1, -1
	for i := range ref {
		view := ref[i].Offset + int64(s.Max())
		c := i
		for c < len(ref) && ref[c].Offset+int64(ref[c].Length) < view {
			c++
		}
		if c < len(ref) && (k < 0 || ref[c].Length < ref[k].Length) {
			j, k = i, c
		}
	}
	if k < 0 {
		t.Fatalf("no chunk of %d holds a boundary's Max-byte view", len(ref))
	}

	pos := ref[j].Offset
	start, end := ref[k].Offset, ref[k].Offset+int64(ref[k].Length)
	counts := map[int]bool{}
	for limit := start + 1; limit <= end; limit++ {
		got, consumed := offer(t, data[pos:limit], pos, s, buf, false)
		checkOffer(t, "every limit", got, consumed, ref, j, heldAt(ref, j, limit, s), pos, limit, s)
		counts[len(got)] = true
	}
	if len(counts) < 2 {
		t.Fatalf("limits (%d, %d] from %d all emitted the same number of chunks, want the first cut crossed", start, end, pos)
	}
}

// The two shapes RFC 2 §10.2 names: an artificial end on Max - 1 bytes with
// no boundary emits nothing and consumes zero, and one on 3 × Max zeros under
// the unkeyed table, whose boundaries fall at Max, emits three chunks of Max
// and holds nothing back.
func TestCutArtificialEndShapes(t *testing.T) {
	for _, target := range carvetest.Targets {
		s, err := carve.NewSettings(target, false, [32]byte{}, [32]byte{0: 1})
		if err != nil {
			t.Fatalf("NewSettings(%d): %v", target, err)
		}

		// A buffer of Max cuts the last zero chunk before the reader reports
		// its end; one of 2 × Max sees the end with that chunk still held.
		short := carvetest.Bytes(carvetest.Random, 7, s.Max()-1)
		for _, n := range []int{s.Max(), 2 * s.Max()} {
			buf := make([]byte, n)
			if got, consumed := offer(t, short, 0, s, buf, false); len(got) != 0 || consumed != 0 {
				t.Errorf("target %d, buffer %d, Max - 1 bytes of Random: %d chunks and consumed %d, want none and 0", target, n, len(got), consumed)
			}

			zeros := carvetest.Bytes(carvetest.Zeros, 7, 3*s.Max())
			got, consumed := offer(t, zeros, 0, s, buf, false)
			if consumed != int64(len(zeros)) || len(got) != 3 {
				t.Fatalf("target %d, buffer %d, 3 × Max zeros: %d chunks and consumed %d, want 3 and %d", target, n, len(got), consumed, len(zeros))
			}
			for i, c := range got {
				if c.Offset != int64(i*s.Max()) || int(c.Length) != s.Max() {
					t.Errorf("target %d, buffer %d, 3 × Max zeros, chunk %d: at %d, %d bytes, want at %d, Max %d", target, n, i, c.Offset, c.Length, i*s.Max(), s.Max())
				}
			}
		}
	}
}
