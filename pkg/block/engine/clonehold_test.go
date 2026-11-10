package engine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// faultHolds is the real open-state table, next, behind fault hooks: each
// Hold runs onHold, when set, and answers its error, and each Release runs
// onRel so; otherwise next answers.
type faultHolds struct {
	mu     sync.Mutex
	next   engine.CloneHold
	onHold func(ctx context.Context) error
	onRel  func(ctx context.Context) error
}

func (h *faultHolds) Hold(ctx context.Context, src engine.CloneSrc) (entity.OpenID, error) {
	h.mu.Lock()
	on, next := h.onHold, h.next
	h.mu.Unlock()
	if on != nil {
		if err := on(ctx); err != nil {
			return 0, err
		}
	}
	return next.Hold(ctx, src)
}

func (h *faultHolds) Release(ctx context.Context, o entity.OpenID) error {
	h.mu.Lock()
	on, next := h.onRel, h.next
	h.mu.Unlock()
	if on != nil {
		if err := on(ctx); err != nil {
			return err
		}
	}
	return next.Release(ctx, o)
}

func (h *faultHolds) Served(ctx context.Context, s entity.ShardID) error {
	h.mu.Lock()
	next := h.next
	h.mu.Unlock()
	return next.Served(ctx, s)
}

// cloneRecord is the source's open record of the clone onto dst in the
// store, which phase 1 writes and phase 2's last batch deletes.
func (u *unit) cloneRecord(t *testing.T, src, dst entity.FileID) (entity.OpenRecord, bool) {
	t.Helper()
	recs, err := u.fs.Store.Opens().Of(context.Background(), u.share.ID, src)
	if err != nil {
		t.Error(err)
	}
	i := slices.IndexFunc(recs, func(o entity.OpenRecord) bool {
		return o.Kind == entity.OpenCloneSource && o.CloneDst == dst
	})
	if i < 0 {
		return entity.OpenRecord{}, false
	}
	return recs[i], true
}

// holdsSource reports whether open state holds an open of src, a clone's
// hold included: Releasing refuses src then, and otherwise marks it, the
// mark ended at once with nothing changed.
func (u *unit) holdsSource(t *testing.T, src entity.FileID) bool {
	done, err := u.tb.Opens().Releasing(context.Background(), openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: src})
	if errors.Is(err, openstate.ErrFileOpen) {
		return true
	}
	if err != nil {
		t.Errorf("Releasing the source: %v", err)
		return false
	}
	done(false)
	return false
}

// released waits for open state to hold src no longer, then checks the
// table holds the clone's open id no longer either.
func (u *unit) released(t *testing.T, src entity.FileID, id entity.OpenID) {
	t.Helper()
	u.until(t, "the clone's hold released", func() bool { return !u.holdsSource(t, src) })
	if err := u.tb.Opens().CloneClose(t.Context(), id); !errors.Is(err, openstate.ErrBadState) {
		t.Errorf("closing the clone's hold once released: %v, want ErrBadState", err)
	}
}

// phase2Done reports whether dst's extent [0, n) is served, its clone's
// phase 2 done: until then the covering lookup over it answers ErrDelay.
func (u *unit) phase2Done(dst entity.FileID, n int64) bool {
	for _, err := range u.fs.Store.Content().Covering(context.Background(), u.share.ID, dst, 0, n) {
		if errors.Is(err, entity.ErrDelay) {
			return false
		}
	}
	return true
}

// holdOf has h read, at each Hold before onHold, the open record of the
// clone of src onto dst, which phase 2's last batch deletes, and answers
// the ID last read: the open the clone's Hold takes.
func (u *unit) holdOf(t *testing.T, h *faultHolds, src, dst entity.FileID) func() entity.OpenID {
	var id atomic.Uint64
	h.mu.Lock()
	on := h.onHold
	h.onHold = func(ctx context.Context) error {
		if r, ok := u.cloneRecord(t, src, dst); ok {
			id.Store(uint64(r.ID))
		}
		if on != nil {
			return on(ctx)
		}
		return nil
	}
	h.mu.Unlock()
	return func() entity.OpenID { return entity.OpenID(id.Load()) }
}

