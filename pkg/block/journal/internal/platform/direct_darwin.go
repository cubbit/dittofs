package platform

import (
	"errors"
	"io"
	"io/fs"

	"golang.org/x/sys/unix"
)

// noCacheOn and noCacheOff are the arguments that turn F_NOCACHE on and off.
const (
	noCacheOn  = 1
	noCacheOff = 0
)

// setNoCache makes fcntl with F_NOCACHE and arg on fd through
// retryInterrupted, and returns the fcntl's own result.
func (c calls) setNoCache(fd uintptr, arg int) error {
	return retryInterrupted(func(fd int) error {
		_, err := c.fcntl(uintptr(fd), unix.F_NOCACHE, arg)

		return err
	}, int(fd))
}

// readDirect reads into p the len(p) bytes at off past the page cache, by a
// pread on the file's own descriptor with F_NOCACHE set around it, over the
// aligned window directRead builds.
//
// ponytail: F_NOCACHE is advisory — macOS may still serve a page the cache
// holds — and it is set per open file description, not per read, so it is
// set around each read and cleared again, and a concurrent cached ReadAt of
// the same file shares the no-cache behaviour for the window's duration.
// Upgrade to a read channel a concurrent cached read cannot share — the
// Linux reopen with O_DIRECT, at the cost of a second descriptor per file —
// when a tail check needs the proof an advisory F_NOCACHE cannot give, or
// when RFC 28 settles whether macOS may refuse F_NOCACHE outright.
//
// decision: a filesystem that refuses F_NOCACHE — an error the set fcntl(2)
// answers — is handed back as itself and never widened or worked around
// with the cached read, which would hand the tail check after a crash a
// proof it does not have; the ceiling is the one at ReadAtDirect. An error
// of the clear after a good read fails the read too: F_NOCACHE stays set on
// the file's descriptor, so returning success would leave every later cached
// read of the file bypassing the cache unasked.
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
		// The closed check runs before the nothing-to-read one, so a closed
		// file answers fs.ErrClosed even for a p of length 0, as ReadAt does.
		return 0, &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if len(p) == 0 {
		return 0, nil
	}

	// The whole read — F_NOCACHE's set, the preads, the clear — runs under
	// directMu, so a Close cannot release the descriptor under it and two
	// reads never interleave their windows on the one descriptor. A cached
	// ReadAt takes no directMu, so it may share the window's no-cache
	// behaviour — the ponytail above names that ceiling.
	rc, err := f.f.SyscallConn()
	if err != nil {
		// As in Sync: for an open file this cannot fail.
		return 0, &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	var n int

	// serr is the set's own error, rerr the read's and cerr the clear's,
	// each returned after the read, itself first.
	var serr, rerr, cerr error

	if err := rc.Control(func(fd uintptr) {
		if serr = f.calls.setNoCache(fd, noCacheOn); serr != nil {
			// The set never happened, so nothing was cleared and nothing
			// read: the refusal is the read's whole answer.
			return
		}

		defer func() {
			if err := f.calls.setNoCache(fd, noCacheOff); err != nil {
				cerr = err
			}
		}()

		n, rerr = directRead(p, off, directAlign, func(buf []byte, off int64) (int, error) {
			return f.calls.pread(int(fd), buf, off)
		})
	}); err != nil {
		// As in Sync: Control itself fails only on a closed file. The
		// deferred clear had its chance under Control, so the descriptor
		// left the call either cleared or never set.
		return 0, &fs.PathError{
			Op:   "pread",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	switch {
	case serr != nil:
		return 0, &fs.PathError{Op: "fcntl", Path: f.f.Name(), Err: serr}
	case cerr != nil:
		// The clear's failure outranks even the read's io.EOF: F_NOCACHE
		// stays set on the file's descriptor, so an answer of a clean
		// short read would leave every later cached read of the file
		// bypassing the cache unasked — never something to return as
		// cleared.
		return n, &fs.PathError{Op: "fcntl", Path: f.f.Name(), Err: cerr}
	case errors.Is(rerr, io.EOF):
		// A short read of the file's end answers io.EOF bare, as ReadAt
		// does — never a PathError wrapping it, which an err == io.EOF
		// caller would not see.
		return n, io.EOF
	case rerr != nil:
		return n, &fs.PathError{Op: "pread", Path: f.f.Name(), Err: rerr}
	}

	return n, nil
}

// closeDirectLocked marks the file closed for ReadAtDirect. The descriptor
// the file read through is the file's own, so every F_NOCACHE a ReadAtDirect
// set was cleared before it returned, and it needs no closing of its own —
// Close's own close of the file releases it. The caller holds directMu.
func (f *file) closeDirectLocked() error {
	f.directFD = directClosed

	return nil
}
