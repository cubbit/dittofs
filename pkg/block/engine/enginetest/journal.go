package enginetest

import (
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// wrapJournal returns j as the engine's Journal, engine.JournalOf, handing
// each handle its Share returns to opened.
func wrapJournal(j *journal.Journal, opened func(journal.ShareTag, engine.Handle)) engine.Journal {
	return &wrappedJournal{engine.JournalOf(j), opened}
}

type wrappedJournal struct {
	engine.Journal
	opened func(journal.ShareTag, engine.Handle)
}

func (j *wrappedJournal) Share(tag journal.ShareTag, limit int64, floor journal.Version) (engine.Handle, error) {
	h, err := j.Journal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	j.opened(tag, h)
	return h, nil
}
