package journal

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// openSegmentCap is the segment size the opens here pass, small enough that a
// spare the pool makes is a real file in no time and the spare the open's
// targets want never reach past the temporary directory.
const openSegmentCap = 64 << 10

// openLimit is the generous version limit an open's caller sets once it has
// the journal, as the journal issues nothing until its first limit.
var openLimit = Version{Counter: 1 << 40}

// openSettings returns the settings with a small segment cap, so the pool's
// spares stay small in a temporary directory.
func openSettings() Settings {
	var s Settings
	core.SetSegmentCap(&s, openSegmentCap)
	return s
}

// openOpts returns the options an open takes: the test clock, the settings, a
// suitable mount for the platform check — a host's own mount table says
// nothing about a test's temporary directory, so the real verdict is fed one
// as an input — and the view of the seam. The clock is shared with what the
// caller built before only through the seam's directory; every journal gets a
// fresh one, since the options of an open name a process that has not started
// yet.
func openOpts(seam *kit.Seam, clock core.Clock) []Option {
	return []Option{
		WithClock(clock),
		WithSettings(openSettings()),
		WithStorage(seam.FS()),
	}
}

// openOptsDir is openOpts over s's directory, feeding the check the suitable
// mount for it.
func openOptsDir(s seamDir, clock core.Clock) []Option {
	return openOptsDirAt(s.seam, s.dir, clock)
}

// openOptsDirAt is openOpts over the directory dir the seam serves, feeding
// the check the suitable mount for it.
func openOptsDirAt(seam *kit.Seam, dir string, clock core.Clock) []Option {
	return append(openOpts(seam, clock), WithMountProbe(platformtest.SuitableGather(dir)))
}

// initAndOpen inits a journal through the seam and opens it with the identity
// Init returned, on a fresh clock, no floor and the small-cap settings. The
// journal is closed when the test ends, so its background work never outlives
// a failed assertion.
func initAndOpen(t *testing.T) (*Journal, seamDir, JournalID) {
	t.Helper()

	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	j, err := Open(context.Background(), s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if j == nil {
		t.Fatal("Open returned nil and no error")
	}
	t.Cleanup(func() {
		// A test that closed the journal itself leaves this second Close
		// answering ErrClosed, which is not a failure.
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("close the opened journal: %v", err)
		}
	})
	return j, s, id
}

// TestOpen_FreshRoundTrip fails an Open that never reaches the core: the
// identity Init returns opens the journal, a share attaches, and a write
// through it reads back whole. The whole cycle — init, open, write, read,
// close — runs under the profiler, so an open that starts background work it
// never joins, or a close that leaves it running, fails here.
func TestOpen_FreshRoundTrip(t *testing.T) {
	prof.Watch(t, prof.Options{})

	j, _, id := initAndOpen(t)
	if err := j.SetVersionLimit(openLimit); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	f := fileOf(1)
	v := writeReady(t, h, f, "round trip")
	if v == (Version{}) {
		t.Fatal("the write drew no version")
	}
	p := []byte("..........")
	n, missing, held, asOf, err := h.ReadAt(context.Background(), f, 0, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 10 || string(p) != "round trip" || len(missing) != 0 {
		t.Fatalf("ReadAt = n %d, %q, missing %+v; want the write back whole", n, p, missing)
	}
	if len(held) != 1 || held[0].Version.Compare(v) != 0 || asOf == (Version{}) {
		t.Fatalf("held = %+v, asOf = %v, want the write's version %v", held, asOf, v)
	}

	// A read at a version the identity does not name: the handle works, and the
	// second Share of one tag returns one handle.
	h2, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("second Share: %v", err)
	}
	if h2 != h {
		t.Fatal("a second Share of the open tag returned another handle")
	}
	if _, _, _, _, err := h2.ReadAt(context.Background(), f, 4, p[:4]); err != nil {
		t.Fatalf("ReadAt through the shared handle: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close the handle: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if id == (JournalID{}) {
		t.Fatal("Init returned the zero identity")
	}
}

// TestOpen_WrongIdentity_IdentityErrorNamesFound fails an Open that accepts
// any identity or answers one the caller cannot act on: a directory whose
// format names another journal is refused, and the error names the identity
// found, the installation it was made under and that no segment is held.
func TestOpen_WrongIdentity_IdentityErrorNamesFound(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	var wrong JournalID
	wrong[0] = id[0] ^ 0xff
	_, err = Open(context.Background(), s.dir, Version{}, wrong, openOptsDir(s, kit.NewStopwatchClock())...)
	var ie *IdentityError
	if !errors.As(err, &ie) {
		t.Fatalf("Open with the wrong identity returned %v, want an IdentityError", err)
	}
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("the IdentityError does not match ErrIdentityMismatch")
	}
	if got := JournalID(ie.Found); got != id {
		t.Errorf("the error names the identity %v, want the one the format holds %v", [16]byte(got), [16]byte(id))
	}
	if ie.Installation != installID {
		t.Errorf("the error names the installation %v, want %v",
			[16]byte(ie.Installation), [16]byte(installID))
	}
	if ie.Segments {
		t.Error("the error says a segment is held, want none in a fresh journal")
	}
}

