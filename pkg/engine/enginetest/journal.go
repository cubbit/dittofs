package enginetest

import (
	"context"
	"errors"
	"io/fs"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// wrapJournal returns j, handing each handle its Share returns to opened,
// and each sync a handle answers without error to synced, when not nil. Once
// crashed is set, its Close answers nil where the crash already closed the
// files it would close.
func wrapJournal(
	j engine.Journal,
	crashed *atomic.Bool,
	opened func(journal.ShareTag, engine.Handle),
	synced func([]journal.FileID, []entity.SyncMark),
) engine.Journal {
	return &wrappedJournal{j, crashed, opened, synced}
}

type wrappedJournal struct {
	engine.Journal
	crashed *atomic.Bool
	opened  func(journal.ShareTag, engine.Handle)
	synced  func([]journal.FileID, []entity.SyncMark)
}

func (j *wrappedJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	j.opened(tag, h)

	return wrappedHandle{h, j.crashed, j}, nil
}

type wrappedHandle struct {
	engine.Handle
	crashed *atomic.Bool
	j       *wrappedJournal // its synced, held by pointer so two handles still compare
}

// Sync does nothing once crashed is set: a dead process syncs nothing, and the
// store never sees what the dead engine commits after the crash. A sync that
// reaches the crashed journal fails its records, which the journal remembers
// for the life of this process, so the next open would drop the unsynced writes
// the crash kept. An error here instead would hold the dead engine's close on
// its last group commit up to the stop bound.
func (h wrappedHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	if h.crashed.Load() {
		return nil, nil
	}

	marks, err := h.Handle.Sync(ctx, ids...)
	if err == nil && h.j.synced != nil {
		h.j.synced(ids, marks)
	}

	return marks, err
}

// Close closes the journal. After a crash, the seam has closed every file the
// journal holds, so its close of them failing is the crash's doing, not the
// journal's, and is not reported.
func (j *wrappedJournal) Close() error {
	err := j.Journal.Close()
	if j.crashed.Load() && errors.Is(err, fs.ErrClosed) {
		return nil
	}
	return err
}
