package platform

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// resident returns how many of the pages of the first n bytes of the file at
// path the page cache holds, by mincore over a read-only mapping of them.
// Mapping a file faults none of its pages in, so the count is the cache's own.
func resident(t *testing.T, path string, n int) int {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	m, err := unix.Mmap(int(f.Fd()), 0, n, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap %s: %v", path, err)
	}
	defer func() { _ = unix.Munmap(m) }()

	page := os.Getpagesize()
	vec := make([]byte, (n+page-1)/page)
	// x/sys has no darwin mincore wrapper, so the test makes the raw call.
	mincore := unix.SYS_MINCORE //nolint:staticcheck // SA1019: no libSystem mincore wrapper exists
	if _, _, errno := unix.Syscall(uintptr(mincore), uintptr(unsafe.Pointer(&m[0])), uintptr(n), uintptr(unsafe.Pointer(&vec[0]))); errno != 0 {
		t.Fatalf("mincore %s: %v", path, errno)
	}

	in := 0
	for _, v := range vec {
		if v&1 != 0 {
			in++
		}
	}
	return in
}

// TestReadAheadMakesAllocatedPagesResident checks the hint does what the
// stream's roll asks of it: a segment made as the Maker makes one —
// F_PREALLOCATE'd, sized and synced, no byte written — holds no page in the
// cache, and after ReadAhead over it every page comes in. The hint is
// asynchronous, so the count is polled until whole, with one generous
// backstop.
func TestReadAheadMakesAllocatedPagesResident(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const n = 8 << 20

	dir := t.TempDir()
	o := NewOS(dir)
	f, err := o.Create("seg")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if err := f.Allocate(0, n); err != nil {
		t.Fatalf("Allocate(0, %d): %v", n, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	path := filepath.Join(dir, "seg")
	pages := n / os.Getpagesize()
	if got := resident(t, path, n); got == pages {
		t.Fatalf("resident pages before the hint: got all %d, want fewer: the check cannot see the hint", got)
	}

	ra, ok := f.(storage.ReadAheader)
	if !ok {
		t.Fatalf("the darwin file is not a storage.ReadAheader")
	}
	if err := ra.ReadAhead(0, n); err != nil {
		t.Fatalf("ReadAhead(0, %d): %v", n, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	got := resident(t, path, n)
	for got < pages && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		got = resident(t, path, n)
	}
	if got < pages {
		t.Fatalf("resident pages after ReadAhead(0, %d): got %d, want %d", n, got, pages)
	}
}

// TestReadAheadRefusals checks the hint refuses what Allocate refuses — a
// negative or overflowing range with EINVAL — and a closed file with
// fs.ErrClosed, before any system call.
func TestReadAheadRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	o := NewOS(t.TempDir())
	f, err := o.Create("seg")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ra := f.(storage.ReadAheader)

	for _, c := range []struct{ off, n int64 }{{-1, 1}, {0, -1}, {1, 1<<63 - 1}} {
		if err := ra.ReadAhead(c.off, c.n); !errors.Is(err, unix.EINVAL) {
			t.Errorf("ReadAhead(%d, %d): got %v, want EINVAL", c.off, c.n, err)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ra.ReadAhead(0, 4096); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("ReadAhead on a closed file: got %v, want fs.ErrClosed", err)
	}
}
