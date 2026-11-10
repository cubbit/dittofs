package journal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// installID is the installation ID the checks init with: distinct from any
// identity a journal draws, so a pair the code swapped cannot pass a check.
var installID = InstallationID{0x61}

// seamDir pairs a seam with the directory it serves, so a check can init
// through the seam's view and read the directory through the operating
// system's files, the two routes a caller has.
type seamDir struct {
	seam *kit.Seam
	dir  string
}

// newSeam builds a seam over a fresh empty temporary directory.
func newSeam(t *testing.T) seamDir {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	return seamDir{seam: seam, dir: dir}
}

// init inits a journal in the seam's directory through the seam's view. The
// platform check reads a suitable mount fed to the real verdict as an input:
// a host's own mount table says nothing about a test's temporary directory,
// and only the tests of the refusal feed one that fails a rule.
func (s seamDir) init(t *testing.T) (JournalID, error) {
	t.Helper()
	return Init(context.Background(), s.dir, installID,
		WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
}

// assertNoCalls fails the test when any storage call reached the seam: the
// checks that must reach nowhere read the seam's own counters, a record of
// what reached the real files, rather than a storage built to fail.
func (s seamDir) assertNoCalls(t *testing.T) {
	t.Helper()
	for _, op := range []string{kit.OpCreate, kit.OpOpen, kit.OpRename, kit.OpRemove, kit.OpList, kit.OpSyncDir} {
		if n := s.seam.Calls(op); n != 0 {
			t.Errorf("the call reached the seam: %d %s calls were made", n, op)
		}
	}
}

// mountGather is the gather that answers the probe p for every directory, so
// the real verdict reads the mount a test built for it.
func mountGather(p storage.Probe) storage.Gather {
	return func(string) (storage.Probe, error) {
		return p, nil
	}
}

// create leaves an empty file in the seam's directory, durable, as a
// predecessor's leftover or a foreign entry.
func (s seamDir) create(t *testing.T, name string) {
	t.Helper()
	f, err := s.seam.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	if err := s.seam.SyncDir(); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
}

// listSeam lists the directory through the seam, sorted.
func (s seamDir) listSeam(t *testing.T) []string {
	t.Helper()
	names, err := s.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(names)
	return names
}

// initOS inits a journal in the real directory dir through the operating
// system's files, with a suitable mount fed to the platform check, as
// seamDir.init feeds one, so a host whose own mount table refuses the
// directory cannot shape the answer.
func initOS(t *testing.T, dir string) (JournalID, error) {
	t.Helper()
	return Init(context.Background(), dir, installID, WithMountProbe(platformtest.SuitableGather(dir)))
}

// makeLostAndFound leaves a lost+found holding a file in the real directory
// dir, the RFC's own example of an entry an Init must refuse.
func makeLostAndFound(t *testing.T, dir string) {
	t.Helper()
	inside := filepath.Join(dir, "lost+found", "kept")
	if err := os.MkdirAll(filepath.Dir(inside), 0o700); err != nil {
		t.Fatalf("mkdir lost+found: %v", err)
	}
	if err := os.WriteFile(inside, []byte("kept"), 0o600); err != nil {
		t.Fatalf("write into lost+found: %v", err)
	}
}

// makeStaleTmp leaves a stale format.tmp in the real directory dir, as an
// Init whose process died after syncing the file and before renaming it.
func makeStaleTmp(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, format.FormatTmpName), []byte(content), 0o600); err != nil {
		t.Fatalf("write the stale format.tmp: %v", err)
	}
}

// listOS lists the real directory's names, sorted.
func listOS(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	slices.Sort(names)
	return names
}

// readFormatOS reads and decodes the format file of the real directory dir.
// It fails the test on any damage a correct Init never writes, so the checks
// below need no error branches of their own.
func readFormatOS(t *testing.T, dir string) format.FormatFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, format.FormatFileName))
	if err != nil {
		t.Fatalf("read the format file: %v", err)
	}
	return decodeFormat(t, b)
}