// TestOpen_ExpectZero_IdentityErrorNames fails an Open that invents a journal
// where the caller recorded none: expect zero always refuses, and on a
// directory holding a journal the refusal names the identity and installation
// it found and that no segment is held, so the caller can tell a lost store
// from a lost device.
func TestOpen_ExpectZero_IdentityErrorNames(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	_, err = Open(context.Background(), s.dir, Version{}, JournalID{}, openOptsDir(s, kit.NewStopwatchClock())...)
	var ie *IdentityError
	if !errors.As(err, &ie) {
		t.Fatalf("Open with expect zero returned %v, want an IdentityError", err)
	}
	if got := JournalID(ie.Found); got != id {
		t.Errorf("the error names the identity %v, want the one the format holds %v", [16]byte(got), [16]byte(id))
	}
	if ie.Installation != installID {
		t.Errorf("the error names the installation %v, want %v",
			[16]byte(ie.Installation), [16]byte(installID))
	}
	if ie.Segments {
		t.Error("the error says a segment is held, want none in a fresh journal")
	}
}

// TestOpen_ExpectZero_EmptyDir_ErrNoJournal fails an Open that creates a
// journal where the caller recorded one, or takes an empty directory for a
// lost journal: with expect zero and no journal in the directory, the refusal
// is ErrNoJournal and nothing is created.
func TestOpen_ExpectZero_EmptyDir_ErrNoJournal(t *testing.T) {
	s := newSeam(t)

	_, err := Open(context.Background(), s.dir, Version{}, JournalID{}, openOptsDir(s, kit.NewStopwatchClock())...)
	if !errors.Is(err, ErrNoJournal) {
		t.Fatalf("Open with expect zero on an empty directory returned %v, want ErrNoJournal", err)
	}
	if names := s.listSeam(t); len(names) != 0 {
		t.Errorf("after the refusal the directory holds %v, want nothing created", names)
	}
}

// TestOpen_MissingDir_ErrNoJournal_CreatesNothing fails an Open that checks
// the platform of, or creates anything in, a directory that does not exist:
// it fails as on a directory that holds no journal, before the platform
// checks run, and nothing is made. The directory's absence is read through
// the operating system's files, so the open runs with the open path's own
// default storage rather than one injected. No mount probe is given: the
// platform check never runs on a directory that does not exist, and where it
// ran first it would answer with the real probe's not-exist error, which the
// ErrNoJournal assertion would refuse.
func TestOpen_MissingDir_ErrNoJournal_CreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")

	if _, err := Open(context.Background(), dir, Version{}, JournalID{0x01},
		WithClock(kit.NewStopwatchClock())); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("Open of a missing directory returned %v, want ErrNoJournal", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused Open left %s behind, want nothing created", dir)
	}
}

