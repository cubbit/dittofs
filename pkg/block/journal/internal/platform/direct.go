package platform

import (
	"errors"
	"io"
	"unsafe"
)

// directAlign is the alignment every argument of the platforms' direct reads
// carries: the offset, the buffer's address and the buffer's length are
// multiples of it. 4096 satisfies what O_DIRECT and FILE_FLAG_NO_BUFFERING
// ask of it — a multiple of the device's logical sector size — on every
// supported platform: no sector size a storage device reports there exceeds
// it.
//
// ponytail: the alignment is the constant 4096 rather than the device's own
// requirement, read where the kernel names it — statx's STATX_DIOALIGN on
// Linux (the statx entry is already in the table), the
// FILE_STORAGE_INFO of the volume's file system on Windows — so a device
// whose logical sectors exceed 4096, which the platforms refuse a direct
// read on (EINVAL on Linux, ERROR_INVALID_PARAMETER on Windows), is
// answered by that refusal as it is, and a journal on it cannot reopen
// after a crash that left an unproven tail. Read the requirement and
// request the alignment per device when such a device has to be supported.
const directAlign int64 = 4096

// directRead reads into p the len(p) bytes at off, calling raw over the aligned
// window that covers them: raw fills a buffer whose address and length are
// multiples of align, at an offset that is a multiple of align too, and raw is
// repeated while it reads part of the window and reports no error, since a
// short read of a direct read is legal. It keeps the storage.File contract of
// ReadAt: a len(p) of 0 reads nothing, and a read that finds fewer than len(p)
// bytes returns what it found with io.EOF. The window's buffer is
// over-allocated and sliced at its first aligned address, since a Go
// allocation comes back at an arbitrary address.
func directRead(p []byte, off int64, align int64, raw func(buf []byte, off int64) (int, error)) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if off < 0 {
		// ReadAt reports no error in particular for a negative offset, and raw
		// answers ReadAt's, wrapped as the platform's caller pairs it.
		_, err := raw(nil, off)
		return 0, err
	}

	start := off &^ (align - 1)
	skip := int(off - start)
	end := off + int64(len(p))
	window := (end+(align-1))&^(align-1) - start

	whole := make([]byte, int(window)+int(align))
	buf := whole[int(-uintptr(unsafe.Pointer(unsafe.SliceData(whole)))&uintptr(align-1)):]
	buf = buf[:int(window)]

	read := 0
	for read < len(buf) {
		n, err := raw(buf[read:], start+int64(read))

		// A raw shaped like a plain pread answers -1 with its error, so the
		// count is clamped before it is added: a good aligned call followed by
		// a failing one must not lose a byte to the failed call's answer.
		n = max(n, 0)
		read += n

		if err == nil && n == 0 {
			// A raw like a plain pread answers 0 and no error at the end of
			// the file, where a buffered raw answers io.EOF; one answer is
			// made into the other, so the branch below ends the window for
			// both and the read never repeats for ever.
			err = io.EOF
		}

		if err == nil && int64(read)%align != 0 {
			// A short, error-free read left the window's remainder off the
			// alignment, so the next call could not carry raw's aligned
			// arguments: its offset, buffer and length are multiples of align,
			// which is why the window exists. The count ends the window, read
			// as the end of the file — a raw answers more only past the file's
			// end — which also keeps ReadAt's contract: the bytes asked for
			// past it come back cut short with io.EOF.
			//
			// decision: an unaligned short count is read as the end of the
			// file, which holds because a direct read into an aligned window
			// on a local block filesystem returns a count off the alignment
			// only at the file's end. A filesystem that returns one mid-file —
			// a network filesystem, or a FUSE mount with direct_io — would
			// make this rule read the file as shorter than it is, so a short
			// read of an unread record would answer io.EOF and the open's tail
			// check would drop the record silently, as it drops the tail past
			// the end. Withdraw the rule, by resuming the window from the next
			// aligned window start instead of ending it, when a filesystem the
			// journal runs on is known to answer one mid-file.
			err = io.EOF
		}

		if err != nil {
			n, _ = cutShort(p, buf, skip, read)

			// An EOF that still left every byte p asked for is ReadAt's nil:
			// raw's aligned window reaching the end of the file says nothing
			// about p, which lies wholly inside what was read. Any other
			// error is returned even with a full p, and with the bytes before
			// it.
			if errors.Is(err, io.EOF) && n == len(p) {
				return n, nil
			}

			return n, err
		}
	}

	// The window was read whole, and it covers p from start to end.
	return copy(p, buf[skip:]), nil
}

// cutShort copies out of the window, read up to its read bytes, the at most
// len(p) bytes the file still had after the file's first skip bytes of the
// window, with io.EOF: the window was read short, so no byte p asked for past
// what was read exists.
func cutShort(p, window []byte, skip, read int) (int, error) {
	if skip >= read {
		return 0, io.EOF
	}

	return copy(p, window[skip:read]), io.EOF
}
