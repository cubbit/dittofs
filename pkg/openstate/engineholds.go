package openstate

import (
	"context"
	"errors"
	"slices"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// EngineHolds adapts t, over the store's durable opens, to the engine's
// CloneHold, so the engine reaches open state without importing it. An
// orphan CloneClose leaves reaches t's Sink.Closed.
func EngineHolds(t *Table, records OpenRecords) engine.CloneHold {
	return cloneHold{opens: t.Opens(), recovery: t.Recovery(), records: records}
}

// OpenRecords reads a file's durable opens: the store's Opens.
type OpenRecords interface {
	Of(ctx context.Context, share entity.ShareID, file entity.FileID) ([]entity.OpenRecord, error)
}

// cloneHold is the open-state table as the engine's CloneHold.
type cloneHold struct {
	opens    Opens
	recovery Recovery
	records  OpenRecords
}

// Hold answers ErrStale only for a source holding no open of the clone's.
// CloneOpen answers ErrBadState for that and for a share whose open state
// was dropped, so the record is read again to tell them apart; the dropped
// share's answer crosses as itself, which the engine retries.
func (h cloneHold) Hold(ctx context.Context, src engine.CloneSrc) (entity.OpenID, error) {
	id, err := h.opens.CloneOpen(ctx, FileKey{Share: src.Share, Shard: src.Shard, ID: src.File}, CloneSource{Dst: src.Dst, Version: src.AsOf})
	if !errors.Is(err, ErrBadState) {
		return id, holdErr(err)
	}
	recs, rerr := h.records.Of(ctx, src.Share, src.File)
	if rerr != nil {
		return 0, rerr
	}
	if !slices.ContainsFunc(recs, func(r entity.OpenRecord) bool {
		return r.Kind == entity.OpenCloneSource && r.CloneDst == src.Dst && r.CloneVersion == src.AsOf
	}) {
		return 0, entity.ErrStale
	}
	return 0, err
}

func (h cloneHold) Release(ctx context.Context, o entity.OpenID) error {
	err := h.opens.CloneClose(ctx, o)
	if errors.Is(err, ErrBadState) {
		return nil // the hold is already gone
	}
	return holdErr(err)
}

// holdErr is open state's answer to a clone's hold in the engine's types: a
// shard not yet served or a table not running holds the clone for a retry.
func holdErr(err error) error {
	var delay *DelayError
	if errors.As(err, &delay) && delay.Cause == DelayFrozen || errors.Is(err, ErrStopped) {
		return &vfserr.Delay{Cause: vfserr.DelayFrozen}
	}
	return err
}

func (h cloneHold) Served(ctx context.Context, s entity.ShardID) error {
	return holdErr(h.recovery.Served(ctx, s))
}