// A clone holds its source across its phase 2, none of its Hold, Resume or
// Release under either file's entry lock or the destination's guard: at the
// Hold, phase 1 has written the clone's open record and phase 2 has not run;
// at the destination's Resume, open state holds the source; at the Release,
// phase 2 is done; and once released, the table holds the open no longer. The hold is keyed in the source share's own shard, the one
// Bind named: with the share bound on shard 7, not the default, and open
// state serving that shard alone, a Hold naming another shard never answers
// and the clone never ends.
func TestAClonesHoldSpansItsPhase2(t *testing.T) {
	h := &faultHolds{}
	var mu sync.Mutex
	var wrong []string
	var u *unit
	var src, dst entity.FileID
	var held entity.OpenRecord
	check := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range []entity.FileID{src, dst} {
			if u.e.EntryLocked(f) {
				wrong = append(wrong, what+" under an entry lock")
			}
		}
		if u.e.GuardHeld(dst) {
			wrong = append(wrong, what+" under the destination's guard")
		}
	}
	h.onHold = func(context.Context) error {
		check("Hold")
		r, ok := u.cloneRecord(t, src, dst)
		mu.Lock()
		defer mu.Unlock()
		if !ok {
			wrong = append(wrong, "Hold before phase 1 wrote the clone's open record")
		}
		if u.phase2Done(dst, mib) {
			wrong = append(wrong, "Hold after phase 2")
		}
		held = r
		return nil
	}
	h.onRel = func(context.Context) error {
		check("Release")
		if !u.phase2Done(dst, mib) {
			mu.Lock()
			wrong = append(wrong, "Release before phase 2 was done")
			mu.Unlock()
		}
		return nil
	}
	u = clonedUnitWith(t, unitConfig{unbound: true, deps: []func(*engine.Deps){
		func(d *engine.Deps) { d.Holds = h },
		onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
			check("Resume")
			if file == dst && !u.holdsSource(t, src) {
				mu.Lock()
				wrong = append(wrong, "Resume with the source not held")
				mu.Unlock()
			}
			return next.Resume(ctx, s, file)
		})}})
	u.share.Shard = 7
	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}
	u.withOpenState(t, h)
	src = u.committedFile(t, "src", fill(mib, 's'))
	dst = u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	mu.Lock()
	id := held.ID
	mu.Unlock()
	u.released(t, src, id)
	mu.Lock()
	defer mu.Unlock()
	if len(wrong) > 0 {
		t.Errorf("the clone's calls: %v", wrong)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source")
	}
}

// A Hold answering DelayFrozen, a shard not yet served, holds phase 2 and
// is retried without counting toward removal_stalled: answered so more
// times than the stall allows, the clone then holds its source, phase 2 not
// yet run, and releases it once done.
func TestAFrozenHoldIsRetriedUncounted(t *testing.T) {
	logs := newLogRecorder()
	h := &faultHolds{}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h; d.Logger = slog.New(logs) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	const frozen = 6 // one more than removal_stalled's five
	var asked atomic.Int64
	var early atomic.Bool
	var id atomic.Uint64
	h.onHold = func(context.Context) error {
		if asked.Add(1) <= frozen {
			return &vfserr.Delay{Cause: vfserr.DelayFrozen}
		}
		early.Store(u.phase2Done(dst, mib))
		r, _ := u.cloneRecord(t, src, dst)
		id.Store(uint64(r.ID))
		return nil
	}
	if err := u.answered(t, "the clone", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if early.Load() {
		t.Error("phase 2 ran before the clone held its source")
	}
	u.released(t, src, entity.OpenID(id.Load()))
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source")
	}
	for _, r := range logs.records() {
		if strings.HasPrefix(r.Message, "removal_stalled") {
			t.Errorf("logged %q for a clone held by a frozen shard", r.Message)
		}
	}
}

// A Hold refused for good, no open of the clone's on the source, is logged
// at Error once, and phase 2 runs without it: the clone ends, its removal
// done, and the destination reads the source.
func TestARefusedHoldRunsPhase2WithoutARelease(t *testing.T) {
	logs := newLogRecorder()
	h := &faultHolds{onHold: func(context.Context) error { return entity.ErrStale }}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h; d.Logger = slog.New(logs) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	if err := u.answered(t, "the clone", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "the clone's removal done", func() bool { return u.e.RemovalsPending() == 0 })
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source")
	}
	errs := 0
	for _, r := range logs.records() {
		if r.Level == slog.LevelError {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("%d Error lines, want the failed hold's one", errs)
	}
}

