package engine

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block"
	blockgc "github.com/cubbit/dittofs/pkg/block/gc"
	"github.com/cubbit/dittofs/pkg/block/journal"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// lostAnswerStore is a fault wrapper over the memory remote: while loseAnswer is
// set, DeleteBlock applies the delete and then holds its answer until a deadline
// of answerDeadline passes, the way a store that deleted the object but never
// answered looks to a caller with a per-call deadline.
type lostAnswerStore struct {
	*remotememory.Store
	loseAnswer     bool
	answerDeadline time.Duration
}

func (s *lostAnswerStore) DeleteBlock(ctx context.Context, blockID string) error {
	if err := s.Store.DeleteBlock(ctx, blockID); err != nil || !s.loseAnswer {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, s.answerDeadline)
	defer cancel()

	<-callCtx.Done()

	return fmt.Errorf("delete block %s: %w", blockID, callCtx.Err())
}

// uncertainDeleteFixture is one share whose file "a" was flushed through the
// production carve seam into one block X of one chunk, and then removed, so the
// chunk is dead and its synced marker is past the sweep's grace window.
type uncertainDeleteFixture struct {
	seam    *seamFixture
	ms      *badger.BadgerMetadataStore
	mem     *remotememory.Store
	remote  *lostAnswerStore
	rec     *gcMSReconciler
	content []byte
	hash    block.ContentHash
	blockX  string
}

func newUncertainDeleteFixture(t *testing.T, seed int64) *uncertainDeleteFixture {
	t.Helper()
	ctx := t.Context()
	ms := badgertest.NewInMemory(t)
	mem := remotememory.New()

	t.Cleanup(func() { _ = mem.Close() })

	f := &uncertainDeleteFixture{
		seam:   newSeamFixture(t, t.TempDir(), ms, mem, realSink(ms, mem)),
		ms:     ms,
		mem:    mem,
		remote: &lostAnswerStore{Store: mem, answerDeadline: 20 * time.Millisecond},
		rec: &gcMSReconciler{
			stores: map[string]metadata.Store{"share-a": ms},
			order:  []string{"share-a"},
		},
		content: seamRandBytes(64<<10, seed), // below the chunker's minimum: one chunk
	}
	if err := f.seam.jrnl.WriteAt(ctx, "a", 0, f.content); err != nil {
		t.Fatalf("WriteAt(a): %v", err)
	}

	f.seam.flush(t, ctx)

	rows, err := ms.ListFileChunks(ctx, "a")
	if err != nil || len(rows) != 1 {
		t.Fatalf("fixture: file a has %d manifest rows (err %v), want 1", len(rows), err)
	}

	f.hash = rows[0].Hash

	loc, ok, err := ms.GetLocator(ctx, f.hash)
	if err != nil || !ok || loc.BlockID == "" {
		t.Fatalf("fixture: no locator for the chunk of file a: ok=%v err=%v", ok, err)
	}

	f.blockX = loc.BlockID
	// Remove file a: its chunk leaves the live set and is past grace.
	if err := ms.Delete(ctx, rows[0].ID); err != nil {
		t.Fatalf("fixture: delete manifest row %s: %v", rows[0].ID, err)
	}

	ms.MarkSyncedAtForTest(f.hash, time.Now().Add(-2*time.Hour))

	return f
}

// sweep runs one remote GC pass through the lost-answer wrapper.
func (f *uncertainDeleteFixture) sweep(t *testing.T) *blockgc.GCStats {
	t.Helper()

	return collectGarbageBlocks(t, f.rec, f.ms, f.remote, &blockgc.Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Hour,
	})
}

func (f *uncertainDeleteFixture) blockRecord(
	t *testing.T,
	blockID string,
) (block.BlockRecord, bool) {
	t.Helper()

	rec, ok, err := f.ms.GetBlockRecord(t.Context(), blockID)
	if err != nil {
		t.Fatalf("GetBlockRecord(%s): %v", blockID, err)
	}

	return rec, ok
}

func (f *uncertainDeleteFixture) objectExists(t *testing.T, blockID string) bool {
	t.Helper()

	found := false

	if err := f.mem.WalkBlocks(t.Context(), func(id string, _ block.Meta) error {
		if id == blockID {
			found = true
		}

		return nil
	}); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}

	return found
}

