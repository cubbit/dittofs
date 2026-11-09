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
// storage.File. An FS row runs its call on the wrapped FS; a File row runs it
// on the file the wrapper returned from Create and, as a separate subtest, on
// the one returned from Open. Name cannot fail and is checked separately.
var observedCalls = []struct {
	name string
	fs   func(fs storage.FS) error
	file func(f storage.File) error
}{
	{name: "FS.Create", fs: func(fs storage.FS) error { _, err := fs.Create("a"); return err }},
	{name: "FS.Open", fs: func(fs storage.FS) error { _, err := fs.Open("a"); return err }},
	{name: "FS.Rename", fs: func(fs storage.FS) error { return fs.Rename("a", "b") }},
	{name: "FS.Remove", fs: func(fs storage.FS) error { return fs.Remove("a") }},
	{name: "FS.List", fs: func(fs storage.FS) error { _, err := fs.List(); return err }},
	{name: "FS.SyncDir", fs: func(fs storage.FS) error { return fs.SyncDir() }},
	{name: "File.ReadAt", file: func(f storage.File) error { _, err := f.ReadAt(make([]byte, 1), 0); return err }},
	{name: "File.ReadAtDirect", file: func(f storage.File) error { _, err := f.ReadAtDirect(make([]byte, 1), 0); return err }},
	{name: "File.WriteAt", file: func(f storage.File) error { _, err := f.WriteAt([]byte{1}, 0); return err }},
	{name: "File.Allocate", file: func(f storage.File) error { return f.Allocate(0, 1) }},
	{name: "File.Sync", file: func(f storage.File) error { return f.Sync() }},
	{name: "File.Truncate", file: func(f storage.File) error { return f.Truncate(0) }},
	{name: "File.Size", file: func(f storage.File) error { _, err := f.Size(); return err }},
	{name: "File.Close", file: func(f storage.File) error { return f.Close() }},
}

// fileOnlyFS fails only the calls on its files, so a File method's error is
// the one observed and the Create or Open that made the file is clean.
type fileOnlyFS struct{ faultFS }

func (f fileOnlyFS) Create(string) (storage.File, error) { return faultFile(f.faultFS), nil }
func (f fileOnlyFS) Open(string) (storage.File, error)   { return faultFile(f.faultFS), nil }

func TestObserveEntersReadOnlyOnEROFSOnly(t *testing.T) {
	for _, c := range observedCalls {
		type variant struct {
			name string
			run  func(fs storage.FS) error
		}
		var variants []variant
		if c.fs != nil {
			variants = []variant{{"", c.fs}}
		} else {
			for _, src := range []string{"Create", "Open"} {
				variants = append(variants, variant{"/via" + src, func(fs storage.FS) error {
					var f storage.File
					var err error
					if src == "Create" {
						f, err = fs.Create("a")
					} else {
						f, err = fs.Open("a")
					}
					if err != nil || f == nil {
						return errors.New("no clean file returned")
					}
					return c.file(f)
				}})
			}
		}
		for _, v := range variants {
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
				t.Run(c.name+v.name+"/"+tc.name, func(t *testing.T) {
					r := newRigUnstarted(t, 1)
					var base storage.FS = faultFS{tc.err}
					if c.file != nil {
						base = fileOnlyFS{faultFS{tc.err}}
					}
					got := v.run(r.d.Observe(base))
					if got != tc.err {
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
}

// TestObserveKeepsNameAndPassesFilesThrough covers the methods that cannot fail.
func TestObserveKeepsNameAndPassesFilesThrough(t *testing.T) {
	r := newRigUnstarted(t, 1)
	fs := r.d.Observe(fileOnlyFS{})
	for name, open := range map[string]func(string) (storage.File, error){"Create": fs.Create, "Open": fs.Open} {
		f, err := open("a")
		if err != nil || f.Name() != "f" {
			t.Fatalf("%s = %v, %v", name, f, err)
		}
	}
}

type errWrap struct{ error }

func (e *errWrap) Unwrap() error { return e.error }
