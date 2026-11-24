package journaltest

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/sim"
)

// fatalRecorder is a testing.TB whose Fatal stops the calling goroutine and
// keeps the message instead of failing the test. Everything else reaches the
// real test, so cleanups and the profiler run as they would.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *fatalRecorder) Fatal(args ...any) {
	r.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

// TestFixtureReportsItsOwnFailures pins that a journal the fixture cannot
// set up fails the test as the fixture's failure, never as the check's.
func TestFixtureReportsItsOwnFailures(t *testing.T) {
	eio := errors.New("injected EIO")
	fault, err := sim.NewSchedule(1, nil, sim.Rule{Op: "*", Key: "*", Action: sim.FaultAction{Err: eio}})
	if err != nil {
		t.Fatal(err)
	}

	r := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		newFixture(r, fault, true, FixtureSegmentCap, nil)
	}()
	<-done

	if !strings.HasPrefix(r.fatal, "fixture: ") {
		t.Fatalf("a journal whose every storage call fails was reported as %q, want a message starting %q", r.fatal, "fixture: ")
	}
}

// errorRecorder is a testing.TB that keeps what Errorf reports instead of
// failing the test.
type errorRecorder struct {
	testing.TB
	errors []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// TestAbandonLeavesTheJournalUnclosed pins that an abandoned fixture's
// cleanup calls nothing on its journal or handles, so a call blocked in them
// cannot hang it.
func TestAbandonLeavesTheJournalUnclosed(t *testing.T) {
	r := &errorRecorder{TB: t}
	f := &Fixture{tb: r, J: &journal.Journal{}, handles: map[int]*journal.Handle{0: {}}}

	f.Abandon()
	f.close()

	if len(r.errors) != 0 || len(f.handles) != 0 {
		t.Fatalf("an abandoned fixture's cleanup reported %q and kept %d handles, want it to touch nothing", r.errors, len(f.handles))
	}
}

// The calls that may be made on a fixture with no journal in its directory,
// each checked for what it leaves behind on both of its outcomes, so the
// checks hold with the journal stubbed and once it exists.

// TestShutdownOnNoJournalIsIdle pins that shutting down a journal that is not
// open changes nothing and makes no call on the seam.
func TestShutdownOnNoJournalIsIdle(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	f.Shutdown()
	f.Shutdown()

	if f.J != nil {
		t.Fatalf("a shut fixture has a journal: %v", f.J)
	}
	for _, op := range []string{kit.OpClose, kit.OpSync, kit.OpSyncDir} {
		if n := f.Seam.Calls(op); n != 0 {
			t.Fatalf("shutting down with no journal made %d %s calls", n, op)
		}
	}
}

// TestTryReopenAsKeepsTheIdentity pins that an open that fails leaves the
// journal shut and the fixture's identity as it was: on an empty directory
// every open fails, whatever identity it expects.
func TestTryReopenAsKeepsTheIdentity(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	for _, expect := range []journal.JournalID{{}, {1}} {
		if err := f.TryReopenAs(expect); err == nil {
			t.Fatalf("opening an empty directory as %x succeeded", expect)
		}
		if f.J != nil {
			t.Fatalf("a failed open left a journal: %v", f.J)
		}
		if f.ID() != (journal.JournalID{}) {
			t.Fatalf("a failed open as %x changed the fixture's identity to %x", expect, f.ID())
		}
	}
	if err := f.TryReopen(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory with the fixture's identity: error %v, journal %v", err, f.J)
	}
}

// TestTryInitRecordsOnSuccessOnly pins that TryInit makes its identity the
// fixture's only when Init succeeded, and opens nothing either way.
func TestTryInitRecordsOnSuccessOnly(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	id, err := f.TryInit()

	switch {
	case err != nil && f.ID() != (journal.JournalID{}):
		t.Fatalf("Init failed (%v) and the fixture's identity is %x, want it unchanged", err, f.ID())
	case err == nil && (id == (journal.JournalID{}) || f.ID() != id):
		t.Fatalf("Init returned identity %x and the fixture's is %x, want the same and not zero", id, f.ID())
	}
	if f.J != nil {
		t.Fatalf("TryInit opened a journal: %v", f.J)
	}
}

// TestTryRestampLeavesTheJournalShut pins that a restamp opens nothing and,
// on an empty directory, fails as an open does.
func TestTryRestampLeavesTheJournalShut(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	if err := f.TryRestamp(journal.InstallationID{1}); err == nil {
		t.Fatalf("restamping an empty directory succeeded")
	}
	if f.J != nil {
		t.Fatalf("TryRestamp left a journal open: %v", f.J)
	}
}

// TestTryCrashReturnsTheRefusalOrTheOpensError pins that a crash the seam
// refuses, and an open that fails after one it made, come back as errors
// with the journal shut.
func TestTryCrashReturnsTheRefusalOrTheOpensError(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	file, err := f.Seam.Create("seg")
	if err != nil {
		t.Fatalf("create a file: %v", err)
	}

	// A crash keeping a write with none unsynced is refused by the seam, and
	// a refused crash changes nothing: the file created is still open.
	if err := f.TryCrash(kit.Keeping(1)); !errors.Is(err, kit.ErrCrashRefused) {
		t.Fatalf("TryCrash of a crash the seam refuses returned %v, want the seam's refusal", err)
	}

	if _, err := file.WriteAt([]byte("a"), 0); err != nil {
		t.Fatalf("a write after the refused crash: %v; the refusal crashed the storage", err)
	}

	if err := f.TryCrash(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory after a crash: error %v, journal %v", err, f.J)
	}
}

// TestTryKillReturnsTheOpensError pins that an open that fails after the
// process was killed comes back as an error with the journal shut.
func TestTryKillReturnsTheOpensError(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	if err := f.TryKill(); err == nil || f.J != nil {
		t.Fatalf("opening an empty directory after a kill: error %v, journal %v", err, f.J)
	}
}

// TestTryKillEndsTheOldProcess pins that a kill closes the files the old
// process held, so a write through one fails from then on.
func TestTryKillEndsTheOldProcess(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil)
	file, err := f.Seam.Create("seg")
	if err != nil {
		t.Fatalf("create a file: %v", err)
	}

	_ = f.TryKill() // the open fails for want of a format file; the kill is what is checked

	if _, err := file.WriteAt([]byte("a"), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a write through the killed process's file returned %v, want an error matching fs.ErrClosed", err)
	}
}