// decodeFormat decodes the bytes of a format file, failing the test on any
// damage a correct Init never writes.
func decodeFormat(t *testing.T, b []byte) format.FormatFile {
	t.Helper()
	f, err := format.DecodeFormatFile(b)
	if err != nil {
		t.Fatalf("decode the format file: %v", err)
	}
	return f
}

// assertFormatWritten checks the one state a finished Init leaves: the
// format file verifies, with the identity Init drew and the installation
// ID it was given, and it is the directory's only entry.
func assertFormatWritten(t *testing.T, dir string, id JournalID) {
	t.Helper()
	f := readFormatOS(t, dir)
	if f.Identity != id {
		t.Errorf("the format file names the identity %v, want the one Init drew %v",
			[16]byte(f.Identity), [16]byte(id))
	}
	if f.Installation != installID {
		t.Errorf("the format file names the installation %v, want %v",
			[16]byte(f.Installation), [16]byte(installID))
	}
	if names := listOS(t, dir); !slices.Equal(names, []string{format.FormatFileName}) {
		t.Errorf("after Init the directory holds %v, want the format file alone", names)
	}
}

// TestInit_EmptyDirWritesFormat fails an Init that writes a format file no
// caller can trust: the one entry a finished Init leaves must verify, with
// the fresh identity Init returns and the installation ID it was given, so
// the caller can record the identity the format file names.
//
// The journal is made twice, through the seam's crash-recording storage and
// through the operating system's files, so a write that passes only by one
// of the two routes fails.
func TestInit_EmptyDirWritesFormat(t *testing.T) {
	t.Run("through the seam", func(t *testing.T) {
		s := newSeam(t)

		id, err := s.init(t)
		if err != nil {
			t.Fatalf("Init: %v", err)
		}
		if id == (JournalID{}) {
			t.Fatal("Init returned the zero identity, want a fresh one")
		}

		b, err := s.seam.Durable(format.FormatFileName)
		if err != nil {
			t.Fatalf("the durable format file: %v", err)
		}
		f, err := format.DecodeFormatFile(b)
		if err != nil {
			t.Fatalf("decode the format file: %v", err)
		}
		if f.Identity != id {
			t.Errorf("the format file names the identity %v, want the one Init returned %v",
				[16]byte(f.Identity), [16]byte(id))
		}
		if f.Installation != installID {
			t.Errorf("the format file names the installation %v, want %v",
				[16]byte(f.Installation), [16]byte(installID))
		}
	})

	t.Run("through the operating system's files", func(t *testing.T) {
		dir := t.TempDir()

		id, err := initOS(t, dir)
		if err != nil {
			t.Fatalf("Init: %v", err)
		}
		if id == (JournalID{}) {
			t.Fatal("Init returned the zero identity, want a fresh one")
		}
		assertFormatWritten(t, dir, id)
	})
}

// TestInit_FormatTmpAlone_Succeeds fails an Init that refuses the one
// leftover an earlier Init can leave: a stale format.tmp is what an Init
// that crashed before its rename leaves, so it does not block the next
// one — it is neither counted in the empty-directory refusal nor left
// behind — and it is replaced, whole, by the new journal's file.
func TestInit_FormatTmpAlone_Succeeds(t *testing.T) {
	t.Run("through the seam", func(t *testing.T) {
		s := newSeam(t)
		s.create(t, format.FormatTmpName)

		id, err := s.init(t)
		if err != nil {
			t.Fatalf("Init over a stale format.tmp: %v", err)
		}
		assertFormatWritten(t, s.dir, id)
	})

	t.Run("through the operating system's files", func(t *testing.T) {
		dir := t.TempDir()
		makeStaleTmp(t, dir, "a stale format.tmp's bytes")

		id, err := initOS(t, dir)
		if err != nil {
			t.Fatalf("Init over a stale format.tmp: %v", err)
		}
		assertFormatWritten(t, dir, id)
	})
}

