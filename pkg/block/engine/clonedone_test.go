package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// doneCloneUnit is a clonedUnit with a source of 2 MiB, a destination, and
// a clone of the source's first MiB onto the destination done.
func doneCloneUnit(t *testing.T, deps ...func(*engine.Deps)) (u *unit, src, dst entity.FileID) {
	t.Helper()
	u = clonedUnit(t, deps...)
	src = u.committedFile(t, "src", fill(2*mib, 's'))
	dst = u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	return u, src, dst
}

// retried answers a Clone of x from src onto dst, within a bound.
func (u *unit) retried(t *testing.T, src, dst entity.FileID, x vfserr.Extent) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, x)
}

// A retry of a done clone, neither extent changed since, answers success
// with no journal call: no second clone marker, and the done clone is still
// remembered (catches a retry that runs the clone again).
func TestARetryOfADoneCloneIsAnsweredAtOnce(t *testing.T) {
	u, src, dst := doneCloneUnit(t)

	if err := u.retried(t, src, dst, vfserr.Extent{Len: mib}); err != nil {
		t.Fatalf("the retry of the done clone = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dst)); n != 1 {
		t.Errorf("the journal holds %d clone markers after the retry, want the first clone's alone", n)
	}
	if n := u.e.DoneClones(); n != 1 {
		t.Errorf("the node remembers %d done clones, want 1", n)
	}
}

// A retry of a done clone answers success while another clone runs into
// the destination elsewhere, where without the memory it would meet that
// clone's registration and answer DelayClone (catches a retry answered from
// the running clones alone).
func TestARetryOfADoneCloneIsAnsweredWhileAnotherRunsIntoItsFile(t *testing.T) {
	entered, hold := make(chan struct{}), make(chan struct{})
	second := func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
		if op.Spec.DstOff != 0 { // the second clone's phase 1 waits for the retry
			close(entered)
			<-hold
		}
		return next.Clone(ctx, op, pending)
	}
	u, src, dst := doneCloneUnit(t, onClone(second))
	u.e.SetConflictBound(time.Minute) // the held phase 1 outlasts no bound
	running := u.startClone(t.Context(), src, dst, vfserr.Extent{SrcOff: mib, DstOff: mib, Len: mib})
	select {
	case <-entered:
	case err := <-running:
		t.Fatalf("the second clone answered %v before its phase 1", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the second clone never reached its phase 1")
	}

	if err := u.retried(t, src, dst, vfserr.Extent{Len: mib}); err != nil {
		t.Fatalf("the retry of the done clone with another running into its file = %v, want nil", err)
	}
	close(hold)
	if err := u.answered(t, "the second clone", running); err != nil {
		t.Errorf("the second clone = %v, want nil", err)
	}
}

// A write, removal or later clone overlapping either extent of a done clone
// drops it: the retry runs the clone again, one more clone marker, and the
// destination reads the source as it is now. The other file, which only the
// memory kept, holds no entry once it is dropped (catches a change that
// leaves the spec remembered, or leaves it keeping the other file alive).
func TestAChangeOverADoneClonesExtentRunsTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		onSrc  bool // the change is to the source; the destination is the other file
		change func(t *testing.T, u *unit, src, dst entity.FileID)
	}{
		{"a write over the source", true, func(t *testing.T, u *unit, src, _ entity.FileID) {
			u.write(t, src, mib/2, fill(4096, 'w'))
		}},
		{"a write over the destination", false, func(t *testing.T, u *unit, _, dst entity.FileID) {
			u.write(t, dst, mib-4096, fill(4096, 'w'))
		}},
		{"a deallocate over the source", true, func(t *testing.T, u *unit, src, _ entity.FileID) {
			if err := u.e.Deallocate(t.Context(), u.share.ID, src, 0, 4096); err != nil {
				t.Fatalf("Deallocate of the source: %v", err)
			}
		}},
		{"a truncate of the destination", false, func(t *testing.T, u *unit, _, dst entity.FileID) {
			if err := u.e.Truncate(t.Context(), u.share.ID, dst, mib/2); err != nil {
				t.Fatalf("Truncate of the destination: %v", err)
			}
		}},
		{"a clone over the destination", false, func(t *testing.T, u *unit, _, dst entity.FileID) {
			u.clone(t, u.committedFile(t, "o", fill(mib, 'o')), dst, vfserr.Extent{Len: mib})
		}},
		{"a clone over the source", true, func(t *testing.T, u *unit, src, _ entity.FileID) {
			u.clone(t, u.committedFile(t, "o", fill(mib, 'o')), src, vfserr.Extent{Len: mib})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, src, dst := doneCloneUnit(t)
			tc.change(t, u, src, dst)

			other := src
			if tc.onSrc {
				other = dst
			}
			if u.e.HasEntry(other) {
				t.Errorf("the file the change did not touch keeps an entry after %s, want it released", tc.name)
			}
			// A clone over the destination leaves a marker of its own there:
			// the retry's is counted from the markers the change left.
			before := len(u.cloneMarkers(dst))
			if err := u.retried(t, src, dst, vfserr.Extent{Len: mib}); err != nil {
				t.Fatalf("the retry after %s = %v, want nil", tc.name, err)
			}
			if n := len(u.cloneMarkers(dst)); n != before+1 {
				t.Errorf("the journal holds %d clone markers after %s and the retry, want %d, the retry run", n, tc.name, before+1)
			}
			if got, want := u.read(t, dst, 0, mib), u.read(t, src, 0, mib); !bytes.Equal(got, want) {
				t.Errorf("after %s and the retry the destination does not read the source", tc.name)
			}
		})
	}
}

