package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// unbuiltSync is a handle whose Sync is not built, as the journal's stub is.
type unbuiltSync struct{ engine.Handle }

func (unbuiltSync) Sync(context.Context, ...journal.FileID) ([]entity.SyncMark, error) {
	return nil, journal.ErrNotImplemented
}

// A commit over a journal whose Sync is not built answers that error at once:
// a retry cannot make a stub succeed, so the Close has no backoff to wait on
// either.
func TestACommitOverAJournalWithNoSyncAnswersItAtOnce(t *testing.T) {
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		return unbuiltSync{Handle: h}
	}})

	f := u.create(t, "a")
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("abc"), engine.WriteOpts{}); err != nil {
		t.Fatal(err)
	}

	// The deadline is a backstop for the failure this test guards, a commit
	// retrying for good over a clock that never moves.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := u.e.CommitExistence(ctx, u.share.ID, f); !errors.Is(err, journal.ErrNotImplemented) {
		t.Fatalf("CommitExistence = %v, want %v", err, journal.ErrNotImplemented)
	}

	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := <-closing(u.e, ctx); err != nil {
		t.Errorf("Close = %v, want nil: nothing is left due to wait on", err)
	}
}
