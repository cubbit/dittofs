package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Write stages p at off of file f of share s and answers once the journal
// holds it: it makes no store call and waits for nothing but the journal. The
// verifier is sampled before the write is staged. The write's operation is in
// f's entry before Write answers, so a read that starts after reflects it. A
// zero-length write stages nothing and calls o.Done once; a refused one never
// calls it. A range below zero or past the largest offset is ErrInvalid.
func (e *Engine) Write(ctx context.Context, s entity.ShareID, f entity.FileID, off int64, p []byte, o WriteOpts) (vfserr.Verifier, error) {
	if off < 0 || int64(len(p)) > math.MaxInt64-off {
		return vfserr.Verifier{}, vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return vfserr.Verifier{}, err
	}
	v := e.sample(b)
	if len(p) == 0 {
		if o.Done != nil {
			o.Done()
		}
		return v, nil
	}
	now := e.d.Clock.Now() // one reading: the entry keeps the time the journal records
	var mtime int64        // nanoseconds since the Unix epoch; 0 leaves the modification time alone
	if o.Suspend&entity.TimeModify == 0 {
		mtime = now.UnixNano()
	}
	// The write is admitted under the entry's lock, where a clone's
	// registration is checked and installed, and stays counted until its
	// operation is in the entry, so a registration waits for it.
	x := entity.Extent{Offset: off, Length: int64(len(p))}
	fe, err := e.admitAfterStaging(ctx, f)
	if err != nil {
		return vfserr.Verifier{}, err
	}
	if err := fe.refuseWrite(x); err != nil {
		e.files.unlock(f, fe)
		return vfserr.Verifier{}, err
	}
	fe.writing = append(fe.writing, x)
	e.files.unlock(f, fe)
	ver, err := b.h.WriteAt(ctx, fileID(f), off, p, mtime)
	if h := e.hooks.written.Load(); h != nil {
		(*h)()
	}
	fe = e.files.lockFor(f) // the admitted write keeps the entry alive
	fe.leave(x)
	if err != nil {
		e.files.unlock(f, fe)
		return vfserr.Verifier{}, e.journalErr(err)
	}
	// A loss drained since WriteAt answered found no pending write to apply
	// to and was kept: a corrupt loss inserts the write with the run it
	// names lost, a failed window's drops it, its Done run and nothing
	// charged.
	var lost []entity.Extent
	for _, ev := range b.gc.losses.unmatched(f, ver) {
		if ev.Reason != journal.LossFailedWindow {
			lost = lostRuns(lost, x, ev.Extent)
			continue
		}
		e.files.unlock(f, fe)
		if o.Done != nil {
			o.Done()
		}
		return v, nil
	}
	// Refused once the engine closed or the journal's device was lost: the
	// write is answered unavailable and its Done is not called, as for any
	// write Write refuses. A file's first pending write also makes it due.
	if len(fe.ops) == 0 {
		err = b.gc.note(b, f, now)
	} else {
		err = b.gc.refused()
	}
	if err != nil {
		e.files.unlock(f, fe)
		return vfserr.Verifier{}, err
	}
	fe.insert(pendingOp{
		version:   ver,
		kind:      opWrite,
		extent:    journal.Extent{Off: off, Len: int64(len(p))},
		at:        now,
		setsMtime: mtime != 0,
		lost:      lost,
		done:      o.Done,
	})
	e.files.unlock(f, fe)
	return v, nil
}

// admitAfterStaging is f's entry, locked, once no removal is waiting out the
// writes admitted before it, so a write admitted now reaches the journal
// after that removal. An entry that died during the wait is looked up again.
func (e *Engine) admitAfterStaging(ctx context.Context, f entity.FileID) (*fileEntry, error) {
	fe := e.files.lockFor(f)
	for fe.staging {
		stop := context.AfterFunc(ctx, func() {
			fe.mu.Lock()
			fe.staged.Broadcast()
			fe.mu.Unlock()
		})
		for fe.staging && ctx.Err() == nil {
			fe.staged.Wait()
		}
		stop()
		if err := ctx.Err(); err != nil {
			e.files.unlock(f, fe)
			return nil, err
		}
		if fe.dead {
			fe.mu.Unlock()
			fe = e.files.lockFor(f)
		}
	}
	return fe, nil
}