// TestInit_LostAndFound_NotEmpty fails an Init that lays a journal over
// files it did not write: a directory that holds any entry but a format.tmp
// — the mount root's lost+found is the RFC's own example — is refused with
// ErrNotEmpty, creating nothing and touching nothing that is there.
func TestInit_LostAndFound_NotEmpty(t *testing.T) {
	dir := t.TempDir()
	makeLostAndFound(t, dir)

	if _, err := initOS(t, dir); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("a directory holding lost+found: Init returned %v, want ErrNotEmpty", err)
	}

	if names := listOS(t, dir); !slices.Equal(names, []string{"lost+found"}) {
		t.Errorf("after the refusal the directory holds %v, want the lost+found alone", names)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "lost+found", "kept"))
	if err != nil || string(kept) != "kept" {
		t.Errorf("the refusal touched lost+found's files: read %q, %v, want \"kept\", no error", kept, err)
	}
}

// TestInit_OtherEntryWithFormatTmp_NotEmpty fails an Init that takes a
// stale format.tmp as a licence to write into a directory with other
// entries: the other entry is the refusal, and the stale format.tmp is
// left exactly as it was, for the next Init's write to replace.
func TestInit_OtherEntryWithFormatTmp_NotEmpty(t *testing.T) {
	t.Run("through the seam", func(t *testing.T) {
		s := newSeam(t)
		s.create(t, format.FormatTmpName)
		s.create(t, "lost+found")

		if _, err := s.init(t); !errors.Is(err, ErrNotEmpty) {
			t.Fatalf("a directory holding a format.tmp and another entry: Init returned %v, want ErrNotEmpty", err)
		}

		if want := []string{"format.tmp", "lost+found"}; !slices.Equal(s.listSeam(t), want) {
			t.Errorf("after the refusal the directory holds %v, want %v", s.listSeam(t), want)
		}
	})

	t.Run("through the operating system's files", func(t *testing.T) {
		dir := t.TempDir()
		makeStaleTmp(t, dir, "a stale format.tmp's bytes")
		makeLostAndFound(t, dir)

		if _, err := initOS(t, dir); !errors.Is(err, ErrNotEmpty) {
			t.Fatalf("a directory holding a format.tmp and another entry: Init returned %v, want ErrNotEmpty", err)
		}

		if want := []string{"format.tmp", "lost+found"}; !slices.Equal(listOS(t, dir), want) {
			t.Errorf("after the refusal the directory holds %v, want %v", listOS(t, dir), want)
		}
		b, err := os.ReadFile(filepath.Join(dir, format.FormatTmpName))
		if err != nil || string(b) != "a stale format.tmp's bytes" {
			t.Errorf("the refusal touched the stale format.tmp: read %q, %v", b, err)
		}
	})
}

// TestInit_CrashBeforeTheWriteIsSynced fails an Init whose format.tmp
// survives the crash its own sync did not cover: with the tmp's write made
// but not yet synced, a power loss takes the file away whole — which is
// why the format file is always a rename of a synced tmp, never a write of
// its own — and the directory comes back empty, the state a crashed Init
// must leave before the rename.
func TestInit_CrashBeforeTheWriteIsSynced(t *testing.T) {
	s := newSeam(t)

	// Hold the tmp's Sync and crash while it waits: the crash rolls the
	// unsynced write back to the file's synced image, the empty file the
	// create left.
	sync := s.seam.Hold(t, kit.OpSync, format.FormatTmpName)
	idc := make(chan JournalID, 1)
	errc := make(chan error, 1)
	go func() {
		id, err := s.init(t)
		idc <- id
		errc <- err
	}()
	awaitHoldOrDone(t, sync.Reached(), errc)

	if err := s.seam.Crash(); err != nil {
		t.Fatalf("Crash while the format.tmp's sync waited: %v", err)
	}
	sync.Release()

	id := <-idc
	if id != (JournalID{}) {
		t.Errorf("the crashed Init returned the identity %v, want the zero one", [16]byte(id))
	}
	if err := <-errc; !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the crashed Init returned %v, want the crash's fs.ErrClosed", err)
	}

	if names := s.listSeam(t); len(names) != 0 {
		t.Fatalf("the crash left the directory holding %v, want it empty: the tmp is a file no Sync made durable, so nothing of it stands", names)
	}
}

