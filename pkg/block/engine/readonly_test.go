package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// readOnlyOnStaged turns u's journal read-only for cause once a removal of
// f is staged, the removal undone by the journal when undid.
func readOnlyOnStaged(u *unit, f entity.FileID, cause error, undid bool) {
	u.e.OnStaged(func() {
		var undone []journal.FileID
		if undid {
			undone = append(undone, journal.NewFileID(f))
		}
		u.j.ReadOnly(cause, undone...)
	})
}

// removeCounter counts the store's Removes.
func removeCounter(n *atomic.Int32) func(*engine.Deps) {
	return onRemove(func(ctx context.Context, next engine.Existence, s entity.ShareID, f entity.FileID, epoch uint64, op kvstore.RemoveOp, p []kvstore.PendingOp) error {
		n.Add(1)
		return next.Remove(ctx, s, f, epoch, op, p)
	})
}

// A removal the journal undid when it turned read-only, between the
// removal's journal step and its phase 1, fails by the journal's cause: the
// store's Remove is never called, the removal's pending operation goes, a
// truncate leaves the committed size, and the guard is released.
func TestARemovalAReadOnlyJournalUndidFails(t *testing.T) {
	truncate := func(t *testing.T, u *unit, f entity.FileID) error {
		return u.e.Truncate(t.Context(), u.share.ID, f, 5)
	}
	for _, c := range []struct {
		name  string
		cause error
		check func(t *testing.T, u *unit, f entity.FileID) error
		want  error
	}{
		{"truncate, EROFS", fmt.Errorf("write segment: %w", syscall.EROFS), truncate, vfserr.ErrReadOnly},
		{"truncate, EIO", syscall.EIO, truncate, &vfserr.Delay{Cause: vfserr.DelayJournal}},
		{"release", syscall.EIO, func(t *testing.T, u *unit, f entity.FileID) error {
			retry, err := u.e.Delete(t.Context(), u.share.ID, f)
			if retry != nil {
				t.Error("the release is pending, want it failed")
			}
			return err
		}, vfserr.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			var removes atomic.Int32
			u := newUnit(t, removeCounter(&removes))
			f := u.committedFile(t, "a", fill(10, 'a'))
			readOnlyOnStaged(u, f, c.cause, true)
			if err := c.check(t, u, f); !errors.Is(err, c.want) {
				t.Fatalf("removal: %v, want %v", err, c.want)
			}
			if n := removes.Load(); n != 0 {
				t.Errorf("the store's Remove ran %d times, want never", n)
			}
			if n := u.size(t, f); n != 10 {
				t.Errorf("size %d, want 10 kept", n)
			}
			if p := u.e.Pending(f); len(p) != 0 {
				t.Errorf("pending %+v, want the removal gone", p)
			}
			if u.e.GuardHeld(f) {
				t.Error("the guard is still held")
			}
		})
	}
}

// A truncate whose record the journal synced before it turned read-only is
// past its journal step, so it does not fail (RFC 8 §8.1): it answers
// retry-later, its guard held for the loop's retry, and a read is served to
// the truncated size, with no loss.
func TestATruncateSyncedBeforeTheJournalTurnedReadOnlyIsRetried(t *testing.T) {
	for _, cause := range []error{syscall.EIO, fmt.Errorf("write segment: %w", syscall.EROFS)} {
		t.Run(cause.Error(), func(t *testing.T) {
			var removes atomic.Int32
			u := newUnit(t, removeCounter(&removes))
			f := u.committedFile(t, "a", fill(10, 'a'))
			readOnlyOnStaged(u, f, cause, false)
			if err := u.e.Truncate(t.Context(), u.share.ID, f, 5); !errors.Is(err, &vfserr.Delay{Cause: vfserr.DelayJournal}) {
				t.Fatalf("Truncate: %v, want ErrDelay (DelayJournal)", err)
			}
			if !u.e.GuardHeld(f) {
				t.Error("the guard is released, want it held for the retry")
			}
			if n := removes.Load(); n != 0 {
				t.Errorf("the store's Remove ran %d times, want never", n)
			}
			p := make([]byte, 10)
			n, err := u.e.Read(t.Context(), u.share.ID, f, 0, p)
			if err != nil || n != 5 || string(p[:n]) != string(fill(5, 'a')) {
				t.Errorf("read [0, 10): %d bytes %q, %v; want the 5 kept, no error", n, p[:n], err)
			}
		})
	}
}

