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
	// decision: a read-only journal adds no refusal of its own here; the
	// layer's answers are RFC 1's. A file whose header-only record the
	// transition undid fails every Sync with the bare ErrJournalReadOnly
	// (§5.4: "every Sync that waits on one fails"); a file with a write in a
	// failed window gets that failure once, as a SyncError wrapping the
	// refusal, and the Sync after it reports only what happened since (§5.3);
	// a file whose records were all synced before the transition, or a Sync
	// naming no file, answers nil with its marks, since nothing it waits on
	// was failed. The engine reads the bare refusal as "the removal was
	// undone" and retries a removal whose Sync answered nil, so a blanket
	// refusal here would fail removals the journal made durable. Overturned
	// if RFC 1 §5.4 comes to refuse every Sync on a read-only journal, which
	// would change the engine's undid contract with it.
	return c.dur.Sync(ctx, ids...)
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
