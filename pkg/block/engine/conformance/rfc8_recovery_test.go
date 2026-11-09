//go:build engine_conformance

package conformance

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// changesOf is every change the journal yields for file id above from.
func changesOf(
	t *testing.T,
	f *enginetest.Fixture,
	id entity.FileID,
	from journal.Version,
) []journal.Change {
	t.Helper()

	var out []journal.Change

	for c, err := range f.Handle().Since(t.Context(), journal.NewFileID(id), from) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}

		out = append(out, c)
	}

	return out
}

// corrupt flips one byte of p in the journal's durable files, as a disk
// that returns other bytes than it was given, so the record holding p fails
// its check at the next open and is reported as a synced loss.
func corrupt(t *testing.T, f *enginetest.Fixture, p []byte) {
	t.Helper()

	names, err := f.Seam.List()
	if err != nil {
		t.Fatalf("list the journal's files: %v", err)
	}

	for _, name := range names {
		image, err := f.Seam.Durable(name)
		if err != nil {
			continue
		}

		at := bytes.Index(image, p)

		if at < 0 {
			continue
		}

		file, err := f.Seam.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}

		if _, err := file.WriteAt([]byte{^image[at]}, int64(at)); err != nil {
			t.Fatalf("flip a byte of %s: %v", name, err)
		}

		if err := file.Sync(); err != nil {
			t.Fatalf("sync %s: %v", name, err)
		}

		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}

		return
	}

	t.Fatal("the written bytes are in none of the journal's durable files")
}

// A truncate whose journal step the crash left without its removal record:
// the journal yields its marker, the restart applies it before the file is
// served, and the journal is settled.
func TestRFC8A_RecoveryThroughSince(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	write(t, f, id, 0, bytes.Repeat([]byte("a"), 8192))

	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	v, err := f.Handle().Truncate(t.Context(), journal.NewFileID(id), 4096)
	if err != nil {
		t.Fatalf("journal Truncate: %v", err)
	}

	f.Crash()

	if got := f.Committed(h).Size; got != 4096 {
		t.Errorf("committed size after the restart %d, want 4096 applied before serving", got)
	}

	if cs := changesOf(t, f, id, journal.VersionFromBytes(f.Committed(h).Applied)); len(cs) != 0 {
		t.Errorf("the journal yields %v above the applied version, want nothing (marker %v)", cs, v)
	}
}

// A write, a truncate to the size the write left and a write past it replay
// in the journal's order: the file ends 12 KiB long, with a hole between.
func TestRFC8A_RecoveryReappliesInVersionOrder(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	p := bytes.Repeat([]byte("a"), 4096)

	write(t, f, id, 0, p)

	v, err := f.Handle().Truncate(t.Context(), journal.NewFileID(id), 4096)
	if err != nil {
		t.Fatalf("journal Truncate: %v", err)
	}

	write(t, f, id, 8192, p)
	f.Crash()

	got := f.Committed(h)

	if got.Size != 12288 {
		t.Errorf("committed size %d after the restart, want 12288 (truncate at %v)", got.Size, v)
	}

	sp, err := f.E.Allocation(t.Context(), f.Share.ID, id, 4096)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 4096 || sp.Length != 4096 {
		t.Errorf("Allocation(4096) = %+v, %v, want the hole [4096, 8192)", sp, err)
	}
}

// A synced write the journal lost reads as lost after the restart, never as
// the file was before the write, and charges nothing.
func TestRFC8A_SyncedLossReadsLost(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	p := bytes.Repeat([]byte("lost"), 1024)

	write(t, f, id, 0, p)

	// Synced, not committed: the loss record stays above the file's applied
	// version, so the replay yields it (RFC 8 §2.5 step 3).
	if _, err := f.Handle().Sync(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	corrupt(t, f, p)
	f.Crash()

	got := f.Committed(h)

	if got.Size != 4096 || got.Charged != 0 {
		t.Errorf("committed size %d and charged %d, want 4096 and 0", got.Size, got.Charged)
	}

	dst := make([]byte, 4096)

	if _, err := f.E.Read(t.Context(), f.Share.ID, id, 0, dst); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("Read = %v, want ErrLost", err)
	}
}

// heldRelease leaves the journal a delete marker for a file a holder will
// refuse to release: the file keeps its name.
func heldRelease(t *testing.T, f *enginetest.Fixture) (entity.Handle, entity.FileID) {
	t.Helper()

	h, id := f.File("a")
	write(t, f, id, 0, []byte("not offloaded"))

	if _, err := f.Handle().Delete(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("journal Delete: %v", err)
	}

	return h, id
}

// A release a holder refuses, found as a marker at start, is settled without
// its removal and reported as lost: the file stays.
func TestRFC8A_AbortedReleaseAtStart(t *testing.T) {
	f := enginetest.New(t)
	h, id := heldRelease(t, f)

	f.Crash()

	if _, err := f.FS.Store.Existence().Size(t.Context(), f.Share.ID, id); err != nil {
		t.Errorf("the file after the abort: %v, want it kept", err)
	}

	if got := f.Committed(h); got.Applied != (entity.JournalVersion{}) && got.Size != 0 {
		t.Errorf("file %+v, want it as before the release", got)
	}
}

// The restart after an aborted release is quiet: the marker is not found
// again, and nothing is reported.
//
// Red twice: the journal's Settle is not persisted, so the marker is yielded
// again until a later operation or a repack passes it.
func TestRFC8A_AbortedReleaseNextRestartQuiet(t *testing.T) {
	f := enginetest.New(t)
	_, id := heldRelease(t, f)

	f.Crash()
	f.Restart()

	cs := changesOf(t, f, id, journal.Version{})

	isDelete := func(c journal.Change) bool {
		return c.Kind == journal.ChangeDelete
	}

	if slices.ContainsFunc(cs, isDelete) {
		t.Errorf("the journal yields the discarded marker again: %v", cs)
	}
}

// A release whose journal step is done and whose first transaction stalled
// when the process died is finished by the restart: the file is gone.
func TestRFC8A_ReleaseLeftMidRetryFinishedByRestart(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, []byte("to release"))

	ns := f.FS.Store.Namespace()

	if _, err := ns.Unlink(t.Context(), f.FS.Admin, f.FS.Root, []byte("a")); err != nil {
		t.Fatalf("Unlink: %v", err)
	}

	f.Stall()

	if _, err := f.Handle().Delete(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("journal Delete: %v", err)
	}

	f.Crash()

	_, err := f.FS.Store.Existence().Size(t.Context(), f.Share.ID, id)
	if !errors.Is(err, entity.ErrStale) {
		t.Errorf("the released file's record: %v, want it gone", err)
	}
}

// The share's usage after recovery is the bytes re-applied, exactly, and a
// second restart leaves it unchanged.
func TestRFC8A_RecoveryUsage(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	before, err := f.FS.Store.Capacity().Usage(t.Context(), f.Share.ID)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	write(t, f, id, 0, bytes.Repeat([]byte("a"), 8192))
	f.Crash()

	got, err := f.FS.Store.Capacity().Usage(t.Context(), f.Share.ID)
	if err != nil || got.Bytes != before.Bytes+8192 {
		t.Fatalf("usage after recovery %+v, %v, want %d bytes", got, err, before.Bytes+8192)
	}

	f.Restart()

	again, err := f.FS.Store.Capacity().Usage(t.Context(), f.Share.ID)
	if err != nil || again != got {
		t.Errorf("usage after a second restart %+v, %v, want %+v unchanged", again, err, got)
	}
}