// makePredecessorTmp leaves in the seam's directory a durable format.tmp
// holding a predecessor's whole format file — the state an Init whose
// process died after the write and sync, before the rename, leaves — with
// an identity the test names, so a check can tell a leftover from a partial
// write of the new journal's own.
func (s seamDir) makePredecessorTmp(t *testing.T) format.FormatFile {
	t.Helper()
	s.create(t, format.FormatTmpName)
	f, err := s.seam.Open(format.FormatTmpName)
	if err != nil {
		t.Fatalf("open the predecessor's format.tmp: %v", err)
	}
	predecessor := format.FormatFile{
		Identity:     core.JournalID{0x0d, 0x0a, 0x07, 0x0a},
		Installation: installID,
	}
	if _, err := f.WriteAt(format.AppendFormatFile(nil, predecessor), 0); err != nil {
		t.Fatalf("write the predecessor's format.tmp: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync the predecessor's format.tmp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the predecessor's format.tmp: %v", err)
	}
	return predecessor
}

// TestInit_CrashBetweenTmpAndRename fails an Init that leaves a directory
// no later Init can recover. The directory starts from a durable format.tmp
// holding a predecessor's whole format file — a predecessor whose process
// died after its write and sync, before its rename — and the new Init is
// crashed at its own rename, a crash that undoes its removal and creation
// and leaves the predecessor's whole synced tmp standing again: the exact
// leftover the RFC says a crashed Init leaves. The next Init must take that
// directory, must not be refused by its own predecessor's leftover, and
// must replace the leftover, whole, with the new journal's file — a power
// cut never leaves the predecessor's bytes under the new identity.
func TestInit_CrashBetweenTmpAndRename(t *testing.T) {
	s := newSeam(t)
	predecessor := s.makePredecessorTmp(t)

	rename := s.seam.Hold(t, kit.OpRename, format.FormatTmpName)
	idc := make(chan JournalID, 1)
	errc := make(chan error, 1)
	go func() {
		id, err := s.init(t)
		idc <- id
		errc <- err
	}()
	awaitHoldOrDone(t, rename.Reached(), errc)

	if err := s.seam.Crash(); err != nil {
		t.Fatalf("Crash with the rename held: %v", err)
	}
	rename.Release()

	id := <-idc
	if id != (JournalID{}) {
		t.Errorf("the crashed Init returned the identity %v, want the zero one", [16]byte(id))
	}
	if err := <-errc; !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the crashed Init returned %v, want the crash's fs.ErrClosed", err)
	}

	// The crash undid the new Init's whole directory log, so the
	// predecessor's synced tmp is back, whole, under its old name.
	b, err := s.seam.Durable(format.FormatTmpName)
	if err != nil {
		t.Fatalf("the predecessor's durable format.tmp: %v", err)
	}
	if got := decodeFormat(t, b); got != predecessor {
		t.Errorf("after the crash format.tmp holds %v, want the predecessor's whole format file %v",
			got, predecessor)
	}

	second, err := s.init(t)
	if err != nil {
		t.Fatalf("Init after the crash: %v", err)
	}
	assertFormatWritten(t, s.dir, second)
}

