package carve_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// cut is every chunk one Cut over r emitted, with the error it returned.
func cut(t *testing.T, r io.Reader, base int64, s carver.Settings, buf int) ([]carver.Chunk, error) {
	t.Helper()

	var got []carver.Chunk
	consumed, err := carve.Cut(r, base, s, make([]byte, buf), true, func(c carver.Chunk, p []byte) error {
		if len(p) != int(c.Length) {
			t.Fatalf("chunk at %d: %d bytes handed to emit, Length %d", c.Offset, len(p), c.Length)
		}
		got = append(got, c)

		return nil
	})

	var sum int64
	for _, c := range got {
		sum += int64(c.Length)
	}
	if consumed != sum {
		t.Errorf("Cut at base %d: consumed %d, want %d, the bytes emit accepted", base, consumed, sum)
	}

	return got, err
}

// mustCut is cut that fails t on an error.
func mustCut(t *testing.T, r io.Reader, base int64, s carver.Settings, buf int) []carver.Chunk {
	t.Helper()

	got, err := cut(t, r, base, s, buf)
	if err != nil {
		t.Fatalf("Cut at base %d through %d bytes: %v", base, buf, err)
	}

	return got
}

// Every golden, under both tables at every profile, is reproduced by Cut, ends
// and hashes, through a buffer of Max bytes and one of 2 × Max: a refill that
// drops or repeats a byte, a hash over the buffer rather than the chunk, or an
// end other than the reference's fails.
func TestCutGivesTheGoldens(t *testing.T) {
	v := carvetest.LoadVectors(t)

	for _, g := range carvetest.LoadBoundaries(t) {
		var chunking [32]byte
		if g.Keyed {
			chunking = v.ChunkingKey
		}

		s, err := carve.NewSettings(g.Target, g.Keyed, chunking, v.ChunkIDKey)
		if err != nil {
			t.Fatalf("NewSettings(%d, keyed %v): %v", g.Target, g.Keyed, err)
		}

		for _, buf := range []int{s.Max(), 2 * s.Max()} {
			got := mustCut(t, carvetest.Input(carvetest.Random, g.Seed, int64(g.Length)), 0, s, buf)

			var end int64
			ends, hashes := make([]int, len(got)), make([][32]byte, len(got))
			for i, c := range got {
				if c.Offset != end {
					t.Fatalf("target %d, keyed %v, buffer %d: chunk %d at offset %d, want %d, the previous end", g.Target, g.Keyed, buf, i, c.Offset, end)
				}
				end += int64(c.Length)
				ends[i], hashes[i] = int(end), c.Hash
			}

			if !slices.Equal(ends, g.Ends) {
				t.Errorf("target %d, keyed %v, buffer %d: ends %v, want %v", g.Target, g.Keyed, buf, ends, g.Ends)
			}
			if !slices.Equal(hashes, g.Hashes) {
				t.Errorf("target %d, keyed %v, buffer %d: hashes differ from the golden ones", g.Target, g.Keyed, buf)
			}
		}
	}
}

// settings64K is the smallest profile, unkeyed, under idKey.
func settings64K(t *testing.T, idKey [32]byte) carver.Settings {
	t.Helper()

	s, err := carve.NewSettings(64<<10, false, [32]byte{}, idKey)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}

	return s
}

