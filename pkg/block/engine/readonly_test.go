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
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A removal over a journal turned read-only between its journal step and its
// phase 1 fails: the store's Remove is never called, a truncate leaves the
// committed size, and the guard is released. It holds whether the journal
// undid the removal, so its Sync is refused, or synced it before the
// transition, so its Sync answers nil.
func TestARemovalOverAReadOnlyJournalFails(t *testing.T) {
	for _, c := range []struct {
		name  string
		undid bool // the journal undid the removal, so its Sync is refused
		check func(t *testing.T, u *unit, f entity.FileID) error
	}{
		{"truncate, undone", true, func(t *testing.T, u *unit, f entity.FileID) error {
			return u.e.Truncate(t.Context(), u.share.ID, f, 5)
		}},
		{"truncate, synced before the transition", false, func(t *testing.T, u *unit, f entity.FileID) error {
			return u.e.Truncate(t.Context(), u.share.ID, f, 5)
		}},
		{"release, undone", true, func(t *testing.T, u *unit, f entity.FileID) error {
			retry, err := u.e.Delete(t.Context(), u.share.ID, f)
			if retry != nil {
				t.Error("the release is pending, want it failed")
			}
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("release: %v, want ErrUnavailable", err)
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var removes atomic.Int32
			u := newUnit(t, onRemove(func(ctx context.Context, next engine.Existence, s entity.ShareID, f entity.FileID, epoch uint64, op kvstore.RemoveOp, p []kvstore.PendingOp) error {
				removes.Add(1)
				return next.Remove(ctx, s, f, epoch, op, p)
			}))
			f := u.committedFile(t, "a", fill(10, 'a'))
			u.e.OnStaged(func() {
				var undone []journal.FileID
				if c.undid {
					undone = append(undone, journal.NewFileID(f))
				}
				u.j.ReadOnly(syscall.EIO, undone...)
			})
			if err := c.check(t, u, f); err == nil {
				t.Fatal("the removal succeeded over a read-only journal")
			}
			if n := removes.Load(); n != 0 {
				t.Errorf("the store's Remove ran %d times, want never", n)
			}
			if n := u.size(t, f); n != 10 {
				t.Errorf("size %d, want 10 kept", n)
			}
			if u.e.GuardHeld(f) {
				t.Error("the guard is still held")
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
		{syscall.EIO, func(err error) bool { var d *vfserr.Delay; return errors.As(err, &d) }},
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
