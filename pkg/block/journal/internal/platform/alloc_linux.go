package platform

import (
	"errors"
	"io"
	"io/fs"
	"math"

	"golang.org/x/sys/unix"
)

// zeroChunk is the most one zero write covers.
const zeroChunk = 1 << 20

// zeros is the one buffer every zero write reads from. Nothing writes to it,
// so concurrent Allocate calls share it, and a write allocates nothing.
var zeros [zeroChunk]byte

// Allocate is fallocate in its default mode over the range, then zeros
// written over every byte of it (RFC 28 Appendix A: "on Linux, `fallocate`
// then writing zeros over the whole segment"). Where the filesystem has no
// fallocate for the file, the zero writes alone allocate the range. It does
// not sync: the caller's Sync makes the allocation durable, as it does a
// WriteAt. Zeros are written over the whole range, so bytes a write had put
// there read as zeros afterwards. A range no file can hold fails with EINVAL,
// and an empty range does nothing, both before any system call. The
// descriptor is reached through the file's own reference, so a concurrent
// Close cannot release it during the calls. On a closed file a valid,
// non-empty range fails with fs.ErrClosed before any system call.
//
// decision: zero-filled on Linux because fallocate alone leaves unwritten
// extents on ext4 and XFS (RFC 28 Appendix A): the first write into each
// converts it in a filesystem-journal transaction, so every fdatasync after
// new appends would also force a filesystem-journal commit, and RFC 1 §5.2's
// one flush per Sync would not hold. Written zeros make an append an
// overwrite of allocated, written blocks. Withdraw the fill if J1's
// Sync-per-write row, run with and without it, shows unwritten-extent
// conversions cost less than its writes, as the RFC says.
func (f *file) Allocate(off, n int64) error {
	if off < 0 || n < 0 || off > math.MaxInt64-n {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  unix.EINVAL,
		}
	}

	if n == 0 {
		return nil
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// As in Sync, Control fails only on a closed file.
	var op string
	var aerr error

	if err := rc.Control(func(fd uintptr) {
		op, aerr = f.allocate(int(fd), off, n)
	}); err != nil {
		return &fs.PathError{
			Op:   "fallocate",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if aerr != nil {
		return &fs.PathError{
			Op:   op,
			Path: f.f.Name(),
			Err:  aerr,
		}
	}

	return nil
}

// allocate makes Allocate's calls on fd, and returns the name of the call
// that failed and its error, if one did. A call interrupted by a signal is
// made again, as the os package does for its own writes: both calls give the
// same result when repeated over the same range, so a repeat hides no
// failure. A fallocate refused with EOPNOTSUPP is followed by the zero writes
// alone. Every other error is returned after one call, and no call follows
// it.
func (f *file) allocate(fd int, off, n int64) (string, error) {
	err := f.calls.fallocate(fd, unix.FALLOC_FL_ALLOCATE_RANGE, off, n)
	for errors.Is(err, unix.EINTR) {
		err = f.calls.fallocate(fd, unix.FALLOC_FL_ALLOCATE_RANGE, off, n)
	}

	// decision: a filesystem with no fallocate for the file (the ext2
	// driver, or the ext4 driver on a file without extents, as an ext3
	// volume makes) is allocated by the zero writes alone, which RFC 28 §2.2
	// allows ("with the platform's allocation call or by writing zeros").
	// Both share ext4's magic, and an ext3 volume can be mounted as ext4, so
	// the directory check cannot refuse them. There the written zeros
	// allocate every block. That is all the fallback guarantees: the
	// directory check passes a filesystem RFC 28 Appendix A does not list,
	// and on ZFS with compression on, its default (ZIO_COMPRESS_DEFAULT in
	// include/sys/zio.h, zfs 2.3.0), neither call allocates. OpenZFS's
	// fallocate without flags only checks the free space and sets the size,
	// reserving no blocks, or returns EOPNOTSUPP when
	// zfs_fallocate_reserve_percent is 0 (zpl_fallocate_common in
	// module/os/linux/zfs/zpl_file.c, zfs 2.3.0), and with compression on a
	// block of zeros is written as a hole (zio_write_compress in
	// module/zfs/zio.c, zfs 2.3.0). A write into a segment allocated there
	// can fail with ENOSPC, and the headroom of RFC 1 §6 does not hold; no
	// data is lost, since the error is returned. On XFS, fallocate is what
	// rules out speculative allocation past the end (RFC 28 Appendix A), so
	// withdraw this fallback if XFS is ever seen returning EOPNOTSUPP.
	if err != nil && !errors.Is(err, unix.EOPNOTSUPP) {
		return "fallocate", err
	}

	end := off + n

	for off < end {
		w, err := f.calls.pwrite(fd, zeros[:min(end-off, zeroChunk)], off)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return "pwrite", err
		}

		// A write that writes nothing and reports no error would repeat for
		// ever; the os package reports it the same way.
		if w <= 0 {
			return "pwrite", io.ErrUnexpectedEOF
		}

		off += int64(w)
	}

	return "", nil
}

// fiemapOp names FIEMAP's calls in the errors Allocated answers with, and
// lseekOp the hole seek's.
const (
	fiemapOp = "fiemap"
	lseekOp  = "lseek"
)

// Allocated reports whether every byte of the file lies in an extent the
// filesystem holds allocated and written. FIEMAP is asked over [0, size); a
// hole between extents or FIEMAP_EXTENT_UNWRITTEN answers false. The
// descriptor is reached through the file's own reference, as Allocate and
// Sync reach theirs, so a concurrent Close cannot release it during the
// calls, and a closed file fails before any system call, with the size read
// first.
//
// decision: a filesystem that answers ENOTTY or EOPNOTSUPP for the ioctl has
// no extent report to give, not a fault to report, so the query falls back to
// the hole lseek names: a holed spare differs from a made one exactly where a
// hole starts below the size, and SEEK_HOLE answers that where extents cannot
// be asked. Failing open is left only for a filesystem that answers neither
// extents nor holes — there nothing distinguishes a holed spare from a made
// one, and the file answers whole rather than failing the open over a device
// that never refused one. tmpfs was the shape that rode the fail-open — the
// directory check passes it, it serves no FIEMAP, and its holes are the
// ones its own SEEK_HOLE reports — so it is covered by the fallback, which
// is the withdrawal condition the earlier fail-open named, met.
func (f *file) Allocated() (bool, error) {
	size, err := f.Size()
	if err != nil {
		return false, err
	}
	if size == 0 {
		// Nothing to cover: an empty file is whole by the size alone.
		return true, nil
	}

	rc, rerr := f.f.SyscallConn()
	if rerr != nil {
		return false, &fs.PathError{Op: fiemapOp, Path: f.f.Name(), Err: rerr}
	}

	var whole bool
	var op string
	var aerr error
	if err := rc.Control(func(fd uintptr) {
		whole, op, aerr = f.allocated(int(fd), size)
	}); err != nil {
		return false, &fs.PathError{Op: fiemapOp, Path: f.f.Name(), Err: fs.ErrClosed}
	}
	if aerr != nil {
		return false, &fs.PathError{Op: op, Path: f.f.Name(), Err: aerr}
	}
	return whole, nil
}

// allocated makes Allocated's FIEMAP calls on fd and walks what they answer
// over [0, size): the file is whole iff every byte lies in an extent that is
// neither a hole — a gap between the extents a window returns — nor
// unwritten, the state a fallocate no write reached leaves, which reads as
// zeros until its first write. A window holds what remains of the range and
// nothing is asked past size, so a file whose whole body one window covers
// costs one call. A call interrupted by a signal is made again, as Allocate
// remakes its own: both give the same answer over the same range. A window
// refused with ENOTTY or EOPNOTSUPP answers no extent report exists to read,
// and the hole seek decides instead, as Allocated's decision says.
func (f *file) allocated(fd int, size int64) (bool, string, error) {
	covered := int64(0)
	for covered < size {
		fm := fiemap{start: uint64(covered), length: uint64(size - covered), count: fiemapWindow}

		err := f.calls.fiemap(fd, &fm)
		for errors.Is(err, unix.EINTR) {
			err = f.calls.fiemap(fd, &fm)
		}
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
			return f.holeSeekWhole(fd, size) // no extent report exists to read, see Allocated
		}
		if err != nil {
			return false, fiemapOp, err
		}
		if fm.mapped == 0 {
			return false, fiemapOp, nil // a hole where the window begins
		}

		was := covered
		for i := range fm.extents[:min(fm.mapped, fiemapWindow)] {
			e := &fm.extents[i]
			if e.flags&fiemapExtentUnwritten != 0 {
				return false, fiemapOp, nil
			}
			// The extents a window returns cover from where it was asked in
			// ascending file order, so one starting past what the ones
			// before it covered is the hole between them.
			if e.logical > uint64(covered) {
				return false, fiemapOp, nil
			}
			end := e.logical + e.length
			if end < e.logical {
				return false, fiemapOp, io.ErrUnexpectedEOF // a length no file can hold
			}
			if end > math.MaxInt64 {
				// covered is an int64 offset: an extent whose end runs past
				// the largest one would wrap it negative and end the walk
				// over bytes nothing covered.
				return false, fiemapOp, io.ErrUnexpectedEOF // an end no offset can name
			}
			if end > uint64(covered) {
				covered = int64(end)
			}
		}
		if covered == was {
			// A window that advanced nothing would repeat for ever; the
			// lengths it answered are not ones the file holds.
			return false, fiemapOp, io.ErrUnexpectedEOF
		}
	}
	return true, fiemapOp, nil
}

