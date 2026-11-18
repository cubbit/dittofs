package engine

// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so JournalOf wraps those two calls to pass
// the journal as the engine's Journal. It also stands in for the sync marks
// the engine cannot act on yet, and drops the ones the journal's Sync does
// return. Every other method is the journal's own.

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// JournalOf is j as the engine's Journal: what the composition root's
// opener returns over journal.Open.
func JournalOf(j *journal.Journal) Journal {
	return journalOf{j}
}

var (
	_ Handle      = journalHandle{}
	_ Reservation = (*journal.Reservation)(nil)
)

type journalOf struct{ *journal.Journal }

func (j journalOf) Share(tag journal.ShareTag, limit int64, floor journal.Version) (Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	return journalHandle{h}, nil
}

// decision: the journal's Sync returns its per-stream marks now, but the
// journal's Stats.SyncMarks and its own MarksRecorded -- what the group
// commit reads and hands its recorded marks -- still do not exist, so over
// the real journal the engine still records none and no stability point
// waits on one. These two stubs and journalHandle.Sync's drop of the marks
// stand in for all of it and withdraw together with the journal's own
// reporting: real marks reaching marksWait (commit.go) while g.marks.recorded
// comes from this stub would wait on a mark nothing ever records, blocking
// the stability point forever.
func (journalOf) SyncMarks() []entity.SyncMark    { return nil }
func (journalOf) MarksRecorded([]entity.SyncMark) {}

type journalHandle struct{ *journal.Handle }

func (h journalHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	_, err := h.Handle.Sync(ctx, ids...)
	return nil, err
}

func (h journalHandle) Reserve(n int64, writes int) (Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}
	return r, nil
}
