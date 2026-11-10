//go:build linux

package platform

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestCheckDir_RefusalMatchesErrUnsuitableStorage fails a CheckDir that
// answers a refusal the caller cannot name: the RFC requires the refusal of
// a directory any platform rule applies to to match ErrUnsuitableStorage,
// while the message still names the rule, the directory and what to change.
// The calls and the mount table the probe reads are swapped for the test, as
// Check's own entry-point test swaps them, so no answer depends on the
// host's mounts.
func TestCheckDir_RefusalMatchesErrUnsuitableStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	f := newFake(dir)
	f.fsType = asInt32(magicExt)

	table := realTable
	path := mountinfoPath
	realTable = f.table()
	mountinfoPath = mountTableWith(t, dir, "ext4", "rw,nobarrier")
	t.Cleanup(func() {
		realTable = table
		mountinfoPath = path
	})

	err := CheckDir(dir, nil)
	wantBarrier(t, err, dir, dir, "nobarrier")

	if !errors.Is(err, core.ErrUnsuitableStorage) {
		t.Errorf("got %v, want it matching core.ErrUnsuitableStorage", err)
	}

	if !strings.Contains(err.Error(), "nobarrier") {
		t.Errorf("got %q, want the message naming what to change", err)
	}

	wantAllClosed(t, f)
}

// TestCheckDir_ProbeErrorIsNotUnsuitable fails a CheckDir that turns an
// error met while checking into a refusal: the mount table is read before
// the directory is opened, so pointing CheckDir at a real empty directory
// with the mount table path swapped to a file that does not exist returns
// that error alone, which must not match ErrUnsuitableStorage, so a caller
// can tell a fault to fix from a refusal.
func TestCheckDir_ProbeErrorIsNotUnsuitable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := realDir(t)
	f := newFake(dir)

	table := realTable
	path := mountinfoPath
	realTable = f.table()
	mountinfoPath = filepath.Join(t.TempDir(), "missing-mountinfo")
	t.Cleanup(func() {
		realTable = table
		mountinfoPath = path
	})

	err := CheckDir(dir, nil)
	wantIOError(t, err, "open", mountinfoPath, fs.ErrNotExist)

	if errors.Is(err, core.ErrUnsuitableStorage) {
		t.Fatalf("got %v, want ErrUnsuitableStorage not matching it", err)
	}

	wantAllClosed(t, f)
}