// TestOpen_EmptyDir_ErrNoJournal_CreatesNothing fails an Open that creates a
// journal in a directory that holds no journal: an empty directory, checked
// and listed whole, is refused with ErrNoJournal and left empty.
func TestOpen_EmptyDir_ErrNoJournal_CreatesNothing(t *testing.T) {
	s := newSeam(t)

	expected := JournalID{0x01}
	if _, err := Open(context.Background(), s.dir, Version{}, expected, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("Open of an empty directory returned %v, want ErrNoJournal", err)
	}
	if names := s.listSeam(t); len(names) != 0 {
		t.Errorf("after the refusal the directory holds %v, want it empty", names)
	}
}

// TestOpen_NeitherFormatNorSegment_ErrNoJournal fails an Open that takes a
// directory of files it does not recognise for a journal or for a lost device:
// names the journal could not have written, with neither a format file nor a
// segment among them, hold no journal, and the directory is left as it is.
func TestOpen_NeitherFormatNorSegment_ErrNoJournal(t *testing.T) {
	s := newSeam(t)
	s.create(t, "a foreign entry")

	expected := JournalID{0x01}
	if _, err := Open(context.Background(), s.dir, Version{}, expected, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("Open of a directory holding only a foreign entry returned %v, want ErrNoJournal", err)
	}
	if names := s.listSeam(t); !slices.Equal(names, []string{"a foreign entry"}) {
		t.Errorf("after the refusal the directory holds %v, want nothing touched", names)
	}
}

// TestOpen_SegmentNoFormat_ErrUnidentified_Untouched fails an Open that
// takes a directory holding segments and no format file for anything else:
// such a journal cannot be identified, so it is refused with ErrUnidentified,
// whatever the caller expected, and written, removed and attached nothing.
// A spare's temporary name counts too: a spare half-made is a segment the
// directory holds, and neither it nor a finished segment is ever mistaken
// for an empty journal.
func TestOpen_SegmentNoFormat_ErrUnidentified_Untouched(t *testing.T) {
	for name, seg := range map[string]string{
		"a segment":                 format.SegmentName(1),
		"a spare's tmp name":        format.SpareName(1),
		"a name of the wrong width": "000000000000000000001.seg",
	} {
		t.Run(name, func(t *testing.T) {
			s := newSeam(t)
			s.create(t, seg)

			expected := JournalID{0x01}
			_, err := Open(context.Background(), s.dir, Version{}, expected, openOptsDir(s, kit.NewStopwatchClock())...)
			if !errors.Is(err, ErrUnidentified) {
				t.Fatalf("a directory holding %s and no format: Open returned %v, want ErrUnidentified", seg, err)
			}
			if names := s.listSeam(t); !slices.Equal(names, []string{seg}) {
				t.Errorf("after the refusal the directory holds %v, want the segment alone", names)
			}
			b, err := s.seam.Durable(seg)
			if err != nil || len(b) != 0 {
				t.Errorf("the refusal touched the segment: %d bytes, %v, want none and no error", len(b), err)
			}
		})
	}
}

