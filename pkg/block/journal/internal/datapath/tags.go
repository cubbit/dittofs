package datapath

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// checkTag refuses a call through s that names id. Every call that names a
// FileID runs it as its first step after its context and handle checks, a
// read's as much as a write's. The index takes the file's lock shared,
// creates no entry and claims nothing, so a read may run it; a file with no
// entry, or one nobody owns, checks clean.
// A file another share owns, or one the journal rebuilt under two tags, is
// refused with core.ErrOtherShare naming both tags, serving, appending,
// changing and offering nothing of it.
//
// The claim is not made here: an appending call claims the file at its
// publish, where the index's WriteRetain re-checks the tag and claims the
// untagged entry in the same update that notes the record. The check below
// and that claim run under the file's append domain — a call holds it from
// before its check until its publish returns — so checking a file's tag and
// claiming it are one step: two first writes of one new FileID through two
// handles cannot both find the file untagged. A call refused between the two
// claims nothing, for the refusal precedes its draw and its record.
//
// decision: the tag is checked per file, not per extent. A file belongs to
// one share for its life, so one tag per file is the whole check, at a few
// bytes per file. Revisit if anything ever moves a file between shares
// inside one journal. The check catches a fault only while a record of the
// file is on disk: the index keeps the tag while any segment holds a record,
// even after the file's last extent was released and its entry dropped, and
// once repack drops the last record the next appending call claims the file
// afresh, as recovery would.
func (c *Core) checkTag(s *Share, id core.FileID) error {
	return c.index.CheckTag(s.Tag, id)
}
