//go:build linux || darwin || windows

package platform

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// OS is a storage.FS over the directory dir, on the operating system's files.
// Create, Open, and a file's WriteAt, Truncate and Size are per system
// (os_unix.go, os_windows.go); the rest is shared here.
type OS struct {
	dir   string
	calls calls
}

var _ storage.FS = (*OS)(nil)

// NewOS returns the storage over the existing directory dir.
func NewOS(dir string) *OS {
	return newOS(dir, realTable)
}

// newOS returns the storage over dir, making its system calls through c.
func newOS(dir string, c calls) *OS {
	return &OS{
		dir:   dir,
		calls: c,
	}
}

func (o *OS) path(name string) string {
	return filepath.Join(o.dir, name)
}

func (o *OS) Rename(oldName, newName string) error {
	return os.Rename(o.path(oldName), o.path(newName))
}

func (o *OS) Remove(name string) error {
	return os.Remove(o.path(name))
}

func (o *OS) List() ([]string, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	return names, nil
}

// file is one open file of an OS.
type file struct {
	f     *os.File
	name  string
	calls calls

	// directMu guards the state of the direct read a ReadAtDirect makes: it
	// holds the descriptor's lazy opening, the read itself and Close's closing
	// of it, so a Close cannot release the descriptor under a read, and a
	// read cannot open one over a Close in flight.
	directMu sync.Mutex
	// directFD is the descriptor the platform's direct read reads through,
	// where it needs one of its own; directUnopened before it is opened,
	// directClosed after Close.
	directFD int
}

const (
	// directUnopened marks a file whose ReadAtDirect has not opened its direct
	// descriptor yet.
	directUnopened = -1
	// directClosed marks a file whose Close ran: every ReadAtDirect after it
	// fails with fs.ErrClosed.
	directClosed = -2
)

var _ storage.File = (*file)(nil)

// newFile wraps f, opened under name, as a file of o.
func (o *OS) newFile(f *os.File, name string) *file {
	return &file{
		f:        f,
		name:     name,
		calls:    o.calls,
		directFD: directUnopened,
	}
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	return f.f.ReadAt(p, off)
}

// ReadAtDirect refuses what ReadAt refuses before the platform's own direct read
// (readDirectLocked, run under directMu) is called. It keeps the
// storage.File contract of ReadAt: the bytes the file gives at off, with
// io.EOF when fewer than len(p) remain.
//
// decision: a filesystem's refusal of the direct read is returned as itself,
// never fallen back to the page cache it exists to bypass. What a refusal
// means for the journal is the open's tail check's to decide, and it may
// fail the open: a direct read is what proves the end of a journal after a
// crash, and a verified record whose direct re-read refuses is re-appended
// from its first copy, so no refusal is widened or worked around here.
// Withdraw it if RFC 28 allows a cached fallback on a filesystem known to
// refuse the direct read.
func (f *file) ReadAtDirect(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: directOp, Path: f.f.Name(), Err: errNegativeOffset}
	}

	f.directMu.Lock()
	defer f.directMu.Unlock()

	// The closed check runs before the nothing-to-read one, so a closed file
	// answers fs.ErrClosed even for a p of length 0, as ReadAt does.
	if f.directFD == directClosed {
		return 0, &fs.PathError{Op: directOp, Path: f.f.Name(), Err: fs.ErrClosed}
	}

	// Before the platform opens anything, so a filesystem refusing the open
	// never fails a read that asks for no byte.
	if len(p) == 0 {
		return 0, nil
	}

	return f.readDirectLocked(p, off)
}

// Close releases the file and the direct descriptor its ReadAtDirect opened.
// The descriptor is marked closed only after it is closed, so the marker never
// hides a descriptor still open. Both happen under directMu, so a ReadAtDirect
// holding it finishes first.
func (f *file) Close() error {
	f.directMu.Lock()
	defer f.directMu.Unlock()

	err := f.f.Close()
	derr := f.closeDirectLocked()
	f.directFD = directClosed

	if derr != nil {
		return errors.Join(err, derr)
	}

	return err
}

func (f *file) Name() string {
	return f.name
}
