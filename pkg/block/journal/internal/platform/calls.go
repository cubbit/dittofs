//go:build linux

package platform

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// calls is the table of system calls the storage makes where the RFC names
// the call. Each entry is the golang.org/x/sys function itself, never a
// wrapper, and an argument the call takes as a command is passed as data, so a
// test that swaps the table sees each call with its arguments as the kernel
// would.
type calls struct {
	open           func(path string, mode int, perm uint32) (fd int, err error)
	openat         func(dirfd int, path string, mode int, perm uint32) (fd int, err error)
	fsync          func(fd int) error
	fdatasync      func(fd int) error
	close          func(fd int) error
	fallocate      func(fd int, mode uint32, off int64, n int64) error
	pwrite         func(fd int, p []byte, off int64) (n int, err error)
	pread          func(fd int, p []byte, off int64) (n int, err error)
	fstatfs        func(fd int, buf *unix.Statfs_t) error
	fstat          func(fd int, stat *unix.Stat_t) error
	statx          func(dirfd int, path string, flags int, mask int, stat *unix.Statx_t) error
	ioctlGetUint32 func(fd int, req uint) (uint32, error)
	getdents       func(fd int, buf []byte) (n int, err error)
	fiemap         func(fd int, fm *fiemap) error
	lseek          func(fd int, off int64, whence int) (n int64, err error)
}

// realTable is the table NewOS gives every OS.
var realTable = calls{
	open:           unix.Open,
	openat:         unix.Openat,
	fsync:          unix.Fsync,
	fdatasync:      unix.Fdatasync,
	close:          unix.Close,
	fallocate:      unix.Fallocate,
	pwrite:         unix.Pwrite,
	pread:          unix.Pread,
	fstatfs:        unix.Fstatfs,
	fstat:          unix.Fstat,
	statx:          unix.Statx,
	getdents:       unix.Getdents,
	ioctlGetUint32: unix.IoctlGetUint32,
	fiemap:         fiemapIoctl,
	lseek:          unix.Seek,
}

// fiemapIOC is FS_IOC_FIEMAP, _IOWR('f', 11, struct fiemap) of linux/fiemap.h.
// golang.org/x/sys v0.49.0 has neither the constant nor the structs, so the
// whole request is here, next to the table entry that carries it.
const fiemapIOC = 0xc020660b

// fiemapWindow is the extents one FIEMAP call offers room for: enough that a
// cap-sized segment is covered by one or two calls, small enough that the
// whole request stays on the caller's stack.
const fiemapWindow = 32

// fiemapExtentUnwritten is FIEMAP_EXTENT_UNWRITTEN of linux/fiemap.h: an
// extent the filesystem allocated (0x800) and no write ever reached, which
// reads back zeros until its first write converts it. FIEMAP_EXTENT_UNKNOWN
// (0x2) reads like it but names data whose location the filesystem will not
// say — an unwritten extent is named 0x800 and only 0x800.
const fiemapExtentUnwritten = 0x800

// fiemapExtent is one FIEMAP extent of a window, linux/fiemap.h's struct
// fiemap_extent. The fields keep the header's order and sizes — fe_flags sits
// past fe_reserved64's sixteen bytes — so the ioctl reads and writes them
// where it expects.
type fiemapExtent struct {
	logical    uint64    // fe_logical: the extent's first byte's offset in the file
	physical   uint64    // fe_physical: the offset on the device holding it
	length     uint64    // fe_length: the extent's bytes
	reserved64 [2]uint64 // fe_reserved64
	flags      uint32    // fe_flags, among them fiemapExtentUnwritten
	_          [3]uint32 // fe_reserved
}

// fiemap is one FIEMAP window: linux/fiemap.h's struct fiemap, its extents
// inline where the header's flexible array sits, so one allocation serves
// the whole request and the kernel finds fm_extents[0] at the header's end.
type fiemap struct {
	start   uint64 // fm_start
	length  uint64 // fm_length
	flags   uint32 // fm_flags: none, the query reads the extents as they stand
	mapped  uint32 // fm_mapped_extents: the extents the window answered
	count   uint32 // fm_extent_count: the room the caller offered
	_       uint32 // fm_reserved
	extents [fiemapWindow]fiemapExtent
}

// fiemapIoctl makes one FIEMAP ioctl call on fd with fm. It is the table's
// own entry where an x/sys call would be, for x/sys ships no FIEMAP wrapper
// to pin: the one call, unread and uninterpreted, is all it does.
func fiemapIoctl(fd int, fm *fiemap) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(fiemapIOC),
		uintptr(unsafe.Pointer(fm)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