// holeSeekWhole answers Allocated over a filesystem with no extent report,
// by the hole lseek names: SEEK_HOLE answers the offset of the first hole —
// the end of the file where none is, for the one past the last byte is
// implicit — so the file is whole iff the seek answers at or past the size.
// A file truncated and never written, whose first hole is at offset 0,
// answers not whole here exactly as a punched one does to FIEMAP.
//
// A call interrupted by a signal is made again, as the FIEMAP walk remakes
// its own: the seek answers the same hole over the same offset.
//
// decision: a seek refused with EINVAL, ENOSYS, ENOTTY or EOPNOTSUPP is the
// one refusal the filesystem can make that says a hole report exists too:
// the query cannot be asked there any more than the extent one could, so the
// file answers whole rather than failing the open over a device that never
// refused one — the fail-open Allocated names for a filesystem that answers
// neither extents nor holes. Anything else the seek answers is a fault, and
// fails the query naming the call — a device that refused the seek was asked
// a question it received.
func (f *file) holeSeekWhole(fd int, size int64) (bool, string, error) {
	off, err := f.calls.lseek(fd, 0, unix.SEEK_HOLE)
	for errors.Is(err, unix.EINTR) {
		off, err = f.calls.lseek(fd, 0, unix.SEEK_HOLE)
	}
	switch {
	case err == nil:
		return off >= size, lseekOp, nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS),
		errors.Is(err, unix.ENOTTY), errors.Is(err, unix.EOPNOTSUPP):
		return true, lseekOp, nil // neither extents nor holes, see Allocated
	default:
		return false, lseekOp, err
	}
}
