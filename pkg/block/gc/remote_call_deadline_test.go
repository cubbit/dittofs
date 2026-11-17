package gc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// deadlineDeleteStore is a fault wrapper over the memory remote whose
// DeleteBlock fails the way a call that reached its deadline does, and deletes
// nothing.
type deadlineDeleteStore struct {
	*remotememory.Store
}

func (s deadlineDeleteStore) DeleteBlock(_ context.Context, blockID string) error {
	return fmt.Errorf("s3 delete block %s: %w", blockID, context.DeadlineExceeded)
}

// A delete that reaches its deadline means the store is not answering: the
// pass stops there and leaves the remaining objects for the next run, instead
// of paying one more deadline per object while it holds the remote's lock.
func TestReclaimOrphanObjects_TimedOutDeleteEndsThePass(t *testing.T) {
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	for _, id := range []string{"blk-orphan-1", "blk-orphan-2", "blk-orphan-3"} {
		putBareBlock(t, rbs, id, time.Now().Add(-2*time.Hour))
	}

	rep, err := ReclaimOrphanObjects(t.Context(), map[string]struct{}{}, deadlineDeleteStore{rbs},
		ReclaimOptions{GracePeriod: time.Hour})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReclaimOrphanObjects err = %v, want the delete's DeadlineExceeded", err)
	}

	if rep.Errors != 1 {
		t.Fatalf(
			"ReclaimOrphanObjects Errors = %d, want 1 (the pass stops at the first timeout)",
			rep.Errors,
		)
	}
}

func TestReclaimRecords_TimedOutDeleteEndsThePass(t *testing.T) {
	st := badgertest.NewInMemory(t)

	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	putZeroRefRecord(t, st, rbs, "blk-zero-1", 64)
	putZeroRefRecord(t, st, rbs, "blk-zero-2", 64)

	rep, err := ReclaimRecords(
		t.Context(),
		[]ReclaimMetaView{st},
		deadlineDeleteStore{rbs},
		ReclaimOptions{},
	)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReclaimRecords err = %v, want the delete's DeadlineExceeded", err)
	}

	if rep.Errors != 1 {
		t.Fatalf(
			"ReclaimRecords Errors = %d, want 1 (the pass stops at the first timed-out delete)",
			rep.Errors,
		)
	}

	for _, id := range []string{"blk-zero-1", "blk-zero-2"} {
		if !recordExists(t, st, id) {
			t.Fatalf("record %s removed although its delete never succeeded", id)
		}
	}
}

func TestGCMarkSweep_TimedOutBlockDeleteEndsTheSweep(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare(t, "share-a")

	dead := []block.ContentHash{
		hashFromString("deadline-dead-1"),
		hashFromString("deadline-dead-2"),
	}
	for _, h := range dead {
		seedRemoteChunk(t, st, rs, h)
	}

	stats := collectGarbageBlocks(t, rec, st, deadlineDeleteStore{rs}, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})

	if stats.ErrorCount != 1 {
		t.Fatalf(
			"ErrorCount = %d, want 1 (the sweep stops at the first timed-out delete); errors=%v",
			stats.ErrorCount,
			stats.FirstErrors,
		)
	}

	for _, h := range dead {
		if !chunkOnRemote(t, st, h) {
			t.Fatalf("chunk %s lost its marker or record although no delete succeeded", h)
		}
	}
}
