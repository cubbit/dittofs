package carve_test

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
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

// decision: the fault sweep's size is set by this flag, never by
// testing.Short(): unit passes -short only on pull requests, so a sweep keyed
// on it would run at full size under -race after every merge and time out.
// Without the flag the sweep fails the reader one byte before, at and after
// every chunk end and every refill, and at 256 seeded offsets; with it, at
// every offset of the stretch, which only a nightly run affords.
var sweepFull = flag.Bool("carver.sweep-full", false, "run the carver's fault sweep at every byte offset")

// sweepCut is one Cut over r at base 0 through buf that fails emit at chunk
// failAtChunk, counted from 1 (0: never), checking that each chunk handed to
// emit holds data's bytes at its offset.
func sweepCut(t *testing.T, data []byte, r io.Reader, s carver.Settings, buf []byte, failAtChunk int) ([]carver.Chunk, int64, error) {
	t.Helper()

	var got []carver.Chunk
	consumed, err := carve.Cut(r, 0, s, buf, true, func(c carver.Chunk, p []byte) error {
		if end := c.Offset + int64(c.Length); c.Offset < 0 || end > int64(len(data)) || !bytes.Equal(p, data[c.Offset:end]) {
			t.Fatalf("chunk %+v: emit was handed bytes other than the stretch's at its offset", c)
		}
		got = append(got, c)
		if len(got) == failAtChunk {
			return errPlanted
		}

		return nil
	})

	return got, consumed, err
}

// sweepOffsets is where the reduced sweep fails the reader: one byte before,
// at and after every chunk end of ref and every refill point, and 256 offsets
// from a fixed seed, within [0, n]. Through a buffer of Max bytes Cut refills
// before every chunk, reading until Max bytes past the chunk's start are held,
// so the refill points are each chunk's start plus Max.
func sweepOffsets(ref []carver.Chunk, s carver.Settings, n int64) []int64 {
	var at []int64
	for _, c := range ref {
		at = append(at, c.Offset+int64(c.Length), c.Offset+int64(s.Max()))
	}

	var out []int64
	for _, a := range at {
		for d := int64(-1); d <= 1; d++ {
			if a+d >= 0 && a+d <= n {
				out = append(out, a+d)
			}
		}
	}
	rng := rand.New(rand.NewPCG(19, 23))
	for range 256 {
		out = append(out, rng.Int64N(n+1))
	}
	slices.Sort(out)

	return slices.Compact(out)
}

// The fault sweep of RFC 2 §10.3, over 640 KiB of Random at the smallest
// Target through a buffer of Max bytes: about ten chunks and ten refills.
// The reader fails having served n bytes, the last of them handed over with
// the error, for each n of sweepOffsets, or for every n with
// -carver.sweep-full. Cut must stop before the bytes that came with the
// error, so it delivers exactly the clean chunks whose fill completed before
// them, those starting more than Max bytes before n: a part-built chunk
// handed over short, a chunk cut from the bytes the error came with, or an
// error lost at the read that fills the buffer, fails at its offset. emit
// then fails at every chunk, and Cut delivers the clean run up to that chunk.
// After each failure a retry through the same buffer gives the clean run, so
// nothing a failed call leaves in the buffer changes a later cut.
func TestFaultSweep(t *testing.T) {
	s := settings64K(t, [32]byte{0: 1})
	data := carvetest.Bytes(carvetest.Random, 17, 640<<10)
	buf := make([]byte, s.Max())

	ref, _, err := sweepCut(t, data, bytes.NewReader(data), s, buf, 0)
	if err != nil || len(ref) < 8 {
		t.Fatalf("the clean run: %d chunks and %v, want at least 8 and no error", len(ref), err)
	}

	retry := func(name string) {
		t.Helper()
		got, _, err := sweepCut(t, data, bytes.NewReader(data), s, buf, 0)
		if err != nil {
			t.Fatalf("%s, the retry: %v", name, err)
		}
		checkClean(t, name+", the retry", got, ref)
	}

	offsets := sweepOffsets(ref, s, int64(len(data)))
	if *sweepFull {
		offsets = offsets[:0]
		for n := range int64(len(data)) + 1 {
			offsets = append(offsets, n)
		}
	}

	for _, n := range offsets {
		name := fmt.Sprintf("the reader failing having served %d bytes", n)
		got, _, err := sweepCut(t, data, &failAt{r: bytes.NewReader(data), n: n, err: errPlanted}, s, buf, 0)
		if err != errPlanted {
			t.Fatalf("%s: err %v, want the planted error itself", name, err)
		}
		want := 0
		for want < len(ref) && ref[want].Offset+int64(s.Max()) < n {
			want++
		}
		checkClean(t, name, got, ref[:want])
		retry(name)
	}

	for i := 1; i <= len(ref); i++ {
		name := fmt.Sprintf("emit failing at chunk %d", i)
		got, consumed, err := sweepCut(t, data, bytes.NewReader(data), s, buf, i)
		if err != errPlanted {
			t.Fatalf("%s: err %v, want the planted error itself", name, err)
		}
		checkClean(t, name, got, ref[:i])
		if want := ref[i-1].Offset; consumed != want {
			t.Fatalf("%s: consumed %d, want %d, the chunks emit accepted", name, consumed, want)
		}
		retry(name)
	}
	t.Logf("%d reader failures and %d emit failures, each with a retry", len(offsets), len(ref))
}
