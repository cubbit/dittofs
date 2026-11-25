package content_test

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/node/content"
	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
)

// The engine's CloneHold over the real table: a clone the store holds no
// hold for is stale, a hold taken is the source's durable open, a release
// ends it in the table, and a release after the hold is gone answers nil.
func TestEngineHoldsTakesAndReleasesAClonesHold(t *testing.T) {
	ctx := t.Context()
	w := composed(t)
	clone := w.cloned(t)
	h := content.EngineHolds(w.table, w.fs.Store.Opens())

	unheld := clone
	unheld.AsOf = entity.VersionOf(4)
	if _, err := h.Hold(ctx, unheld); !errors.Is(err, entity.ErrStale) {
		t.Errorf("Hold of a clone with no hold record: got %v, want ErrStale", err)
	}
	id, err := h.Hold(ctx, clone)
	got, rerr := w.fs.Store.Opens().Of(ctx, clone.Share, clone.File)
	if err != nil || rerr != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("Hold: got open %d, %v, stored %+v (%v); want the source's one durable open", id, err, got, rerr)
	}
	if _, err := w.fs.Store.Content().Resume(ctx, clone.Share, clone.Dst); err != nil {
		t.Fatalf("run the clone to its last batch: %v", err)
	}
	if err := h.Release(ctx, id); err != nil {
		t.Errorf("Release of the hold: got %v, want nil", err)
	}
	if err := w.table.Opens().CloneClose(ctx, id); !errors.Is(err, openstate.ErrBadState) {
		t.Errorf("CloneClose of the hold after Release: got %v, want ErrBadState, the hold gone", err)
	}
	if err := h.Release(ctx, id); err != nil {
		t.Errorf("Release of a hold already gone: got %v, want nil", err)
	}
}

// cloned starts w's table, creates a source and a destination in the
// share's root, serves the source's shard, and runs a clone's first phase of
// the source's first 4 KiB onto the destination at version 5, as the engine
// does. It returns that clone's source as the engine names it.
func (w wiring) cloned(t *testing.T) engine.CloneSrc {
	t.Helper()
	ctx := t.Context()
	if err := w.table.Start(ctx, &enginetest.Sink{}, func(entity.FileID) func() { return func() {} }); err != nil {
		t.Fatalf("open state start: %v", err)
	}
	t.Cleanup(w.table.Stop)
	fs := w.fs
	_, src := fs.Create(t, fs.Admin, fs.Root, "src", kvstore.Regular(0o644))
	_, dst := fs.Create(t, fs.Admin, fs.Root, "dst", kvstore.Regular(0o644))
	if err := w.table.Recovery().ServeShard(ctx, fs.Share.ID, entity.ShardID(src.Shard), 1); err != nil {
		t.Fatalf("serve shard: %v", err)
	}
	ex := fs.Store.Existence()
	failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: fs.Share.ID, File: src.ID,
		Ops: []kvstore.PendingOp{{Kind: kvstore.OpWrite, Version: entity.VersionOf(1), Extent: entity.ExtentOf(0, 4096)}}}}, nil)
	if err == nil {
		err = failed[src.ID]
	}
	if err == nil {
		err = ex.Clone(ctx, kvstore.CloneOp{Share: fs.Share.ID, File: dst.ID, Version: entity.VersionOf(5),
			Spec: entity.CloneSpec{SrcShare: fs.Share.ID, Src: src.ID, Len: 4096}}, nil)
	}
	if err != nil {
		t.Fatalf("clone src onto dst: %v", err)
	}
	return engine.CloneSrc{Share: fs.Share.ID, Shard: entity.ShardID(src.Shard), File: src.ID, Dst: dst.ID, AsOf: entity.VersionOf(5)}
}