// TestOpen_SecondOpenAfterClose_ErrNotImplemented fails an Open that recovers
// a journal this implementation cannot yet: a reopen of a directory whose
// journal has run — even one whose only segments are the spares the spare pool
// made — is refused with ErrNotImplemented, and the directory is left as it
// is, since rebuilding its state is a later step.
func TestOpen_SecondOpenAfterClose_ErrNotImplemented(t *testing.T) {
	j, seam, id := initAndOpen(t)
	if err := j.SetVersionLimit(openLimit); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	writeReady(t, h, fileOf(1), "run")

	// Close the handle and the journal, so every file the run touched is
	// closed and the directory's names are all there is to see. initAndOpen's
	// own cleanup then closes the journal a second time too, relying on a
	// second Close joining the core's own first close and reporting what it
	// did.
	if err := h.Close(); err != nil {
		t.Fatalf("Close the handle: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	names := listOS(t, dirOfSeam(seam))
	segmentsBefore := countSegments(names)
	if segmentsBefore == 0 {
		t.Fatalf("the run left no segment: %v", names)
	}

	_, err = Open(context.Background(), dirOfSeam(seam), Version{}, id, openOptsDir(seam, kit.NewStopwatchClock())...)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("a second Open of a journal that has run returned %v, want ErrNotImplemented", err)
	}
	if got := countSegments(listOS(t, dirOfSeam(seam))); got != segmentsBefore {
		t.Errorf("after the refusal the directory holds %d segment names, want the %d it held", got, segmentsBefore)
	}
}

// TestOpen_UnsuitableChecksBeforeWrite fails an Open that builds the journal
// before the platform checks run, or not at all: the check runs before
// anything is written, and an unsuitable directory is refused with
// ErrUnsuitableStorage, matching no other refusal, with the directory left as
// it is. The mount the verdict reads is fed to the real one through the
// option as an input, so no answer depends on the host's platform.
func TestOpen_UnsuitableChecksBeforeWrite(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	open := append(openOptsDir(s, kit.NewStopwatchClock()), WithMountProbe(mountGather(platformtest.NoBarrierMount(s.dir))))
	_, err = Open(context.Background(), s.dir, Version{}, id, open...)
	if !errors.Is(err, ErrUnsuitableStorage) {
		t.Fatalf("an unsuitable directory: Open returned %v, want ErrUnsuitableStorage", err)
	}
	if !strings.Contains(err.Error(), platformtest.ProbeDir(s.dir)) || !strings.Contains(err.Error(), "remount with barriers") {
		t.Errorf("the refusal %q names neither the directory nor the rule", err)
	}
	for _, target := range []error{ErrNoJournal, ErrNotImplemented, ErrIdentityMismatch} {
		if errors.Is(err, target) {
			t.Errorf("the unsuitable refusal also matches %v, want it matching no other refusal", target)
		}
	}
	if names := s.listSeam(t); !slices.Equal(names, []string{format.FormatFileName}) {
		t.Errorf("after the refusal the directory holds %v, want the format file alone", names)
	}
}

// TestOpen_CheckError_ReturnedAsItself fails an Open that turns an error met
// while checking into a refusal: an I/O error or a mount table that cannot
// be read is returned as itself, matching no refusal, so a caller can tell a
// fault to fix from a directory to refuse. The error is the gather's, so the
// real verdict is what answers it.
func TestOpen_CheckError_ReturnedAsItself(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	probe := &fs.PathError{Op: "open", Path: "/proc/self/mountinfo", Err: errors.New("an I/O error met while reading the mount table")}
	open := append(openOptsDir(s, kit.NewStopwatchClock()), WithMountProbe(func(string) (storage.Probe, error) {
		return storage.Probe{}, probe
	}))

	_, err = Open(context.Background(), s.dir, Version{}, id, open...)
	if !errors.Is(err, probe) {
		t.Fatalf("Open returned %v, want the probe's error as itself", err)
	}
	for _, target := range []error{ErrUnsuitableStorage, ErrNoJournal, ErrNotImplemented} {
		if errors.Is(err, target) {
			t.Errorf("the probe's error matches %v, want it matching no refusal", target)
		}
	}
}

// TestOpen_NoClock fails an Open that reaches its directory with no clock: a
// journal has no default clock to fall back on, so a call without one fails
// with ErrNoClock before any directory access — proven by the seam's own
// counters, which stay at zero — so the directory is left as it was.
func TestOpen_NoClock(t *testing.T) {
	s := newSeam(t)

	if _, err := Open(context.Background(), s.dir, Version{}, JournalID{0x01}, WithStorage(s.seam.FS())); !errors.Is(err, ErrNoClock) {
		t.Fatalf("Open with no clock returned %v, want ErrNoClock", err)
	}
	s.assertNoCalls(t)
	if names := listOS(t, s.dir); len(names) != 0 {
		t.Errorf("after the refusal the directory holds %v, want nothing created", names)
	}
}

// TestOpen_CtxCheckedFirst fails an Open that reads its directory with a
// cancelled context: the context is checked after the settings and the clock
// and before any directory access — proven by the seam's own counters, which
// the cancelled open leaves where Init put them — and its error is returned
// having changed nothing.
func TestOpen_CtxCheckedFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	ops := []string{kit.OpCreate, kit.OpOpen, kit.OpRename, kit.OpRemove, kit.OpList, kit.OpSyncDir}
	before := make([]int, len(ops))
	for i, op := range ops {
		before[i] = s.seam.Calls(op)
	}
	if _, err := Open(ctx, s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context: Open returned %v, want context.Canceled", err)
	}
	for i, op := range ops {
		if after := s.seam.Calls(op); after != before[i] {
			t.Errorf("the cancelled call reached the seam: %d more %s calls were made, want none", after-before[i], op)
		}
	}
	if names := s.listSeam(t); !slices.Equal(names, []string{format.FormatFileName}) {
		t.Errorf("after the cancelled call the directory holds %v, want the format file alone", names)
	}
}

