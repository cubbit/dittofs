package enginetest

import (
	"errors"
	"io/fs"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// wrapJournal returns j, handing each handle its Share returns to opened.
// Once crashed is set, its Close answers nil where the crash already closed
// the files it would close.
func wrapJournal(j engine.Journal, crashed *atomic.Bool, opened func(journal.ShareTag, engine.Handle)) engine.Journal {
	return &wrappedJournal{j, crashed, opened}
}

type wrappedJournal struct {
	engine.Journal
	crashed *atomic.Bool
	opened  func(journal.ShareTag, engine.Handle)
}

func (j *wrappedJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	j.opened(tag, h)
	return h, nil
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
