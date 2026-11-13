package enginetest

// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so the fixture wraps those two calls to pass
// the journal as the engine's Journal. It also stands in for the sync marks
// the engine cannot act on yet, and drops the ones the journal's Sync does
// return. Every other method is the journal's own.

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// wrapJournal returns j as the engine's Journal. The handles its Share
// returns are the engine's Handle, each handed to opened, and their Reserve
// returns the engine's Reservation.
func wrapJournal(j *journal.Journal, opened func(journal.ShareTag, engine.Handle)) engine.Journal {
	return &wrappedJournal{j, opened}
}

var (
	_ engine.Handle      = handle{}
	_ engine.Reservation = (*journal.Reservation)(nil)
)

type wrappedJournal struct {
	*journal.Journal
	opened func(journal.ShareTag, engine.Handle)
}

func (j *wrappedJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	j.opened(tag, handle{h})
	return handle{h}, nil
}

// decision: the journal's Sync returns its per-stream marks now, but the
// journal's Stats.SyncMarks and its own MarksRecorded -- what the group
// commit reads and hands its recorded marks -- still do not exist, so over
// the real journal the engine still records none and no stability point
// waits on one. These two stubs and handle.Sync's drop of the marks stand in
// for all of it and withdraw together with the journal's own reporting:
// real marks reaching marksWait (commit.go) while g.marks.recorded comes
// from this stub would wait on a mark nothing ever records, blocking the
// stability point forever.
func (j *wrappedJournal) SyncMarks() []entity.SyncMark    { return nil }
func (j *wrappedJournal) MarksRecorded([]entity.SyncMark) {}

type handle struct{ *journal.Handle }

func (h handle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	_, err := h.Handle.Sync(ctx, ids...)
	return nil, err
}

func (h handle) Reserve(n int64, writes int) (engine.Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}
	return r, nil
}
