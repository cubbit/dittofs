//go:build engine_conformance

package conformance

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
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

// A clean stop commits every pending existence in a last group commit and
// forgets no tag: the restart finds nothing above the applied version for the
// journal to yield, the committed size is the write's, and the write reads
// back.
func TestRFC8A_CleanCloseThenRestartReappliesNothing(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	p := []byte("a clean stop")

	write(t, f, id, 0, p)

	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Checked while the process is stopped: the restart's recovery commits an
	// append's existence itself, so a check after it passes with the last group
	// commit skipped.
	f.RestartAfter(func() {
		if got := f.Committed(h).Size; got != int64(len(p)) {
			t.Errorf("committed size after a clean stop %d, want %d", got, len(p))
		}
	})

	applied := journal.VersionFromBytes(f.Committed(h).Applied)
	if cs := changesOf(t, f, id, applied); len(cs) != 0 {
		t.Errorf("the journal yields %v above the applied version, want nothing", cs)
	}

	if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
		t.Errorf("read %q after the restart, want %q", got, p)
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

	// Synced, so the crash keeps all three records for the replay.
	if _, err := f.Handle().Sync(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

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
	_, other := f.File("b")
	p := bytes.Repeat([]byte("lost"), 1024)

	write(t, f, other, 0, []byte("committed"))
	write(t, f, id, 0, p)

	// A fresh file's Commit waits for no recorded mark, so another file's
	// group commit after the sync records the stream's mark, which covers a's
	// record and is handed to the open as the proof it synced. a's own
	// existence stays uncommitted: the loss record stays above its applied
	// version, so the replay yields it (RFC 8 §2.5 step 3).
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := f.E.CommitExistence(t.Context(), f.Share.ID, other); err != nil {
		t.Fatalf("CommitExistence: %v", err)
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

	// Synced, so the crash keeps the write for the replay to re-apply.
	if _, err := f.Handle().Sync(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

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

// holeOffsets are where the removal test below deallocates four KiB, a gap
// apart, so the holes stay separate and the truncate that follows has one to
// drop per batch.
var holeOffsets = []int64{4096, 12288, 20480}

// cutCursor is where the first of the three batches leaves the truncate's
// cursor: the start of the second hole.
const cutCursor = 12288

// readsRemoved checks the file reads as its truncate left it: 2048 bytes not
// yet carved, and a hole from there on.
func readsRemoved(t *testing.T, f *enginetest.Fixture, id entity.FileID) {
	t.Helper()

	sp, err := f.E.Allocation(t.Context(), f.Share.ID, id, 0)
	if err != nil || sp.Offset != 0 || sp.Length != 2048 || sp.Class != entity.Uncarved {
		t.Errorf("Allocation(0) = %+v, %v, want [0, 2048) uncarved", sp, err)
	}

	sp, err = f.E.Allocation(t.Context(), f.Share.ID, id, 4096)
	if err != nil || sp.Offset != 4096 || sp.Length != 0 || sp.Class != entity.Hole {
		t.Errorf("Allocation(4096) = %+v, %v, want a hole at 4096", sp, err)
	}
}

// unfinishedOf is the removals of the share's shard not yet pruned.
func unfinishedOf(t *testing.T, f *enginetest.Fixture) []kvstore.UnfinishedRemoval {
	t.Helper()

	var out []kvstore.UnfinishedRemoval

	for u, err := range f.FS.Store.Content().UnfinishedRemovals(t.Context(), f.Share.ID, 0) {
		if err != nil {
			t.Fatalf("UnfinishedRemovals: %v", err)
		}

		out = append(out, u)
	}

	return out
}

// A removal crashed between two batches resumes at the restart: the extent
// reads as removed before the loop runs and after, and the record is pruned.
func TestRFC8A_RemovalCrashedBetweenBatchesResumes(t *testing.T) {
	f := enginetest.NewWithSmallBatches(t, 14)
	_, id := f.File("a")
	write(t, f, id, 0, bytes.Repeat([]byte("a"), 24576))

	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for _, off := range holeOffsets {
		if err := f.E.Deallocate(t.Context(), f.Share.ID, id, off, 4096); err != nil {
			t.Fatalf("Deallocate: %v", err)
		}
	}

	// The deallocations are pruned first, so the truncate is the only
	// removal the restarts find.
	eventually(t, "the deallocations pruned", func() bool { return len(unfinishedOf(t, f)) == 0 })
	f.HoldBatches()

	if _, err := f.Handle().Truncate(t.Context(), journal.NewFileID(id), 2048); err != nil {
		t.Fatalf("journal Truncate: %v", err)
	}

	f.Crash()

	// The first batch commits and the second fails: the process dies there.
	f.FailUpdatesAfter(1)
	f.ReleaseBatches()
	f.WaitBatches(2)
	f.HoldBatches()
	f.Crash()

	left := unfinishedOf(t, f)
	if len(left) != 1 || left[0].Removal.Cursor != cutCursor {
		t.Fatalf("unfinished at the restart %+v, want one with cursor %d", left, cutCursor)
	}

	readsRemoved(t, f, id)
	f.ReleaseBatches()
	f.WaitBatches(1)
	readsRemoved(t, f, id)

	if got := unfinishedOf(t, f); len(got) != 0 {
		t.Errorf("unfinished removals %v after the loop, want none", got)
	}
}

// A release whose last batch committed and whose prune did not is pruned at
// the restart, and its entry in the unfinished-removal index goes.
func TestRFC8A_RemovalDoneNotPrunedIsPrunedAtRestart(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, []byte("to release"))

	ns := f.FS.Store.Namespace()

	if _, err := ns.Unlink(t.Context(), f.FS.Admin, f.FS.Root, []byte("a")); err != nil {
		t.Fatalf("Unlink: %v", err)
	}

	f.HoldBatches()

	if _, err := f.Handle().Delete(t.Context(), journal.NewFileID(id)); err != nil {
		t.Fatalf("journal Delete: %v", err)
	}

	f.Crash()

	// The last batch committed and the process died before the prune.
	if _, err := f.FS.Store.Content().Resume(t.Context(), f.Share.ID, id); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	f.HoldBatches()
	f.Crash()

	if got := len(unfinishedOf(t, f)); got != 1 {
		t.Fatalf("%d removals unfinished at the restart, want the one done", got)
	}

	if len(f.KeysOf(id)) == 0 {
		t.Fatal("the released file holds no key before its prune, want its removal and fences")
	}

	f.ReleaseBatches()
	f.WaitBatches(1)

	if got := unfinishedOf(t, f); len(got) != 0 {
		t.Errorf("unfinished removals %v after the prune, want none", got)
	}

	if got := f.KeysOf(id); len(got) != 0 {
		t.Errorf("the released file still holds %d keys after its prune: %q", len(got), got)
	}
}

// A dirty extent a Commit's drain took away is not placed found at open again
// by an in-process reopen: the drain is the journal's genuine loss reader —
// its read marks the events served — so the failed window's loss records the
// reopen finds on disk were answered once already, and the reopened journal
// publishes nothing for them. The drain's read is the only one that answers;
// a Stats figure of the same events would mark nothing served, and the
// reopen would re-report the loss as new, the duplicate RFC 1 §9.4's row
// rules out.
func TestRFC8A_ALossTheDrainServedIsNotRefoundAtTheReopen(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")

	// The dirty extent: a write whose window the journal fails drops it and
	// leaves its loss records on disk.
	write(t, f, id, 0, []byte("lost in a failed window"))
	f.FailNextWindow()

	if _, seq, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil || seq != 1 {
		t.Fatalf("Commit over the failed window: sequence %d, %v, want 1: the drain it ran carried the loss away", seq, err)
	}

	// The close runs no batch — nothing is due — and the reopen places found
	// at open only what no drain reached.
	f.Restart()

	if seq := f.E.LossSeq(f.Share.ID, id); seq != 0 {
		t.Errorf("loss sequence after the reopen = %d, want 0: the loss a drain served stays served past the stop, never placed found at open again", seq)
	}
}

// A loss no drain of the ended instance ever reached IS there at its reopen,
// the mirror of the leg above: a dirty extent written through the raw handle
// holds nothing pending in the engine, so its failed sync window publishes
// live loss events no engine sync drains, and the close that follows — an
// engine holding nothing due runs no last batch — drains nothing either. The
// ended instance's claims die unread, so the reopen places the loss records
// found at open again, into its own ring, and the drain of the reopened
// process applies them: the file's loss sequence rises over the loss the
// ended process never told a client. The leg above asserts the same reopen
// silent over a loss a Commit's drain took; this one is its standing
// opposite, proving the fixture can see a found-at-open event through the
// public surface when the journal places one — the answer the row's
// revert-check rests on it being able to give.
func TestRFC8A_ALossNoDrainReachedIsReportedAtTheReopen(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	p := bytes.Repeat([]byte("lost"), 1024)

	// The dirty extent: held by the journal alone, through the raw handle,
	// so the engine holds no pending operation and no batch of its own can
	// drain the ring.
	if _, err := f.Handle().WriteAt(t.Context(), journal.NewFileID(id), 0, p, 0); err != nil {
		t.Fatalf("journal write of the dirty extent: %v", err)
	}
	f.FailNextWindow()

	// The journal's own timer sync fails the window and publishes the loss
	// events live; no engine sync ever reads them.
	f.Clock.Advance(time.Second)
	eventually(t, "the journal's timer sync fails the window", func() bool {
		for _, j := range f.E.Stats().Journals {
			if j.Stats.Counters.SyncFailures > 0 && len(j.Stats.Losses) > 0 {
				return true
			}
		}
		return false
	})

	// Nothing is due, so the close runs no last batch and no drain: the
	// reopen is the loss's first reader.
	f.Restart()

	// The found-at-open event is in the reopened journal's ring, naming the
	// lost extent, and the drain of the reopened process applies it.
	found := false
	for _, j := range f.E.Stats().Journals {
		for _, ev := range j.Stats.Losses {
			if ev.AtOpen && ev.File == journal.NewFileID(id) {
				found = true
			}
		}
	}
	if !found {
		t.Error("the reopened journal holds no found-at-open event naming the extent: the un-drained loss is not re-placed at the reopen")
	}

	if seq := f.E.LossSeq(f.Share.ID, id); seq != 1 {
		t.Errorf("loss sequence after the reopen = %d, want 1: the found-at-open event reached the drain and raised the lost file", seq)
	}
}
