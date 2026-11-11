package platform

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
	want  string // "nil" or "eof"
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

		switch tc.want {
		case "nil":
			if err != nil {
				t.Errorf("{off %d, len %d}: got %v, want no error", tc.off, tc.n, err)
			}
		case "eof":
			if !errors.Is(err, io.EOF) {
				t.Errorf("{off %d, len %d}: got %v, want io.EOF", tc.off, tc.n, err)
			}
		}

		if !bytes.Equal(p[:n], want[tc.off:tc.off+int64(n)]) {
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

// TestDirectReadPreadErrorRaw checks directRead over a raw shaped like a raw
// pread(2) that answers -1 with its error, as x/sys's Pread does on a failing
// call: a good aligned call answered by a short aligned one, then a failing
// one, keeps the bytes the good calls read — the failed call's -1 is clamped
// to 0 rather than counted, which would drop read to the byte before the
// window's end — and the error itself is returned with them.
func TestDirectReadPreadErrorRaw(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()
	f := directFile(t, want)
	t.Cleanup(func() {
		_ = f.Close()
	})

	calls := 0
	p := make([]byte, 2*4096)
	n, err := directRead(p, 0, directAlign, func(buf []byte, off int64) (int, error) {
		calls++

		if calls == 1 {
			// The first aligned block, answered short of the window: a legal
			// raw answer at the alignment, which the loop resumes from.
			return f.ReadAt(buf[:4096], off)
		}

		// The resumed call fails as a plain pread answers failure.
		return -1, syscall.EIO
	})

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("directRead over a raw answering -1 with EIO: %d, %v; want the error", n, err)
	}

	if want := 4096; n != want {
		t.Fatalf(
			"directRead over a raw answering -1 with EIO: read %d bytes, want the %d the good call read",
			n,
			want,
		)
	}

	if !bytes.Equal(p[:n], want[:n]) {
		t.Fatalf("directRead over a raw answering -1 with EIO: got %v, want the file's bytes", p[:n])
	}
}

// TestReadAtDirectUnalignedRecord checks ReadAtDirect over ranges no raw
// direct read accepts as they are — the first and the last byte of a
// three-block file, and a record straddling a block boundary — against the
// bytes ReadAt returns.
func TestReadAtDirectUnalignedRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()
	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", want[:3*4096])

	for _, tc := range []struct {
		off int64
		n   int
	}{
		{0, 1},
		{3*4096 - 1, 1},
		{4096 - 20, 40},
	} {
		cached := make([]byte, tc.n)
		if n, err := f.ReadAt(cached, tc.off); n != tc.n || err != nil {
			t.Fatalf("ReadAt {off %d, len %d}: %d, %v", tc.off, tc.n, n, err)
		}

		direct := make([]byte, tc.n)
		n, err := f.ReadAtDirect(direct, tc.off)
		if n != tc.n || err != nil {
			t.Fatalf(
				"ReadAtDirect {off %d, len %d}: %d, %v; want %d and no error",
				tc.off, tc.n, n, err, tc.n,
			)
		}

		if !bytes.Equal(direct, cached) {
			t.Fatalf(
				"ReadAtDirect {off %d, len %d}: %v; want ReadAt's %v",
				tc.off, tc.n, direct, cached,
			)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectEndOfFile checks a read the file's end cuts short answers
// io.EOF as bare as ReadAt does — never an fs.PathError wrapping it, the
// shape os.File.ReadAt wraps only the errors that are not io.EOF — so a
// caller comparing err == io.EOF, as the format catalogue does, holds.
func TestReadAtDirectEndOfFile(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := directCheckFile(t, NewOS(t.TempDir()), nil, "a", []byte("record"))

	p := make([]byte, 6)
	n, err := f.ReadAtDirect(p, 3)

	if err != io.EOF {
		t.Fatalf("ReadAtDirect at the end: %d, %v (%T); want io.EOF bare", n, err, err)
	}

	if n != 3 || string(p[:n]) != "ord" {
		t.Fatalf("ReadAtDirect at the end: %d, %q; want the 3 bytes after 3 the file had", n, p[:n])
	}

	if n, err := f.ReadAtDirect(make([]byte, 1), 6); n != 0 || err != io.EOF {
		t.Fatalf("ReadAtDirect past the end: %d, %v (%T); want 0 and io.EOF bare", n, err, err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectAfterClose checks a closed file's ReadAtDirect fails with
// fs.ErrClosed, as its ReadAt does, before any call through the table, and
// does so even for a read of no byte.
func TestReadAtDirectAfterClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	r.take()

	for _, p := range [][]byte{make([]byte, 1), nil} {
		if n, err := f.ReadAtDirect(p, 0); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("ReadAtDirect of %d bytes after Close: %d, %v; want an error matching fs.ErrClosed", len(p), n, err)
		}
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("ReadAtDirect after Close made %v; want no call", log)
	}
}

// TestReadAtDirectNegativeOffset checks ReadAtDirect at a negative offset
// fails before any system call, with an *fs.PathError that names the file and
// wraps the platform's named error for it.
func TestReadAtDirectNegativeOffset(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	n, err := f.ReadAtDirect(make([]byte, 1), -1)

	var pe *fs.PathError
	if !errors.As(err, &pe) || !errors.Is(err, errNegativeOffset) || pe.Path != filepath.Join(dir, "a") {
		t.Fatalf("ReadAtDirect at -1: %v (%T); want an *fs.PathError naming the file and wrapping %v", err, err, errNegativeOffset)
	}

	if n != 0 {
		t.Fatalf("ReadAtDirect at -1: %d bytes, want 0", n)
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("ReadAtDirect at -1 made %v, want no call", log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectConcurrent checks concurrent ReadAtDirect calls on one
// file: every read returns the file's bytes, at the offsets the goroutines
// pick without coordination.
func TestReadAtDirectConcurrent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()
	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", want)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for off := int64(i * 100); off < int64(len(want)); off += 8192 {
				n := min(1024, len(want)-int(off))
				p := make([]byte, n)
				r, err := f.ReadAtDirect(p, off)
				if r != n || err != nil || !bytes.Equal(p, want[off:off+int64(n)]) {
					t.Errorf(
						"ReadAtDirect at %d: %d, %v; want %d and the file's bytes",
						off, r, err, n,
					)
					return
				}
			}
		})
	}
	wg.Wait()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