// A Hold that never answers is cut at the conflict bound and counted: five
// in a row raise removal_stalled for the destination, with no phase 2 run,
// another file's removal on the same journal runs meanwhile, and once Hold
// answers the clone runs its phase 2. A Release that never answers is cut
// so too, and retried until the hold is released.
func TestAHungHoldOrReleaseIsCutAndCounted(t *testing.T) {
	x := newResuming()
	h := &faultHolds{}
	var hung, relHung atomic.Bool
	hung.Store(true)
	relHung.Store(true)
	h.onHold = func(ctx context.Context) error {
		if hung.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h.onRel = func(ctx context.Context) error {
		if relHung.Swap(false) {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h; x.Content = d.Content; d.Content = x })
	u.e.SetConflictBound(10 * time.Millisecond)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	g := u.committedFile(t, "g", fill(16, 'g'))
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	var held entity.OpenRecord
	u.until(t, "the clone's phase 1", func() bool { var ok bool; held, ok = u.cloneRecord(t, src, dst); return ok })
	if err := u.e.Truncate(t.Context(), u.share.ID, g, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "another file's removal pruned while the hold hangs", func() bool { _, ok := x.prunedAt(g); return ok })
	u.until(t, "removal_stalled for the destination", func() bool { return u.e.RemovalStalled(dst) })
	if u.phase2Done(dst, mib) {
		t.Error("phase 2 ran while the hold hung")
	}
	hung.Store(false)
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.released(t, src, held.ID)
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source")
	}
}

// A Release that fails is retried alone, after the clone is answered:
// while it keeps failing, counted to removal_stalled, the clone's
// registrations are gone and its destination extent is written; phase 2 is
// not run again, so engine_removal_runs_total{result="done"} moves once; and
// the Release is retried until the hold is released.
func TestAFailedReleaseRetriesOnlyTheRelease(t *testing.T) {
	h := &faultHolds{}
	var failing atomic.Bool
	failing.Store(true)
	h.onRel = func(context.Context) error {
		if failing.Load() {
			return errors.New("an injected release failure")
		}
		return nil
	}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	hold := u.holdOf(t, h, src, dst)
	before := u.e.Stats().RemovalRuns["done"]
	if err := u.answered(t, "the clone while its Release fails", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "removal_stalled for the failing Release", func() bool { return u.e.RemovalStalled(dst) })
	s, _ := u.e.Registrations(src)
	_, d := u.e.Registrations(dst)
	if s+d != 0 {
		t.Errorf("registrations while the Release fails: source %d, destination %d; want none", s, d)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the destination extent while the Release fails: %v, want served", err)
	}
	if !u.holdsSource(t, src) {
		t.Error("the hold was released while every Release failed")
	}
	failing.Store(false)
	u.released(t, src, hold())
	if got := u.e.Stats().RemovalRuns["done"] - before; got != 1 {
		t.Errorf("engine_removal_runs_total{done} moved by %d, want 1 for the clone's one phase 2", got)
	}
}

// A clone whose loop ends after its hold was taken releases the hold: a
// share dropped while its phase 2 runs ends the clone, unavailable, with
// the hold released, so open state keeps no open of the clone's.
func TestALoopEndReleasesTheClonesHold(t *testing.T) {
	h := &faultHolds{}
	var dst entity.FileID
	entered, open := make(chan struct{}), make(chan struct{})
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h },
		onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
			if file != dst {
				return next.Resume(ctx, s, file)
			}
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-open
			return false, errors.New("an injected phase 2 failure")
		}))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst = u.create(t, "dst")
	hold := u.holdOf(t, h, src, dst)
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-entered
	if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
		t.Fatal(err)
	}
	close(open)
	if err := u.answered(t, "the dropped clone", errc); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a clone of a dropped share: %v, want ErrUnavailable", err)
	}
	u.released(t, src, hold())
}

// A clone whose loop ends while its Release fails, its phase 2 done and the
// clone answered, releases the hold once more at the loop's end. The loop
// retries only as the clock moves, and the clock stands still from the
// first failure to the loop's end, so only the loop's end can release it.
func TestALoopEndRetriesAFailedRelease(t *testing.T) {
	h := &faultHolds{}
	var failing atomic.Bool
	failing.Store(true)
	failed := make(chan struct{})
	var once sync.Once
	h.onRel = func(context.Context) error {
		if failing.Load() {
			once.Do(func() { close(failed) })
			return errors.New("an injected release failure")
		}
		return nil
	}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	hold := u.holdOf(t, h, src, dst)
	// Answered with the clock unmoved, so the failed Release is never due again.
	select {
	case err := <-u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib}):
		if err != nil {
			t.Fatalf("Clone: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the clone was never answered")
	}
	<-failed
	failing.Store(false)
	if !u.holdsSource(t, src) {
		t.Fatal("the hold was released before the loop's end")
	}
	u.e.StopRemovalLoops()
	u.released(t, src, hold())
}