// TestInit_CrashWhileTheDirectorySyncWaits fails an Init that could leave a
// torn format file behind a rename whose directory entry no sync made
// durable: the rename stands, but a power loss before the SyncDir erases
// it, so the directory comes back without the format file — and without the
// tmp, whose removal and creation the crash undo the same way. Either way
// nothing partial stands: the journal is whole or absent, never a mix, and
// the next Init takes the empty directory.
func TestInit_CrashWhileTheDirectorySyncWaits(t *testing.T) {
	s := newSeam(t)

	syncDir := s.seam.Hold(t, kit.OpSyncDir, "*")
	idc := make(chan JournalID, 1)
	errc := make(chan error, 1)
	go func() {
		id, err := s.init(t)
		idc <- id
		errc <- err
	}()
	awaitHoldOrDone(t, syncDir.Reached(), errc)

	if err := s.seam.Crash(); err != nil {
		t.Fatalf("Crash while the directory sync waited: %v", err)
	}
	syncDir.Release()

	id := <-idc
	if id != (JournalID{}) {
		t.Errorf("the crashed Init returned the identity %v, want the zero one", [16]byte(id))
	}
	if err := <-errc; !errors.Is(err, fs.ErrClosed) {
		t.Errorf("the crashed Init returned %v, want the crash's fs.ErrClosed", err)
	}

	// The rename reached no SyncDir, so the crash undoes the rename — and
	// the create and the removal before it: the directory holds nothing.
	if names, err := s.seam.List(); err != nil || len(names) != 0 {
		t.Fatalf("the crash left the directory holding %v, %v, want it empty: the rename was never made durable", names, err)
	}

	second, err := s.init(t)
	if err != nil {
		t.Fatalf("Init after the crash: %v", err)
	}
	assertFormatWritten(t, s.dir, second)
}

// TestInit_CrashAfterInitReturns fails an Init whose finished format file a
// power loss could erase or tear: the SyncDir Init ends with made the
// rename durable, so a crash after the call returns leaves the format file
// standing, whole, and the durable image the seam keeps — what a power loss
// would leave — verifies to the identity Init returned, with the
// installation ID it was given. This is the check that fails if Init drops
// its directory sync: the seam's durable image then holds no format file at
// all.
func TestInit_CrashAfterInitReturns(t *testing.T) {
	s := newSeam(t)

	id, err := s.init(t)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := s.seam.Crash(); err != nil {
		t.Fatalf("Crash after Init returned: %v", err)
	}

	b, err := s.seam.Durable(format.FormatFileName)
	if err != nil {
		t.Fatalf("the durable format file after the crash: %v", err)
	}
	f := decodeFormat(t, b)
	if f.Identity != id {
		t.Errorf("the durable format file names the identity %v, want the one Init returned %v",
			[16]byte(f.Identity), [16]byte(id))
	}
	if f.Installation != installID {
		t.Errorf("the durable format file names the installation %v, want %v",
			[16]byte(f.Installation), [16]byte(installID))
	}

	// The durable image is exactly what a power cut leaves, not the
	// in-memory directory List still reports: prove the seam honours the
	// directory sync by deleting the SyncDir from Init and watching this
	// assertion fail (Durable then answers ErrNotExist).
	second, err := s.init(t)
	if err == nil {
		t.Fatalf("Init over the journal it already made returned %v, want ErrNotEmpty", second)
	} else if !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Init over the journal it already made returned %v, want ErrNotEmpty", err)
	}
}

// awaitHoldOrDone waits until the hold stops a call, and fails the test on
// a call that returned without ever reaching it.
func awaitHoldOrDone(t *testing.T, reached <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the call returned (%v) without ever reaching the held step", err)
	}
}

