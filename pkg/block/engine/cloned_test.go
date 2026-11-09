package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// clonedUnit is a cloneUnit whose clones run to their done signal, past
// the gate a served engine keeps, over a recording CloneHold unless opts
// sets another.
func clonedUnit(t *testing.T, opts ...func(*engine.Deps)) *unit {
	t.Helper()
	return clonedUnitWith(t, unitConfig{deps: opts})
}

// clonedUnitWith is clonedUnit as c asks.
func clonedUnitWith(t *testing.T, c unitConfig) *unit {
	t.Helper()
	c.deps = append([]func(*engine.Deps){func(d *engine.Deps) { d.Holds = &recHolds{} }}, c.deps...)
	u := cloneUnitWith(t, c)
	u.e.FakeCloneDone()
	return u
}

// committedFile creates a file holding p at 0, its existence committed.
func (u *unit) committedFile(t *testing.T, name string, p []byte) entity.FileID {
	t.Helper()
	f := u.create(t, name)
	u.write(t, f, 0, p)
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (u *unit) read(t *testing.T, f entity.FileID, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, off, p); err != nil {
		t.Fatalf("read [%d, %d): %v", off, off+int64(n), err)
	}
	return p
}

// clone runs a clone of x and fails the test on its error.
func (u *unit) clone(t *testing.T, src, dst entity.FileID, x vfserr.Extent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, x); err != nil {
		t.Fatalf("Clone: %v", err)
	}
}

// startClone runs a clone of x from src onto dst on a goroutine of its own,
// under ctx, and answers the channel of its answer.
func (u *unit) startClone(ctx context.Context, src, dst entity.FileID, x vfserr.Extent) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, x) }()
	return errc
}

// answered waits for errc's answer, advancing the clock as until does.
func (u *unit) answered(t *testing.T, what string, errc <-chan error) (err error) {
	t.Helper()
	u.until(t, what, func() bool {
		select {
		case err = <-errc:
			return true
		default:
			return false
		}
	})
	return err
}

// cloneFn answers a clone's phase 1 in place of the store, next.
type cloneFn func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error

// cloneHook is the store with its clones' phase 1 answered by f.
type cloneHook struct {
	engine.Existence
	f cloneFn
}

func (x cloneHook) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	return x.f(ctx, x.Existence, op, pending)
}

// onClone has f answer the unit's clones' phase 1.
func onClone(f cloneFn) func(*engine.Deps) {
	return func(d *engine.Deps) { d.Existence = cloneHook{d.Existence, f} }
}

// clonedHookedUnit is clonedUnit with its share's handle a hookedHandle.
func clonedHookedUnit(t *testing.T, deps ...func(*engine.Deps)) (*unit, *hookedHandle) {
	t.Helper()
	c, h := hooked(unitConfig{deps: deps})
	u := clonedUnitWith(t, c)
	h.e = u.e
	return u, h
}

// heldBeforePhase1 starts a clone of x and returns once it is held between
// its copies and its phase 1, with the function that lets it go on and the
// channel of its answer.
func (u *unit) heldBeforePhase1(t *testing.T, src, dst entity.FileID, x vfserr.Extent) (release func(), answer <-chan error) {
	t.Helper()
	held, hold := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	u.e.OnStaged(func() {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-hold
		}
	})
	errc := u.startClone(context.Background(), src, dst, x)
	select {
	case <-held:
	case err := <-errc:
		t.Fatalf("the clone answered %v before its phase 1", err)
	}
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(hold) }) })
	return func() { released.Do(func() { close(hold) }) }, errc
}

// A reservation the journal refuses fails the clone with the mapped error
// before its journal step: both registrations are gone, the destination
// reads its prior content, and the journal saw no CloneTarget.
func TestARefusedReservationChangesNothing(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	u.j.RefuseReserve(&journal.NoSpaceError{Limit: journal.LimitShare, Missing: 1, Unit: journal.UnitBytes})
	err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 2 * mib})
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Fatalf("a refused reservation: %v, want DelayCapacity", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 {
		t.Errorf("%d CloneTarget calls after a refused reservation, want none", n)
	}
	for _, f := range []entity.FileID{src, dst} {
		if s, d := u.e.Registrations(f); s+d != 0 {
			t.Errorf("registrations left on %v: %d source, %d destination", f, s, d)
		}
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the source extent after the refusal: %v, want served", err)
	}
	if got := u.read(t, dst, 0, 2*mib); !bytes.Equal(got, fill(2*mib, 'd')) {
		t.Error("the destination lost its prior content to a refused clone")
	}
}