// A clone's phase 2 failing past removal_stalled's five raises it naming
// the destination, keeps both registrations, and clears once it succeeds,
// the registrations then dropped.
func TestAStalledClonePhase2KeepsItsRegistrations(t *testing.T) {
	x := newResuming()
	logs := newLogRecorder()
	u := clonedUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x; d.Logger = slog.New(logs) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	x.mu.Lock()
	x.failing[dst] = true
	x.mu.Unlock()
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "removal_stalled for the destination", func() bool { return u.e.RemovalStalled(dst) })
	if x.resumed(dst) < 5 {
		t.Errorf("stalled after %d Resumes, want five failures first", x.resumed(dst))
	}
	if !logged(logs, "removal_stalled", dst) {
		t.Error("no removal_stalled line names the destination")
	}
	s, _ := u.e.Registrations(src)
	_, d := u.e.Registrations(dst)
	if s != 1 || d != 1 {
		t.Errorf("registrations while stalled: source %d, destination %d; want both held", s, d)
	}
	x.mu.Lock()
	x.failing[dst] = false
	x.mu.Unlock()
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !logged(logs, "removal_stalled cleared", dst) || u.e.RemovalStalled(dst) {
		t.Error("removal_stalled not cleared once phase 2 succeeded")
	}
	s, _ = u.e.Registrations(src)
	_, d = u.e.Registrations(dst)
	if s+d != 0 {
		t.Errorf("registrations after the clone: source %d, destination %d; want none", s, d)
	}
}

// logged reports whether logs hold a line msg naming file f.
func logged(logs logRecorder, msg string, f entity.FileID) bool {
	return slices.ContainsFunc(logs.records(), func(r slog.Record) bool {
		return r.Message == msg && attr(r, "file") == f.String()
	})
}

// withOpenState composes the real open-state table over the unit's store,
// serving its share's shard, behind h, and keeps it and its sink on u.
func (u *unit) withOpenState(t *testing.T, h *faultHolds) {
	t.Helper()
	tb, err := openstate.New(openstate.Deps{Records: u.fs.Store, Clock: u.clock, Rand: rand.Reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	sink := &enginetest.Sink{}
	if err := tb.Start(t.Context(), sink, func(entity.FileID) func() { return func() {} }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tb.Stop)
	if err := tb.Recovery().ServeShard(t.Context(), u.share.ID, u.share.Shard, 1); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.next = enginetest.Holds(tb, u.fs.Store.Opens())
	h.mu.Unlock()
	u.tb, u.sink = tb, sink
}

// unlink removes name, the last entry of key's file, from the share's
// root, writing open state's durable records in its transaction as the
// filesystem service does.
func (u *unit) unlink(t *testing.T, tb *openstate.Table, key openstate.FileKey, name string) {
	t.Helper()
	recs, commit, err := tb.Opens().Durable(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.fs.Store.Namespace().Unlink(t.Context(), u.fs.Admin, u.fs.Root, []byte(name), recs...)
	commit(err == nil)
	if err != nil {
		t.Fatal(err)
	}
}

// A source unlinked with no open between phase 1 and the hold keeps its
// content while open state holds nothing of it: with its shard answering
// DelayFrozen, and with Hold refused for good while phase 2 runs, Releasing
// passes the source's durable open by, and the clone's registration on the
// source answers the release pending, yielding nothing while the clone
// runs; it runs again once the registration drops, releasing it.
func TestASourceUnlinkedBeforeTheHoldIsReleasedAfterTheClone(t *testing.T) {
	for _, c := range []struct {
		name   string
		hold   error
		always bool // Hold answers it after the clone is let go too
	}{
		{"frozen", &vfserr.Delay{Cause: vfserr.DelayFrozen}, false},
		{"refused", entity.ErrStale, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &faultHolds{}
			var held atomic.Bool // the clone is held: Hold answers c.hold, phase 2 fails
			held.Store(true)
			h.onHold = func(context.Context) error {
				if c.always || held.Load() {
					return c.hold
				}
				return nil
			}
			var dst entity.FileID
			u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h },
				onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
					if file == dst && held.Load() {
						return false, errors.New("an injected phase 2 failure")
					}
					return next.Resume(ctx, s, file)
				}))
			tb := u.tb
			src := u.committedFile(t, "src", fill(mib, 's'))
			dst = u.create(t, "dst")
			hold := u.holdOf(t, h, src, dst)
			key := openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: src}
			errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
			u.until(t, "the clone's hold asked", func() bool { return hold() != 0 })

			u.unlink(t, tb, key, "src")
			done, err := tb.Opens().Releasing(t.Context(), key)
			if err != nil {
				t.Fatalf("Releasing a source open state holds nothing of: %v", err)
			}
			ch, err := u.e.Delete(t.Context(), u.share.ID, src)
			if err != nil || ch == nil {
				t.Fatalf("the release of the cloning source: %v, %v; want it pending", ch, err)
			}
			ran := u.e.LoopSteps(u.share.ID)
			u.until(t, "the loop running on", func() bool { return u.e.LoopSteps(u.share.ID) > ran+2 })
			select {
			case got := <-ch:
				t.Fatalf("the pending release answered %v while the clone was held", got)
			default:
			}
			if got := u.read(t, src, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
				t.Error("the source lost its content while its release was pending")
			}

			held.Store(false)
			if err := u.answered(t, "the clone", errc); err != nil {
				t.Fatalf("Clone: %v", err)
			}
			if got := u.answered(t, "the pending release", ch); !isDelay(got, vfserr.DelayClone) {
				t.Fatalf("the pending release: %v, want DelayClone", got)
			}
			done(false)
			if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
				t.Error("the destination does not read the source")
			}
			// The clone's open, which the Release closes after the clone is
			// answered, holds the source until then.
			if !c.always {
				u.released(t, src, hold())
			}
			done, err = tb.Opens().Releasing(t.Context(), key)
			if err != nil {
				t.Fatalf("Releasing the source after the clone: %v", err)
			}
			if ch, err := u.e.Delete(t.Context(), u.share.ID, src); ch != nil || err != nil {
				t.Errorf("the release run again: %v, %v; want it released", ch, err)
			}
			done(true)
		})
	}
}

