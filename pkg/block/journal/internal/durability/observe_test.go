package durability

import (
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// faultFS is an FS whose every call returns err, and whose files do the same.
type faultFS struct{ err error }

func (f faultFS) Create(string) (storage.File, error) { return faultFile(f), f.err }
func (f faultFS) Open(string) (storage.File, error)   { return faultFile(f), f.err }
func (f faultFS) Rename(string, string) error         { return f.err }
func (f faultFS) Remove(string) error                 { return f.err }
func (f faultFS) List() ([]string, error)             { return nil, f.err }
func (f faultFS) SyncDir() error                      { return f.err }

type faultFile struct{ err error }

func (f faultFile) ReadAt([]byte, int64) (int, error)       { return 0, f.err }
func (f faultFile) ReadAtDirect([]byte, int64) (int, error) { return 0, f.err }
func (f faultFile) WriteAt([]byte, int64) (int, error)      { return 0, f.err }
func (f faultFile) Allocate(int64, int64) error             { return f.err }
func (f faultFile) Sync() error                             { return f.err }
func (f faultFile) Truncate(int64) error                    { return f.err }
func (f faultFile) Size() (int64, error)                    { return 0, f.err }
func (f faultFile) Close() error                            { return f.err }
func (f faultFile) Name() string                            { return "f" }

// The wrapper must stay a storage.FS, and the faults must stay full FS and
// File implementations, so a method added to the seam breaks this build.
var (
	_ storage.FS   = faultFS{}
	_ storage.File = faultFile{}
)

// observedCalls names every error-returning method of storage.FS and
// storage.File; each runs one call through the wrapper and returns its error.
// Name is the one method that cannot fail and is checked separately.
var observedCalls = []struct {
	name string
	call func(fs storage.FS) error
}{
	{"FS.Create", func(fs storage.FS) error { _, err := fs.Create("a"); return err }},
	{"FS.Open", func(fs storage.FS) error { _, err := fs.Open("a"); return err }},
	{"FS.Rename", func(fs storage.FS) error { return fs.Rename("a", "b") }},
	{"FS.Remove", func(fs storage.FS) error { return fs.Remove("a") }},
	{"FS.List", func(fs storage.FS) error { _, err := fs.List(); return err }},
	{"FS.SyncDir", func(fs storage.FS) error { return fs.SyncDir() }},
	{"File.ReadAt", viaFile(func(f storage.File) error { _, err := f.ReadAt(make([]byte, 1), 0); return err })},
	{"File.ReadAtDirect", viaFile(func(f storage.File) error { _, err := f.ReadAtDirect(make([]byte, 1), 0); return err })},
	{"File.WriteAt", viaFile(func(f storage.File) error { _, err := f.WriteAt([]byte{1}, 0); return err })},
	{"File.Allocate", viaFile(func(f storage.File) error { return f.Allocate(0, 1) })},
	{"File.Sync", viaFile(func(f storage.File) error { return f.Sync() })},
	{"File.Truncate", viaFile(func(f storage.File) error { return f.Truncate(0) })},
	{"File.Size", viaFile(func(f storage.File) error { _, err := f.Size(); return err })},
	{"File.Close", viaFile(func(f storage.File) error { return f.Close() })},
}

// viaFile runs call on a file the wrapper returned, from both Create and Open,
// so each File method is checked on both. The file's error is returned; the
// open itself succeeds because the fault is armed only for the file.
func viaFile(call func(storage.File) error) func(storage.FS) error {
	return func(fs storage.FS) error {
		for _, open := range []func(string) (storage.File, error){fs.Create, fs.Open} {
			f, _ := open("a")
			if f == nil {
				return errors.New("no file returned")
			}
			if err := call(f); err != nil {
				return err
			}
		}
		return nil
	}
}

// fileOnlyFS fails only the calls on its files, so a File method's error is
// the one observed and the Create or Open that made the file is clean.
type fileOnlyFS struct{ faultFS }

func (f fileOnlyFS) Create(string) (storage.File, error) { return faultFile(f.faultFS), nil }
func (f fileOnlyFS) Open(string) (storage.File, error)   { return faultFile(f.faultFS), nil }

func TestObserveEntersReadOnlyOnEROFSOnly(t *testing.T) {
	for _, c := range observedCalls {
		for _, tc := range []struct {
			name   string
			err    error
			wantRO bool
		}{
			{"EROFS", syscall.EROFS, true},
			{"wrapped EROFS", &errWrap{syscall.EROFS}, true},
			{"EIO", syscall.EIO, false},
			{"ENOSPC", syscall.ENOSPC, false},
			{"nil", nil, false},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				r := newRigUnstarted(t, 1)
				var base storage.FS = faultFS{tc.err}
				if len(c.name) > 4 && c.name[:4] == "File" {
					base = fileOnlyFS{faultFS{tc.err}}
				}
				got := c.call(r.d.Observe(base))
				if got != tc.err && !(tc.wantRO && errors.Is(got, syscall.EROFS)) {
					t.Fatalf("the call returned %v, want %v unchanged", got, tc.err)
				}
				st := r.d.ReadOnlyState()
				if st.ReadOnly != tc.wantRO {
					t.Fatalf("ReadOnly = %v, want %v", st.ReadOnly, tc.wantRO)
				}
				if tc.wantRO && !errors.Is(st.Err, syscall.EROFS) {
					t.Fatalf("Err = %v, want EROFS", st.Err)
				}
			})
		}
	}
}

// TestObserveKeepsNameAndPassesFilesThrough covers the methods that cannot fail.
func TestObserveKeepsNameAndPassesFilesThrough(t *testing.T) {
	r := newRigUnstarted(t, 1)
	f, err := r.d.Observe(fileOnlyFS{}).Create("a")
	if err != nil || f.Name() != "f" {
		t.Fatalf("Create = %v, %v", f, err)
	}
}

type errWrap struct{ error }

func (e *errWrap) Unwrap() error { return e.error }
