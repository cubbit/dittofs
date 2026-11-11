package platform

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"golang.org/x/sys/unix"
)

// TestReadAtDirectSetsNoCache checks ReadAtDirect reads on the file's own
// descriptor with F_NOCACHE set around the read, over the aligned window
// directRead builds: the table's log shows fcntl(F_NOCACHE, 1), preads on the
// file's descriptor inside the read, and fcntl(F_NOCACHE, 0) after them, whose
// descriptor is the file's own. F_NOCACHE is advisory, so the test does not
// ask the reads to diverge from the page cache, the way the Linux one asks
// O_DIRECT to; the table's log is what pins the calls.
func TestReadAtDirectSetsNoCache(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()[:3*4096]
	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", want)

	direct := make([]byte, 4096)
	if n, err := f.ReadAtDirect(direct, 4096); n != 4096 || err != nil ||
		string(direct) != string(want[4096:2*4096]) {
		t.Fatalf("ReadAtDirect at 4096: %d, %v; want 4096 and the file's bytes", n, err)
	}

	log := r.take()
	if len(log) != 3 || log[0].op != "fcntl" || log[1].op != "pread" || log[2].op != "fcntl" {
		t.Fatalf("want fcntl, one pread and fcntl, got %v", log)
	}

	for _, c := range []struct {
		c    call
		want int
	}{
		{log[0], 1},
		{log[2], 0},
	} {
		if c.c.cmd != unix.F_NOCACHE || c.c.arg != c.want {
			t.Errorf("%v: want fcntl(F_NOCACHE, %d)", c.c, c.want)
		}
	}

	if log[0].fd != log[1].fd || log[0].fd != log[2].fd {
		t.Errorf("%v, %v, %v: want the three calls on one descriptor", log[0], log[1], log[2])
	}

	if log[1].off != 4096 || log[1].asked != 4096 {
		t.Errorf("%v: want a pread of 4096 bytes at 4096", log[1])
	}

	dev, ino := identity(t, filepath.Join(dir, "a"))
	for _, c := range log {
		if c.dev != dev || c.ino != ino {
			t.Errorf("%v: want a call on a's descriptor (dev %d ino %d)", c, dev, ino)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
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

		if string(direct) != string(cached) {
			t.Fatalf(
				"ReadAtDirect {off %d, len %d}: %q; want ReadAt's %q",
				tc.off, tc.n, direct, cached,
			)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectRefusedByFilesystem checks a filesystem that refuses
// F_NOCACHE hands the refusal back as itself, never a cached read:
// errors.Is names the errno, and no byte of the file's comes back with it.
// The refusal is injected at the first call of the file's first ReadAtDirect,
// since no supported macOS filesystem refuses a plain fcntl on a regular
// file in a directory a test can make.
func TestReadAtDirectRefusedByFilesystem(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))
	r.failNext("fcntl", syscall.EOPNOTSUPP)

	p := make([]byte, 5)
	n, err := f.ReadAtDirect(p, 0)

	if !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("ReadAtDirect: %d, %v; want an error matching EOPNOTSUPP", n, err)
	}

	if n != 0 || bytes.Equal(p, []byte("recor")) {
		t.Fatalf("ReadAtDirect: %d, %q; want 0 bytes, no cached read into p", n, p)
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
// fs.ErrClosed, as its ReadAt does.
func TestReadAtDirectAfterClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", []byte("record"))

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if n, err := f.ReadAtDirect(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("ReadAtDirect after Close: %d, %v; want an error matching fs.ErrClosed", n, err)
	}
}

// TestReadAtDirectNegativeOffset checks ReadAtDirect at a negative offset
// fails before any system call, with the error ReadAt's own shape gives it:
// an *fs.PathError wrapping fs.ErrInvalid.
func TestReadAtDirectNegativeOffset(t *testing.T) {
	prof.Watch(t, prof.Options{})

	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", []byte("record"))
	t.Cleanup(func() {
		_ = f.Close()
	})

	n, err := f.ReadAtDirect(make([]byte, 1), -1)

	var pe *fs.PathError
	if !errors.As(err, &pe) || n != 0 {
		t.Fatalf("ReadAtDirect at -1: %d, %v; want an *fs.PathError", n, err)
	}
}

// TestReadAtDirectConcurrent checks concurrent ReadAtDirect calls on one
// file: every read returns the file's bytes, at the offsets the goroutines
// pick without coordination, and F_NOCACHE's set and clear on the shared
// descriptor stays paired under the file's direct mu.
func TestReadAtDirectConcurrent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()
	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", want)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			for off := int64(i * 100); off < int64(len(want)); off += 8192 {
				n := min(1024, len(want)-int(off))
				p := make([]byte, n)
				r, err := f.ReadAtDirect(p, off)
				if r != n || err != nil || string(p) != string(want[off:off+int64(n)]) {
					t.Errorf(
						"ReadAtDirect at %d: %d, %v; want %d and the file's bytes",
						off, r, err, n,
					)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectEpochsPair checks every ReadAtDirect's F_NOCACHE set is
// closed by its clear: per read the first fcntl(F_NOCACHE, 1) and the last
// fcntl(F_NOCACHE, 0) leave the log with as many clears as sets, and no
// other F_NOCACHE call between reads — the calls the direct mu keeps in the
// file's program order.
func TestReadAtDirectEpochsPair(t *testing.T) {
	prof.Watch(t, prof.Options{})

	want := directBytes()[:4096]
	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", want)

	sets, clears := 0, 0
	other := 0
	for _, off := range []int64{0, 1, 100} {
		p := make([]byte, 10)
		if n, err := f.ReadAtDirect(p, off); n != 10 || err != nil ||
			string(p) != string(want[off:off+10]) {
			t.Fatalf("ReadAtDirect at %d: %d, %v; want 10 and the file's bytes", off, n, err)
		}

		log := r.take()

		var seen [3]bool // set, pread, clear, each seen and in this order
		var past bool
		for _, c := range log {
			if past {
				t.Fatalf("a call past the clear in the read at %d: %v", off, log)
			}

			switch {
			case c.op == "fcntl" && c.cmd == unix.F_NOCACHE && c.arg == 1:
				sets++
				seen[0] = true
			case c.op == "pread":
				if !seen[0] {
					t.Fatalf("a pread before F_NOCACHE was set, read at %d: %v", off, log)
				}

				seen[1] = true
			case c.op == "fcntl" && c.cmd == unix.F_NOCACHE && c.arg == 0:
				if !seen[1] {
					t.Fatalf("F_NOCACHE cleared with no pread between, read at %d: %v", off, log)
				}

				clears++
				seen[2] = true
				past = true
			default:
				other++
			}
		}

		if !seen[0] || !seen[1] || !seen[2] {
			t.Fatalf("the read at %d made %v; want one F_NOCACHE set, its pread and its clear", off, log)
		}
	}

	if sets != 3 || clears != 3 || other != 0 {
		t.Fatalf("%d sets, %d clears, %d other calls; want 3 sets, 3 clears, no other", sets, clears, other)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectInterruptedSet checks a set of F_NOCACHE interrupted before
// it completed is made again, as the os package's own File.Sync repeats its
// interrupted fcntl: the read still comes back whole, and the set's repeated
// attempt is the one that held.
func TestReadAtDirectInterruptedSet(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	r.failNext("fcntl", syscall.EINTR)
	p := make([]byte, 6)
	if n, err := f.ReadAtDirect(p, 0); n != 6 || err != nil || string(p) != "record" {
		t.Fatalf("ReadAtDirect with its set interrupted: %d, %v, %q", n, err, p)
	}

	log := r.take()
	if n := countFnocache(log, 1); n != 2 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 1) in %v; want the interrupted set made again", n, log)
	}

	if n := countFnocache(log, 0); n != 1 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 0) in %v; want one clear", n, log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectInterruptedClear checks a clear of F_NOCACHE interrupted
// before it completed is made again too: the read's bytes come back whole,
// and the clear's repeated attempt is the one that held. The fault waits for
// the op's second call, so the set before the read runs clear and the one
// the interruption lands on is the clear the read ends with.
func TestReadAtDirectInterruptedClear(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	r.failNth("fcntl", 2, syscall.EINTR)
	p := make([]byte, 6)
	if n, err := f.ReadAtDirect(p, 0); n != 6 || err != nil || string(p) != "record" {
		t.Fatalf("ReadAtDirect with its clear interrupted: %d, %v, %q; want 6, no error and the file's bytes", n, err, p)
	}

	log := r.take()
	if n := countFnocache(log, 0); n != 2 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 0) in %v; want the interrupted clear made again", n, log)
	}

	if n := countFnocache(log, 1); n != 1 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 1) in %v; want one set", n, log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectFailedClear checks a clear that fails on an error no call
// is repeated for fails the read it belonged to, with the bytes read kept
// and the clear's error returned: F_NOCACHE stays set on the file's
// descriptor, so a success here would leave every later cached read of the
// file bypassing the cache unasked — the decision at readDirect. The fault
// waits for the op's second call, so the failing one is the clear, past the
// set and the read.
func TestReadAtDirectFailedClear(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	r.failNth("fcntl", 2, syscall.EIO)
	p := make([]byte, 6)
	n, err := f.ReadAtDirect(p, 0)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("ReadAtDirect with a failed clear: %d, %v; want an error matching EIO", n, err)
	}

	if n != 6 || string(p) != "record" {
		t.Fatalf("ReadAtDirect with a failed clear: %d, %q; want the 6 bytes read, kept with the error", n, p)
	}

	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "fcntl" {
		t.Fatalf("ReadAtDirect with a failed clear: %v, want an *fs.PathError of the fcntl", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectFailedClearAtEnd checks the failure the read's own io.EOF
// does not mask: a read the file's end cuts short, whose clear then fails,
// still fails the read with the clear's error — F_NOCACHE stays set on the
// shared descriptor, so an io.EOF returned instead would leave the caller
// reading a normal short read off a file whose every later cached read
// bypasses the cache unasked.
func TestReadAtDirectFailedClearAtEnd(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	r.failNth("fcntl", 2, syscall.EIO)
	p := make([]byte, 9)
	n, err := f.ReadAtDirect(p, 3)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %d, %v; want an error matching EIO", n, err)
	}

	if err == io.EOF {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %d, io.EOF; want the clear's EIO, not the read's end", n)
	}

	if n != 3 || string(p[:n]) != "ord" {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %d, %q; want the 3 bytes read, kept with the error", n, p[:n])
	}

	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "fcntl" {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %v, want an *fs.PathError of the fcntl", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
func countFnocache(log []call, arg int) int {
	n := 0
	for _, c := range log {
		if c.op == "fcntl" && c.cmd == unix.F_NOCACHE && c.arg == arg {
			n++
		}
	}

	return n
}

// TestReadAtDirectCloseCallsNothing checks Close makes no call through the
// table, and that a file whose ReadAtDirect ran closes clean, so an F_NOCACHE
// set with no clear does not leak past the file's life.
func TestReadAtDirectCloseCallsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	if _, err := f.ReadAtDirect(make([]byte, 6), 0); err != nil {
		t.Fatalf("ReadAtDirect: %v", err)
	}

	if log := r.take(); len(log) == 0 || countFnocache(log, 0) == 0 {
		t.Fatalf("ReadAtDirect made no clear of F_NOCACHE: %v", log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("Close made %v; want no calls through the table", log)
	}
}