// A source extent past the source's end is refused ErrInvalid before the
// journal step: no CloneTarget, the destination unchanged.
func TestACloneOfASourcePastItsEndIsRefused(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 2 * mib})
	if !errors.Is(err, vfserr.ErrInvalid) {
		t.Fatalf("a clone past the source's end: %v, want ErrInvalid", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 {
		t.Errorf("%d CloneTarget calls, want none", n)
	}
	if got := u.read(t, dst, 0, 2*mib); !bytes.Equal(got, fill(2*mib, 'd')) {
		t.Error("the destination lost its prior content to a refused clone")
	}
}

// The reservation is sized from the source's held runs at asOf, and every
// copy goes through it above the clone's version: three held runs and two
// holes reserve the runs' bytes and three writes. Phase 1 commits the copies
// through Copied: the destination's applied is at or above the newest copy,
// and the copied runs read the source.
func TestACloneReservesItsCopiesAndWritesThemAboveItsVersion(t *testing.T) {
	u := clonedUnit(t)
	src := u.create(t, "src")
	u.write(t, src, 0, fill(mib, 'a'))
	u.write(t, src, 2*mib, fill(mib, 'b'))
	u.write(t, src, 4*mib, fill(mib/2, 'c'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: 4*mib + mib/2})

	if r := u.j.Reserves(); len(r) != 1 || r[0].N != 2*mib+mib/2 || r[0].Writes != 3 {
		t.Errorf("reserved %+v, want one of %d bytes and 3 writes", r, 2*mib+mib/2)
	}
	ct := u.j.CloneTargets()
	if len(ct) != 1 {
		t.Fatalf("%d CloneTarget calls, want one", len(ct))
	}
	copies := u.j.ReservedWrites()
	if len(copies) != 3 {
		t.Fatalf("%d copies through the reservation, want 3", len(copies))
	}
	newest := copies[0]
	for _, v := range copies {
		if v.Compare(ct[0].Version) <= 0 {
			t.Errorf("a copy at %v, at or below the clone's %v", v, ct[0].Version)
		}
		if newest.Compare(v) < 0 {
			newest = v
		}
	}
	applied, err := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Less(entity.JournalVersion(newest.Bytes())) {
		t.Errorf("the destination's applied %v is below the newest copy %v", applied, newest)
	}
	if got := u.read(t, dst, 2*mib, mib); !bytes.Equal(got, fill(mib, 'b')) {
		t.Error("a copied run of the destination does not read the source")
	}
	if len(u.e.Pending(dst)) != 0 {
		t.Errorf("the destination's entry kept %+v after phase 1, want nothing", u.e.Pending(dst))
	}
	for c, err := range u.h.Since(t.Context(), journal.NewFileID(dst), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind == journal.ChangeCloneTarget {
			t.Errorf("the clone's journal marker at %v is still unsettled after phase 1", c.Version)
		}
	}
}

// While the clone is held between its copies and its phase 1, the
// destination's guard keeps its operations out of every batch: a group
// commit asked for then commits none of them, and the clone then commits.
// With the guard's skip removed a batch may commit the write below the
// clone, but stops at the clone's operation: the destination's applied and
// every Settle of it stay below the clone's version.
func TestTheGuardKeepsACloneOutOfEveryBatch(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprint("skip ", skip), func(t *testing.T) { guardKeepsACloneOut(t, skip) })
	}
}

func guardKeepsACloneOut(t *testing.T, skip bool) {
	u, h := clonedHookedUnit(t)
	if !skip {
		u.e.NoGuardSkip()
	}
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w')) // below the clone, outside its extent
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	v := u.j.CloneTargets()[0].Version
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_ = u.e.CommitAll(ctx, u.rec)
	cancel()
	applied, _ := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst)
	switch {
	case skip && !applied.IsZero():
		t.Errorf("a batch committed the destination's write at %v while the clone held its guard", applied)
	case !applied.Less(entity.JournalVersion(v.Bytes())):
		t.Errorf("a batch raised the destination's applied to %v, at or above the held clone's %v", applied, v)
	}
	for _, s := range h.settleLog() {
		if s.v.Compare(v) >= 0 {
			t.Errorf("settled at %v while the clone at %v is held before its phase 1", s.v, v)
		}
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if got := u.read(t, dst, 4*mib, 10); !bytes.Equal(got, fill(10, 'w')) {
		t.Error("the destination's earlier write did not survive the clone")
	}
}

