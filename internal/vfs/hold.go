package vfs

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// held runs attempt as one call of the filesystem, entered once, and is the
// service's one wait inside a call (RFC 17 §5.9): while attempt answers
// ErrDelay with DelayClone, a running clone or copy chunk holding an extent
// of one of files, it waits for every clone registered on those files to
// end, at most the call's hold (vfs.WithHold) in all, and runs attempt again
// from its admission, so the call re-runs its checks once the clone is done.
// attempt has given up every lock and open-state step by the time it
// answers, so the wait holds none of them. Past the hold, at the call's
// deadline or cancel, or at the filesystem's Stop, it answers the last
// attempt's DelayClone; with no hold, or a zero one, it answers it at once.
//
// decision: only DelayClone is waited on. A call behind a clone that waits
// for journal space is answered DelayCapacity by the engine, which does not
// tell it from a journal that is full, so it is answered at once, the
// cause the RFC names for a call past its hold behind such a clone.
// Overturned by the engine telling a clone's wait for space apart.
//
// decision: a call waits for every clone on its files, not only those over
// its extent, since the engine's DelayClone does not say which clone refused
// it; a call is refused only by a clone registered on one of its files, so
// the wait always has a clone to end and never spins. Overturned by calls
// seen held behind a clone on another extent of their file.
func (fs *Filesystem) held(ctx context.Context, files []entity.FileID, attempt func(context.Context) error) error {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return err
	}
	defer done()
	var hold context.Context // the call's hold, from its first wait
	for {
		err := attempt(ctx)
		var d *vfs.Delay
		if !errors.As(err, &d) || d.Cause != vfs.DelayClone {
			return err
		}
		if hold == nil {
			max, _ := vfs.Hold(ctx)
			if max <= 0 {
				return err
			}
			var cancel context.CancelFunc
			hold, cancel = context.WithTimeout(ctx, max)
			defer cancel()
		}
		for _, f := range files {
			if hold.Err() != nil || fs.engine.AwaitClones(hold, f) != nil {
				return err
			}
		}
	}
}
