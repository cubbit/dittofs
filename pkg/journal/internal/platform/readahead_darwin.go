package platform

import (
	"io/fs"
	"math"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// readAheadChunk is the most one F_RDADVISE covers: struct radvisory's
// ra_count is a C int.
const readAheadChunk = math.MaxInt32

// ReadAhead is fcntl with F_RDADVISE over the n bytes at off, in chunks of
// at most readAheadChunk, so the pages a later WriteAt only partly covers
// are already resident when it lands. A segment the Maker made is
// F_PREALLOCATE'd and never written, so none of its pages is resident, and
// a record write that ends mid-page makes the kernel fill the rest of that
// page first, synchronously; the hint lets that fill happen ahead of the
// write. It changes no byte and no size. A range no file can hold fails with
// EINVAL before any system call, and the descriptor is reached through the
// file's own reference, as Sync reaches its own, so a closed file fails with
// fs.ErrClosed.
//
// The call goes through the table's fcntl entry, unix.FcntlInt, whose int
// argument carries the struct's address: fcntl takes it as the pointer it
// is. The struct is pinned for the calls, which also keeps it off the
// goroutine's stack, so the address stays valid while the kernel reads it.
func (f *file) ReadAhead(off, n int64) error {
	if off < 0 || n < 0 || off > math.MaxInt64-n {
		return &fs.PathError{Op: "F_RDADVISE", Path: f.f.Name(), Err: unix.EINVAL}
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{Op: "F_RDADVISE", Path: f.f.Name(), Err: err}
	}

	ra := new(unix.Radvisory_t)
	var pin runtime.Pinner
	pin.Pin(ra)
	defer pin.Unpin()

	var aerr error
	if err := rc.Control(func(fd uintptr) {
		for end := off + n; off < end && aerr == nil; {
			count := min(end-off, readAheadChunk)
			*ra = unix.Radvisory_t{Offset: off, Count: int32(count)}
			aerr = retryInterrupted(func(fd int) error {
				_, err := f.calls.fcntl(uintptr(fd), unix.F_RDADVISE, int(uintptr(unsafe.Pointer(ra))))
				return err
			}, int(fd))
			off += count
		}
	}); err != nil {
		return &fs.PathError{Op: "F_RDADVISE", Path: f.f.Name(), Err: fs.ErrClosed}
	}

	if aerr != nil {
		return &fs.PathError{Op: "F_RDADVISE", Path: f.f.Name(), Err: aerr}
	}

	return nil
}