// Phase 1 passes each pending write of the destination with the runs
// where it overlaps an earlier one, as a batch does: a write outside the
// extent and an overwrite of part of it, both pending below the clone, leave
// an overwrite record over the overlapped run once phase 1 commits, whose
// edges cut the destination's committed runs.
func TestAClonesPhase1PassesTheRunsEachWriteSupersedes(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'a'))
	u.write(t, dst, 4*mib+2, fill(4, 'b')) // supersedes [2, 6) of the first
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	var runs []entity.Extent
	for sp, err := range u.fs.Store.Content().Covering(t.Context(), u.share.ID, dst, 4*mib, 10) {
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, sp.Extent)
	}
	want := []entity.Extent{entity.ExtentOf(4*mib, 4*mib+2), entity.ExtentOf(4*mib+2, 4*mib+6), entity.ExtentOf(4*mib+6, 4*mib+10)}
	if !slices.Equal(runs, want) {
		t.Errorf("the destination's committed runs over the writes: %v, want them cut by an overwrite record at %v", runs, want[1])
	}
}

// The clone's journal step raises the destination's overlay to the extent's
// end. A write to the destination outside the extent, staged after the
// journal step and before phase 1, growing the file past a hole, reads back after
// the clone, and the store records its size and the hole: phase 1 applies
// it after the clone, so no later commit skips it.
func TestAWriteOutsideTheExtentDuringACloneSurvives(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	if err := u.e.Deallocate(t.Context(), u.share.ID, src, mib, mib); err != nil {
		t.Fatal(err)
	}
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: 2 * mib})
	if o, _, err := u.e.Overlay(t.Context(), u.share.ID, dst); err != nil || o.Size != 2*mib {
		t.Errorf("the empty destination's overlay during the clone: %+v, %v; want the extent's end", o, err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatalf("a write outside the extent during the clone: %v", err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if got := u.read(t, dst, 3*mib, 10); !bytes.Equal(got, fill(10, 'w')) {
		t.Error("the write outside the extent did not read back after the clone")
	}
	size, err := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst)
	if err != nil || size != 3*mib+10 {
		t.Errorf("the destination's committed size: %d, %v; want %d", size, err, 3*mib+10)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if again, _ := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst); again != size {
		t.Errorf("a later commit changed the size to %d", again)
	}
}

// After phase 1 over the real store, when the clone takes its hold, the
// source holds an open record of the clone kind naming the destination and
// the clone's version, and the destination's covering lookup over the
// extent waits for the clone to be done; once done, the record is gone.
func TestPhase1HoldsTheSourceAndHoldsTheDestinationExtent(t *testing.T) {
	h := &recHolds{}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	cloneOpens := func() (n int, v entity.JournalVersion) {
		opens, err := u.fs.Store.Opens().Of(context.Background(), u.share.ID, src)
		if err != nil {
			t.Error(err)
		}
		for _, o := range opens {
			if o.Kind == entity.OpenCloneSource && o.CloneDst == dst {
				n, v = n+1, o.CloneVersion
			}
		}
		return n, v
	}
	var (
		atHold   int
		heldAt   entity.JournalVersion
		covering error
	)
	h.onHold = func() error {
		atHold, heldAt = cloneOpens()
		for _, err := range u.fs.Store.Content().Covering(context.Background(), u.share.ID, dst, 0, mib) {
			covering = err
			break
		}
		return nil
	}
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	v := entity.JournalVersion(u.j.CloneTargets()[0].Version.Bytes())
	if atHold != 1 || heldAt != v {
		t.Errorf("the source's clone-kind opens at the hold: %d at %v; want one naming the destination at %v", atHold, heldAt, v)
	}
	if !errors.Is(covering, entity.ErrDelay) {
		t.Errorf("the destination extent served before the clone is done: %v", covering)
	}
	if n, _ := cloneOpens(); n != 0 {
		t.Errorf("%d clone-kind opens left on the source after the clone, want the last batch to delete it", n)
	}
}

// A source run the journal lost after its existence committed is copied as
// far as the journal holds it: the destination reads the source around it,
// and lost there, never zeros, once phase 2 adopts the source.
func TestAnUncarvedSourceRunReadsLostInTheDestination(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(3*mib, 's'))
	u.lose(src, mib, mib)
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: 3 * mib})
	if _, err := u.fs.Store.Content().Resume(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source before the lost run")
	}
	if got := u.read(t, dst, 2*mib, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source after the lost run")
	}
	_, err := u.e.Read(t.Context(), u.share.ID, dst, mib, make([]byte, mib))
	if !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("the lost run in the destination: %v, want ErrLost", err)
	}
}

