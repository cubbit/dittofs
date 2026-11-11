package platform

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestReadAtDirectHandleIsNoBuffering checks ReadAtDirect reads through a
// second handle of the file opened with FILE_FLAG_NO_BUFFERING: the table's
// log shows one createFile of the file's path, with read access alone, sharing
// reads, writes and deletes — FILE_SHARE_DELETE included, so the handle holds
// a file nothing else renames or removes — OPEN_EXISTING, and no attribute
// but FILE_FLAG_NO_BUFFERING, and a readFile on the handle it returned, of
// the window's own size at the offset its overlapped names, returning the
// file's synced bytes. It does not ask a direct read to diverge from the page
// cache over another handle's buffered write: FILE_FLAG_NO_BUFFERING reads
// only what writes through caching handles write back, so both reads answer
// the same. The test's bytes are one window wide, so the table's log is one
// readFile alone after the open.
func TestReadAtDirectHandleIsNoBuffering(t *testing.T) {
	prof.Watch(t, prof.Options{})

	synced := bytes.Repeat([]byte("A"), 4096)

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", synced)

	direct := make([]byte, 4096)
	if n, err := f.ReadAtDirect(direct, 4096); n != 4096 || err != nil {
		t.Fatalf("ReadAtDirect: %d, %v; want 4096 and no error", n, err)
	}

	if !bytes.Equal(direct, synced) {
		t.Fatalf("ReadAtDirect: got %q, want the file's bytes %q", direct[:8], synced[:8])
	}

	log := r.take()
	if len(log) != 2 || log[0].op != "createFile" || log[1].op != "readFile" {
		t.Fatalf("want one createFile and one readFile in the table's log, got %v", log)
	}

	open, read := log[0], log[1]

	if open.path != filepath.Join(dir, "a") {
		t.Errorf("%s: want the file's own path opened", open)
	}

	if open.access != windows.GENERIC_READ {
		t.Errorf("%s: want read access alone, which a reader asks no more of", open)
	}

	for _, bit := range []struct {
		name string
		bit  uint32
	}{
		{"FILE_SHARE_READ", windows.FILE_SHARE_READ},
		{"FILE_SHARE_WRITE", windows.FILE_SHARE_WRITE},
		{"FILE_SHARE_DELETE", windows.FILE_SHARE_DELETE},
	} {
		if open.share&bit.bit == 0 {
			t.Errorf("%s: want %s, so the handle holds a file nothing else renames or removes", open, bit.name)
		}
	}

	if open.createmode != windows.OPEN_EXISTING {
		t.Errorf("%s: want OPEN_EXISTING, never CREATE_*: a direct read creates nothing", open)
	}

	if open.attrs != windows.FILE_FLAG_NO_BUFFERING {
		t.Errorf("%s: want FILE_FLAG_NO_BUFFERING alone, the one thing the handle exists for", open)
	}

	if open.handle == handleOf(t, f) {
		t.Errorf("%s: a second open returned the file's own handle %#x", open, handleOf(t, f))
	}

	if read.handle != open.handle || read.off != 4096 || read.n != 4096 {
		t.Errorf("%s: want a readFile of 4096 bytes at 4096 on the handle %s opened", read, open)
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

// TestReadAtDirectRefusedByFilesystem checks a filesystem that refuses the
// direct open hands the refusal back as itself — never a cached read, which
// would hand the tail check after a crash a proof it does not have: the error
// matches the code Windows returned, in an *fs.PathError naming the path,
// and no byte of the file's comes back with it. The next ReadAtDirect makes
// the open again and reads the file's bytes: nothing keeps the refused file
// answered as one whose direct handle once failed to open, since the file
// never held one. The refusal is injected at the open, since no supported
// filesystem of Windows refuses FILE_FLAG_NO_BUFFERING on a regular file in
// a directory a test can make.
func TestReadAtDirectRefusedByFilesystem(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))
	r.failNext("createFile", windows.ERROR_ACCESS_DENIED)

	p := make([]byte, 6)
	n, err := f.ReadAtDirect(p, 0)
	log := r.take()

	if got := ops(log); len(got) != 1 || got[0] != "createFile" {
		t.Fatalf("the refused ReadAtDirect made %v, want the open alone, never a cached read", log)
	}

	expectPathError(t, err, windows.ERROR_ACCESS_DENIED, filepath.Join(dir, "a"))

	if n != 0 {
		t.Fatalf("ReadAtDirect: %d bytes with the error, want 0", n)
	}

	if !bytes.Equal(p, make([]byte, 6)) {
		t.Fatalf("ReadAtDirect: %q; want no cached read into p", p)
	}

	if n, err := f.ReadAtDirect(p, 0); n != 6 || err != nil || string(p) != "record" {
		t.Fatalf(
			"second ReadAtDirect: %d, %v, %q; want the open made again, answering the file's bytes",
			n, err, p,
		)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectEndOfFile checks a read the file's end cuts short answers
// io.EOF as bare as ReadAt does — never an fs.PathError wrapping it, the
// shape ReadAt wraps only the errors that are not io.EOF — so a caller
// comparing err == io.EOF, as the format catalogue does, holds. The second
// block of the window reads past the file's end, where Windows answers
// ERROR_HANDLE_EOF, which the read maps to the same io.EOF.
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
// fs.ErrClosed, as its ReadAt does, before any call through the table.
func TestReadAtDirectAfterClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if n, err := f.ReadAtDirect(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("ReadAtDirect after Close: %d, %v; want an error matching fs.ErrClosed", n, err)
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("ReadAtDirect after Close made %v; want no call", log)
	}
}

// TestReadAtDirectNegativeOffset checks ReadAtDirect at a negative offset
// fails before any system call, with the error ReadAt's own shape gives it.
func TestReadAtDirectNegativeOffset(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := directCheckFile(t, newOS(dir, r.table()), r, "a", []byte("record"))

	n, err := f.ReadAtDirect(make([]byte, 1), -1)

	expectPathError(t, err, windows.ERROR_INVALID_PARAMETER, filepath.Join(dir, "a"))

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
// file, every read returning the file's bytes, at the offsets the goroutines
// pick without coordination, over the one handle directMu opens once for the
// file: without the mu the reads would race the open, or the window of one
// would read past a Close's release of the handle.
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

// TestReadAtDirectOpensOnce checks the first ReadAtDirect opens the direct
// handle and the reads after it read on that one: one createFile of the
// file's path with FILE_FLAG_NO_BUFFERING, and readFile calls alone after it,
// each on the handle the open returned.
func TestReadAtDirectOpensOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))

	for range 2 {
		p := make([]byte, 6)
		if n, err := f.ReadAtDirect(p, 0); n != 6 || err != nil || string(p) != "record" {
			t.Fatalf("ReadAtDirect: %d, %v, %q; want 6, nil, %q", n, err, p, "record")
		}
	}

	log := r.take()
	opens := 0
	for _, c := range log {
		if c.op == "createFile" {
			opens++

			if c.path != filepath.Join(dir, "a") {
				t.Errorf("%s: want the file's own path opened", c)
			}

			if c.attrs != windows.FILE_FLAG_NO_BUFFERING {
				t.Errorf("%s: want the open with FILE_FLAG_NO_BUFFERING", c)
			}
		}
	}

	if opens != 1 {
		t.Fatalf("%d opens in %v; want exactly one", opens, log)
	}

	reads := 0
	for _, c := range log {
		if c.op == "readFile" {
			reads++

			if c.handle != log[0].handle {
				t.Errorf("%s: want the reads on the handle the open returned %#x", c, log[0].handle)
			}
		}
	}

	if len(log) != opens+reads {
		t.Fatalf("calls in %v; want one open and the readFile calls alone", log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectCloseClosesHandle checks Close closes the direct handle
// through the table: after a ReadAtDirect, Close's log is exactly one
// closeHandle of the handle the open returned, and a closed file's Close —
// the second on the same file — makes none, so the direct handle is closed
// once and never twice.
func TestReadAtDirectCloseClosesHandle(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))

	if _, err := f.ReadAtDirect(make([]byte, 1), 0); err != nil {
		t.Fatalf("ReadAtDirect: %v", err)
	}

	h := r.take()[0].handle

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	log := r.take()
	if len(log) != 1 || log[0].op != "closeHandle" || log[0].handle != h {
		t.Fatalf("Close made %v; want exactly one closeHandle of the direct handle %#x", log, h)
	}
}

// TestReadAtDirectCloseError checks Close reports the direct handle's close
// failure through the table, so a handle that failed to close is not lost
// silently.
func TestReadAtDirectCloseError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))

	if _, err := f.ReadAtDirect(make([]byte, 1), 0); err != nil {
		t.Fatalf("ReadAtDirect: %v", err)
	}

	r.failNext("closeHandle", windows.ERROR_INVALID_HANDLE)
	if err := f.Close(); !hasPathError(err, windows.ERROR_INVALID_HANDLE, filepath.Join(dir, "a")) {
		t.Fatalf("Close with a failed close of the direct handle: %v; want its error reported", err)
	}
}