// TestAwaitSparesFailsTheTestOnASettlingShortPool pins the fixture's failure
// when the pool cannot reach its spares: a journal whose spare makes all
// fail settles short, the wait the open handed over answers the spare
// refusal, and the fixture fails its own test naming the pool, before any
// check runs — a check never reads a journal whose pool it cannot account
// for. The fault is armed between the init and the open, on the real seam's
// spare creates, so the pool that settles short is a real one.
func TestAwaitSparesFailsTheTestOnASettlingShortPool(t *testing.T) {
	eio := errors.New("injected EIO")

	r := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f := bareFixture(r, nil, FixtureSegmentCap, nil)
		if _, err := f.TryInit(); err != nil {
			r.Fatalf("fixture: init: %v", err)
		}
		if err := f.Seam.Schedule(kit.Failing(kit.OpCreate, "*.spare.tmp", 0, eio)); err != nil {
			r.Fatalf("fixture: schedule the spare fault: %v", err)
		}
		f.open(true)
		f.WaitSpares()
	}()
	<-done

	if !strings.HasPrefix(r.fatal, "fixture: ") ||
		!strings.Contains(r.fatal, "the journal's pool did not reach its spares") {
		t.Fatalf("a pool that settled short was reported as %q, want the fixture's own failure naming it", r.fatal)
	}
}

// TestFixtureHandsOpenTheMarksItsSyncsRecorded pins that the marks the
// fixture's Sync records reach Open on Crash and on Reopen: a correct mark
// lets both open, and a recorded mark past the stream's synced tail makes the
// next open refuse the range it names. Each leg syncs into a fresh fixture,
// so the mark names the newest segment of its stream, the one whose tail an
// open checks against it.
func TestFixtureHandsOpenTheMarksItsSyncsRecorded(t *testing.T) {
	synced := func(t *testing.T) *Fixture {
		t.Helper()
		f := NewUnwatched(t)
		id := File(0)
		if _, err := f.Share(0).WriteAt(t.Context(), id, 0, []byte(strings.Repeat("a", 4096)), 1); err != nil {
			t.Fatalf("write: %v", err)
		}

		marks, err := f.Sync(0, id)
		if err != nil || len(marks) == 0 {
			t.Fatalf("Sync returned %d marks and %v, want the stream's mark and no error", len(marks), err)
		}
		if len(f.Marks) != len(marks) || f.Marks[0] != marks[0] {
			t.Fatalf("the fixture recorded marks %+v, want the %+v its Sync returned", f.Marks, marks)
		}
		return f
	}

	t.Run("correct", func(t *testing.T) {
		f := synced(t)
		if err := f.TryCrash(); err != nil {
			t.Fatalf("an open after a crash with the correct marks recorded: %v, want nil", err)
		}

		// A later sync of the same stream replaces its recorded mark.
		if _, err := f.Share(0).WriteAt(t.Context(), File(0), 4096, []byte("b"), 1); err != nil {
			t.Fatalf("write: %v", err)
		}
		marks, err := f.Sync(0, File(0))
		if err != nil || len(marks) != 1 {
			t.Fatalf("the second Sync returned marks %+v and %v, want one mark and no error", marks, err)
		}
		if len(f.Marks) != 1 || f.Marks[0] != marks[0] {
			t.Fatalf("after a second sync the fixture recorded %+v, want only the newer mark %+v", f.Marks, marks[0])
		}
		if err := f.TryReopen(); err != nil {
			t.Fatalf("a reopen with the correct marks recorded: %v, want nil", err)
		}
	})

	// A mark past what the stream synced: the tail it names holds no record.
	for _, reopen := range []string{"crash", "reopen"} {
		t.Run("wrong_"+reopen, func(t *testing.T) {
			f := synced(t)
			f.Marks[0].Through += 4096
			f.Marks[0].Offset += 4096

			var err error
			if reopen == "crash" {
				err = f.TryCrash()
			} else {
				err = f.TryReopen()
			}
			if !errors.Is(err, journal.ErrOpenRefused) {
				t.Fatalf("an open after a %s with a mark past the synced tail returned %v, want ErrOpenRefused", reopen, err)
			}
			if want := fmt.Sprintf("segment %d: a refused range at", f.Marks[0].Segment); !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal is %q, want it to name a range of the mark's segment, %q", err, want)
			}
		})
	}
}