// A release of the source while a clone holds it is pending before the
// journal's Delete, which it never reaches; its channel yields DelayClone
// once the clone is done, and nothing after.
func TestAReleaseOfARegisteredSourceIsPending(t *testing.T) {
	u, h := clonedHookedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ch, err := u.e.Delete(t.Context(), u.share.ID, src)
	if err != nil || ch == nil {
		t.Fatalf("a release of a registered source: %v, %v; want a pending channel", ch, err)
	}
	ran := u.e.LoopSteps(u.share.ID)
	u.until(t, "the pending release's item running once more", func() bool { return u.e.LoopSteps(u.share.ID) > ran })
	select {
	case early := <-ch:
		t.Fatalf("the pending release answered %v while the clone still held the source", early)
	default:
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	var got error
	u.until(t, "the pending release's outcome", func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	if !isDelay(got, vfserr.DelayClone) {
		t.Errorf("the pending release's outcome: %v, want DelayClone", got)
	}
	if n := h.deletes.Load(); n != 0 {
		t.Errorf("the journal saw %d Delete calls for a release behind a clone, want none", n)
	}
	u.clock.Advance(10 * time.Second)
	time.Sleep(10 * time.Millisecond)
	select {
	case again := <-ch:
		t.Errorf("the channel answered twice: %v", again)
	default:
	}
}

// refusingClone refuses its clones' phase 1 left times, then passes them
// on, through its clone, a cloneFn.
type refusingClone struct {
	left  atomic.Int64
	calls atomic.Int64
	err   error // the failure; a store refusal when nil
	// entered and hold, when set, hold the second call until hold closes,
	// closing entered once it is held.
	entered, hold chan struct{}
}

func (x *refusingClone) clone(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	if x.calls.Add(1) == 2 && x.hold != nil {
		close(x.entered)
		<-x.hold
	}
	if x.left.Add(-1) >= 0 {
		if x.err != nil {
			return x.err
		}
		// A store refusal, as the store answers one: a clone of nothing.
		return next.Clone(ctx, kvstore.CloneOp{}, nil)
	}
	return next.Clone(ctx, op, pending)
}

// A phase 1 refused after the journal step is not a failure: the clone is
// logged at Error and retried, and completes on the next attempt.
func TestARefusedClonePhase1IsRetried(t *testing.T) {
	x := &refusingClone{}
	x.left.Store(1)
	var logged lockedBuffer
	u := clonedUnit(t, onClone(x.clone), func(d *engine.Deps) { d.Logger = slog.New(slog.NewTextHandler(&logged, nil)) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	if got := u.answered(t, "the retried clone's answer", errc); got != nil {
		t.Fatalf("a clone refused once at phase 1: %v, want it retried to success", got)
	}
	if n := strings.Count(logged.String(), "phase 1 was refused"); n != 1 {
		t.Errorf("the refusal was logged %d times, want once", n)
	}
	if !strings.Contains(logged.String(), "level=ERROR") {
		t.Error("the refusal was not logged at Error")
	}
}

// Six refusals in a row raise removal_stalled for the destination, keep its
// guard, so a group commit between two attempts captures none of its
// operations, and block no other file's commit on the journal.
func TestARefusedClonePinsItsGuardAndNothingElse(t *testing.T) {
	x := &refusingClone{}
	x.left.Store(6)
	u := clonedUnit(t, onClone(x.clone))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "removal_stalled for the destination", func() bool { return u.e.RemovalStalled(dst) })
	other := u.create(t, "other")
	u.write(t, other, 0, fill(10, 'o'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, other); err != nil {
		t.Errorf("another file's commit behind the pinned guard: %v", err)
	}
	if applied, _ := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst); !applied.IsZero() {
		t.Errorf("a batch committed the destination at %v while the clone held its guard", applied)
	}
	if err := u.answered(t, "the clone's answer", errc); err != nil {
		t.Errorf("the clone: %v", err)
	}
}

// gatedCommit is the store's existence view holding its commits while on.
type gatedCommit struct {
	engine.Existence
	on               atomic.Bool
	entered, open    chan struct{}
	commitReturned   atomic.Bool
	cloneAfterCommit atomic.Bool
	cloned           atomic.Bool
}

func (x *gatedCommit) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.on.CompareAndSwap(true, false) {
		close(x.entered)
		<-x.open
		defer x.commitReturned.Store(true)
	}
	return x.Existence.Commit(ctx, p, marks)
}

func (x *gatedCommit) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	x.cloneAfterCommit.Store(x.commitReturned.Load())
	x.cloned.Store(true)
	return x.Existence.Clone(ctx, op, pending)
}

// A batch held between its capture and its existence commit on the
// destination is waited out: the clone's phase 1 starts only once it
// returns.
func TestACloneWaitsOutABatchInFlight(t *testing.T) {
	x := &gatedCommit{entered: make(chan struct{}), open: make(chan struct{})}
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	x.on.Store(true)
	committed := make(chan error, 1)
	go func() { committed <- u.e.CommitNow(context.Background(), u.share.ID, dst) }()
	<-x.entered
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	time.Sleep(50 * time.Millisecond)
	if x.cloned.Load() {
		t.Error("the clone's phase 1 ran while a batch on the destination was in flight")
	}
	close(x.open)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if !x.cloneAfterCommit.Load() {
		t.Error("the clone's phase 1 overtook the batch in flight")
	}
}

// Phase 1 syncs the destination before the store records it: a write
// outside the extent staged during the clone, then left undurable by
// another file's failed sync window, is durable by the time the store's
// Clone runs.
func TestAClonesPhase1SyncsWhatItCommits(t *testing.T) {
	var u *unit
	var dst entity.FileID
	var durable atomic.Bool // the destination was durable when the store's Clone was called
	u = clonedUnit(t, onClone(func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
		durable.Store(u.j.Durable(journal.NewFileID(dst)))
		return next.Clone(ctx, op, pending)
	}))
	src := u.committedFile(t, "src", fill(mib, 's'))
	other := u.committedFile(t, "other", fill(10, 'o'))
	dst = u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.j.FailSync(journal.NewFileID(other), journal.Extent{Off: 0, Len: 10}, journal.NewFileID(dst))
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if !durable.Load() {
		t.Error("the store's Clone ran over a destination not synced since its last write")
	}
}

