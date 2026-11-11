package platform

import (
	"errors"
	"io/fs"
	"strconv"

	"golang.org/x/sys/unix"
)

// directOpenFlags opens the file's direct descriptor: read only, which a
// reader asks no more of, not inherited by a child process, and bypassing the
// page cache, which is the one thing the descriptor exists for.
const directOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECT

// procSelfFD is where the kernel names the calling process's open descriptors,
// one symlink per descriptor.
const procSelfFD = "/proc/self/fd/"

// readDirect reads into p the len(p) bytes at off past the page cache, by
// pread on a descriptor of the file reopened with O_DIRECT, over the aligned
// window directRead builds.
//
// decision: the direct descriptor is the file's first descriptor reopened
// through /proc/self/fd, opened once and kept for the file's life, rather than
// O_DIRECT on the first descriptor itself. Reopening by its descriptor keeps
// every other read and write the file makes cached, so an ordinary ReadAt
// keeps io.ReaderAt's and the journal's own reads still coalesce in the cache;
// and it survives a rename of the segment, which a path would not. A
// filesystem that refuses the reopen — EINVAL of O_DIRECT, which tmpfs
// answered before the kernel learned to accept it — is handed back as itself,
// never widened and never worked around with the cached read, which would
// hand the tail check after a crash a proof it does not have. The direct mu
// closes the last gap: an open of /proc/self/fd/N whose N a concurrent Close
// released could open the next open's file, so the open, the read and the
// close all run under it, which Close also takes.
func (f *file) readDirect(p []byte, off int64) (int, error) {
	if off < 0 {
		// ReadAt names EINVAL for a negative offset; pread would answer its
		// own, unnamed one.
		return 0, &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  unix.EINVAL,
		}
	}

	f.directMu.Lock()
	defer f.directMu.Unlock()

	if f.directFD == directClosed {
		return 0, &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if f.directFD == directUnopened {
		if err := f.openDirectLocked(); err != nil {
			return 0, err
		}
	}

	fd := f.directFD

	return directRead(p, off, directAlign, func(buf []byte, off int64) (int, error) {
		n, err := f.calls.pread(fd, buf, off)
		for errors.Is(err, unix.EINTR) {
			n, err = f.calls.pread(fd, buf, off)
		}

		return n, err
	})
}

// openDirectLocked opens the file's direct descriptor and caches it in f. The
// caller holds directMu. A filesystem's refusal of the open is returned as
// itself, wrapped as an open's PathError so errors.Is names the errno through
// the os package's own shape; a file whose first descriptor was closed is
// reported as fs.ErrClosed.
func (f *file) openDirectLocked() error {
	rc, err := f.f.SyscallConn()
	if err != nil {
		// As in Sync and Allocate: for an open file this cannot fail.
		return &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	var oerr error
	if err := rc.Control(func(fd uintptr) {
		// An open interrupted by a signal is made again, as SyncDir does:
		// repeating an open hides nothing.
		dfd, err := openRetrying(func() (int, error) {
			return f.calls.open(procSelfFD+strconv.Itoa(int(fd)), directOpenFlags, 0)
		})

		if err != nil {
			oerr = err
			return
		}

		f.directFD = dfd
	}); err != nil {
		// As in Sync and Allocate: Control itself fails only on a closed file.
		return &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if oerr != nil {
		return &fs.PathError{
			Op:   "open",
			Path: f.f.Name(),
			Err:  oerr,
		}
	}

	return nil
}

// closeDirectLocked closes the file's direct descriptor through the table. The
// caller holds directMu, and close is never repeated, as in SyncDir: Linux
// releases the descriptor even when close reports EINTR.
func (f *file) closeDirectLocked() error {
	if f.directFD < 0 {
		return nil
	}

	if err := f.calls.close(f.directFD); err != nil {
		return &fs.PathError{
			Op:   "close",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	return nil
}
