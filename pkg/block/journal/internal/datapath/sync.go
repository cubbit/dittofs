package datapath

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Sync returns once every record of the named files appended before the call
// is durable, through the durability layer's Sync, whose answers it returns as
// they are: the per-stream marks on success, a *core.SyncError for a failed
// window, reported once per file, ctx.Err() when ctx is done first,
// core.ErrClosed once the layer stopped.
//
// A done ctx is refused first, then a handle whose close began, then a file
// of another share, with core.ErrOtherShare naming both tags; a refusal syncs
// nothing, clears no file's mark and returns no marks, the files of this share
// it names included. No lock domain is held across the wait: the tag checks
// release the file index before it, and the file's append domain is never
// taken, since the layer's worker takes it to complete the sync. The call
// counts as in flight on the handle, so the handle's Close waits for it.
func (c *Core) Sync(ctx context.Context, s *Share, ids ...core.FileID) ([]core.SyncMark, error) {
	exit, err := beginCall(ctx, s)
	if err != nil {
		return nil, err
	}
	defer exit()
	for _, id := range ids {
		if err := c.checkTag(s, id); err != nil {
			return nil, err
		}
	}
	marks, err := c.dur.Sync(ctx, ids...)
	// decision: a read-only journal acknowledges no Sync (RFC 1 §5.4, and
	// §3.1's "on a journal turned read-only Sync fails with
	// ErrJournalReadOnly"), so every one answers the read-only refusal, a
	// Sync naming only files synced before the transition or no file at all
	// included, though such a Sync issues no sync and its marks would be
	// true. The refusal is asked after the layer's Sync, not before, so a
	// failed window's report still reaches the first Sync of each file it
	// names as a SyncError, which wraps the same refusal. Overturned if a
	// caller is seen to need the marks of files a read-only journal had
	// already synced, which would answer those with their marks.
	if err == nil {
		if err = c.dur.Admit(); err != nil {
			return nil, err
		}
	}
	return marks, err
}

// latestRecord returns the whole place of the file's newest record the index
// names, and that record's sequence number: Config.Latest, which a Sync that
// names a file holding nothing unsynced takes its mark from. The index names
// the place but not the number or the length, so the record's header is read
// through its stream, which the caller never holds and which no file's append
// domain is held across. A header that does not read leaves the file
// unmarked: a mark is built only from numbers the storage itself names.
func (c *Core) latestRecord(id core.FileID) (segment.Location, core.Seq, bool) {
	var best index.RecordAt
	for _, e := range c.index.FileEntries(id) {
		if e.Record == (index.RecordAt{}) {
			continue // a marker names no record
		}
		if best == (index.RecordAt{}) || e.Record.Segment > best.Segment ||
			(e.Record.Segment == best.Segment && e.Record.Offset > best.Offset) {
			best = e.Record
		}
	}
	if best == (index.RecordAt{}) {
		return segment.Location{}, 0, false
	}
	head := make([]byte, format.RecordHeaderSize)
	if _, err := c.streams.For(id).ReadAt(best.Segment, head, int64(best.Offset)); err != nil {
		return segment.Location{}, 0, false
	}
	h, err := format.DecodeRecordHeader(head, c.identity, best.Segment, int64(best.Offset))
	if err != nil {
		return segment.Location{}, 0, false
	}
	return segment.Location{Segment: best.Segment, Off: int64(best.Offset), Len: h.Size()}, h.Seq, true
}