// Draw is the next version of the journal holding file f of share s, for a
// change stored outside the journal: above every version staged or drawn on
// that journal before it, and no version of the engine's own. The caller
// holds the file's accept lock, so Draw takes no lock and creates no entry.
func (e *Engine) Draw(ctx context.Context, s entity.ShareID, f entity.FileID) (entity.JournalVersion, error) {
	b, err := e.bindingOf(s)
	if err != nil {
		return entity.JournalVersion{}, err
	}
	v, err := b.j.Draw()
	if err != nil {
		return entity.JournalVersion{}, e.journalErr(err)
	}
	return entity.JournalVersion(v.Bytes()), nil
}

// insert adds op to the entry's operations in version order. The caller
// holds the entry's lock.
//
// decision: the entry's lock is not held across the journal's WriteAt, so an
// operation is inserted after the journal answers, by its version. Writes of
// one file reach the engine in version order because the filesystem holds the
// file's accept lock across Write, so a capture never sees a later write
// before an earlier one. Withdraw this if a caller can ever stage two writes
// of one file at once.
func (fe *fileEntry) insert(op pendingOp) {
	i, _ := slices.BinarySearchFunc(fe.ops, op.version, func(o pendingOp, v journal.Version) int {
		return o.version.Compare(v)
	})
	fe.ops = slices.Insert(fe.ops, i, op)
}

// fileID is f as the journal names it.
func fileID(f entity.FileID) journal.FileID { return journal.NewFileID(f) }

// journalErr is the content methods' answer for an error the journal
// returned, so no journal type crosses the content boundary.
func (e *Engine) journalErr(err error) error {
	var noSpace *journal.NoSpaceError
	var other *journal.OtherShareError
	switch {
	case errors.As(err, &noSpace):
		// decision: every space refusal is answered as retry-later, never as
		// a full disk, as the refusal table that tells a lasting shortage
		// from a passing one is not built. The version limit is DelayCommit,
		// as it is raised only once a metadata commit records a higher one;
		// every other limit is DelayCapacity. Answer each limit as that table rules
		// once it is.
		if noSpace.Limit == journal.LimitVersion {
			return &vfserr.Delay{Cause: vfserr.DelayCommit}
		}
		return &vfserr.Delay{Cause: vfserr.DelayCapacity}
	case errors.As(err, &other):
		return fmt.Errorf("engine: %v: %w", err, vfserr.ErrCorrupt)
	case errors.Is(err, journal.ErrJournalReadOnly):
		// A read-only journal refuses every write until it is reopened, so
		// a refusal is expected and logged at Debug: the journal_read_only
		// condition names the state once. EROFS, a remount only an operator
		// clears, is ErrReadOnly; any other cause may pass, so it is
		// retry-later, DelayJournal (RFC 8 §11.2; the cause per RFC 17 §4.3).
		e.d.Logger.Debug("journal read-only", "err", err)
		if errors.Is(err, syscall.EROFS) {
			return fmt.Errorf("engine: %v: %w", err, vfserr.ErrReadOnly)
		}
		return &vfserr.Delay{Cause: vfserr.DelayJournal}
	case errors.Is(err, journal.ErrClosed):
		return fmt.Errorf("engine: journal closed: %w", vfserr.ErrUnavailable)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	e.d.Logger.Error("journal call failed", "err", err)
	return fmt.Errorf("engine: %v: %w", err, vfserr.ErrUnavailable)
}

// journalReadErr is journalErr for the journal's answer to a read: the
// device's EIO is ErrIO, answered at once and never retried here, the bytes
// kept (RFC 17 §4.3). A write's EIO stays journalErr's ErrUnavailable.
//
// decision: any EIO a read returns is ErrIO. The RFC names the journal's
// ErrUnreadable, a read its bounded retry could not complete (RFC 8 §12.1,
// RFC 17 §4.3); the journal exports no such error yet, and a read it fails on
// the device ends in EIO. Match ErrUnreadable here once the journal exports
// it.
func (e *Engine) journalReadErr(err error) error {
	if errors.Is(err, syscall.EIO) {
		e.d.Logger.Error("journal device read failed", "err", err)
		return fmt.Errorf("engine: %v: %w", err, vfserr.ErrIO)
	}
	return e.journalErr(err)
}