// The same bytes at three bases give one list of hashes, each offset moved by
// the base, so nothing of the offset reaches a chunk ID; under two chunk-ID
// keys they cut at the same ends with no ID in common.
func TestCutHashesOnlyTheChunk(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 7, 8*s.Max())

	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())
	if len(ref) < 2 {
		t.Fatalf("%d bytes cut into %d chunks, want several", len(data), len(ref))
	}

	for _, base := range []int64{1, 12345} {
		got := mustCut(t, bytes.NewReader(data), base, s, s.Max())
		if len(got) != len(ref) {
			t.Fatalf("base %d: %d chunks, want %d as at base 0", base, len(got), len(ref))
		}
		for i := range got {
			if want := (carver.Chunk{Offset: ref[i].Offset + base, Length: ref[i].Length, Hash: ref[i].Hash}); got[i] != want {
				t.Errorf("base %d, chunk %d: got %+v, want %+v", base, i, got[i], want)
			}
		}
	}

	other := mustCut(t, bytes.NewReader(data), 0, settings64K(t, [32]byte{0: 2}), s.Max())
	if len(other) != len(ref) {
		t.Fatalf("under a second chunk-ID key: %d chunks, want %d", len(other), len(ref))
	}

	ids := map[entity.ChunkHash]bool{}
	for _, c := range ref {
		ids[c.Hash] = true
	}
	for i, c := range other {
		if c.Offset != ref[i].Offset || c.Length != ref[i].Length {
			t.Errorf("chunk %d under a second chunk-ID key: at %d, %d bytes, want %d, %d", i, c.Offset, c.Length, ref[i].Offset, ref[i].Length)
		}
		if ids[c.Hash] {
			t.Errorf("chunk %d under a second chunk-ID key: ID %s, also an ID under the first", i, c.Hash)
		}
	}
}

// The bytes handed to emit are the caller's buffer, not a copy: the first
// chunk's slice, kept, reads other bytes once a later chunk has been emitted.
func TestCutLendsTheBuffer(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 7, 8*s.Max())

	var kept, first []byte
	chunks := 0
	_, err := carve.Cut(bytes.NewReader(data), 0, s, make([]byte, s.Max()), true, func(_ carver.Chunk, p []byte) error {
		if chunks == 0 {
			kept, first = p, bytes.Clone(p)
		}
		chunks++

		return nil
	})
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}

	if chunks < 2 {
		t.Fatalf("%d chunks, want several", chunks)
	}
	if bytes.Equal(kept, first) {
		t.Errorf("the first chunk's slice, kept past %d chunks, still reads its own bytes; want the buffer reused", chunks)
	}
}

// Each refused call fails with its error before a byte is read; a stretch
// running past the largest file offset fails before any chunk is emitted.
func TestCutRefuses(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 7, 100)

	for _, tc := range []struct {
		name string
		s    carver.Settings
		buf  int
		base int64
		want error
	}{
		{"a buffer of Max - 1 bytes", s, s.Max() - 1, 0, io.ErrShortBuffer},
		{"the zero settings value", carver.Settings{}, s.Max(), 0, carver.ErrInvalidSettings},
		{"a base of -1", s, s.Max(), -1, fs.ErrInvalid},
	} {
		r := bytes.NewReader(data)
		got, err := cut(t, r, tc.base, tc.s, tc.buf)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if r.Len() != len(data) || len(got) != 0 {
			t.Errorf("%s: %d bytes read and %d chunks emitted, want none", tc.name, len(data)-r.Len(), len(got))
		}
	}

	got, err := cut(t, bytes.NewReader(data), math.MaxInt64-10, s, s.Max())
	if !errors.Is(err, fs.ErrInvalid) || len(got) != 0 {
		t.Errorf("%d bytes at base MaxInt64 - 10: %d chunks and %v, want none and %v", len(data), len(got), err, fs.ErrInvalid)
	}
}

// Cut over 4 × Max bytes allocates nothing, at the smallest and the largest
// Target, hashing included.
func TestAllocsCut(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector allocates")
	}

	for _, target := range []int{64 << 10, 4 << 20} {
		s, err := carve.NewSettings(target, false, [32]byte{}, [32]byte{0: 1})
		if err != nil {
			t.Fatalf("NewSettings(%d): %v", target, err)
		}

		data := carvetest.Bytes(carvetest.Random, 7, 4*s.Max())
		buf := make([]byte, s.Max())
		r := bytes.NewReader(data)
		emit := func(carver.Chunk, []byte) error { return nil }

		got := testing.AllocsPerRun(2, func() {
			r.Reset(data)
			if _, err := carve.Cut(r, 0, s, buf, true, emit); err != nil {
				t.Fatalf("Cut at target %d: %v", target, err)
			}
		})
		if got != 0 {
			t.Errorf("Cut over %d bytes at target %d: %v allocations, want 0", len(data), target, got)
		}
	}
}
