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
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// recHolds is a recording CloneHold. Each Hold runs onHold, when set, and
// answers its error; otherwise next's answer, or a new hold when next is
// nil. Each Release runs onRel, when set, and answers its error; otherwise
// next's, or nil, counted in freed once it answers nil.
type recHolds struct {
	mu       sync.Mutex
	next     engine.CloneHold
	holds    []engine.CloneSrc
	releases []entity.OpenID
	freed    int
	ids      entity.OpenID
	onHold   func(ctx context.Context) error
	onRel    func(ctx context.Context) error
}

func (h *recHolds) Hold(ctx context.Context, src engine.CloneSrc) (entity.OpenID, error) {
	h.mu.Lock()
	h.holds = append(h.holds, src)
	on := h.onHold
	h.mu.Unlock()
	if on != nil {
		if err := on(ctx); err != nil {
			return 0, err
		}
	}
	if h.next != nil {
		return h.next.Hold(ctx, src)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids++
	return h.ids, nil
}

func (h *recHolds) Release(ctx context.Context, o entity.OpenID) error {
	h.mu.Lock()
	h.releases = append(h.releases, o)
	on := h.onRel
	h.mu.Unlock()
	if on != nil {
		if err := on(ctx); err != nil {
			return err
		}
	}
	if h.next != nil {
		if err := h.next.Release(ctx, o); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.freed++
	h.mu.Unlock()
	return nil
}

// released reports how many Releases answered nil.
func (h *recHolds) released() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.freed
}

func (h *recHolds) Served(ctx context.Context, s entity.ShardID) error {
	if h.next != nil {
		return h.next.Served(ctx, s)
	}
	return nil
}

func (h *recHolds) counts() (holds, releases int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holds), len(h.releases)
}

// A clone calls its store and open state in order: phase 1's Clone, then
// Hold, the Resume of its batches, and Release once they are done, after
// the clone is answered, none of them under either file's entry lock or the
// destination's guard. The hold is keyed in the source share's own shard, the
// one Bind named: with the share bound on shard 7, not the default, and open
// state serving that shard alone, Hold names it and CloneOpen finds the record
// there.
func TestAClonesHoldSpansItsPhase2(t *testing.T) {
	h := &recHolds{}
	var mu sync.Mutex
	var calls, locked []string
	var u *unit
	var src, dst entity.FileID
	log := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, what)
		if what == "Clone" {
			return // phase 1 runs under the destination's guard
		}
		for _, f := range []entity.FileID{src, dst} {
			if u.e.EntryLocked(f) {
				locked = append(locked, what+" under an entry lock")
			}
		}
		if u.e.GuardHeld(dst) {
			locked = append(locked, what+" under the destination's guard")
		}
	}
	h.onHold = func(context.Context) error { log("Hold"); return nil }
	h.onRel = func(context.Context) error { log("Release"); return nil }
	u = clonedUnitWith(t, unitConfig{unbound: true, deps: []func(*engine.Deps){
		func(d *engine.Deps) { d.Holds = h },
		onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
			log("Resume")
			return next.Resume(ctx, s, file)
		}),
		onClone(func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
			err := next.Clone(ctx, op, pending)
			log("Clone")
			return err
		})}})
	u.share.Shard = 7
	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}
	u.withOpenState(t, h)
	src = u.committedFile(t, "src", fill(mib, 's'))
	dst = u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	u.until(t, "the hold released", func() bool { return h.released() > 0 })
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Clone", "Hold", "Resume", "Release"}; !slices.Equal(calls, want) {
		t.Errorf("the clone's calls: %v, want %v", calls, want)
	}
	if len(locked) > 0 {
		t.Errorf("called under a lock: %v", locked)
	}
	if len(h.holds) == 0 {
		return
	}
	if got := h.holds[0]; got.Share != u.share.ID || got.Shard != 7 || got.File != src || got.Dst != dst ||
		got.AsOf != entity.JournalVersion(u.j.CloneTargets()[0].Version.Bytes()) {
		t.Errorf("Hold(%+v), want the source's share, shard 7 and file, the destination and the clone's version", got)
	}
}