// A source unlinked while phase 2 runs keeps its content: open state holds
// the clone's open, so the release is refused, and only after Release does
// Sink.Closed report the orphan, whose release then runs.
func TestASourceUnlinkedDuringPhase2IsAnOrphanAfterRelease(t *testing.T) {
	var dst entity.FileID
	entered, open := make(chan struct{}), make(chan struct{})
	u := clonedUnit(t,
		onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
			if file == dst {
				select { // the first Resume of dst waits; its run then finishes the clone
				case <-entered:
				default:
					close(entered)
				}
				<-open
			}
			return next.Resume(ctx, s, file)
		}))
	tb, sink := u.tb, u.sink
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst = u.create(t, "dst")
	key := openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: src}
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-entered

	u.unlink(t, tb, key, "src")
	if _, err := tb.Opens().Releasing(t.Context(), key); !errors.Is(err, openstate.ErrFileOpen) {
		t.Errorf("Releasing the source while the clone's phase 2 runs: %v, want ErrFileOpen", err)
	}
	if got := u.read(t, src, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the source lost its content while the clone held it")
	}
	if c := sink.ClosedCalls(); len(c) != 0 {
		t.Errorf("Closed before the clone's Release: %+v", c)
	}

	close(open)
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "the orphan reported", func() bool {
		return slices.ContainsFunc(sink.ClosedCalls(), func(c enginetest.Closed) bool { return c.File == src && c.Result.Orphan })
	})
	done, err := tb.Opens().Releasing(t.Context(), key)
	if err != nil {
		t.Fatalf("Releasing the orphan: %v", err)
	}
	if ch, err := u.e.Delete(t.Context(), u.share.ID, src); ch != nil || err != nil {
		t.Errorf("the orphan's release: %v, %v; want it released", ch, err)
	}
	done(true)
}