// retriedClone starts a clone of the first MiB of src onto dst whose phase 1
// the store refuses until x.left runs out, and returns once the loop retried
// it, with the clone's version and the channel of its answer. dst's first
// pending operation is a write below the clone, which makes it due.
func (u *unit) retriedClone(t *testing.T, x *refusingClone, src, dst entity.FileID) (journal.Version, <-chan error) {
	t.Helper()
	x.left.Store(1 << 30)
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "the loop retrying the clone's phase 1", func() bool { return x.calls.Load() >= 2 })
	p := u.e.Pending(dst)
	if len(p) < 2 || p[1].Off != 0 || p[1].Len != mib {
		t.Fatalf("the destination's pending operations %+v, want the write below and the clone's", p)
	}
	return p[1].Version, errc
}

// A clone whose retried phase 1 Close's stop of the work queue cuts short
// stays out of the last group commit: the destination's write outside the
// extent, staged above the clone, is not committed, no Settle reaches the
// clone's version, the entry keeps the clone's operation and its copies for
// the next start's rebuild, and the call answers unavailable. Dropping the
// clone's operation at the loop's end lets the last group commit commit the
// write above it, and settle past it.
func TestALoopEndingLeavesARetriedCloneForRecovery(t *testing.T) {
	x := &refusingClone{}
	u, h := clonedHookedUnit(t, onClone(x.clone))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	cv, errc := u.retriedClone(t, x, src, dst)
	u.write(t, dst, 6*mib, fill(10, 'z'))
	before := len(u.e.Pending(dst))
	u.e.StopRemovalLoops()
	if err := <-errc; !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a clone its loop's end cut short: %v, want ErrUnavailable", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_ = u.e.CommitAll(ctx, u.rec) // the last group commit
	cancel()
	if applied, _ := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst); !applied.IsZero() {
		t.Errorf("the last group commit committed the destination at %v", applied)
	}
	for _, s := range h.settleLog() {
		if s.v.Compare(cv) >= 0 {
			t.Errorf("settled at %v, at or above the cut-short clone's %v", s.v, cv)
		}
	}
	if p := u.e.Pending(dst); len(p) != before || p[1].Version != cv {
		t.Errorf("after the loop ended the entry holds %+v, want its %d operations, the clone's among them", p, before)
	}
}