// A Hold answering DelayFrozen, a shard not yet served, holds phase 2 and
// is retried without counting toward removal_stalled: answered so more
// times than the stall allows, Resume runs only once it holds.
func TestAFrozenHoldIsRetriedUncounted(t *testing.T) {
	x := newResuming()
	logs := newLogRecorder()
	h := &recHolds{}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h; x.Content = d.Content; d.Content = x; d.Logger = slog.New(logs) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	const frozen = 6 // one more than removal_stalled's five
	var early atomic.Int64
	h.onHold = func(context.Context) error {
		if x.resumed(dst) > 0 {
			early.Add(1)
		}
		if n, _ := h.counts(); n <= frozen {
			return &vfserr.Delay{Cause: vfserr.DelayFrozen}
		}
		return nil
	}
	if err := u.answered(t, "the clone", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if n, _ := h.counts(); n != frozen+1 || early.Load() != 0 || x.resumed(dst) == 0 {
		t.Errorf("%d Holds, %d with Resume run before, Resume run %d times; want %d, none, and Resume after the last",
			n, early.Load(), x.resumed(dst), frozen+1)
	}
	for _, r := range logs.records() {
		if strings.HasPrefix(r.Message, "removal_stalled") {
			t.Errorf("logged %q for a clone held by a frozen shard", r.Message)
		}
	}
}

// A Hold refused for good, no open of the clone's on the source, is logged
// at Error once, and phase 2 runs without it: Resume runs and no Release
// follows.
func TestARefusedHoldRunsPhase2WithoutARelease(t *testing.T) {
	x := newResuming()
	logs := newLogRecorder()
	h := &recHolds{onHold: func(context.Context) error { return entity.ErrStale }}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h; x.Content = d.Content; d.Content = x; d.Logger = slog.New(logs) })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	if err := u.answered(t, "the clone", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if holds, releases := h.counts(); holds != 1 || releases != 0 || x.resumed(dst) == 0 {
		t.Errorf("%d Holds, %d Releases, Resume run %d times; want one Hold, no Release, Resume run", holds, releases, x.resumed(dst))
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
// so too, and retried.
func TestAHungHoldOrReleaseIsCutAndCounted(t *testing.T) {
	x := newResuming()
	h := &recHolds{}
	var hung atomic.Bool
	hung.Store(true)
	h.onHold = func(ctx context.Context) error {
		if hung.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h.onRel = func(ctx context.Context) error {
		if _, releases := h.counts(); releases == 1 {
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
	u.until(t, "the clone's hold asked", func() bool { n, _ := h.counts(); return n > 0 })
	if err := u.e.Truncate(t.Context(), u.share.ID, g, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "another file's removal pruned while the hold hangs", func() bool { _, ok := x.prunedAt(g); return ok })
	u.until(t, "removal_stalled for the destination", func() bool { return u.e.RemovalStalled(dst) })
	if n, _ := h.counts(); n < 5 || x.resumed(dst) != 0 {
		t.Errorf("stalled after %d Holds with Resume run %d times; want five failures first and no phase 2", n, x.resumed(dst))
	}
	hung.Store(false)
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "the hold released", func() bool { return h.released() > 0 })
	if _, releases := h.counts(); releases != 2 || x.resumed(dst) == 0 {
		t.Errorf("%d Releases, Resume run %d times; want the hung one cut and the hold released after phase 2", releases, x.resumed(dst))
	}
}

// A Release that fails is retried alone, after the clone is answered:
// while it keeps failing, the clone's registrations are gone and its
// destination extent is written; phase 2 is not run again, so
// engine_removal_runs_total{result="done"} moves once; and the Release is
// retried until it succeeds.
func TestAFailedReleaseRetriesOnlyTheRelease(t *testing.T) {
	h := &recHolds{}
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
	before := u.e.Stats().RemovalRuns["done"]
	if err := u.answered(t, "the clone while its Release fails", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "three failed Releases", func() bool { _, r := h.counts(); return r >= 3 })
	s, _ := u.e.Registrations(src)
	_, d := u.e.Registrations(dst)
	if s+d != 0 {
		t.Errorf("registrations while the Release fails: source %d, destination %d; want none", s, d)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the destination extent while the Release fails: %v, want served", err)
	}
	failing.Store(false)
	u.until(t, "the Release retried to success", func() bool { return h.released() == 1 })
	if got := u.e.Stats().RemovalRuns["done"] - before; got != 1 {
		t.Errorf("engine_removal_runs_total{done} moved by %d, want 1 for the clone's one phase 2", got)
	}
}

// A clone whose loop ends after its hold was taken releases the hold: a
// share dropped while its phase 2 runs ends the clone, unavailable, with
// the hold released, so open state keeps no open of the clone's.
func TestALoopEndReleasesTheClonesHold(t *testing.T) {
	h := &recHolds{}
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
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-entered
	if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
		t.Fatal(err)
	}
	close(open)
	if err := u.answered(t, "the dropped clone", errc); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a clone of a dropped share: %v, want ErrUnavailable", err)
	}
	if holds, releases := h.counts(); holds != 1 || releases != 1 {
		t.Errorf("%d Holds, %d Releases; want the hold released once", holds, releases)
	}
}

// A clone whose loop ends while its Release fails, its phase 2 done and the
// clone answered, releases the hold once more at the loop's end, and is not
// ended a second time.
func TestALoopEndRetriesAFailedRelease(t *testing.T) {
	h := &recHolds{onRel: func(context.Context) error { return errors.New("an injected release failure") }}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	if err := u.answered(t, "the clone", u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	u.until(t, "a failed Release", func() bool { _, r := h.counts(); return r > 0 })
	_, before := h.counts()
	u.e.StopRemovalLoops()
	if _, after := h.counts(); after <= before {
		t.Errorf("%d Releases at the loop's end after %d, want one more", after, before)
	}
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
// serving its share's shard, behind h, and answers it with its sink.
func (u *unit) withOpenState(t *testing.T, h *recHolds) (*openstate.Table, *enginetest.Sink) {
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
	h.next = enginetest.Holds(tb, u.fs.Store.Opens())
	return tb, sink
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
			h := &recHolds{}
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
			tb, _ := u.withOpenState(t, h)
			src := u.committedFile(t, "src", fill(mib, 's'))
			dst = u.create(t, "dst")
			key := openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: src}
			errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
			u.until(t, "the clone's hold asked", func() bool { n, _ := h.counts(); return n > 0 })

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
			u.until(t, "the hold released", func() bool { return c.always || h.released() > 0 })
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
	h := &recHolds{}
	var released atomic.Bool
	h.onRel = func(context.Context) error { released.Store(true); return nil }
	var dst entity.FileID
	entered, open := make(chan struct{}), make(chan struct{})
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h },
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
	tb, sink := u.withOpenState(t, h)
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
	u.until(t, "the clone's Release", released.Load)
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
	h := &recHolds{onHold: func(context.Context) error { return errors.New("held off") }}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	tb, _ := u.withOpenState(t, h)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "the clone's hold asked", func() bool { n, _ := h.counts(); return n > 0 })
	h.mu.Lock()
	held := h.holds[0]
	h.mu.Unlock()
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
