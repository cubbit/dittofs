package enginetest

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Once the process crashed, its engine's store views never reach the store,
// so a closing engine commits nothing that only recovery may. An existence
// commit answers done, so the closing engine's last group commit ends; every
// other change is refused. The embedded views are nil: a call reaching them
// panics.
func TestACrashedProcessChangesNothingInTheStore(t *testing.T) {
	crashed := new(atomic.Bool)
	crashed.Store(true)
	x, c := fencedExistence{crashed: crashed}, fencedContent{crashed: crashed}
	ctx := t.Context()
	if failed, err := x.Commit(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Errorf("Commit after the crash: %v, %v; want done, with nothing written", failed, err)
	}
	var share entity.ShareID
	var file entity.FileID
	if err := x.Remove(ctx, share, file, 0, kvstore.RemoveOp{}, nil); !errors.Is(err, errCrashed) {
		t.Errorf("Remove after the crash: %v, want errCrashed", err)
	}
	if err := x.Clone(ctx, kvstore.CloneOp{}, nil); !errors.Is(err, errCrashed) {
		t.Errorf("Clone after the crash: %v, want errCrashed", err)
	}
	if _, err := c.Resume(ctx, share, file); !errors.Is(err, errCrashed) {
		t.Errorf("Resume after the crash: %v, want errCrashed", err)
	}
	if err := c.PruneRemovals(ctx, share, file, entity.JournalVersion{}); !errors.Is(err, errCrashed) {
		t.Errorf("PruneRemovals after the crash: %v, want errCrashed", err)
	}
}