// A write outside both extents keeps the done clone: its retry is still
// answered with no journal call (catches a memory dropped by any write to
// either file).
func TestAWriteOutsideADoneClonesExtentsKeepsIt(t *testing.T) {
	u, src, dst := doneCloneUnit(t)
	u.write(t, src, mib, fill(4096, 'w'))
	u.write(t, dst, mib, fill(4096, 'w'))

	if err := u.retried(t, src, dst, vfserr.Extent{Len: mib}); err != nil {
		t.Fatalf("the retry = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dst)); n != 1 {
		t.Errorf("the journal holds %d clone markers, want the first clone's alone", n)
	}
}

// A clone of another spec is not answered from a done clone's memory: it
// runs (catches a lookup that matches the files alone).
func TestACloneOfAnotherSpecIsNotAnsweredFromMemory(t *testing.T) {
	for _, x := range []vfserr.Extent{
		{Len: mib / 2},
		{SrcOff: 4096, Len: mib},
		{DstOff: 4096, Len: mib},
	} {
		t.Run(fmt.Sprintf("%+v", x), func(t *testing.T) {
			u, src, dst := doneCloneUnit(t)
			if err := u.retried(t, src, dst, x); err != nil {
				t.Fatalf("the clone of %+v = %v, want nil", x, err)
			}
			if n := len(u.cloneMarkers(dst)); n != 2 {
				t.Errorf("the journal holds %d clone markers after a clone of %+v, want two clones", n, x)
			}
		})
	}
}

// Past the node's bound the oldest done clone is forgotten: its retry runs
// the clone again while the newer one's is answered from memory, and the
// forgotten clone's files, which only the memory kept, hold no entry
// (catches an unbounded memory, or an evicted one still keeping its files
// alive). The bound is lowered to one so two clones evict.
func TestTheOldestDoneCloneIsEvictedPastTheBound(t *testing.T) {
	u := clonedUnit(t)
	u.e.SetDoneClones(1)
	srcA, dstA := u.committedFile(t, "srcA", fill(mib, 'a')), u.create(t, "dstA")
	srcB, dstB := u.committedFile(t, "srcB", fill(mib, 'b')), u.create(t, "dstB")
	x := vfserr.Extent{Len: mib}
	u.clone(t, srcA, dstA, x)
	u.clone(t, srcB, dstB, x)

	if u.e.HasEntry(srcA) || u.e.HasEntry(dstA) {
		t.Errorf("the evicted clone's files keep entries (source %v, destination %v), want both released",
			u.e.HasEntry(srcA), u.e.HasEntry(dstA))
	}
	if n := u.e.DoneClones(); n != 1 {
		t.Errorf("the node remembers %d done clones, want the bound, 1", n)
	}
	if err := u.retried(t, srcB, dstB, x); err != nil {
		t.Fatalf("the retry of the newer clone = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dstB)); n != 1 {
		t.Errorf("the newer clone's retry left %d clone markers, want it answered from memory", n)
	}
	if err := u.retried(t, srcA, dstA, x); err != nil {
		t.Fatalf("the retry of the evicted clone = %v, want nil", err)
	}
	if n := len(u.cloneMarkers(dstA)); n != 2 {
		t.Errorf("the evicted clone's retry left %d clone markers, want it run again", n)
	}
}

// An Unbind of the share forgets its done clones and releases the entries
// they kept (catches a memory that answers a retry across an Unbind, after
// which the files may have changed where this node did not see them).
func TestUnbindForgetsTheSharesDoneClones(t *testing.T) {
	u, src, dst := doneCloneUnit(t)
	if !u.e.HasEntry(src) || !u.e.HasEntry(dst) {
		t.Fatal("the done clone keeps no entry of its files before the Unbind")
	}
	u.unbind(t)

	if n := u.e.DoneClones(); n != 0 {
		t.Errorf("the node remembers %d done clones after the Unbind, want 0", n)
	}
	if u.e.HasEntry(src) || u.e.HasEntry(dst) {
		t.Errorf("the files keep entries after the Unbind (source %v, destination %v), want both released",
			u.e.HasEntry(src), u.e.HasEntry(dst))
	}
}

// A release of a done clone's destination forgets the clone and releases
// the source's entry, which only the memory kept, and the clone's retry runs,
// answered by the store (catches a release that leaves the spec remembered,
// whose retry is answered nil with no destination left).
func TestAReleaseOfADoneClonesDestinationForgetsIt(t *testing.T) {
	u, src, dst := doneCloneUnit(t)
	key := openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: dst}
	u.unlink(t, u.tb, key, "dst")
	done, err := u.tb.Opens().Releasing(t.Context(), key)
	if err != nil {
		t.Fatalf("Releasing the destination: %v", err)
	}
	ch, err := u.e.Delete(t.Context(), u.share.ID, dst)
	done(err == nil && ch == nil)
	if ch != nil || err != nil {
		t.Fatalf("the release of the destination: %v, %v; want it released", ch, err)
	}

	if n := u.e.DoneClones(); n != 0 {
		t.Errorf("the node remembers %d done clones after the release, want 0", n)
	}
	if u.e.HasEntry(src) {
		t.Error("the source keeps an entry after the release, want it released")
	}
	// The released destination's ID names nothing now, and a file recreated
	// under its name has another, so the retry that stale memory would answer
	// nil reaches the store, which refuses it.
	if err := u.retried(t, src, dst, vfserr.Extent{Len: mib}); !errors.Is(err, vfserr.ErrStale) {
		t.Errorf("the retry into the released destination = %v, want ErrStale", err)
	}
}

// A clone within one file is remembered in its entry once, and a write over
// either of its extents drops it, its retry then run (catches the record put
// in the file's entry twice, as source and as destination).
func TestADoneCloneWithinOneFileIsRememberedOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  int64
	}{{"the source extent", 0}, {"the destination extent", mib}} {
		t.Run(tc.name, func(t *testing.T) {
			u := clonedUnit(t)
			f := u.committedFile(t, "f", fill(2*mib, 's'))
			x := vfserr.Extent{DstOff: mib, Len: mib}
			u.clone(t, f, f, x)
			if n := u.e.DoneIn(f); n != 1 {
				t.Fatalf("the file's entry holds %d done clones, want the self-clone once", n)
			}

			u.write(t, f, tc.off, fill(4096, 'w'))
			if n := u.e.DoneIn(f); n != 0 {
				t.Errorf("the file's entry holds %d done clones after a write over %s, want 0", n, tc.name)
			}
			if n := u.e.DoneClones(); n != 0 {
				t.Errorf("the node remembers %d done clones after a write over %s, want 0", n, tc.name)
			}
			before := len(u.cloneMarkers(f))
			if err := u.retried(t, f, f, x); err != nil {
				t.Fatalf("the retry after a write over %s = %v, want nil", tc.name, err)
			}
			if n := len(u.cloneMarkers(f)); n != before+1 {
				t.Errorf("the journal holds %d clone markers after the retry, want %d, the retry run", n, before+1)
			}
		})
	}
}

