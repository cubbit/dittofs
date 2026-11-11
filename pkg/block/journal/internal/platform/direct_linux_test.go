package platform

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"golang.org/x/sys/unix"
)

// TestReadAtDirectDescriptorIsDirect checks ReadAtDirect reads through a
// descriptor of the file reopened with O_DIRECT: the table's log shows one
// open of /proc/self/fd/N, read-only, close-on-exec and O_DIRECT, whose
// descriptor is the file's own, and a read on it returning the file's synced
// bytes. It does not ask a direct read to diverge from the page cache over
// another descriptor's buffered write: that write is written back before the
// direct read runs, so both reads answer the same. The test's bytes are one
// window wide, so the table's log is one pread alone after the open.
func TestReadAtDirectDescriptorIsDirect(t *testing.T) {
	prof.Watch(t, prof.Options{})

	synced := bytes.Repeat([]byte("A"), 4096)

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", synced)

	direct := make([]byte, 4096)
	if n, err := f.ReadAtDirect(direct, 0); n != 4096 || err != nil {
		t.Fatalf("ReadAtDirect: %d, %v; want 4096 and no error", n, err)
	}

	if !bytes.Equal(direct, synced) {
		t.Fatalf("ReadAtDirect: got %q, want the file's bytes %q", direct[:8], synced[:8])
	}

	log := r.take()
	if len(log) != 2 || log[0].op != "open" || log[1].op != "pread" {
		t.Fatalf("want one open and one pread in the table's log, got %v", log)
	}

	opened := log[0]
	if !strings.HasPrefix(opened.path, procSelfFD) {
		t.Errorf("%s: want an open under %s", opened, procSelfFD)
	}

	if opened.mode&directOpenFlags != directOpenFlags {
		t.Errorf("%s: want the open %#x with O_DIRECT", opened, directOpenFlags)
	}

	if log[1].fd != opened.fd || log[1].off != 0 || log[1].asked != 4096 {
		t.Errorf("%s: want a pread of 4096 bytes at 0 on %s's descriptor", log[1], opened)
	}

	dev, ino := identity(t, filepath.Join(dir, "a"))
	if log[1].dev != dev || log[1].ino != ino {
		t.Errorf("%s: the descriptor is not the file's (dev %d ino %d)", log[1], dev, ino)
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
// direct open — EINVAL of O_DIRECT, which tmpfs answered before the kernel
// learned to accept it — is handed back as itself: errors.Is names the errno,
// and no byte of the file's comes back with it. The refusal is injected at the
// open the file's first ReadAtDirect makes, the call the refusal surfaces at,
// since every live filesystem of this kernel accepts O_DIRECT on every
// directory a test can make one in.
func TestReadAtDirectRefusedByFilesystem(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))
	r.failNext("open", unix.EINVAL)

	p := make([]byte, 5)
	n, err := f.ReadAtDirect(p, 0)

	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("ReadAtDirect: %d, %v; want an error matching syscall.EINVAL", n, err)
	}

	if n != 0 || strings.Contains(string(p), "record") {
		t.Fatalf("ReadAtDirect: %d, %q; want 0 bytes, not the file's", n, p)
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
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

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
		}(i)
	}
	wg.Wait()

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadAtDirectOpensOnce checks the first ReadAtDirect opens the direct
// descriptor and the calls after it read on that one: one open of
// /proc/self/fd/N with O_DIRECT, and preads alone after it.
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
		if c.op == "open" {
			opens++
			if !strings.HasPrefix(c.path, procSelfFD) {
				t.Errorf("%s: want an open under %s", c, procSelfFD)
			}

			if c.mode&unix.O_DIRECT == 0 {
				t.Errorf("%s: want the open with O_DIRECT", c)
			}
		}
	}

	if opens != 1 {
		t.Fatalf("%d opens in %v; want exactly one", opens, log)
	}

	if len(log) != opens+countPreads(log) {
		t.Fatalf("calls in %v; want one open and the preads alone", log)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// countPreads returns the preads of a log.
func countPreads(log []call) int {
	n := 0
	for _, c := range log {
		if c.op == "pread" {
			n++
		}
	}

	return n
}

// TestReadAtDirectCloseError checks Close reports the direct descriptor's
// close failure through the table, so a descriptor that failed to close is
// not lost silently.
func TestReadAtDirectCloseError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	f := directCheckFile(t, o, r, "a", []byte("record"))

	if _, err := f.ReadAtDirect(make([]byte, 1), 0); err != nil {
		t.Fatalf("ReadAtDirect: %v", err)
	}

	r.failNext("close", unix.EIO)
	if err := f.Close(); err == nil {
		t.Fatal("Close with a failed close of the direct descriptor: no error")
	}
}

