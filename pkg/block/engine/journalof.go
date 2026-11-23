package engine

// The journal's Share and Reserve return its concrete handle and reservation,
// and Go does not accept a concrete result for a method whose interface
// declares an interface result, so JournalOf wraps those two calls to pass
// the journal as the engine's Journal. It also converts the journal's sync
// marks to the metadata store's, which the two spell alike. Every other method
// is the journal's own.

import (
	"cmp"
	"context"
	"slices"

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

// SyncMarks is the mark of each append stream's last completed sync, the
// journal's Stats.SyncMarks, ordered by stream.
//
// ponytail: a whole Stats is built to read the marks, once per group commit.
// Upgrade to a journal call that answers the marks alone when the commit rate
// shows the rest of the answer in a profile.
func (j journalOf) SyncMarks() []entity.SyncMark {
	m := j.Journal.Stats().SyncMarks
	out := make([]entity.SyncMark, 0, len(m))
	for _, mark := range m {
		out = append(out, entity.SyncMark(mark))
	}
	slices.SortFunc(out, func(a, b entity.SyncMark) int { return cmp.Compare(a.Stream, b.Stream) })
	return out
}

// MarksRecorded hands the journal the marks a committed metadata transaction
// recorded.
func (j journalOf) MarksRecorded(marks []entity.SyncMark) {
	m := make([]journal.SyncMark, len(marks))
	for i, mark := range marks {
		m[i] = journal.SyncMark(mark)
	}
	j.Journal.MarksRecorded(m)
}

type journalHandle struct{ *journal.Handle }

func (h journalHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	marks, err := h.Handle.Sync(ctx, ids...)
	if err != nil {
		return nil, err
	}
	out := make([]entity.SyncMark, len(marks))
	for i, mark := range marks {
		out[i] = entity.SyncMark(mark)
	}
	return out, nil
}

func (h journalHandle) Reserve(n int64, writes int) (Reservation, error) {
	r, err := h.Handle.Reserve(n, writes)
	if err != nil {
		return nil, err
	}
	return r, nil
}
