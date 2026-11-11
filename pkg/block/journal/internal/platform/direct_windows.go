package platform

import (
	"errors"
	"io"
	"io/fs"

	"golang.org/x/sys/windows"
)

// readDirect reads into p the len(p) bytes at off past the page cache, by
// ReadFile on a second handle of the file opened with FILE_FLAG_NO_BUFFERING,
// over the aligned window directRead builds. The handle is opened lazily by
// the file's first direct read, asks for read access alone — a reader asks
// nothing of the file but its bytes — and shares reads, writes and deletes as
// the journal's own opens do, so this second handle never blocks the
// journal's own rename or remove of the file while it is open. It is kept for
// the file's life and closed by Close, and the direct mu holds the open, the
// read and the close together, so a Close cannot release the handle under a
// read and two reads never open a handle twice.
//
// decision: a filesystem that refuses the open or a read of the direct handle
// is handed back as itself, never widened and never worked around with the
// cached read, which would hand the tail check after a crash a proof it does
// not have. What the refusal means for the journal is not decided here — the
// open's tail check decides it, against the refusal ReadAtDirect returns (see
// ReadAtDirect's own decision).
func (f *file) readDirect(p []byte, off int64) (int, error) {
	if off < 0 {
		// ReadAt refuses a negative offset with an error of its own; this one
		// names ERROR_INVALID_PARAMETER so errors.Is can match it, where
		// ReadFile would answer its own, unnamed one.
		return 0, &fs.PathError{
			Op:   "ReadFile",
			Path: f.f.Name(),
			Err:  windows.ERROR_INVALID_PARAMETER,
		}
	}

	f.directMu.Lock()
	defer f.directMu.Unlock()

	// The closed check runs before the nothing-to-read one, so a closed file
	// answers fs.ErrClosed even for a p of length 0, as ReadAt does.
	if f.directFD == directClosed {
		return 0, &fs.PathError{
			Op:   "ReadFile",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if len(p) == 0 {
		return 0, nil
	}

	if f.directFD == directUnopened {
		if err := f.openDirectLocked(); err != nil {
			return 0, err
		}
	}

	h := windows.Handle(f.directFD)

	n, rerr := directRead(p, off, directAlign, func(buf []byte, off int64) (int, error) {
		// ponytail: the buffer reaches ReadFile uncapped, and its length is
		// passed to the syscall as a 32-bit count, so a window of 4 GiB or
		// more would be truncated and misread. Cap it as WriteAt caps its
		// writes when a journal read ever asks for that much memory at once.
		o := windows.Overlapped{
			Offset:     uint32(off),
			OffsetHigh: uint32(off >> 32),
		}

		var done uint32

		if err := f.calls.readFile(h, buf, &done, &o); err != nil {
			// ERROR_HANDLE_EOF is a short read at the file's end, not a
			// failure, as internal/poll's own Pread reads it, and it becomes
			// io.EOF for the same reason: directRead ends its window there
			// as the end of the file. Windows has no interrupted read to
			// make again (SetFilePointer is never asked).
			if errors.Is(err, windows.ERROR_HANDLE_EOF) {
				return int(done), io.EOF
			}

			return int(done), err
		}

		// A read of nothing and no error is the end of the file on a
		// synchronous handle, which directRead's own zero-read guard reads
		// as io.EOF, ReadAt's own answer for it.
		return int(done), nil
	})

	switch {
	case rerr == io.EOF:
		// A short read of the file's end answers io.EOF bare, as ReadAt does
		// — never a PathError wrapping it, which an err == io.EOF caller
		// would not see.
		return n, io.EOF
	case rerr != nil:
		return n, &fs.PathError{
			Op:   "ReadFile",
			Path: f.f.Name(),
			Err:  rerr,
		}
	}

	return n, nil
}

// openDirectLocked opens the file's direct handle, with FILE_FLAG_NO_BUFFERING
// and the sharing of the journal's own opens, and caches its value in
// f.directFD. The caller holds directMu. The open is by the file's path; a
// refusal by the filesystem is returned as itself, wrapped as an open's
// PathError.
//
// ponytail: the handle is opened by the file's path, not by the handle the
// file already holds (Linux reopens through /proc/self/fd, which Windows has
// no counterpart of), so a segment renamed while its direct handle is
// unopened has its next direct read open the file now carrying the name — a
// different segment, if one was created under the old one.
//
// Upgrade when a direct read is made on a segment after a spare rename or a
// recycle. The cheaper upgrade is windows.GetFinalPathNameByHandle, already in
// golang.org/x/sys/windows: resolving the path through the handle the file
// holds survives the rename with no new kernel call wrapped here, since the
// handle follows the file, not the name. ReOpenFile, which reopens by the
// handle itself, is not in x/sys; ask for it, or wrap its LazyProc here, only
// if even the resolved name's own window — a rename after the resolution —
// bites.
func (f *file) openDirectLocked() error {
	name, err := windows.UTF16PtrFromString(f.f.Name())
	if err != nil {
		return &fs.PathError{
			Op:   "open",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	h, err := f.calls.createFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING,
		0,
	)
	if err != nil {
		return &fs.PathError{
			Op:   "open",
			Path: f.f.Name(),
			Err:  fullVolume(err),
		}
	}

	f.directFD = int(h)

	return nil
}

// closeDirectLocked closes the file's direct handle through the table, which
// Close reaches under the direct mu, so it cannot race a read. The caller
// holds directMu.
func (f *file) closeDirectLocked() error {
	if f.directFD < 0 {
		return nil
	}

	if err := f.calls.closeHandle(windows.Handle(f.directFD)); err != nil {
		return &fs.PathError{
			Op:   "CloseHandle",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	return nil
}
