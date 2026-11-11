package platform

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// directReadCases are the reads directRead must answer as ReadAt does: the
// file holds 5*4096+100 bytes, and the offsets straddle a block boundary,
// alone and in pairs, and read at and past the end.
var directReadCases = []struct {
	off int64
	n   int

	wantN int    // the bytes read
	want  string // "nil", "eof" or "error", which the case only names, as ReadAt does
}{
	{off: 0, n: 1, wantN: 1, want: "nil"},
	{off: 1, n: 4095, wantN: 4095, want: "nil"},
	{off: 4095, n: 2, wantN: 2, want: "nil"},
	{off: 4096, n: 4096, wantN: 4096, want: "nil"},
	{off: 5 * 4096, n: 100, wantN: 100, want: "nil"},
	{off: 5*4096 + 99, n: 1, wantN: 1, want: "nil"},
	{off: 5*4096 + 50, n: 200, wantN: 50, want: "eof"},
	{off: 5*4096 + 100, n: 1, wantN: 0, want: "eof"},
	{off: 0, n: 0, wantN: 0, want: "nil"},
	{off: -1, n: 1, wantN: 0, want: "error"},
}

// directBytes returns the contents the direct-read cases read from: 5*4096+100
// pseudo-random bytes, so no slice of the file equals another and a wrong copy
// does not read as correct.
func directBytes() []byte {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, 5*4096+100)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}

	return b
}

// directFile returns a file in a t.TempDir() directory holding want.
func directFile(t *testing.T, want []byte) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}

	return f
}

// checkDirectRead answers every case against raw, which reads into buf the
// bytes at off.
func checkDirectRead(t *testing.T, want []byte, raw func(buf []byte, off int64) (int, error)) {
	t.Helper()

	for _, tc := range directReadCases {
		p := make([]byte, tc.n)
		n, err := directRead(p, tc.off, directAlign, raw)

		if n != tc.wantN {
			t.Errorf("{off %d, len %d}: got n %d, want %d", tc.off, tc.n, n, tc.wantN)
			continue
		}

		switch {
		case tc.want == "nil":
			if err != nil {
				t.Errorf("{off %d, len %d}: got %v, want no error", tc.off, tc.n, err)
			}
		case tc.want == "eof":
			if !errors.Is(err, io.EOF) {
				t.Errorf("{off %d, len %d}: got %v, want io.EOF", tc.off, tc.n, err)
			}
		default:
			// The read at the negative offset, whose error ReadAt also leaves
			// unnamed.
			if err == nil {
				t.Errorf("{off %d, len %d}: the read succeeded, want an error", tc.off, tc.n)
			}
		}

		if tc.off >= 0 && !bytes.Equal(p[:n], want[tc.off:tc.off+int64(n)]) {
			t.Errorf(
				"{off %d, len %d}: got %v, want the file's bytes %v",
				tc.off, tc.n, p[:n], want[tc.off:tc.off+int64(n)],
			)
		}
	}
}

// TestDirectRead checks directRead answers as ReadAt does, raw reading into
// any buffer ReadAt accepts, with no alignment of its own.
func TestDirectRead(t *testing.T) {
	prof.Watch(t, prof.Options{})
	want := directBytes()
	f := directFile(t, want)
	t.Cleanup(func() {
		_ = f.Close()
	})

	checkDirectRead(t, want, f.ReadAt)
}

// TestDirectReadPreadRaw checks directRead over a raw shaped like a raw
// pread(2), which answers 0 and no error at the end of the file and EOF never:
// the aligned window's read crosses the file's end without one.
func TestDirectReadPreadRaw(t *testing.T) {
	prof.Watch(t, prof.Options{})
	want := directBytes()
	f := directFile(t, want)
	t.Cleanup(func() {
		_ = f.Close()
	})

	size := int64(len(want))
	checkDirectRead(t, want, func(buf []byte, off int64) (int, error) {
		if off >= size {
			return 0, nil
		}

		if off+int64(len(buf)) > size {
			buf = buf[:size-off]
		}

		return f.ReadAt(buf, off)
	})
}
