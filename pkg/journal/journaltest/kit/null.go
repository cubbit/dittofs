package kit

import (
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// NullFS is the platform's storage over a directory with every segment file's
// data calls dropped: a write past the segment header returns at once and is
// only counted, and Sync, Allocate and SyncDir return at once. The format file
// and every segment header are written for real, so the journal opens and
// recognises its files as usual. A journal over it runs its own code whole and
// no I/O behind it, so a benchmark over it measures the journal's Go path.
//
// It is a load, not a double: nothing may assert on its answers, and a read of
// a dropped record returns whatever the file holds there. Use it only where
// nothing reads records back.
type NullFS struct {
	storage.FS
	nulled *atomic.Int64
}

// NewNullFS returns a NullFS over the existing directory dir.
func NewNullFS(dir string) NullFS {
	return NullFS{FS: platform.NewOS(dir), nulled: new(atomic.Int64)}
}

// Nulled returns how many bytes of record writes the storage dropped, which a
// benchmark checks to prove its writes reached the null storage and not a
// device.
func (n NullFS) Nulled() int64 { return n.nulled.Load() }

func (n NullFS) wrap(f storage.File, err error) (storage.File, error) {
	if err != nil || f.Name() == format.FormatFileName || f.Name() == format.FormatTmpName {
		return f, err
	}
	return nullFile{File: f, nulled: n.nulled}, nil
}

// Create creates name as the platform does; a segment file drops its records.
func (n NullFS) Create(name string) (storage.File, error) { return n.wrap(n.FS.Create(name)) }

// Open opens name as the platform does; a segment file drops its records.
func (n NullFS) Open(name string) (storage.File, error) { return n.wrap(n.FS.Open(name)) }

// SyncDir returns at once.
func (n NullFS) SyncDir() error { return nil }

// nullFile drops the record writes of one segment file.
type nullFile struct {
	storage.File
	nulled *atomic.Int64
}

func (f nullFile) WriteAt(p []byte, off int64) (int, error) {
	if off < format.SegmentHeaderSize {
		return f.File.WriteAt(p, off)
	}
	f.nulled.Add(int64(len(p)))
	return len(p), nil
}

func (f nullFile) Sync() error                 { return nil }
func (f nullFile) Allocate(off, n int64) error { return nil }
