package datapath

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Sync returns once every record of the named files appended before the call
// is durable, through the durability layer's Sync, whose answers it returns as
// they are: a *core.SyncError for a failed window, reported once per file,
// ctx.Err() when ctx is done first, core.ErrClosed once the layer stopped.
//
// A done ctx is refused first, then a handle whose close began, then a file
// of another share, with core.ErrOtherShare naming both tags; a refusal syncs
// nothing and clears no file's mark, the files of this share it names
// included. No lock domain is held across the wait: the tag checks release the
// file index before it, and the file's append domain is never taken, since the
// layer's worker takes it to complete the sync. The call counts as in flight
// on the handle, so the handle's Close waits for it.
func (c *Core) Sync(ctx context.Context, s *Share, ids ...core.FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exit, err := s.enter()
	if err != nil {
		return err
	}
	defer exit()
	for _, id := range ids {
		if err := c.checkTag(s, id); err != nil {
			return err
		}
	}
	return c.dur.Sync(ctx, ids...)
}