// TestInit_UnsuitableChecksBeforeAnyFile fails an Init that checks the
// platform after it has written, or not at all: the check runs before any
// file is created, and before the empty-directory refusal, so an unsuitable
// directory that also holds another entry is refused as unsuitable, not as
// non-empty, and nothing is created. The mount the verdict reads is fed to
// the real one through the option as an input, so no answer depends on the
// host's platform.
func TestInit_UnsuitableChecksBeforeAnyFile(t *testing.T) {
	t.Run("an empty directory", func(t *testing.T) {
		dir := t.TempDir()
		_, err := Init(context.Background(), dir, installID, WithMountProbe(mountGather(platformtest.NoBarrierMount(dir))))
		if !errors.Is(err, ErrUnsuitableStorage) {
			t.Fatalf("an unsuitable directory: Init returned %v, want ErrUnsuitableStorage", err)
		}
		if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "remount with barriers") {
			t.Errorf("the refusal %q names neither the directory nor the rule", err)
		}
		if names := listOS(t, dir); len(names) != 0 {
			t.Errorf("after the refusal the directory holds %v, want it empty", names)
		}
	})

	t.Run("a directory holding another entry", func(t *testing.T) {
		dir := t.TempDir()
		makeLostAndFound(t, dir)

		_, err := Init(context.Background(), dir, installID, WithMountProbe(mountGather(platformtest.NoBarrierMount(dir))))
		if !errors.Is(err, ErrUnsuitableStorage) {
			t.Fatalf("an unsuitable directory holding another entry: Init returned %v, want ErrUnsuitableStorage, not the empty-directory refusal", err)
		}
		if names := listOS(t, dir); !slices.Equal(names, []string{"lost+found"}) {
			t.Errorf("after the refusal the directory holds %v, want the lost+found alone", names)
		}
	})
}

// TestInit_CheckError_ReturnedAsItself fails an Init that turns an error
// met while checking into a refusal, or answers one with it: an error from
// the platform checks — an I/O error, a mount table that cannot be read —
// is returned as itself, matching neither ErrUnsuitableStorage nor
// ErrNotEmpty, so a caller can tell a fault to fix from a refusal, and
// nothing is created. The verdict takes it as the gather's error, so the
// real one runs and answers it.
func TestInit_CheckError_ReturnedAsItself(t *testing.T) {
	probe := &fs.PathError{Op: "open", Path: "/proc/self/mountinfo", Err: errors.New("an I/O error met while reading the mount table")}

	dir := t.TempDir()
	makeLostAndFound(t, dir)

	_, err := Init(context.Background(), dir, installID, WithMountProbe(func(string) (storage.Probe, error) {
		return storage.Probe{}, probe
	}))
	if !errors.Is(err, probe) {
		t.Fatalf("Init returned %v, want the probe's error as itself", err)
	}
	for _, target := range []error{ErrUnsuitableStorage, ErrNotEmpty} {
		if errors.Is(err, target) {
			t.Errorf("the probe's error matches %v, want it matching no refusal", target)
		}
	}
	if names := listOS(t, dir); !slices.Equal(names, []string{"lost+found"}) {
		t.Errorf("after the error the directory holds %v, want the lost+found alone", names)
	}
}

// TestInit_NegativeSetting fails an Init that accepts a negative setting or
// a negative descriptor limit: settings a caller cannot take meaning from
// are refused with ErrInvalidSetting, before any directory access, so the
// directory is touched not at all and the caller can tell an installation
// to raise them.
func TestInit_NegativeSetting(t *testing.T) {
	for name, opts := range map[string][]Option{
		"MaxBytes":           {WithSettings(Settings{MaxBytes: -1})},
		"Streams":            {WithSettings(Settings{Streams: -1})},
		"ExtentLimit":        {WithSettings(Settings{ExtentLimit: -1})},
		"HeadroomRecords":    {WithSettings(Settings{HeadroomRecords: -1})},
		"IdleSealBytes":      {WithSettings(Settings{IdleSealBytes: -1})},
		"SpareTarget":        {WithSettings(Settings{SpareTarget: -1})},
		"a negative budget":  {WithDescriptorBudget(core.NewDescriptorBudget(-1))},
		"after a zeroed one": {WithSettings(Settings{}), WithSettings(Settings{Streams: -1})},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			makeLostAndFound(t, dir)

			// The refusal must come before any directory access, so the
			// seam's own counters say no call was made: a listing that ran
			// would answer the real directory, and an empty-directory
			// refusal with no listing at all cannot pass its own check.
			s := newSeam(t)
			if _, err := Init(context.Background(), dir, installID,
				append(append([]Option{}, opts...), WithStorage(s.seam.FS()))...); !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("a negative %s: Init returned %v, want ErrInvalidSetting", name, err)
			}
			s.assertNoCalls(t)

			if names := listOS(t, dir); !slices.Equal(names, []string{"lost+found"}) {
				t.Errorf("after the refusal the directory holds %v, want the lost+found alone", names)
			}
		})
	}
}

