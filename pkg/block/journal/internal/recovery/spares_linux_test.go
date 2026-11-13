package recovery

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The fallocate flags the fixture's punch passes (falloc.h: FALLOC_FL_PUNCH_HOLE
// keeps the size while freeing the range's blocks, with FALLOC_FL_KEEP_SIZE).
// The stdlib's syscall carries the call, but not the flags — and golang.org/x/sys
// may not leave internal/platform, which the journal's import rule holds to.
const (
	fallocFlPunchHole = 0x2
	fallocFlKeepSize  = 0x1
)

// TestSparesPhaseUnlinksASpareWithAnAllocatedHole plants a spare of the cap's
// size whose middle a punch holed, over the operating system's own storage
// rather than the crash seam: nothing in its bytes differs from a spare the
// pool made — a hole reads as zeros — so only the platform's allocation query
// can catch it, and the phase unlinks it and adopts nothing, leaving its id
// for the pool to make again. The row never asks the query itself: the guard
// below asks the punch's own effect — the blocks it freed — and where a
// filesystem answers neither extents nor holes the phase's answer will fail
// the row here as anywhere, never skip it away.
func TestSparesPhaseUnlinksASpareWithAnAllocatedHole(t *testing.T) {
	dir := t.TempDir()
	fs := platform.NewOS(dir)

	// The format file, written and synced the way the fixture plants it.
	f, err := fs.Create(format.FormatFileName)
	if err != nil {
		t.Fatalf("fixture: create %s: %v", format.FormatFileName, err)
	}
	if _, err := f.WriteAt(
		format.AppendFormatFile(nil, format.FormatFile{Identity: journalID, Installation: install}),
		0,
	); err != nil {
		t.Fatalf("fixture: write %s: %v", format.FormatFileName, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fixture: sync %s: %v", format.FormatFileName, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("fixture: close %s: %v", format.FormatFileName, err)
	}
	if err := fs.SyncDir(); err != nil {
		t.Fatalf("fixture: sync dir: %v", err)
	}

	// The spare: created, allocated whole and synced, so the device holds
	// every byte of it written.
	id := core.SegmentID(6)
	name := format.SegmentName(id)
	if f, err = fs.Create(name); err != nil {
		t.Fatalf("fixture: create %s: %v", name, err)
	}
	if err = f.Allocate(0, testCap); err != nil {
		t.Fatalf("fixture: allocate %s: %v", name, err)
	}
	if err = f.Sync(); err != nil {
		t.Fatalf("fixture: sync %s: %v", name, err)
	}
	if err = f.Close(); err != nil {
		t.Fatalf("fixture: close %s: %v", name, err)
	}
	if err = fs.SyncDir(); err != nil {
		t.Fatalf("fixture: sync dir: %v", err)
	}

	// A punch through the middle, on a handle the fixture opened itself, the
	// shape a spare whose recycle a crash cut short of re-filling could hold.
	// The punch begins at the block boundary before the file's midpoint and
	// runs to its end: a punch inside one block of the filesystem's
	// allocation size — or across two halves of blocks — is zeroed in place
	// and deallocates nothing, so no extent hole exists and nothing, the
	// guard below included, could see it.
	var fi syscall.Stat_t
	if err = syscall.Stat(filepath.Join(dir, name), &fi); err != nil {
		t.Fatalf("fixture: stat %s: %v", name, err)
	}
	blk := int64(fi.Blksize)
	from := testCap / 2 / blk * blk
	if from <= 0 {
		t.Skipf(
			"%s allocates in %d-byte blocks, so a %d-byte spare holds no whole block past its midpoint to punch out",
			name,
			blk,
			testCap,
		)
	}

	punched, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("fixture: open %s: %v", name, err)
	}
	if err = syscall.Fallocate(
		int(punched.Fd()),
		fallocFlPunchHole|fallocFlKeepSize,
		from,
		testCap-from,
	); err != nil {
		punched.Close()
		t.Skipf("%s does not support punching a hole (FALLOC_FL_PUNCH_HOLE: %v)", name, err)
	}
	if err = punched.Close(); err != nil {
		t.Fatalf("fixture: close %s: %v", name, err)
	}

	// The guard asks the punch's own effect, never the query the phase asks:
	// blocks the punch freed are the hole, whatever the platform would answer
	// over it. A punch that freed nothing zeroed in place — the filesystem
	// deallocates no block inside a block boundary — so no extent hole exists
	// and nothing, the phase included, could see one.
	var after syscall.Stat_t
	if err = syscall.Stat(filepath.Join(dir, name), &after); err != nil {
		t.Fatalf("fixture: stat %s: %v", name, err)
	}
	if after.Blocks >= fi.Blocks {
		t.Skipf("%s zeroed the punch in place (%d blocks before, %d after), so no allocated hole exists to catch", name, fi.Blocks, after.Blocks)
	}

	st, err := Run(context.Background(), Deps{
		FS:         fs,
		Clock:      kit.NewStopwatchClock(),
		SegmentCap: testCap,
	}, core.Version{}, journalID, nil)

	if err != nil {
		t.Fatalf("Run over the holed spare: %v", err)
	}
	if got, want := st.Spares.Unlinked, []core.SegmentID{id}; !slices.Equal(got, want) {
		t.Errorf("spares: the holed spare got unlinking %v, want %v: the size is the cap's, so the allocation query is what finds it", got, want)
	}
	if len(st.Spares.Adopted) != 0 {
		t.Errorf("spares: the holed spare was adopted %v, want nothing: its middle is a hole the pool's writes would fall into", st.Spares.Adopted)
	}
	names, err := fs.List()
	if err != nil {
		t.Fatalf("listing after the run: %v", err)
	}
	if slices.Contains(names, name) {
		t.Errorf("spares: the holed spare %s is still in the directory %v, want it unlinked", name, names)
	}
}
