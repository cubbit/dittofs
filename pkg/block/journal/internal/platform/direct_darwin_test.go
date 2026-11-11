package platform

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
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
// file bypassing the cache unasked — the decision at readDirectLocked. The fault
// waits for the op's second call, so the failing one is the clear, past the
// set and the read.
func TestReadAtDirectFailedClear(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	r.failNth("fcntl", 2, syscall.EIO)
	p := make([]byte, 6)
	n, err := f.ReadAtDirect(p, 0)

	expectPathError(t, err, "fcntl", syscall.EIO, filepath.Join(dir, "a"))

	if n != 6 || string(p) != "record" {
		t.Fatalf("ReadAtDirect with a failed clear: %d, %q; want the 6 bytes read, kept with the error", n, p)
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

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	r.failNth("fcntl", 2, syscall.EIO)
	p := make([]byte, 9)
	n, err := f.ReadAtDirect(p, 3)

	expectPathError(t, err, "fcntl", syscall.EIO, filepath.Join(dir, "a"))

	if err == io.EOF {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %d, io.EOF; want the clear's EIO, not the read's end", n)
	}

	if n != 3 || string(p[:n]) != "ord" {
		t.Fatalf("ReadAtDirect with a failed clear at the end: %d, %q; want the 3 bytes read, kept with the error", n, p[:n])
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectInterruptedRead checks a pread interrupted before it read
// anything is made again, as the os package's own ReadAt repeats its
// interrupted pread and Linux's readDirectLocked does: the read still comes back
// whole, and the repeated pread is the one that delivered it.
func TestReadAtDirectInterruptedRead(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	f := directCheckFile(t, newOS(t.TempDir(), r.table()), r, "a", []byte("record"))

	r.failNext("pread", syscall.EINTR)
	p := make([]byte, 6)
	if n, err := f.ReadAtDirect(p, 0); n != 6 || err != nil || string(p) != "record" {
		t.Fatalf("ReadAtDirect with its pread interrupted: %d, %v, %q; want 6, no error and the file's bytes", n, err, p)
	}

	log := r.take()
	preads := 0
	for _, c := range log {
		if c.op == "pread" {
			preads++
		}
	}

	if preads != 2 {
		t.Fatalf("got %v; want the interrupted pread made again", log)
	}

	if n := countFnocache(log, 1); n != 1 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 1) in %v; want one set", n, log)
	}

	if n := countFnocache(log, 0); n != 1 {
		t.Fatalf("got %d calls of fcntl(F_NOCACHE, 0) in %v; want one clear", n, log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// countFnocache returns the calls of fcntl with F_NOCACHE and arg of log.
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

// TestReadAtDirectFailedReadAndClear checks a read that fails on its own and
// whose F_NOCACHE clear then fails keeps both errors, the read's first: the
// clear's failure no longer swallows a failed read's, which the caller would
// read as a clean refusal of the cache and lose the read's own reason. The
// clear's error alone still outranks even the read's io.EOF, as
// TestReadAtDirectFailedClearAtEnd checks.
func TestReadAtDirectFailedReadAndClear(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	// The read's own failure first, then the clear's: the fault on pread
	// fires at its only call, the one on fcntl at its second, the clear the
	// read ends with.
	r.failNext("pread", syscall.EIO)
	r.failNth("fcntl", 2, syscall.EIO)

	p := make([]byte, 6)
	n, err := f.ReadAtDirect(p, 0)

	if n != 0 {
		t.Fatalf("ReadAtDirect of a failed read: read %d bytes, want 0", n)
	}

	if !hasPathError(err, syscall.EIO) {
		t.Fatalf("ReadAtDirect of a failed read: %v; want the read's EIO kept", err)
	}

	j, ok := err.(interface{ Unwrap() []error })
	if !ok || len(j.Unwrap()) != 2 {
		t.Fatalf(
			"ReadAtDirect of a failed read: %v (%T); want both the read's and the clear's errors joined",
			err,
			err,
		)
	}

	for _, e := range j.Unwrap() {
		pe, ok := e.(*fs.PathError)
		if !ok || !errors.Is(pe.Err, syscall.EIO) || pe.Path != filepath.Join(dir, "a") {
			t.Fatalf("ReadAtDirect of a failed read: %v; want each error an *fs.PathError of EIO naming the file", err)
		}
	}

	if pe := j.Unwrap()[0].(*fs.PathError); pe.Op != "pread" {
		t.Fatalf("ReadAtDirect of a failed read: %v; want the read's error first", err)
	}

	if pe := j.Unwrap()[1].(*fs.PathError); pe.Op != "fcntl" {
		t.Fatalf("ReadAtDirect of a failed read: %v; want the clear's error second", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