// TestOpen_CorruptFormat_ErrCorrupt fails an Open that repairs or ignores a
// format file whose checksum does not verify: the open fails with ErrCorrupt
// and writes nothing.
func TestOpen_CorruptFormat_ErrCorrupt(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	damageFormat(t, s.dir)

	if _, err := Open(context.Background(), s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of a corrupt format returned %v, want ErrCorrupt", err)
	}
	for _, target := range []error{ErrUnknownFormat, ErrNoJournal, ErrIdentityMismatch} {
		if errors.Is(err, target) {
			t.Errorf("the corrupt refusal also matches %v, want it matching no other refusal", target)
		}
	}
}

// TestOpen_WrongFormatFileLength_ErrCorrupt fails an Open that reads only the
// first FormatFileSize bytes of the format file and casts the rest away: a
// file longer than the format's is damaged like one whose checksum does not
// verify, so every trailing byte makes the open fail with ErrCorrupt, and a
// file exactly FormatFileSize still opens. A file one byte short is the
// damaged case the decoder's own length check already refuses, checked here
// so both wrong lengths are held by one place.
func TestOpen_WrongFormatFileLength_ErrCorrupt(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	appendFormatByte(t, s)
	_, err = Open(context.Background(), s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of a format file one byte long returned %v, want ErrCorrupt", err)
	}
	for _, target := range []error{ErrUnknownFormat, ErrNoJournal, ErrIdentityMismatch} {
		if errors.Is(err, target) {
			t.Errorf("the long refusal also matches %v, want it matching no other refusal", target)
		}
	}

	// Truncate back to the exact size, so the same directory shows the open
	// taking a file of the format's own length again; then one byte below
	// it, the damaged file on the other side.
	resetFormat(t, s)
	j, err := Open(context.Background(), s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...)
	if err != nil {
		t.Fatalf("Open of a format file of the exact size: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	shortenFormat(t, s)
	if _, err := Open(context.Background(), s.dir, Version{}, id, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of a format file one byte short returned %v, want ErrCorrupt", err)
	}
}

// appendFormatByte writes one byte past the format file's end, so the file
// holds a whole format file and one byte more, through the seam's view — the
// route the journal's own open reads.
func appendFormatByte(t *testing.T, s seamDir) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(s.dir, format.FormatFileName))
	if err != nil {
		t.Fatalf("read the format file: %v", err)
	}
	if len(b) != format.FormatFileSize {
		t.Fatalf("the format file is %d bytes, want the format's %d", len(b), format.FormatFileSize)
	}
	f, err := s.seam.Open(format.FormatFileName)
	if err != nil {
		t.Fatalf("open the format file through the seam: %v", err)
	}
	if _, err := f.WriteAt([]byte{0}, format.FormatFileSize); err != nil {
		t.Fatalf("extend the format file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the extended format file: %v", err)
	}
}

// resetFormat takes the format file back to its exact size, through the
// seam's view.
func resetFormat(t *testing.T, s seamDir) {
	t.Helper()

	if err := truncateFormat(s.seam, format.FormatFileSize); err != nil {
		t.Fatalf("restore the format file's size: %v", err)
	}
}

// shortenFormat takes one byte off the format file's end, through the seam's
// view.
func shortenFormat(t *testing.T, s seamDir) {
	t.Helper()

	if err := truncateFormat(s.seam, format.FormatFileSize-1); err != nil {
		t.Fatalf("shorten the format file: %v", err)
	}
}