// Removals of a destination whose clone is in phase 2 are held until the
// clone is done: a punch in its extent, a truncate, a clone onto it and an
// unlink with no open answer DelayClone or pending, and the clone's removal
// is not pruned while they wait. Once the clone is done the unlink's channel
// answers, and each applies: the punched range reads zeros, never the cloned
// bytes, around the clone's own.
func TestRemovalsOfACloningDestinationWaitForIt(t *testing.T) {
	x := newResuming()
	var dst entity.FileID
	entered, open := make(chan struct{}), make(chan struct{})
	var once sync.Once
	u := clonedUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x },
		onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
			if file == dst {
				once.Do(func() { close(entered) })
				<-open
			}
			return next.Resume(ctx, s, file)
		}))
	src := u.committedFile(t, "src", fill(mib, 's'))
	other := u.committedFile(t, "other", fill(mib, 'o'))
	dst = u.committedFile(t, "dst", fill(2*mib, 'd'))
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-entered

	// Each call is answered at once, so a bound no answer needs ends a
	// removal admitted mid-clone, which would wait on the held phase 2.
	held, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx, s := t.Context(), u.share.ID
	if err := u.e.Deallocate(held, s, dst, 0, 4096); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a punch in the destination extent during phase 2: %v, want DelayClone", err)
	}
	if err := u.e.Truncate(held, s, dst, 0); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a truncate of the destination during phase 2: %v, want DelayClone", err)
	}
	if err := u.e.Clone(held, s, other, s, dst, vfserr.Extent{DstOff: mib, Len: mib}); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a clone onto the destination during phase 2: %v, want DelayClone", err)
	}
	if _, err := u.fs.Store.Namespace().Unlink(ctx, u.fs.Admin, u.fs.Root, []byte("dst")); err != nil {
		t.Fatal(err)
	}
	ch, err := u.e.Delete(held, s, dst)
	if err != nil || ch == nil {
		t.Fatalf("an unlink of the destination during phase 2: %v, %v; want it pending", ch, err)
	}
	if _, ok := x.prunedAt(dst); ok {
		t.Error("the clone's removal was pruned before its phase 2 ran")
	}

	close(open)
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got := u.answered(t, "the pending unlink", ch); !isDelay(got, vfserr.DelayClone) {
		t.Errorf("the pending unlink: %v, want DelayClone to run it again", got)
	}
	if _, ok := x.prunedAt(dst); !ok {
		t.Error("the clone's removal was not pruned once done")
	}
	if err := u.e.Deallocate(ctx, s, dst, 0, 4096); err != nil {
		t.Fatalf("the punch once the clone is done: %v", err)
	}
	if got := u.read(t, dst, 0, 4096); !zeros(got) {
		t.Error("the punched range does not read zeros")
	}
	if got := u.read(t, dst, 4096, mib-4096); !bytes.Equal(got, fill(mib-4096, 's')) {
		t.Error("the destination extent around the punch does not read the source")
	}
	u.clone(t, other, dst, vfserr.Extent{DstOff: mib, Len: mib})
	if got := u.read(t, dst, mib, mib); !bytes.Equal(got, fill(mib, 'o')) {
		t.Error("the clone onto the destination once done does not read its source")
	}
	if err := u.e.Truncate(ctx, s, dst, 0); err != nil {
		t.Errorf("the truncate once the clone is done: %v", err)
	}
	if ch, err := u.e.Delete(ctx, s, dst); ch != nil || err != nil {
		t.Errorf("the unlink run again: %v, %v; want it released", ch, err)
	}
}

// The fixture's hold answers ErrStale only for a source holding no open of
// the clone's: once open state dropped the source's share, a Hold of the
// clone's durable open, still in the store, answers an error the engine
// retries, not the refusal it runs phase 2 without.
func TestTheFixturesHoldTellsADroppedShareFromNoOpen(t *testing.T) {
	h := &faultHolds{onHold: func(context.Context) error { return errors.New("held off") }}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	tb := u.tb
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	var rec entity.OpenRecord
	u.until(t, "the clone's open record", func() bool { var ok bool; rec, ok = u.cloneRecord(t, src, dst); return ok })
	held := engine.CloneSrc{Share: u.share.ID, Shard: u.share.Shard, File: src, Dst: dst, AsOf: rec.CloneVersion}
	fixture := enginetest.Holds(tb, u.fs.Store.Opens())
	none := held
	none.Dst = entity.NewFileID()
	if _, err := fixture.Hold(t.Context(), none); !errors.Is(err, entity.ErrStale) {
		t.Errorf("a Hold naming no open of the source: %v, want ErrStale", err)
	}
	if err := tb.Recovery().DropShare(t.Context(), u.share.ID, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Hold(t.Context(), held); err == nil || errors.Is(err, entity.ErrStale) {
		t.Errorf("a Hold on a dropped share: %v, want an error other than ErrStale", err)
	}
}
