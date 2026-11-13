package model

import (
	"context"
	"iter"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// View is what Compare reads from the journal for one share: the calls a
// caller makes on the share's handle. HandleView serves it from a real
// journal; a test can serve it from anything else, such as a wrapper that
// plants a divergence.
//
// decision: the index's answer — the offloaded bit of each held extent and
// the index's markers — is not in View, because the journal's inspection is
// not wired to it until recovery lands, so every view of the journal as it
// is carries no index. Compare skips only those two answers over such a
// view, and only until the view can serve them; move Index back into View
// when every view must carry it.
type View interface {
	ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (n int, missing []journal.Extent, held []journal.Held, asOf journal.Version, err error)
	Extents(id journal.FileID) ([]journal.Extent, error)
	Since(ctx context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error]
	Files() iter.Seq[journal.FileID]
	DirtyFiles() iter.Seq2[journal.FileID, time.Time]
}

// IndexedView is a View whose journal also exposes its index of a file,
// which carries the offloaded bit of each held extent. The handle never
// answers with the bit, so it is read here, from the journal's inspection.
type IndexedView interface {
	View
	Index(id journal.FileID) ([]journal.IndexEntry, error)
}

// HandleView is the view of one share of a journal: its handle, and the
// journal's inspection for the index.
type HandleView struct {
	*journal.Handle
	Journal *journal.Journal
}

// Index returns the index entries of file id from the journal's inspection.
func (v HandleView) Index(id journal.FileID) ([]journal.IndexEntry, error) {
	return v.Journal.Inspect().File(id)
}