// A write to a read-only journal is ErrReadOnly when EROFS entered the state,
// and retry-later for any other cause.
func TestAWriteToAReadOnlyJournal(t *testing.T) {
	for _, c := range []struct {
		cause error
		want  func(error) bool
	}{
		{fmt.Errorf("write segment: %w", syscall.EROFS), func(err error) bool { return errors.Is(err, vfserr.ErrReadOnly) }},
		{syscall.EIO, func(err error) bool { return errors.Is(err, &vfserr.Delay{Cause: vfserr.DelayJournal}) }},
	} {
		t.Run(c.cause.Error(), func(t *testing.T) {
			u := newUnit(t)
			f := u.create(t, "a")
			u.j.ReadOnly(c.cause)
			if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(4, 'a'), engine.WriteOpts{Done: func() {}}); !c.want(err) {
				t.Errorf("Write: %v", err)
			}
		})
	}
}

// The group commit commits nothing over a read-only journal, though the
// journal's Sync, with nothing left unsynced, answers nil: the write stays
// pending and the committed size is unchanged.
func TestTheGroupCommitCommitsNothingOverAReadOnlyJournal(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(4, 'a'))
	u.j.ReadOnly(syscall.EIO)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = u.e.CommitNow(ctx, u.share.ID, f)
	if n := u.size(t, f); n != 0 {
		t.Errorf("size %d, want 0: the write committed", n)
	}
	if len(u.e.Pending(f)) != 1 {
		t.Errorf("pending %+v, want the write", u.e.Pending(f))
	}
}

// A read-only journal raises journal_read_only naming its device and the
// error that entered the state.
func TestAReadOnlyJournalRaisesItsCondition(t *testing.T) {
	u := newUnit(t)
	u.j.ReadOnly(syscall.EIO)
	for _, js := range u.e.Stats().Journals {
		for _, c := range js.Conditions {
			if c.Name == "journal_read_only" && c.Subject.Scope == engine.ScopeJournal && errors.Is(c.Cause, syscall.EIO) {
				return
			}
		}
	}
	t.Errorf("Stats %+v: no journal_read_only naming EIO", u.e.Stats())
}

// A release retried from the loop that the journal then undid, turning
// read-only, fails: its channel yields ErrUnavailable and the guard is
// released.
func TestARetriedReleaseAReadOnlyJournalUndidFails(t *testing.T) {
	var u *unit
	var f entity.FileID
	var first atomic.Bool
	u = newUnit(t, onRemove(func(ctx context.Context, next engine.Existence, s entity.ShareID, id entity.FileID, epoch uint64, op kvstore.RemoveOp, p []kvstore.PendingOp) error {
		if first.CompareAndSwap(false, true) {
			u.j.ReadOnly(syscall.EIO, journal.NewFileID(f))
			return kvstore.ErrConflict
		}
		return next.Remove(ctx, s, id, epoch, op, p)
	}))
	f = u.unlinked(t, enginetest.NewReservations(t))
	ch, err := u.e.Delete(t.Context(), u.share.ID, f)
	if ch == nil || err != nil {
		t.Fatalf("Delete: %v, %v; want a channel and nil", ch, err)
	}
	var got error
	u.until(t, "the channel yields", func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	if !errors.Is(got, vfserr.ErrUnavailable) {
		t.Errorf("the channel yields %v, want ErrUnavailable", got)
	}
	if u.e.GuardHeld(f) {
		t.Error("the guard is still held")
	}
}