// truncateFormat sets the format file's size through the seam.
func truncateFormat(seam *kit.Seam, size int64) error {
	f, err := seam.Open(format.FormatFileName)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// TestOpen_UnknownFormatVersion fails an Open that upgrades or ignores a
// format file naming a version this binary does not read: the open fails with
// ErrUnknownFormat and writes nothing. The file stands in the directory
// before the seam is made, so the seam's own view of the files the directory
// holds includes it.
func TestOpen_UnknownFormatVersion(t *testing.T) {
	dir := t.TempDir()
	if err := writeUnknownFormat(t, dir); err != nil {
		t.Fatalf("write the future format: %v", err)
	}
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}

	if _, err := Open(context.Background(), dir, Version{}, JournalID{0x01},
		openOptsDirAt(seam, dir, kit.NewStopwatchClock())...); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Open of an unknown format version returned %v, want ErrUnknownFormat", err)
	}
	for _, target := range []error{ErrCorrupt, ErrNoJournal, ErrIdentityMismatch} {
		if errors.Is(err, target) {
			t.Errorf("the unknown-version refusal also matches %v, want it matching no other refusal", target)
		}
	}
}

// TestOpen_FloorRaisesCounter fails an Open that leaves the counter where a
// crashed journal's records found it: the floor is raised before the journal
// is returned, so the first version the journal issues exceeds the floor even
// before a share of its own has raised it. A fresh journal issues nothing
// until its first limit, so the check sets one before it draws.
func TestOpen_FloorRaisesCounter(t *testing.T) {
	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	floor := Version{Counter: 1 << 30}
	j, err := Open(context.Background(), s.dir, floor, id, openOptsDir(s, kit.NewStopwatchClock())...)
	if err != nil {
		t.Fatalf("Open with a floor: %v", err)
	}
	if err := j.SetVersionLimit(Version{Counter: floor.Counter + 1<<20}); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	v, err := j.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if v.Compare(floor) <= 0 {
		t.Fatalf("the first version drawn = %v, want one above the floor %v", v, floor)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A floor with a non-zero epoch is refused and opens nothing.
	s2 := newSeam(t)
	id2, err := Init(context.Background(), s2.dir, installID, WithMountProbe(platformtest.SuitableGather(s2.dir)), WithStorage(s2.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := Open(context.Background(), s2.dir, Version{Epoch: 1, Counter: 5}, id2, openOptsDir(s2, kit.NewStopwatchClock())...); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Open with an epochal floor returned %v, want ErrUnknownFormat", err)
	}
	if got := countSegments(listOS(t, s2.dir)); got != 0 {
		t.Errorf("the refused open left %d segment names behind, want none: it must create nothing", got)
	}
}

// TestOpen_RefusedFloor_LeaksNothing_CreatesNothing fails an Open that builds
// the core before the floor is refused: a refused floor must leak no
// goroutine and no descriptor — checked under the profiler — and must create
// nothing, so the directory it leaves holds exactly the format file, with no
// spare's segment name ever appearing in it: a spare is a segment file, and a
// journal whose open failed cannot be left holding segments, which no later
// open could refuse cleanly. The check watches a little past the refusal,
// because the spare pool makes its files off the open path and one could land
// after the open returned.
func TestOpen_RefusedFloor_LeaksNothing_CreatesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if _, err := Open(context.Background(), s.dir, Version{Epoch: 1, Counter: 5}, id, openOptsDir(s, kit.NewStopwatchClock())...); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Open with an epochal floor returned %v, want ErrUnknownFormat", err)
	}

	var bad []string
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, name := range listOS(t, s.dir) {
			if name != format.FormatFileName {
				bad = append(bad, name)
			}
		}
		if len(bad) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(bad) > 0 {
		t.Fatalf("a refused open left %v in the directory, want the format file alone", bad)
	}
}