// A write staged outside a clone's extent while its phase 1 retries is
// committed by no group commit, the guard's skip removed: the clone's
// operation sits in the destination's pending operations at its version and
// the capture stops there, so the destination's applied and every Settle of
// it stay below the clone's version until phase 1 commits. A clone whose
// operation is not pending lets a batch commit the write, and the store
// then skips the clone as applied.
func TestAGroupCommitStopsAtARetriedClone(t *testing.T) {
	x := &refusingClone{}
	u, h := clonedHookedUnit(t, onClone(x.clone))
	u.e.NoGuardSkip()
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	cv, errc := u.retriedClone(t, x, src, dst)
	u.write(t, dst, 6*mib, fill(10, 'z'))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_ = u.e.CommitAll(ctx, u.rec)
	cancel()
	applied, err := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst)
	if err != nil {
		t.Fatal(err)
	}
	if applied.IsZero() {
		t.Error("the group commit did not commit the write below the clone")
	}
	if applied.Compare(entity.JournalVersion(cv.Bytes())) >= 0 {
		t.Errorf("applied %v while the clone at %v retries phase 1", applied, cv)
	}
	for _, s := range h.settleLog() {
		if s.v.Compare(cv) >= 0 {
			t.Errorf("settled at %v while the clone at %v retries phase 1", s.v, cv)
		}
	}
	x.left.Store(0)
	if err := u.answered(t, "the clone's answer", errc); err != nil {
		t.Errorf("the clone: %v", err)
	}
	applied, _ = u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst)
	if applied.Compare(entity.JournalVersion(cv.Bytes())) < 0 {
		t.Errorf("applied %v after the clone's phase 1, want at least its %v", applied, cv)
	}
	if got := u.read(t, dst, 0, 4); !bytes.Equal(got, fill(4, 's')) {
		t.Error("the destination does not read the clone")
	}
	if got := u.read(t, dst, 6*mib, 10); !bytes.Equal(got, fill(10, 'z')) {
		t.Error("the write above the clone does not read back")
	}
}

// probeHandle holds the clone's first copy read once CloneTarget is done,
// and, while gated, a write once the journal gave it its version.
type probeHandle struct {
	engine.Handle
	targeted, goClone chan struct{}
	reads             atomic.Int64
	gate              atomic.Bool
	staged, goWrite   chan struct{}
}

func (h *probeHandle) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	if h.reads.Add(1) == 2 { // the first copy's read, after CloneTarget
		close(h.targeted)
		<-h.goClone
	}
	return h.Handle.ReadAt(ctx, id, off, p)
}

func (h *probeHandle) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, m int64) (journal.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, m)
	if h.gate.CompareAndSwap(true, false) {
		close(h.staged)
		<-h.goWrite
	}
	return v, err
}

// A write to the destination outside the extent that the journal gave a
// version between the clone's and its copies' newest, still on its way into
// the entry when phase 1 claims, is waited out and claimed: phase 1 records
// it, rather than setting applied past it, which every later commit would
// skip.
func TestAWriteInFlightDuringPhase1IsClaimed(t *testing.T) {
	called := make(chan struct{}) // closed once the store's Clone returned
	h := &probeHandle{targeted: make(chan struct{}), goClone: make(chan struct{}), staged: make(chan struct{}), goWrite: make(chan struct{})}
	u := clonedUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){onClone(func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
			defer close(called)
			return next.Clone(ctx, op, pending)
		})},
		wrap: func(x engine.Handle) engine.Handle {
			h.Handle = x
			return h
		},
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	h.reads.Store(0)
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-h.targeted
	h.gate.Store(true)
	werr := make(chan error, 1)
	go func() {
		_, err := u.e.Write(context.Background(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}})
		werr <- err
	}()
	<-h.staged // the write has its version, above the clone's, below the copies
	close(h.goClone)
	select {
	case <-called:
		t.Error("phase 1 ran while a write to the destination was still on its way into the entry")
	case <-time.After(200 * time.Millisecond):
	}
	close(h.goWrite)
	if err := <-werr; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if size, _ := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst); size != 3*mib+10 {
		t.Errorf("the destination's committed size %d, want %d: the write was skipped as applied", size, 3*mib+10)
	}
}