// fsName names the filesystem type magic is, for the message of the test
// whose precondition the filesystem can refuse to hold.
func fsName(magic int64) string {
	for _, fs := range []struct {
		magic int64
		name  string
	}{
		{unix.TMPFS_MAGIC, "tmpfs"},
		{unix.OVERLAYFS_SUPER_MAGIC, "overlayfs"},
		{unix.EXT4_SUPER_MAGIC, "ext4"},
		{unix.XFS_SUPER_MAGIC, "xfs"},
		{unix.BTRFS_SUPER_MAGIC, "btrfs"},
	} {
		if fs.magic == magic {
			return fs.name
		}
	}

	return "an unknown filesystem"
}

// residentPages reports, through mincore on a mapping of the file open on fd,
// how many of its first length bytes' pages the page cache holds.
func residentPages(t *testing.T, fd int, length int) int {
	t.Helper()

	front, err := unix.Mmap(fd, 0, length, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap of the file's first %d bytes: %v", length, err)
	}

	defer unix.Munmap(front)

	vec := make([]byte, (length+unix.Getpagesize()-1)/unix.Getpagesize())

	// mincore(2) answers through the vec one byte per page, whose lowest
	// bit is set when the page is resident. It is not in x/sys, so it is
	// made by its own number, as the syscall it is: no result is read from
	// it but the residency of the pages the mapping names.
	r1, _, errno := unix.Syscall(
		unix.SYS_MINCORE,
		uintptr(unsafe.Pointer(&front[0])),
		uintptr(length),
		uintptr(unsafe.Pointer(&vec[0])),
	)

	if errno != 0 {
		t.Fatalf("mincore of the file's first %d bytes: %v", length, errno)
	}

	_ = r1

	resident := 0
	for _, b := range vec {
		if b&1 != 0 {
			resident++
		}
	}

	return resident
}

// TestReadAtDirectReadsPastTheCache proves ReadAtDirect reads past the page
// cache, which no comparison of bytes can: a multi-block file is written and
// synced, its clean pages are dropped with FADV_DONTNEED, and residency
// through mincore shows no page left before the direct read — and still none
// after it, where a read through the cache would have brought them in —
// while the plain ReadAt made afterwards does make them resident, so a probe
// that cannot see residency fails rather than passing empty. The calls are
// made on the file's own descriptor, inside its Control callback where a
// Close cannot release it; a filesystem that will not drop the pages a
// journal's directory can sit on — a tmpfs, which has no page cache to drop
// these pages into — fails the test naming the filesystem, never skips.
func TestReadAtDirectReadsPastTheCache(t *testing.T) {
	prof.Watch(t, prof.Options{})

	length := 2 * 4096
	o := NewOS(t.TempDir())
	f := directCheckFile(t, o, nil, "a", bytes.Repeat([]byte("A"), length))

	var residentBefore, residentAfter int

	rc, err := f.f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	if err := rc.Control(func(fd uintptr) {
		// The precondition: a filesystem whose pages FADV_DONTNEED cannot
		// drop — tmpfs keeps them until the file is removed, since it has
		// no backing device to evict them to — would leave the read with
		// nothing to prove, so it fails the test naming itself.
		if err := unix.Fadvise(int(fd), 0, 0, unix.FADV_DONTNEED); err != nil {
			var sfs unix.Statfs_t

			if err2 := unix.Fstatfs(int(fd), &sfs); err2 != nil {
				t.Fatalf("fadvise: %v; its directory does not stat either: %v", err, err2)
			}

			t.Fatalf("FADV_DONTNEED on %s: %v", fsName(sfs.Type), err)
		}

		residentBefore = residentPages(t, int(fd), length)

		direct := make([]byte, 4096)
		if n, err := f.ReadAtDirect(direct, 0); n != 4096 || err != nil {
			t.Fatalf("ReadAtDirect: %d, %v; want 4096 and no error", n, err)
		}

		residentAfter = residentPages(t, int(fd), length)
	}); err != nil {
		t.Fatal(err)
	}

	if residentBefore != 0 {
		var sfs unix.Statfs_t

		if err := unix.Statfs(filepath.Join(o.dir, "a"), &sfs); err != nil {
			t.Fatal(err)
		}

		t.Fatalf(
			"%s kept %d of the file's %d pages resident after FADV_DONTNEED; the read past the cache cannot be proven on it",
			fsName(sfs.Type),
			residentBefore,
			(length+unix.Getpagesize()-1)/unix.Getpagesize(),
		)
	}

	if residentAfter != 0 {
		t.Fatalf(
			"ReadAtDirect left %d page(s) resident; a read through the page cache would, one past it does not",
			residentAfter,
		)
	}

	// The probe can fail: a plain ReadAt brings the pages in, so a residency
	// check that answered nothing would catch this read too.
	if n, err := f.f.ReadAt(make([]byte, 4096), 0); n != 4096 || err != nil {
		t.Fatalf("ReadAt of the file's first block: %d, %v", n, err)
	}

	rc, err = f.f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	var residentCached int

	if err := rc.Control(func(fd uintptr) {
		residentCached = residentPages(t, int(fd), length)
	}); err != nil {
		t.Fatal(err)
	}

	if residentCached == 0 {
		t.Fatal(
			"a plain ReadAt left no page resident, so this test's residency probe sees nothing a cached read brings in",
		)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