// TestOpen_ConcurrentCalls_IndependentJournalEach fails an Open that shares
// state across directories: four journals opened on four directories at once,
// written through concurrently, serve their own files and stay free of each
// other's. The seams and inits are made before the goroutines, so nothing in
// them may fail the test outright.
func TestOpen_ConcurrentCalls_IndependentJournalEach(t *testing.T) {
	const n = 4
	seams := make([]*kit.Seam, n)
	ids := make([]JournalID, n)
	dirs := make([]string, n)
	for i := range seams {
		s := newSeam(t)
		id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
		if err != nil {
			t.Fatalf("Init: %v", err)
		}
		seams[i] = s.seam
		ids[i] = id
		dirs[i] = s.dir
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range errs {
		wg.Go(func() {
			j, err := Open(context.Background(), dirs[i], Version{}, ids[i], openOptsDirAt(seams[i], dirs[i], kit.NewStopwatchClock())...)
			if err != nil {
				errs[i] = err
				return
			}
			if err := j.SetVersionLimit(openLimit); err != nil {
				errs[i] = err
				return
			}
			h, err := j.Share(1, 0, Version{})
			if err != nil {
				errs[i] = err
				return
			}
			// The spare pool readies its spares off the open path, so the
			// first write may find none yet: retry the refused one, as the
			// caller of a retry-later refusal does.
			var v Version
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				v, err = h.WriteAt(context.Background(), fileOf(1), 0, []byte("each its own"), 1)
				var ns *NoSpaceError
				if errors.As(err, &ns) && ns.Limit == LimitSpare && time.Now().Before(deadline) {
					continue
				}
				break
			}
			if err != nil {
				errs[i] = err
				return
			}
			if v == (Version{}) {
				errs[i] = errors.New("the concurrent write drew no version")
				return
			}
			p := []byte("............")
			if _, _, _, _, err := h.ReadAt(context.Background(), fileOf(1), 0, p); err != nil || string(p) != "each its own" {
				errs[i] = errors.Join(err, errRoundTrip(p))
				return
			}
			if err := h.Close(); err != nil {
				errs[i] = err
				return
			}
			if err := j.Close(); err != nil {
				errs[i] = err
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("journal %d: %v", i, err)
		}
	}
}

// errRoundTrip names a failed round trip what the joined error above cannot.
func errRoundTrip(p []byte) error {
	return errors.New("the read served " + string(p) + ", want the write back")
}

// damageFormat flips a byte of the format file in the real directory dir, so
// its checksum does not verify.
func damageFormat(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, format.FormatFileName))
	if err != nil {
		t.Fatalf("read the format file: %v", err)
	}
	b[8] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, format.FormatFileName), b, 0o600); err != nil {
		t.Fatalf("damage the format file: %v", err)
	}
}

// writeUnknownFormat writes a format file naming a version no binary of this
// format reads, whole and verifying by checksum, so the version alone is what
// refuses it.
func writeUnknownFormat(t *testing.T, dir string) error {
	t.Helper()
	var id JournalID
	id[0] = 0x55
	b := format.AppendFormatFile(nil, format.FormatFile{Identity: id, Installation: installID})
	binary.LittleEndian.PutUint16(b[4:6], 99) // a future version, whose file this is not
	c := crc32.Checksum(b[:52], crc32.MakeTable(crc32.Castagnoli))
	binary.LittleEndian.PutUint32(b[52:56], c)
	return os.WriteFile(filepath.Join(dir, format.FormatFileName), b, 0o600)
}

// dirOfSeam returns the real directory the seam serves, where the seam
// serves its directory as s.dir does; the seam keeps it private, so the
// helper holds the pair the callers of these tests need.
func dirOfSeam(s seamDir) string { return s.dir }

// countSegments returns how many names in names are segment files: a segment
// ends in .seg or a spare's .spare.tmp, recognised or not, format files
// excepted. A spare's finished name is a segment file, so it counts too.
func countSegments(names []string) int {
	n := 0
	for _, name := range names {
		if strings.HasSuffix(name, ".seg") || strings.HasSuffix(name, ".spare.tmp") {
			n++
		}
	}
	return n
}

