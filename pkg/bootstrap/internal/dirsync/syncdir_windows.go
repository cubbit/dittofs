package dirsync

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows"
)

// Sync makes the entries of the directory dir durable the way the
// journal's own directory sync does on Windows: the directory is opened with
// backup semantics, without which a directory does not open, and write
// access, which FlushFileBuffers needs, sharing every access so no other open
// of the directory is refused, and flushed.
func Sync(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}

	h, err := windows.CreateFile(
		p,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}

	var errs []error

	if err := windows.FlushFileBuffers(h); err != nil {
		errs = append(errs, &fs.PathError{Op: "FlushFileBuffers", Path: dir, Err: err})
	}

	if err := windows.CloseHandle(h); err != nil {
		errs = append(errs, &fs.PathError{Op: "close", Path: dir, Err: err})
	}

	return errors.Join(errs...)
}
