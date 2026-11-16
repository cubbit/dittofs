package openstate

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The engine's CloneHold over the real table: a clone the store holds no
// hold for is stale, a hold taken is the source's durable open, and a
// release after the hold is gone answers nil.
func TestEngineHoldsTakesAndReleasesAClonesHold(t *testing.T) {
	f := newFixture(t)
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	c := f.cloned(t, src, dst)
	h := EngineHolds(f.Table, f.fs.Store.Opens())
	clone := engine.CloneSrc{Share: src.Share, Shard: src.Shard, File: src.ID, Dst: dst.ID, AsOf: c.Version}

	unheld := clone
	unheld.AsOf = entity.VersionOf(4)
	if _, err := h.Hold(ctx, unheld); !errors.Is(err, entity.ErrStale) {
		t.Errorf("Hold of a clone with no hold record: got %v, want ErrStale", err)
	}
	id, err := h.Hold(ctx, clone)
	if got := f.stored(t, src); err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("Hold: got open %d, %v, stored %+v; want the source's one durable open", id, err, got)
	}
	f.lastBatch(t, dst)
	if err := h.Release(ctx, id); err != nil {
		t.Errorf("Release of the hold: got %v, want nil", err)
	}
	if err := h.Release(ctx, id); err != nil {
		t.Errorf("Release of a hold already gone: got %v, want nil", err)
	}
}

// Open state's answers to a clone's hold cross in the engine's types: a
// shard not yet served and a stopped table are a frozen delay, and any other
// error is itself.
func TestTheHoldsErrorsCrossInTheEnginesTypes(t *testing.T) {
	for _, err := range []error{&DelayError{Cause: DelayFrozen}, ErrStopped} {
		var d *vfserr.Delay
		if got := holdErr(err); !errors.As(got, &d) || d.Cause != vfserr.DelayFrozen {
			t.Errorf("holdErr(%v) = %v, want ErrDelay(DelayFrozen)", err, got)
		}
	}
	if got := holdErr(ErrBadState); !errors.Is(got, ErrBadState) {
		t.Errorf("holdErr(ErrBadState) = %v, want it as itself", got)
	}
}