// TestOpen_SparesReady_WaitsForThePool fails an Open that hands over no
// spare-pool wait, or one that answers before the pool it belongs to has
// filled: the callback receives the wait, the wait blocks while the pool's
// first make is held at the seam, and a write right after it needs no
// spare-retry — the pool the open sized is what filled.
func TestOpen_SparesReady_WaitsForThePool(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	var wait core.SparesWait
	open := append(openOptsDir(s, kit.NewStopwatchClock()), WithSparesReady(func(w core.SparesWait) {
		if w == nil {
			t.Error("the open handed over a nil spare-pool wait")
			return
		}
		wait = w
	}))

	// The pool's very first make is parked at the seam before the open
	// begins, so the wait the callback received cannot answer until it is
	// let go: an answer before that names a wait over a pool it did not
	// watch.
	held := s.seam.HoldFirst(t, kit.OpCreate, "*")
	j, err := Open(context.Background(), s.dir, Version{}, id, open...)
	if err != nil {
		held.Release()
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Errorf("close the opened journal: %v", err)
		}
	}()
	if wait == nil {
		held.Release()
		t.Fatal("the callback Open took was never called")
	}

	answered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		answered <- wait.WaitSpares(ctx)
	}()
	select {
	case err := <-answered:
		held.Release()
		t.Fatalf("WaitSpares answered %v while the pool's first make was still held", err)
	case <-time.After(20 * time.Millisecond):
	}

	held.Release()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("WaitSpares over a filled pool: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WaitSpares never answered once the fill it watched settled")
	}

	// The pool the wait watched is the journal's own: a write through a
	// share, made right after the wait, needs no spare-retry — a journal
	// whose first write rolls finds a spare the pool made. The write
	// carries its own retry, exactly as the fixture's checks do, so a pool
	// the wait never filled still writes and the round trip stands.
	if err := j.SetVersionLimit(openLimit); err != nil {
		t.Fatalf("SetVersionLimit: %v", err)
	}
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	defer func() { _ = h.Close() }()
	v := writeReady(t, h, fileOf(1), "spares ready")
	if v == (Version{}) {
		t.Fatal("the write after the wait drew no version")
	}
}

// TestOpen_SparesReady_CloseDuringTheWait fails a journal whose Close leaves
// the spare-pool wait blocked: the pool's Stop answers it with the journal's
// own closed refusal, and the journal's Close — which stops the pool —
// leaves nothing the wait still parks on.
func TestOpen_SparesReady_CloseDuringTheWait(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newSeam(t)
	id, err := Init(context.Background(), s.dir, installID, WithMountProbe(platformtest.SuitableGather(s.dir)), WithStorage(s.seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	var wait core.SparesWait
	open := append(openOptsDir(s, kit.NewStopwatchClock()), WithSparesReady(func(w core.SparesWait) {
		wait = w
	}))
	held := s.seam.HoldFirst(t, kit.OpCreate, "*")
	j, err := Open(context.Background(), s.dir, Version{}, id, open...)
	if err != nil {
		held.Release()
		t.Fatalf("Open: %v", err)
	}
	if wait == nil {
		held.Release()
		t.Fatal("the callback Open took was never called")
	}

	answered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		answered <- wait.WaitSpares(ctx)
	}()

	// Close stops the pool with the first make still held at the seam, so
	// its join of the worker cannot pass until the hold below lets the make
	// land: Close runs on a goroutine of its own, the wait is answered by
	// the stop — whatever the pool's last pass left — and the release then
	// lets the join return, so nothing of the close leaks past the test.
	closed := make(chan error, 1)
	go func() { closed <- j.Close() }()
	select {
	case err := <-answered:
		if !errors.Is(err, core.ErrClosed) {
			t.Fatalf("WaitSpares over a journal its Close stopped returned %v, want core.ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		held.Release()
		t.Fatal("WaitSpares never answered the Close that stopped its pool")
	}

	held.Release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never joined the make its stop was held on")
	}
}

// TestJournal_SecondCloseIsErrClosed fails a Close that answers a second call
// as if it were the first: the documented answer to a journal already closed
// is ErrClosed, so a caller can tell a double close from a clean one.
func TestJournal_SecondCloseIsErrClosed(t *testing.T) {
	j, _, _ := initAndOpen(t)

	if err := j.Close(); err != nil {
		t.Fatalf("the first Close returned %v, want nil", err)
	}
	if err := j.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("the second Close returned %v, want ErrClosed", err)
	}
}
