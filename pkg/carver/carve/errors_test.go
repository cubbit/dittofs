package carve_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"testing/iotest"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
)

// failAt is a fault wrapper around a real reader: it reads through r until it
// has served n bytes, returning the read that reaches n together with err,
// and err alone after that.
type failAt struct {
	r   io.Reader
	n   int64
	err error
}

func (f *failAt) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}

	k, err := f.r.Read(p)
	if f.n -= int64(k); f.n == 0 {
		return k, f.err
	}

	return k, err
}

// stall is a fault wrapper around a real reader: its first n calls return no
// bytes and no error, and later ones read through r.
type stall struct {
	r io.Reader
	n int
}

func (s *stall) Read(p []byte) (int, error) {
	if s.n > 0 {
		s.n--

		return 0, nil
	}

	return s.r.Read(p)
}

// errPlanted is compared by identity, not errors.Is: Cut returns an error
// from r or emit as it came, unwrapped.
var errPlanted = errors.New("planted")

// checkPrefix fails t unless got is a prefix of the clean run ref.
func checkPrefix(t *testing.T, name string, got, ref []carver.Chunk) {
	t.Helper()

	if len(got) > len(ref) {
		t.Fatalf("%s: %d chunks, more than the clean run's %d", name, len(got), len(ref))
	}
	for i, c := range got {
		if c != ref[i] {
			t.Fatalf("%s: chunk %d is %+v, want %+v, the clean run's", name, i, c, ref[i])
		}
	}
}

// checkClean fails t unless got is the clean run ref, chunk for chunk.
func checkClean(t *testing.T, name string, got, ref []carver.Chunk) {
	t.Helper()

	checkPrefix(t, name, got, ref)
	if len(got) != len(ref) {
		t.Fatalf("%s: %d chunks, want the clean run's %d", name, len(got), len(ref))
	}
}

// emit failing at the third chunk stops Cut with that error, unwrapped, after
// two chunks; consumed counts those two and not the refused one, so a retry
// from consumed re-offers the refused chunk's bytes.
func TestCutStopsOnEmitsError(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 3, 8*s.Max())
	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())
	if len(ref) < 3 {
		t.Fatalf("the clean run cut %d chunks, want at least 3", len(ref))
	}

	var got []carver.Chunk
	consumed, err := carve.Cut(bytes.NewReader(data), 0, s, make([]byte, s.Max()), true, func(c carver.Chunk, _ []byte) error {
		got = append(got, c)
		if len(got) == 3 {
			return errPlanted
		}

		return nil
	})
	if err != errPlanted {
		t.Fatalf("Cut with emit failing at chunk 3: err %v, want the planted error itself", err)
	}
	if !slices.Equal(got, ref[:3]) {
		t.Fatalf("emit failing at chunk 3: emit saw %v, want the clean run's first 3, %v", got, ref[:3])
	}
	if want := int64(ref[0].Length) + int64(ref[1].Length); consumed != want {
		t.Fatalf("emit failing at chunk 3: consumed %d, want %d, the two chunks emit accepted", consumed, want)
	}
}

// A reader returning bytes with an error stops Cut before those bytes are cut,
// and the error comes back unwrapped: at the first byte, the first fill's last
// byte, the second fill's first and middle bytes, and the stretch's last byte.
// Every other offset is the fault sweep's. One-byte reads make the byte that comes with the error the
// last one served, so no chunk may reach it.
func TestCutStopsBeforeBytesThatCameWithAnError(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 5, 4*s.Max())
	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())

	for _, n := range []int64{1, int64(s.Max()), int64(s.Max()) + 1, int64(s.Max() + s.Max()/2), int64(len(data))} {
		r := iotest.OneByteReader(&failAt{r: bytes.NewReader(data), n: n, err: errPlanted})
		got, err := cut(t, r, 0, s, s.Max())
		if err != errPlanted {
			t.Fatalf("a reader failing with byte %d: err %v, want the planted error itself", n-1, err)
		}
		checkPrefix(t, fmt.Sprintf("a reader failing with byte %d", n-1), got, ref)
		if len(got) > 0 {
			if last := got[len(got)-1]; last.Offset+int64(last.Length) > n-1 {
				t.Fatalf("a reader failing with byte %d: a chunk ends at %d, past the bytes served before the error", n-1, last.Offset+int64(last.Length))
			}
		}
	}
}

// Readers that are legal but awkward cut as a whole read does: one byte per
// call, half the room per call, and the last bytes handed over with io.EOF,
// which must be cut, not dropped.
func TestCutAwkwardReadersGiveTheCleanRun(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 9, 4*s.Max()+12345)
	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())

	for _, tc := range []struct {
		name string
		r    func(io.Reader) io.Reader
	}{
		{"OneByteReader", iotest.OneByteReader},
		{"HalfReader", iotest.HalfReader},
		{"DataErrReader", iotest.DataErrReader},
	} {
		checkClean(t, tc.name, mustCut(t, tc.r(bytes.NewReader(data)), 0, s, s.Max()), ref)
	}
}

// A reader that fails after its data, or at its second read, has its error
// returned as it came, never wrapped or taken for the end.
func TestCutReturnsTheReadersError(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 11, 3*s.Max())
	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())

	for _, tc := range []struct {
		name string
		r    io.Reader
		want error
	}{
		{"MultiReader then ErrReader", io.MultiReader(bytes.NewReader(data), iotest.ErrReader(errPlanted)), errPlanted},
		{"TimeoutReader", iotest.TimeoutReader(bytes.NewReader(data)), iotest.ErrTimeout},
	} {
		got, err := cut(t, tc.r, 0, s, s.Max())
		if err != tc.want {
			t.Errorf("%s: err %v, want %v itself", tc.name, err, tc.want)
		}
		checkPrefix(t, tc.name, got, ref)
	}
}

// A reader returning no bytes and no error is not the end: 99 such reads in a
// row and then its data give the clean run, and so do 99 before every piece
// of a stretch served in pieces that straddle the refills, since a read with
// bytes ends the run; 100 in a row fail Cut with io.ErrNoProgress rather than
// spin on it.
func TestCutBoundsEmptyReads(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 13, 3*s.Max())
	ref := mustCut(t, bytes.NewReader(data), 0, s, s.Max())

	checkClean(t, "99 empty reads", mustCut(t, &stall{r: bytes.NewReader(data), n: 99}, 0, s, s.Max()), ref)

	var pieces []io.Reader
	for p := data; len(p) > 0; {
		k := min(len(p), 10007)
		pieces = append(pieces, &stall{r: bytes.NewReader(p[:k]), n: 99})
		p = p[k:]
	}
	checkClean(t, fmt.Sprintf("99 empty reads before each of %d pieces", len(pieces)), mustCut(t, io.MultiReader(pieces...), 0, s, s.Max()), ref)

	got, err := cut(t, &stall{r: bytes.NewReader(data), n: 100}, 0, s, s.Max())
	if err != io.ErrNoProgress || len(got) != 0 {
		t.Errorf("100 empty reads: %d chunks and %v, want none and io.ErrNoProgress", len(got), err)
	}
}