// TestInit_MissingDir_ReturnsTheStorageError fails an Init that answers a
// directory I/O fault with a refusal, or creates the directory: initOS feeds
// a suitable mount through the injected probe on every OS, so the first step
// to fail on a directory that does not exist is the listing, and it returns
// its not-exist error as itself, matching neither ErrNotEmpty nor
// ErrUnsuitableStorage, and the directory is never made, so the caller can
// tell a missing directory from a refused one.
func TestInit_MissingDir_ReturnsTheStorageError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")

	_, err := initOS(t, dir)
	if err == nil {
		t.Fatal("Init on a directory that does not exist returned no error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want the storage's not-exist error matching fs.ErrNotExist", err)
	}
	for _, target := range []error{ErrNotEmpty, ErrUnsuitableStorage} {
		if errors.Is(err, target) {
			t.Errorf("the storage's error matches %v, want it matching no refusal", target)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused Init left %s behind, want nothing created", dir)
	}
}

// TestInit_CtxCheckedFirst fails an Init that writes with a cancelled
// context: a call checks the context first, before any refusal check and
// before it changes any state, and if it is done returns the context's
// error having changed nothing. With the settings resolved, the context is
// the next thing Init consults, so the seam's own counters prove no write
// was attempted.
func TestInit_CtxCheckedFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newSeam(t)
	if _, err := Init(ctx, s.dir, installID, WithStorage(s.seam.FS())); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context: Init returned %v, want context.Canceled", err)
	}
	s.assertNoCalls(t)
	if names := listOS(t, s.dir); len(names) != 0 {
		t.Errorf("after the cancelled call the directory holds %v, want it empty", names)
	}
}

// TestInit_WriteFailureCleansUp fails an Init that leaves its format.tmp
// behind when a write fails: the failure is reported, and the file — which
// the next Init would take for a crashed predecessor's whole synced one —
// is taken away.
func TestInit_WriteFailureCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("the write failed")
	if err := s.seam.Schedule(kit.Failing(kit.OpWrite, format.FormatTmpName, 1, boom)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if _, err := s.init(t); !errors.Is(err, boom) {
		t.Fatalf("a failed format.tmp write: Init returned %v, want the injected one", err)
	}

	if names := s.listSeam(t); len(names) != 0 {
		t.Fatalf("after the failed write the directory holds %v, want it empty", names)
	}
}

// TestInit_SyncFailureCleansUp fails an Init that keeps a synced
// format.tmp when its sync fails: the file is a full copy of what the
// format file would have been, and leaving it tells the caller the journal
// was never made while the directory holds its whole content under the
// tmp's name. The failure is reported and the file taken away.
func TestInit_SyncFailureCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("the sync failed")
	if err := s.seam.Schedule(kit.Failing(kit.OpSync, format.FormatTmpName, 1, boom)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if _, err := s.init(t); !errors.Is(err, boom) {
		t.Fatalf("a failed format.tmp sync: Init returned %v, want the injected one", err)
	}

	if names := s.listSeam(t); len(names) != 0 {
		t.Fatalf("after the failed sync the directory holds %v, want it empty", names)
	}
}