// Close after a done clone or copy answers nil within its bound. The
// destination's write outside the extent, left to the group commit, is
// committed by the clone's phase 1 instead, so the destination stays on the
// due set with nothing pending: only the done clone keeps its entry (catches
// a remembered done clone taken for pending existence, which the last group
// commit waits for until the stop bound).
func TestCloseAfterADoneCloneOrCopyIsPrompt(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, u *unit, src, dst entity.FileID)
	}{
		{"clone", func(t *testing.T, u *unit, src, dst entity.FileID) {
			u.clone(t, src, dst, vfserr.Extent{Len: mib})
		}},
		{"copy", func(t *testing.T, u *unit, src, dst entity.FileID) {
			if n, err := u.copy(t.Context(), src, dst, vfserr.Extent{Len: mib}); err != nil || n != mib {
				t.Fatalf("Copy: %d, %v; want %d, nil", n, err, mib)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := clonedUnit(t)
			src := u.committedFile(t, "src", fill(mib, 's'))
			dst := u.create(t, "dst")
			u.write(t, dst, 2*mib, fill(4096, 'd'))
			tc.run(t, u, src, dst)
			if !u.e.Due(u.rec, dst) || len(u.e.Pending(dst)) != 0 || u.e.DoneIn(dst) == 0 {
				t.Fatalf("after the %s the destination is due %v with %d pending and %d done clones, want due, none pending, one remembered",
					tc.name, u.e.Due(u.rec, dst), len(u.e.Pending(dst)), u.e.DoneIn(dst))
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := u.e.Close(ctx); err != nil {
				t.Errorf("Close after a done %s = %v, want nil", tc.name, err)
			}
		})
	}
}