// A release or a truncate of a destination a clone holds is answered at
// once, the release pending and the truncate DelayClone, rather than behind
// the clone's guard.
func TestARemovalOfAClonesDestinationAnswersAtOnce(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if ch, err := u.e.Delete(ctx, u.share.ID, dst); err != nil || ch == nil {
		t.Errorf("a release of the clone's destination: %v, %v; want a pending channel", ch, err)
	}
	if err := u.e.Truncate(ctx, u.share.ID, dst, 4*mib); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a truncate of the clone's destination: %v, want DelayClone", err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// A file holds one clone into it at a time: a second clone into another
// extent of a destination a clone writes is refused DelayClone.
func TestASecondCloneIntoADestinationIsRefused(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{SrcOff: mib, DstOff: 4 * mib, Len: mib})
	if !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a second clone into the destination: %v, want DelayClone", err)
	}
	if _, d := u.e.Registrations(dst); d != 1 {
		t.Errorf("%d destination registrations on the file, want the first clone's alone", d)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// An allocation of an empty destination cloned far from its start answers a
// hole from 0 up to the extent, which the clone's journal step leaves.
func TestAnAllocationBeforeAFarCloneIsAHole(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{DstOff: 8 * mib, Len: mib})
	sp, err := u.e.Allocation(t.Context(), u.share.ID, dst, 0)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 0 || sp.End() != 8*mib {
		t.Errorf("an allocation from 0 of the destination: %+v, %v; want a hole up to 8 MiB", sp, err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// A loss that splits a source run after the clone sized its copies takes one
// more copy write than it reserved: the copy is refused, retried by the loop
// with a reservation sized again, and the clone completes, the source read
// around the loss.
func TestACopyPastItsReservationIsRetried(t *testing.T) {
	u, h := clonedHookedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	h.cloneTarget.set(func() { u.lose(src, mib/4, mib/4) }) // after the clone sized its copies
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	if got := u.answered(t, "the clone's answer", errc); got != nil {
		t.Fatalf("a clone whose copies outran their reservation: %v, want it retried to success", got)
	}
	if r := u.j.Reserves(); len(r) != 2 || r[1].Writes != 2 {
		t.Errorf("reserves %+v, want the retry's second one of 2 writes", r)
	}
	if got := u.read(t, dst, mib/2, mib/2); !bytes.Equal(got, fill(mib/2, 's')) {
		t.Error("the destination does not read the source after the loss")
	}
}

// A phase 1 failing on a conflict is retried without being logged as a
// refusal, and the clone completes.
func TestAConflictedClonePhase1IsRetriedQuietly(t *testing.T) {
	x := &refusingClone{err: errors.Join(kvstore.ErrConflict, context.DeadlineExceeded)}
	x.left.Store(1)
	var logged lockedBuffer
	u := clonedUnit(t, onClone(x.clone), func(d *engine.Deps) { d.Logger = slog.New(slog.NewTextHandler(&logged, nil)) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	if got := u.answered(t, "the clone's answer", errc); got != nil {
		t.Fatalf("a clone conflicted once at phase 1: %v", got)
	}
	if strings.Contains(logged.String(), "refused") {
		t.Errorf("a conflict was logged as a refusal: %q", logged.String())
	}
}

// A share dropped while its loop is running its clone's phase 1 ends the
// clone once that run returns, unavailable, and leaves the destination off
// the group commit's due set.
func TestADroppedShareEndsItsCloneAndNotesNothing(t *testing.T) {
	x := &refusingClone{entered: make(chan struct{}), hold: make(chan struct{})}
	x.left.Store(1 << 30)
	u := clonedUnit(t, onClone(x.clone))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "the loop running the clone's phase 1", func() bool {
		select {
		case <-x.entered:
			return true
		default:
			return false
		}
	})
	if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
		t.Fatal(err)
	}
	close(x.hold)
	if err := u.answered(t, "the dropped clone's answer", errc); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a clone of a dropped share: %v, want ErrUnavailable", err)
	}
	if u.e.Due(u.rec, dst) {
		t.Error("the dropped share's destination was noted for a commit again")
	}
}

// Until a clone's hold, phase 2 and done signal exist, a clone answers
// ErrUnbuilt once registered and leaves nothing behind.
func TestACloneWithoutItsDoneSignalIsUnbuilt(t *testing.T) {
	u := cloneUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	if err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib}); !errors.Is(err, engine.ErrUnbuilt) {
		t.Errorf("a clone with no done signal: %v, want ErrUnbuilt", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 || u.e.HasEntry(src) || u.e.HasEntry(dst) {
		t.Errorf("an unbuilt clone left %d CloneTargets, entries %v %v", n, u.e.HasEntry(src), u.e.HasEntry(dst))
	}
}

// The copies never reach the store's Clone, which Copied covers: none of the
// operations it is handed is a copy's.
func TestACloneHandsTheStoreNoCopy(t *testing.T) {
	var mu sync.Mutex
	var handled []kvstore.PendingOp // the operations each store Clone was handed
	u := clonedUnit(t, onClone(func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
		mu.Lock()
		handled = append(handled, pending...)
		mu.Unlock()
		return next.Clone(ctx, op, pending)
	}))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	copies := map[entity.JournalVersion]bool{}
	for _, v := range u.j.ReservedWrites() {
		copies[entity.JournalVersion(v.Bytes())] = true
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 1 {
		t.Errorf("the store's Clone was handed %d operations, want the destination's one write", len(handled))
	}
	for _, op := range handled {
		if copies[op.Version] {
			t.Errorf("the store's Clone was handed a copy at %v", op.Version)
		}
	}
}

// A clone's phase 1 committing the destination's pending write after the
// store answers and before the entry is read makes the pass read again:
// destination data [0, 2m), a pending write [2m, 2m+8k), then a clone into
// [0, 1m) commits it between the two reads, and a write at 5m is staged
// after. Read as one pass, Allocation(2m) would answer hole [2m, 5m). The
// clone lands below the offset asked, since a store lookup over a clone not
// done waits for it.
func TestAllocationReadsAgainAfterAClonesPhase1BetweenItsReads(t *testing.T) {
	ce := &committingExistence{}
	u := clonedUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	u.write(t, dst, 2*mib, fill(8*kib, 'w'))
	ce.after = func() {
		u.clone(t, src, dst, vfserr.Extent{Len: mib})
		u.write(t, dst, 5*mib, fill(2*kib, 'l'))
	}
	u.allocations(t, dst, run{2 * mib, 8 * kib, false})
}

// A store answering stale at phase 1 for a source gone, the destination
// still there, does not end the clone past its journal step: it is retried,
// and once phase 1 commits the destination reads the source's bytes.
func TestAStaleSourceAtPhase1IsRetried(t *testing.T) {
	x := &refusingClone{err: fmt.Errorf("the source: %w", entity.ErrStale)}
	x.left.Store(1)
	u := clonedUnit(t, onClone(x.clone))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	if n := x.calls.Load(); n != 2 {
		t.Errorf("phase 1 ran %d times, want the stale one retried once", n)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source's bytes")
	}
}

// idleResume is the store's content view whose Resume runs no batch and
// answers done, so a clone's removal stays not done in the store.
type idleResume struct{ engine.Content }

func (idleResume) Resume(context.Context, entity.ShareID, entity.FileID) (bool, error) {
	return false, nil
}

// A clone into a destination whose earlier clone the store still records
// as not done is answered DelayClone before its journal step, rather than
// taking it and retrying phase 1 with the destination's guard held.
func TestACloneIntoADestinationStillCloningIsDelayedBeforeItsJournalStep(t *testing.T) {
	u := clonedUnit(t, func(d *engine.Deps) { d.Content = idleResume{d.Content} })
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib}) // its Resume runs no batch: the store keeps it not done
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{SrcOff: mib, DstOff: mib, Len: mib})
	if !isDelay(err, vfserr.DelayClone) {
		t.Fatalf("a second clone into the destination: %v, want DelayClone", err)
	}
	if n := len(u.j.CloneTargets()); n != 1 {
		t.Errorf("the journal saw %d CloneTargets, want the first clone's alone", n)
	}
	if err := u.e.Truncate(ctx, u.share.ID, dst, 4*mib); err != nil {
		t.Errorf("a grow of the destination: %v, want its guard free", err)
	}
}

// A clone from a running clone's destination extent is answered DelayClone
// at once, not behind that clone's guard.
func TestACloneFromARunningClonesDestinationAnswersAtOnce(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	other := u.create(t, "other")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := u.e.Clone(ctx, u.share.ID, dst, u.share.ID, other, vfserr.Extent{Len: mib}); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a clone from the running clone's destination: %v, want DelayClone", err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// A call cancelled once its clone took the journal step answers DelayClone,
// never the cancel, which would read as refused with nothing changed: the
// clone runs on and changes the destination.
func TestACancelledCloneCallAnswersDelayClone(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	held, hold := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	u.e.OnStaged(func() {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-hold
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	errc := u.startClone(ctx, src, dst, vfserr.Extent{Len: mib})
	<-held
	cancel()
	if err := <-errc; !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a clone call cancelled past its journal step: %v, want DelayClone", err)
	}
	close(hold)
	u.until(t, "the clone's phase 1", func() bool { _, d := u.e.Registrations(dst); return d == 0 })
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source's bytes")
	}
}