// TestInit_RenameFailureCleansUp fails an Init that leaves a file behind a
// rename the storage failed: whichever side of the rename the failure
// landed on, the caller must find neither a format file — which would say
// the journal exists while the storage may have failed before anything
// moved — nor a leftover format.tmp, and the failure is reported.
func TestInit_RenameFailureCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("the rename failed")
	if err := s.seam.Schedule(kit.Failing(kit.OpRename, format.FormatTmpName, 1, boom)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if _, err := s.init(t); !errors.Is(err, boom) {
		t.Fatalf("a failed rename: Init returned %v, want the injected one", err)
	}

	for _, stale := range []string{format.FormatTmpName, format.FormatFileName} {
		if names := s.listSeam(t); slices.Contains(names, stale) {
			t.Errorf("after the failed rename the directory holds %s, want neither name left behind", stale)
		}
	}
}

// TestInit_SyncDirFailureCleansUp fails an Init that reports success on a
// failed directory sync: without the SyncDir the rename was never made
// durable, so the whole-file crash guarantee does not hold and the failure
// is returned as itself. The journal the failure leaves is one the caller
// was told was never made, so the write is taken away; the removals make no
// directory sync of their own, so a power cut can resurrect the file, and
// this check asserts the in-memory cleanup alone — the recovery path when
// that happens is the refusal the journal left behind forces on the next
// call.
func TestInit_SyncDirFailureCleansUp(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("the directory sync failed")
	if err := s.seam.Schedule(kit.Failing(kit.OpSyncDir, "*", 1, boom)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	id, err := s.init(t)
	if !errors.Is(err, boom) {
		t.Fatalf("a failed directory sync: Init returned %v, want the injected one", err)
	}
	if id != (JournalID{}) {
		t.Errorf("the failing Init returned the identity %v, want the zero one: the caller records the identity only on success", [16]byte(id))
	}

	if names := s.listSeam(t); len(names) != 0 {
		t.Fatalf("after the failed sync the directory holds %v, want it cleaned of the journal whose creation failed", names)
	}
}

// TestInit_FailedInitKeepsAnotherInitsFormat fails an Init whose cleanup
// takes away a journal it did not make: another Init finishes in the same
// directory while this one waits at its sync, then this one fails, and the
// format file standing there names the other journal's identity. Only a
// format file that names the identity this Init drew is its own to remove.
func TestInit_FailedInitKeepsAnotherInitsFormat(t *testing.T) {
	s := newSeam(t)
	boom := errors.New("the sync failed")
	if err := s.seam.Schedule(kit.Failing(kit.OpSync, format.FormatTmpName, 1, boom)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	held := s.seam.Hold(t, kit.OpSync, format.FormatTmpName)

	errc := make(chan error, 1)
	go func() {
		_, err := s.init(t)
		errc <- err
	}()
	awaitHoldOrDone(t, held.Reached(), errc)

	// The other Init's finished journal, put in place while this one waits.
	other := JournalID{0xf0, 0x0d}
	f, err := s.seam.Create(format.FormatFileName)
	if err != nil {
		t.Fatalf("plant the other journal's format file: %v", err)
	}
	b := format.AppendFormatFile(nil, format.FormatFile{Identity: other, Installation: installID})
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("write the other journal's format file: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync the other journal's format file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the other journal's format file: %v", err)
	}
	if err := s.seam.SyncDir(); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
	held.Release()

	if err := <-errc; !errors.Is(err, boom) {
		t.Fatalf("the failing Init returned %v, want the injected failure", err)
	}
	if names := listOS(t, s.dir); !slices.Equal(names, []string{format.FormatFileName}) {
		t.Fatalf("after the failed Init the directory holds %v, want the other journal's format file alone", names)
	}
	if got := readFormatOS(t, s.dir); got.Identity != other {
		t.Errorf("the format file names %v, want the other journal's %v", [16]byte(got.Identity), [16]byte(other))
	}
}