// uncertainPass runs the pass whose delete of block X is applied but answered
// only by the deadline, and checks the state it leaves: the object gone, the
// record at count 0 and the marker still naming X.
func (f *uncertainDeleteFixture) uncertainPass(t *testing.T) {
	t.Helper()

	f.remote.loseAnswer = true
	stats := f.sweep(t)
	f.remote.loseAnswer = false

	if stats.ErrorCount != 1 || stats.ObjectsSwept != 0 {
		t.Fatalf(
			"pass with the lost answer: ErrorCount=%d ObjectsSwept=%d, want 1 and 0; errors=%v",
			stats.ErrorCount,
			stats.ObjectsSwept,
			stats.FirstErrors,
		)
	}

	if f.objectExists(t, f.blockX) {
		t.Fatalf("fixture: block %s still on the remote, want the delete applied", f.blockX)
	}

	if rec, ok := f.blockRecord(t, f.blockX); !ok || rec.LiveChunkCount != 0 {
		t.Fatalf(
			"after the lost answer: record of %s ok=%v count=%d, want kept at 0",
			f.blockX,
			ok,
			rec.LiveChunkCount,
		)
	}

	if loc, ok, err := f.ms.GetLocator(t.Context(), f.hash); err != nil || !ok ||
		loc.BlockID != f.blockX {
		t.Fatalf(
			"after the lost answer: marker ok=%v block=%q err=%v, want kept on %s",
			ok,
			loc.BlockID,
			err,
			f.blockX,
		)
	}
}

// TestGCUncertainDelete covers a sweep whose delete of a block reached the
// remote while its answer was lost to the call's deadline: the block is gone,
// but its record and its chunk's synced marker are still in place.
func TestGCUncertainDelete(t *testing.T) {
	// Nothing rewrites the content: the next pass finds the marker, repeats
	// the delete on its own and finishes.
	t.Run("the next pass repeats the delete", func(t *testing.T) {
		f := newUncertainDeleteFixture(t, 0x4386_0001)
		f.uncertainPass(t)

		stats := f.sweep(t)
		if stats.ErrorCount != 0 || stats.ObjectsSwept != 1 {
			t.Fatalf(
				"next pass: ErrorCount=%d ObjectsSwept=%d, want 0 and 1; errors=%v",
				stats.ErrorCount,
				stats.ObjectsSwept,
				stats.FirstErrors,
			)
		}

		if _, ok := f.blockRecord(t, f.blockX); ok {
			t.Fatalf(
				"next pass: record of %s still present, want removed after the repeated delete",
				f.blockX,
			)
		}

		if _, ok, err := f.ms.GetLocator(t.Context(), f.hash); err != nil || ok {
			t.Fatalf("next pass: marker present=%v err=%v, want removed", ok, err)
		}
	})

	// The same content is written again before the next pass. The flush must
	// not dedup onto block X, whose bytes may be gone: it uploads a block of
	// its own, which the read then resolves to. X's record is left for the
	// operator's reclaim.
	t.Run("a rewrite uploads a new block", func(t *testing.T) {
		ctx := t.Context()
		f := newUncertainDeleteFixture(t, 0x4386_0002)
		f.uncertainPass(t)

		if err := f.seam.jrnl.WriteAt(ctx, journal.FileID("b"), 0, f.content); err != nil {
			t.Fatalf("WriteAt(b): %v", err)
		}

		f.seam.flush(t, ctx)

		loc, ok, err := f.ms.GetLocator(ctx, f.hash)
		if err != nil || !ok {
			t.Fatalf("after the rewrite: marker ok=%v err=%v, want present", ok, err)
		}

		got, err := f.mem.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, f.hash)
		if err != nil {
			t.Fatalf(
				"read of the rewritten content from block %s: %v "+
					"(the flush deduped onto a block whose delete was uncertain)",
				loc.BlockID,
				err,
			)
		}

		if !bytes.Equal(got, f.content) {
			t.Fatalf(
				"read of the rewritten content from block %s: %d bytes, differ from the %d written",
				loc.BlockID,
				len(got),
				len(f.content),
			)
		}

		stats := f.sweep(t)
		if stats.ErrorCount != 0 || stats.ObjectsSwept != 0 {
			t.Fatalf(
				"pass after the rewrite: ErrorCount=%d ObjectsSwept=%d, "+
					"want 0 and 0 (the chunk is live); errors=%v",
				stats.ErrorCount,
				stats.ObjectsSwept,
				stats.FirstErrors,
			)
		}

		if rec, ok := f.blockRecord(t, f.blockX); !ok || rec.LiveChunkCount != 0 {
			t.Fatalf("pass after the rewrite: record of %s ok=%v count=%d, want left alone at 0",
				f.blockX, ok, rec.LiveChunkCount)
		}

		rep, err := blockgc.ReclaimRecords(
			ctx,
			[]blockgc.ReclaimMetaView{f.ms},
			f.mem,
			blockgc.ReclaimOptions{},
		)
		if err != nil {
			t.Fatalf("ReclaimRecords: %v", err)
		}

		if _, ok := f.blockRecord(t, f.blockX); ok || rep.Reclaimed.Count != 1 {
			t.Fatalf(
				"reclaim: record of %s present=%v, %d zero-ref reclaimed, want removed and 1",
				f.blockX,
				ok,
				rep.Reclaimed.Count,
			)
		}

		if !f.objectExists(t, loc.BlockID) {
			t.Fatalf("reclaim removed block %s, which holds the rewritten content", loc.BlockID)
		}
	})
}
