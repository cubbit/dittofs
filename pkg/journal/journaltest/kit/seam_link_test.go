package kit

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/sim"
)

func TestLinkSurvivesOnlyAfterSyncDir(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")

	write(t, a, "data", 0)
	syncFile(t, a)

	if err := s.Link("a", "b"); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if exists(s, "b") {
		t.Fatal("after an unsynced link, b exists")
	}

	if got := onDisk(t, s, "a"); got != "data" {
		t.Fatalf("after an unsynced link, a holds %q, want %q", got, "data")
	}

	if err := s.Link("a", "b"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if exists(s, "a") {
		t.Fatal("after a synced link and removal, a still exists")
	}

	if got := onDisk(t, s, "b"); got != "data" {
		t.Fatalf("after a synced link, b holds %q, want %q", got, "data")
	}
}

func TestLinkNeverReplaces(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")
	write(t, a, "new", 0)
	syncFile(t, a)

	b := create(t, s, "b")
	write(t, b, "old", 0)
	syncFile(t, b)

	if err := s.Link("a", "b"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Link onto an existing name: got %v, want fs.ErrExist", err)
	}

	if got := onDisk(t, s, "b"); got != "old" {
		t.Fatalf("after a refused link, b holds %q, want %q", got, "old")
	}
}

func TestLinkKeepsOnlyWhatWasSyncedBeforeIt(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")
	write(t, a, "data", 0)

	if err := s.Link("a", "b"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if got := onDisk(t, s, "b"); got != "" {
		t.Fatalf("a link of an unsynced file, after a crash: b holds %q, want it empty", got)
	}
}

func TestLinkKeepsTheImageOfALostChange(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")
	write(t, a, "old", 0)
	syncFile(t, a)

	sched, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)), sim.Rule{
		Op:     OpSync,
		Key:    "a",
		Count:  1,
		Action: sim.FaultAction{Err: syscall.EIO},
	})
	if err != nil {
		t.Fatal(err)
	}

	s.SetFault(sched)

	write(t, a, "NEW", 0)

	if err := a.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failing Sync: got %v, want EIO", err)
	}

	write(t, a, "x", 3)
	syncFile(t, a)

	if err := s.Link("a", "b"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if got := onDisk(t, s, "b"); got != "oldx" {
		t.Fatalf(
			"a link of a file with a lost change, after a crash: b holds %q, want %q",
			got,
			"oldx",
		)
	}
}

func TestLinkKeepsStaleReads(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")
	write(t, a, "old", 0)
	syncFile(t, a)
	write(t, a, "new", 0)

	s.StaleReads("a")

	if err := s.Link("a", "b"); err != nil {
		t.Fatal(err)
	}

	b, err := s.Open("b")
	if err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 3)
	if _, err := b.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}

	if string(got) != "old" {
		t.Fatalf("a read through the link of a stale file: got %q, want %q", got, "old")
	}
}
