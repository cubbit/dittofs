package platform

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

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

// directCheckFile creates in o's directory the file name holding b and
// returns it as a file of o, drained of its table's log: the Create, WriteAt
// and Sync the helper makes the storage's own calls, not the direct read's.
// r may be nil for an o built on the real table.
func directCheckFile(t *testing.T, o *OS, r *recorder, name string, b []byte) *file {
	t.Helper()

	f, err := o.Create(name)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}

	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("WriteAt %s: %v", name, err)
	}

	if err := f.Sync(); err != nil {
		t.Fatalf("Sync %s: %v", name, err)
	}

	if r != nil {
		r.take()
	}

	return f.(*file)
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

// TestDirectReadShortRaw checks directRead over a raw that reports a short,
// error-free read mid-window: a wrapper over the real file's ReadAt, capped at
// limit bytes per call, which also asserts raw's own contract — an aligned
// offset, buffer address and length for every call of a window, the arguments
// no raw accepts any other way. A cap of 4096 is a multiple of the alignment,
// so a short read is resumed where it stopped and every case reads whole; a
// cap of 1000 is not, so the count lands off the alignment and the window must
// end at the bytes the raw gave, mapped as the end of the file a short pread
// answers.
func TestDirectReadShortRaw(t *testing.T) {
	prof.Watch(t, prof.Options{})
	want := directBytes()
	f := directFile(t, want)
	t.Cleanup(func() {
		_ = f.Close()
	})

	for _, limit := range []int{4096, 1000} {
		t.Run(fmt.Sprintf("cap%d", limit), func(t *testing.T) {
			raw := func(buf []byte, off int64) (int, error) {
				if off >= 0 && (off%directAlign != 0 || int64(len(buf))%directAlign != 0 ||
					uintptr(unsafe.Pointer(unsafe.SliceData(buf)))&uintptr(directAlign-1) != 0) {
					t.Errorf(
						"raw called at %d for %d bytes: not window-aligned",
						off, len(buf),
					)
				}

				if len(buf) > limit {
					buf = buf[:limit]
				}

				return f.ReadAt(buf, off)
			}

			if limit == int(directAlign) {
				checkDirectRead(t, want, raw)
				return
			}

			// A cap off the alignment ends the first window block at the
			// bytes the raw gave: the byte at 999 answers, the byte at 1000 is
			// past what the raw delivered, and every read crossing it is
			// cut short with io.EOF.
			for _, tc := range []struct {
				off int64
				n   int

				wantN int    // the bytes read
				want  string // "nil" or "eof"
			}{
				{off: 0, n: 1, wantN: 1, want: "nil"},
				{off: 999, n: 1, wantN: 1, want: "nil"},
				{off: 1000, n: 1, wantN: 0, want: "eof"},
				{off: 1, n: 4095, wantN: 999, want: "eof"},
				{off: 0, n: 2000, wantN: 1000, want: "eof"},
				{off: 4097, n: 1, wantN: 1, want: "nil"},
				{off: 8196, n: 1, wantN: 1, want: "nil"},
			} {
				p := make([]byte, tc.n)
				n, err := directRead(p, tc.off, directAlign, raw)

				if n != tc.wantN {
					t.Errorf("{off %d, len %d}: got n %d, want %d", tc.off, tc.n, n, tc.wantN)
					continue
				}

				if tc.want == "nil" && err != nil {
					t.Errorf("{off %d, len %d}: got %v, want no error", tc.off, tc.n, err)
				} else if tc.want == "eof" && !errors.Is(err, io.EOF) {
					t.Errorf("{off %d, len %d}: got %v, want io.EOF", tc.off, tc.n, err)
				}

				if !bytes.Equal(p[:n], want[tc.off:tc.off+int64(n)]) {
					t.Errorf(
						"{off %d, len %d}: got %v, want the file's bytes %v",
						tc.off, tc.n, p[:n], want[tc.off:tc.off+int64(n)],
					)
				}
			}
		})
	}
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
