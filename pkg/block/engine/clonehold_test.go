package engine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// recHolds is a recording CloneHold. Each Hold runs onHold, when set, and
// answers its error; otherwise next's answer, or a new hold when next is
// nil. Each Release runs onRel, when set, then answers next's, or nil.
type recHolds struct {
	mu       sync.Mutex
	next     engine.CloneHold
	holds    []engine.CloneSrc
	releases []entity.OpenID
	ids      entity.OpenID
	onHold   func() error
	onRel    func()
}

func (h *recHolds) Hold(ctx context.Context, src engine.CloneSrc) (entity.OpenID, error) {
	h.mu.Lock()
	h.holds = append(h.holds, src)
	on := h.onHold
	h.mu.Unlock()
	if on != nil {
		if err := on(); err != nil {
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
		on()
	}
	if h.next != nil {
		return h.next.Release(ctx, o)
	}
	return nil
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

// gatedResume is the store's content view whose Resume of file f waits,
// once entered is closed, until open is closed, then runs.
type gatedResume struct {
	engine.Content
	f             entity.FileID
	entered, open chan struct{}
	once          sync.Once
}

func newGatedResume(f entity.FileID) *gatedResume {
	return &gatedResume{f: f, entered: make(chan struct{}), open: make(chan struct{})}
}

func (x *gatedResume) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) (bool, error) {
	if file == x.f {
		x.once.Do(func() { close(x.entered) })
		<-x.open
	}
	return x.Content.Resume(ctx, share, file)
}

// logContent is the store's content view logging each Resume through log.
type logContent struct {
	engine.Content
	log func(string)
}

func (x logContent) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) (bool, error) {
	x.log("Resume")
	return x.Content.Resume(ctx, share, file)
}

// A clone calls its store and open state in order: phase 1's Clone, then
// Hold, the Resume of its batches, and Release once they are done, none of
// them under either file's entry lock or the destination's guard.
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
	h.onHold = func() error { log("Hold"); return nil }
	h.onRel = func() { log("Release") }
	u = clonedUnit(t,
		func(d *engine.Deps) { d.Holds = h; d.Content = logContent{d.Content, log} },
		onClone(func(ctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
			err := next.Clone(ctx, op, pending)
			log("Clone")
			return err
		}))
	src = u.committedFile(t, "src", fill(mib, 's'))
	dst = u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
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
	if got := h.holds[0]; got.Share != u.share.ID || got.Shard != u.share.Shard || got.File != src || got.Dst != dst ||
		got.AsOf != entity.JournalVersion(u.j.CloneTargets()[0].Version.Bytes()) {
		t.Errorf("Hold(%+v), want the source's share, shard and file, the destination and the clone's version", got)
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
	h.onHold = func() error {
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

// A Hold answering any other error is logged at Error once, and phase 2
// runs without it: Resume runs and no Release follows.
func TestAFailedHoldRunsPhase2WithoutARelease(t *testing.T) {
	x := newResuming()
	logs := newLogRecorder()
	h := &recHolds{onHold: func() error { return errors.New("an injected hold failure") }}
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
	for _, r := range logs.records() {
		if r.Message != msg {
			continue
		}
		named := false
		r.Attrs(func(a slog.Attr) bool {
			named = named || a.Key == "file" && a.Value.String() == f.String()
			return true
		})
		if named {
			return true
		}
	}
	return false
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
	h.next = enginetest.Holds(tb)
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

// A source unlinked with no open between phase 1 and the hold, while open
// state's shard answers DelayFrozen, keeps its content: its release is
// answered pending, with nothing yielded while the clone runs, and runs
// again once the registration drops, releasing it.
func TestASourceUnlinkedBeforeTheHoldIsReleasedAfterTheClone(t *testing.T) {
	h := &recHolds{}
	var frozen atomic.Bool
	frozen.Store(true)
	h.onHold = func() error {
		if frozen.Load() {
			return &vfserr.Delay{Cause: vfserr.DelayFrozen}
		}
		return nil
	}
	u := clonedUnit(t, func(d *engine.Deps) { d.Holds = h })
	tb, _ := u.withOpenState(t, h)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
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

	frozen.Store(false)
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
	done, err = tb.Opens().Releasing(t.Context(), key)
	if err != nil {
		t.Fatalf("Releasing the source after the clone: %v", err)
	}
	if ch, err := u.e.Delete(t.Context(), u.share.ID, src); ch != nil || err != nil {
		t.Errorf("the release run again: %v, %v; want it released", ch, err)
	}
	done(true)
}

// A source unlinked while phase 2 runs keeps its content: open state holds
// the clone's open, so the release is refused, and only after Release does
// Sink.Closed report the orphan, whose release then runs.
func TestASourceUnlinkedDuringPhase2IsAnOrphanAfterRelease(t *testing.T) {
	h := &recHolds{}
	var released atomic.Bool
	h.onRel = func() { released.Store(true) }
	var x *gatedResume
	u := clonedUnit(t, func(d *engine.Deps) {
		d.Holds = h
		x = newGatedResume(entity.FileID{})
		x.Content = d.Content
		d.Content = x
	})
	tb, sink := u.withOpenState(t, h)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	x.f = dst
	key := openstate.FileKey{Share: u.share.ID, Shard: u.share.Shard, ID: src}
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	<-x.entered

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

	close(x.open)
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !released.Load() {
		t.Fatal("the clone answered with no Release")
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

// The hold is keyed in the source share's own shard, the one Bind named:
// with the share bound on shard 7 and open state serving that shard alone,
// Hold names shard 7 and CloneOpen finds the record there, the clone then
// releasing it.
func TestAClonesHoldIsKeyedInTheSourcesShard(t *testing.T) {
	h := &recHolds{}
	u := clonedUnitWith(t, unitConfig{unbound: true, deps: []func(*engine.Deps){func(d *engine.Deps) { d.Holds = h }}})
	u.share.Shard = 7
	if err := u.e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}
	u.withOpenState(t, h)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})
	u.until(t, "the clone's hold asked", func() bool { n, _ := h.counts(); return n > 0 })
	h.mu.Lock()
	got := h.holds[0].Shard
	h.mu.Unlock()
	if got != u.share.Shard {
		t.Fatalf("Hold named shard %v, want the source share's %v", got, u.share.Shard)
	}
	if err := u.answered(t, "the clone", errc); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, releases := h.counts(); releases != 1 {
		t.Errorf("%d Releases, want the hold CloneOpen found released once", releases)
	}
}
