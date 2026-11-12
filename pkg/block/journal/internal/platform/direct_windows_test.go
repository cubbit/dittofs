package platform

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/cubbit/dittofs/internal/prof"
)

// TestReadAtDirectHandleIsNoBuffering checks ReadAtDirect reads through a
// second handle of the file opened with FILE_FLAG_NO_BUFFERING: the table's
// log shows one createFile of the file's path, with read access alone, sharing
// reads, writes and deletes — FILE_SHARE_DELETE included, without which this
// second handle would block the journal's own rename or remove of the file
// while it is open — OPEN_EXISTING, and no attribute but
// FILE_FLAG_NO_BUFFERING, and a readFile on the handle it returned, of the
// window's own size at the offset its overlapped names, returning the file's
// synced bytes. It does not ask a direct read to diverge from the page cache
// over another handle's buffered write: FILE_FLAG_NO_BUFFERING reads only
// what writes through caching handles write back, so both reads answer the
// same. The read asked for is one window wide — a block of a two-block file
// — so the table's log is one readFile alone after the open.
func TestReadAtDirectHandleIsNoBuffering(t *testing.T) {
	prof.Watch(t, prof.Options{})

	synced := bytes.Repeat([]byte("A"), 2*4096)

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", synced)

	direct := make([]byte, 4096)
	if n, err := f.ReadAtDirect(direct, 4096); n != 4096 || err != nil {
		t.Fatalf("ReadAtDirect at 4096: %d, %v; want the second block, 4096 and no error", n, err)
	}

	if !bytes.Equal(direct, synced[4096:]) {
		t.Fatalf(
			"ReadAtDirect at 4096: got %q, want the file's second block %q",
			direct[:8], synced[4096:4096+8],
		)
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
			t.Errorf(
				"%s: want %s, without which this second handle would block the journal's own rename or remove of the file",
				open,
				bit.name,
			)
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

// TestReadAtDirectHandleEOFAtBlockEnd is the EOF case the mapping in
// readDirectLocked's raw exists for, which every shorter file never reaches: a
// file whole blocks long, asked for a byte more than it holds, whose window
// spans a second block that starts at the file's end — where ReadFile can
// answer ERROR_HANDLE_EOF with no byte, the refusal the raw maps to io.EOF.
// The second raw call's answer is injected, since a synchronous handle may
// instead read it as zero bytes and no error, which directRead maps the same
// way; the first raw call runs for real, reading the file's one block whole
// as a partial count of a wider window. The read answers io.EOF bare with
// the 4096 bytes the file had, and the table's log holds the second readFile
// call at 4096 whose queued refusal it was. Delete the ERROR_HANDLE_EOF
// branch in readDirectLocked and its error reaches the caller wrapped in an
// *fs.PathError instead of io.EOF, and the test fails on its bare-io.EOF
// assertion.
func TestReadAtDirectHandleEOFAtBlockEnd(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", bytes.Repeat([]byte("A"), 4096))
	r.failNext("readFile", nil, windows.ERROR_HANDLE_EOF)

	p := make([]byte, 4097)
	n, err := f.ReadAtDirect(p, 0)

	if err != io.EOF {
		t.Fatalf("ReadAtDirect past the end: %d, %v (%T); want io.EOF bare", n, err, err)
	}

	if n != 4096 || !bytes.Equal(p[:4096], bytes.Repeat([]byte("A"), 4096)) {
		t.Fatalf(
			"ReadAtDirect past the end: read %d bytes, %q...; want the file's whole block",
			n,
			p[:min(8, n)],
		)
	}

	log := r.take()
	if len(log) != 3 || log[0].op != "createFile" || log[1].op != "readFile" || log[2].op != "readFile" {
		t.Fatalf("want the direct open and two readFile calls in the table's log, got %v", log)
	}

	if log[1].off != 0 || log[1].n != 8192 {
		t.Fatalf("%s: want the first read of the whole two-block window at 0", log[1])
	}

	if log[2].off != 4096 || log[2].n != 4096 || log[2].handle != log[1].handle {
		t.Fatalf(
			"%s: want the second readFile of the window's last block at the file's end, on the first's handle",
			log[2],
		)
	}

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
// closeHandle of the handle the open returned, and after it the file is
// marked closed, so a second Close — its own file handle is already gone, an
// error its os answer carries and nothing here cares about — makes no
// further closeHandle, and the direct handle is closed once and never twice.
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

	_ = f.Close()

	if log := r.take(); len(log) != 0 {
		t.Fatalf("the second Close made %v; want no call, the direct handle is closed once", log)
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
