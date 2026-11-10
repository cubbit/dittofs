package enginetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Settings are the engine settings the fixture applies. The existence age is
// far past the journal's sync bound, so a test that moves the clock by the
// sync bound makes a write durable without making its group commit due.
var Settings = engine.Settings{
	ExistenceAge:        time.Minute,
	GroupCommitMaxFiles: 64,
	CloneMaxLen:         1 << 30,
}

// fixtureStart is when the fixture's clock starts.
var fixtureStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fixtureInstall is the installation ID the fixture's journal is created
// under.
var fixtureInstall = journal.InstallationID{'e', 'n', 'g', 'i', 'n', 'e'}

// fixtureNode is the node the fixture's engine runs as.
var fixtureNode = entity.NodeID{'n', 'o', 'd', 'e'}

// Fixture is the engine as production composes it: one device's real journal
// on a temporary directory, reached through the journal's crash-simulating
// seam; the real metadata store on an in-memory kv.KV holding one share; the
// real open-state table over the same store, serving the share's shard; and
// one test clock read by all of them. Everything runs under the journal's
// profiler, which fails the test on a leaked goroutine, descriptor or heap.
//
// Every failure of the fixture's own is reported with the prefix "fixture:",
// so a test failing in it is never mistaken for one failing its own check.
type Fixture struct {
	E     *engine.Engine   // the engine running now; Restart and Crash replace it
	FS    *kvstoretest.FS  // the store, its share and the share's root
	Opens *openstate.Table // the open-state table running now, replaced with E
	Seam  *kit.Seam
	Clock *sim.TestClock
	Share engine.Share // the share as the engine binds it
	// Second is a second share of the store, on the same journal, which no
	// Start binds: a row binds it through Bind. SecondRoot is its root.
	Second     engine.Share
	SecondRoot entity.Handle
	// Journal is the ID Init gave the fixture's journal, which the store
	// records for Node and Dir, so its Lifecycle view answers it there.
	Journal journal.JournalID
	Node    entity.NodeID // the node the engine runs as, its Deps.Node
	Dir     string        // the journal's directory, the engine's one device

	t      *testing.T
	logger *slog.Logger
	faults *kvtest.FaultKV // the store's kv.KV, failing or stalling its calls as a row asks
	starts uint64          // the processes started, each serving the shard under a new grace instance
	// crashed fences the running engine's existence and content views: once
	// set, by Crash, that engine's closing changes nothing in the store.
	crashed *atomic.Bool
	// stalled holds every existence commit until its context ends, as a
	// store that commits nothing does; Stall sets it.
	stalled atomic.Bool
	handles sync.Map // journal.ShareTag → the handle the running engine opened on it

	kv      *failingKV                // the store's kv.KV, failing Updates when a test asks
	batches atomic.Pointer[batchHold] // holds every removal batch at its span while set
	ended   chan struct{}             // one value per removal batch span that ended
}

// batchHold holds the removal batches of the engine running now at their span
// until released, or until that engine stops.
type batchHold struct {
	release chan struct{}
	once    sync.Once
}

// Stall makes the store commit no existence from now on: each commit waits
// until its context ends.
func (f *Fixture) Stall() { f.stalled.Store(true) }

// FailNextSync makes the journal's next fsync fail, so the sync window it
// belongs to fails.
func (f *Fixture) FailNextSync() {
	f.t.Helper()
	if err := f.Seam.Schedule(kit.Failing(kit.OpSync, "*", 1, errors.New("fixture: an injected sync failure"))); err != nil {
		f.t.Fatalf("fixture: schedule a sync failure: %v", err)
	}
}

// HoldBatches holds every removal batch the engine running now, and the
// engines started after it, starts at its span, until ReleaseBatches. Call it
// before the Crash or Restart whose start posts the removals.
func (f *Fixture) HoldBatches() {
	f.batches.Store(&batchHold{release: make(chan struct{})})
}

// ReleaseBatches lets the held removal batches run, and every later one.
func (f *Fixture) ReleaseBatches() {
	if h := f.batches.Load(); h != nil {
		h.once.Do(func() {
			close(h.release)
		})
	}
}

// WaitBatches returns once n removal batch spans have ended since the last
// call.
func (f *Fixture) WaitBatches(n int) {
	f.t.Helper()

	for range n {
		select {
		case <-f.ended:
		case <-f.t.Context().Done():
			f.t.Fatal("fixture: the removal loop ran fewer batches than the test waited for")
		}
	}
}

// KeysOf is every key in the store, in hex, that holds file id's ID, whatever
// it is under: a released file leaves its removal, the removal's index entry,
// its fences and its version-floor entry until its prune deletes them.
func (f *Fixture) KeysOf(id entity.FileID) []string {
	f.t.Helper()

	var out []string

	err := f.kv.View(f.t.Context(), func(r kv.Reader) error {
		for row, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}

			if bytes.Contains(row.Key, id[:]) {
				out = append(out, fmt.Sprintf("%x", row.Key))
			}
		}

		return nil
	})
	if err != nil {
		f.t.Fatalf("fixture: scan the store: %v", err)
	}

	return out
}

// FailUpdatesAfter lets the store's next n Updates through and fails every
// one after them, as a store that stopped between two batches of a removal
// does. The next Crash clears it.
func (f *Fixture) FailUpdatesAfter(n int) {
	f.t.Helper()

	f.kv.left.Store(int64(n))
}

// trace is the engine's Tracer: it holds a removal batch while a hold is set
// and counts the spans that end.
func (f *Fixture) trace(
	ctx context.Context,
	name string,
	_ ...slog.Attr,
) (context.Context, func(error)) {
	if name != "engine.removal_batch" {
		return ctx, func(error) {}
	}

	if h := f.batches.Load(); h != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}

	return ctx, func(error) {
		select {
		case f.ended <- struct{}{}:
		default:
		}
	}
}

// New creates the journal and the store, then starts a process over them: the
// open-state table and the engine, the engine started with the share bound.
// Everything is closed when the test ends.
func New(t *testing.T) *Fixture {
	t.Helper()

	return newFixture(t, nil)
}

// NewWithSmallBatches is New over a store whose transactions hold at most
// entries keys, so a removal of several holes takes several batches.
func NewWithSmallBatches(t *testing.T, entries int) *Fixture {
	t.Helper()

	return newFixture(t, func(db kv.KV) kv.KV {
		return limitedKV{
			KV:      db,
			entries: entries,
		}
	})
}

// failingKV is a kv.KV that lets left Updates through and then fails each
// one; a negative left fails none.
type failingKV struct {
	kv.KV
	left atomic.Int64
}

func (k *failingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if k.left.Load() >= 0 && k.left.Add(-1) < 0 {
		k.left.Store(0)

		return errors.New("fixture: an injected store failure")
	}

	return k.KV.Update(ctx, fn)
}

// limitedKV is a kv.KV whose transactions hold at most entries keys.
type limitedKV struct {
	kv.KV
	entries int
}

func (l limitedKV) Limits() kv.TxnLimits {
	lim := l.KV.Limits()
	lim.TxnEntries = l.entries

	return lim
}

func newFixture(t *testing.T, wrap func(kv.KV) kv.KV) *Fixture {
	t.Helper()

	// Registered first, so its checks run after every other cleanup has
	// closed what it opened.
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("fixture: seam: %v", err)
	}
	clock := sim.NewTestClock(fixtureStart)
	f := &Fixture{
		Seam:   seam,
		Clock:  clock,
		Node:   fixtureNode,
		Dir:    dir,
		t:      t,
		logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
		ended:  make(chan struct{}, 64),
	}

	if f.Journal, err = journal.Init(t.Context(), dir, fixtureInstall, journal.WithClock(clock), journal.WithStorage(seam.FS())); err != nil {
		t.Fatalf("fixture: journal init: %v", err)
	}

	f.FS = kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db := memKV(tb)

		if wrap != nil {
			db = wrap(db)
		}

		f.faults = kvtest.NewFaultKV(db)
		f.kv = &failingKV{KV: f.faults}
		f.kv.left.Store(-1)

		return f.kv
	})

	err = f.FS.Store.RecordJournal(
		t.Context(),
		f.Journal,
		f.Node,
		entity.JournalDir(f.Dir),
	)
	if err != nil {
		t.Fatalf("fixture: record the journal: %v", err)
	}

	f.Share = engine.Share{
		ID:    f.FS.Share.ID,
		Tag:   journal.ShareTag(f.FS.Share.Tag),
		Shard: 0, // every share has the one shard, zero
		State: engine.Serving,
	}

	f.addSecond()

	t.Cleanup(func() { f.stop(true) })
	f.start()
	return f
}

// Restart stops the process cleanly and starts a new one over the same
// journal directory and store.
func (f *Fixture) Restart() {
	f.t.Helper()
	f.stop(true)
	f.start()
}

// RestartAfter stops the process cleanly, runs change over the stopped
// journal directory and store, and starts a new process over them.
func (f *Fixture) RestartAfter(change func()) {
	f.t.Helper()
	f.stop(true)
	change()
	f.start()
}

// Reinit replaces the journal in the fixture's directory with a new, empty
// one, as an operator re-initialising a lost device would, and returns its
// identity. It records nothing in the store. Call it only while the process
// is stopped, inside RestartAfter.
func (f *Fixture) Reinit() journal.JournalID {
	f.t.Helper()

	if err := os.RemoveAll(f.Dir); err != nil {
		f.t.Fatalf("fixture: remove the journal: %v", err)
	}

	if err := os.Mkdir(f.Dir, 0o700); err != nil {
		f.t.Fatalf("fixture: make the journal directory: %v", err)
	}

	id, err := journal.Init(
		f.t.Context(),
		f.Dir,
		fixtureInstall,
		journal.WithClock(f.Clock),
		journal.WithStorage(f.Seam.FS()),
	)
	if err != nil {
		f.t.Fatalf("fixture: journal init: %v", err)
	}

	return id
}

// Crash simulates a power loss under the journal and starts a new process on
// what it left; the store keeps what it committed. The engine from before the
// crash is closed through a view of the seam the crash ended, so nothing it
// does while closing reaches the directory. opts are the seam's crash
// variants.
func (f *Fixture) Crash(opts ...kit.CrashOption) {
	f.t.Helper()
	f.crashed.Store(true) // before the crash, so nothing the old engine runs meanwhile reaches the store
	f.kv.left.Store(-1)
	if err := f.Seam.Crash(opts...); err != nil {
		f.t.Fatalf("fixture: crash: %v", err)
	}
	f.stop(false)
	f.start()
}

// File creates a regular file in the share's root and returns its handle and
// ID.
func (f *Fixture) File(name string) (entity.Handle, entity.FileID) {
	f.t.Helper()
	h, file := f.FS.Create(f.t, f.FS.Admin, f.FS.Root, name, kvstore.Regular(0o644))
	return h, file.ID
}

// Handle is the handle the running engine opened on the share's tag, for a
// row that reads back what the journal holds.
func (f *Fixture) Handle() engine.Handle {
	f.t.Helper()

	return f.HandleOf(f.Share.Tag)
}

// addSecond creates the second share, with the fixture's administrator
// granted on it.
func (f *Fixture) addSecond() {
	ctx := f.t.Context()
	f.Second = AddShare(f.t, f.FS.Store, "second", 0)
	grant := entity.ShareGrant{
		Share:     f.Second.ID,
		Principal: f.FS.Admin.Principal,
		Access:    entity.AccessAdmin,
	}

	if err := f.FS.Store.ControlPlane().SetGrant(ctx, grant); err != nil {
		f.t.Fatalf("fixture: grant on the second share: %v", err)
	}

	root, _, err := f.FS.Store.Files().Root(ctx, f.FS.Admin, f.Second.ID)
	if err != nil {
		f.t.Fatalf("fixture: the second share's root: %v", err)
	}

	f.SecondRoot = root
}

// HandleOf is the handle the running engine opened on tag.
func (f *Fixture) HandleOf(tag journal.ShareTag) engine.Handle {
	f.t.Helper()

	h, ok := f.handles.Load(tag)
	if !ok {
		f.t.Fatalf("fixture: the engine opened no handle on tag %d", tag)
	}

	return h.(engine.Handle)
}

// Committed is file h's record as the store has committed it.
func (f *Fixture) Committed(h entity.Handle) entity.File {
	f.t.Helper()
	file, err := f.FS.Store.Files().Get(context.Background(), f.FS.Admin, h)
	if err != nil {
		f.t.Fatalf("fixture: read a file's record: %v", err)
	}
	return file
}

// start builds the open-state table and the engine over the fixture's
// journal directory and store, and starts both.
func (f *Fixture) start() {
	t := f.t
	t.Helper()
	ctx := t.Context()

	f.crashed = new(atomic.Bool)
	var err error
	f.Opens, err = openstate.New(openstate.Deps{Records: f.FS.Store, Clock: f.Clock, Rand: rand.Reader, Logger: f.logger})
	if err != nil {
		t.Fatalf("fixture: open state: %v", err)
	}
	if err := f.Opens.Start(ctx, quietSink{}, noOrder); err != nil {
		t.Fatalf("fixture: open state start: %v", err)
	}
	f.starts++
	if err := f.Opens.Recovery().ServeShard(ctx, f.Share.ID, f.Share.Shard, f.starts); err != nil {
		t.Fatalf("fixture: serve shard: %v", err)
	}

	e, err := engine.New(engine.Deps{
		Node:    f.Node,
		Devices: []string{f.Dir},
		Open: func(ctx context.Context, dir string, floor journal.Version, expect journal.JournalID) (engine.Journal, error) {
			j, err := journal.Open(ctx, dir, floor, expect, journal.WithClock(f.Clock), journal.WithStorage(f.Seam.FS()))
			if err != nil {
				return nil, err
			}
			return wrapJournal(j, func(tag journal.ShareTag, h engine.Handle) { f.handles.Store(tag, h) }), nil
		},
		Existence: fencedExistence{Existence: f.FS.Store.Existence(), crashed: f.crashed, stalled: &f.stalled},
		Content:   fencedContent{Content: f.FS.Store.Content(), crashed: f.crashed},
		Lifecycle: f.FS.Store.Lifecycle(),
		Holds:     Holds(f.Opens, f.FS.Store.Opens()),
		Clock:     f.Clock,
		Logger:    f.logger,
		Tracer:    f.trace,
	})
	if err != nil {
		t.Fatalf("fixture: engine: %v", err)
	}
	if err := e.Apply(Settings); err != nil {
		t.Fatalf("fixture: engine settings: %v", err)
	}
	if err := e.Start(ctx, []engine.Share{f.Share}); err != nil {
		t.Fatalf("fixture: engine start: %v", err)
	}
	f.E = e
}

// stop closes the engine running now, if one started, and stops the
// open-state table. clean is false after a crash, when the engine's close is
// expected to fail and is only logged.
func (f *Fixture) stop(clean bool) {
	f.t.Helper()
	if f.E != nil {
		if err := f.E.Close(context.Background()); err != nil {
			if clean {
				f.t.Errorf("fixture: engine close: %v", err)
			} else {
				f.t.Logf("fixture: closing the crashed engine: %v", err)
			}
		}
		f.E = nil
	}
	if f.Opens != nil {
		f.Opens.Stop()
		f.Opens = nil
	}
}

// memKV is a fresh in-memory kv.KV, closed when the test ends.
func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatalf("fixture: store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

// quietSink is open state's Sink with no client and no service behind it:
// the fixture serves no protocol, so nothing it raises has anyone to reach.
type quietSink struct{}

func (quietSink) ClientEnded(entity.ClientID, []openstate.ShardID, openstate.RevokeReason) {}
func (quietSink) Closed(entity.FileID, openstate.CloseResult)                              {}
func (quietSink) LockWaitOver(openstate.LockWaiter, bool)                                  {}
func (quietSink) RecallGrant(openstate.CachingGrant)                                       {}
func (quietSink) RecallAny(entity.ClientID, int)                                           {}
func (quietSink) Revoked(openstate.Revocation)                                             {}
func (quietSink) Notify(entity.ClientID, openstate.WatchID, []openstate.Change)            {}
func (quietSink) GraceEnded(entity.ShareID, openstate.ShardID)                             {}

// Sink is open state's Sink with no client and no service behind it,
// recording each Closed: a file's last open closed, an orphan among them.
type Sink struct {
	quietSink
	mu     sync.Mutex
	closed []Closed
}

// Closed is one Sink.Closed call.
type Closed struct {
	File   entity.FileID
	Result openstate.CloseResult
}

func (s *Sink) Closed(f entity.FileID, r openstate.CloseResult) {
	s.mu.Lock()
	s.closed = append(s.closed, Closed{f, r})
	s.mu.Unlock()
}

// ClosedCalls is every Closed so far, oldest first.
func (s *Sink) ClosedCalls() []Closed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.closed)
}

// noOrder takes no admission lock: the fixture has no service holding one.
func noOrder(entity.FileID) func() { return func() {} }

// Holds adapts t, over the store's durable opens, to the engine's
// CloneHold, as the composition root does. An orphan CloneClose leaves
// reaches t's Sink.Closed.
func Holds(t *openstate.Table, records OpenRecords) engine.CloneHold {
	return cloneHold{opens: t.Opens(), recovery: t.Recovery(), records: records}
}

// OpenRecords reads a file's durable opens: the store's Opens.
type OpenRecords interface {
	Of(ctx context.Context, share entity.ShareID, file entity.FileID) ([]entity.OpenRecord, error)
}

// cloneHold is the open-state table as the engine's CloneHold.
type cloneHold struct {
	opens    openstate.Opens
	recovery openstate.Recovery
	records  OpenRecords
}

// Hold answers ErrStale only for a source holding no open of the clone's.
// CloneOpen answers ErrBadState for that and for a share whose open state
// was dropped, so the record is read again to tell them apart; the dropped
// share's answer crosses as itself, which the engine retries.
func (h cloneHold) Hold(ctx context.Context, src engine.CloneSrc) (entity.OpenID, error) {
	id, err := h.opens.CloneOpen(ctx, openstate.FileKey{Share: src.Share, Shard: src.Shard, ID: src.File}, openstate.CloneSource{Dst: src.Dst, Version: src.AsOf})
	if !errors.Is(err, openstate.ErrBadState) {
		return id, holdErr(err)
	}
	recs, rerr := h.records.Of(ctx, src.Share, src.File)
	if rerr != nil {
		return 0, rerr
	}
	if !slices.ContainsFunc(recs, func(r entity.OpenRecord) bool {
		return r.Kind == entity.OpenCloneSource && r.CloneDst == src.Dst && r.CloneVersion == src.AsOf
	}) {
		return 0, entity.ErrStale
	}
	return 0, err
}

func (h cloneHold) Release(ctx context.Context, o entity.OpenID) error {
	err := h.opens.CloneClose(ctx, o)
	if errors.Is(err, openstate.ErrBadState) {
		return nil // the hold is already gone
	}
	return holdErr(err)
}

// holdErr is open state's answer to a clone's hold in the engine's types: a
// shard not yet served or a table not running holds the clone for a retry.
func holdErr(err error) error {
	var delay *openstate.DelayError
	if errors.As(err, &delay) && delay.Cause == openstate.DelayFrozen || errors.Is(err, openstate.ErrStopped) {
		return &vfserr.Delay{Cause: vfserr.DelayFrozen}
	}
	return err
}

func (h cloneHold) Served(ctx context.Context, s entity.ShardID) error {
	return holdErr(h.recovery.Served(ctx, s))
}

// errCrashed answers a store change the crashed process's engine makes.
var errCrashed = errors.New("fixture: the process crashed: its engine commits nothing")

// fencedExistence is the store's existence view, refusing every change once
// crashed is set: a crashed process's engine closing must not commit what
// only recovery may, or a row after the crash would pass without recovery.
type fencedExistence struct {
	engine.Existence
	crashed *atomic.Bool
	stalled *atomic.Bool
}

func (x fencedExistence) Commit(ctx context.Context, pending []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.crashed.Load() {
		return nil, errCrashed
	}
	if x.stalled.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return x.Existence.Commit(ctx, pending, marks)
}

func (x fencedExistence) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	if x.crashed.Load() {
		return errCrashed
	}
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

func (x fencedExistence) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	if x.crashed.Load() {
		return errCrashed
	}
	return x.Existence.Clone(ctx, op, pending)
}

// fencedContent is the store's content view, refusing its changes once
// crashed is set, as fencedExistence does.
type fencedContent struct {
	engine.Content
	crashed *atomic.Bool
}

func (c fencedContent) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) (bool, error) {
	if c.crashed.Load() {
		return false, errCrashed
	}
	return c.Content.Resume(ctx, share, file)
}

func (c fencedContent) PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error {
	if c.crashed.Load() {
		return errCrashed
	}
	return c.Content.PruneRemovals(ctx, share, file, atOrBelow)
}

// errFaulted is what a store call FailWrites fails answers.
var errFaulted = errors.New("fixture: an injected store failure")

// FailWrites makes every write transaction of the store fail at once from
// now on, its reads served. The channel closes once one has been tried.
func (f *Fixture) FailWrites() <-chan struct{} {
	return f.fault(sim.Rule{Op: kvtest.OpUpdate, Key: "*", Action: sim.FaultAction{Err: errFaulted}})
}

// HoldNextWrite holds the store's next write transaction for d on the clock,
// then fails it with err, or runs it when err is nil. The channel closes once
// it reached the store.
func (f *Fixture) HoldNextWrite(d time.Duration, err error) <-chan struct{} {
	return f.fault(sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Delay: d, Err: err}})
}

// FailFile makes every existence commit of file id fail for that file alone,
// as an absent record, the other files of the same commit committing. The
// channel closes once a write transaction reached the store.
func (f *Fixture) FailFile(id entity.FileID) <-chan struct{} {
	key := keys.Describe(keys.ID(keys.ID([]byte{keys.KindF}, f.Share.ID[:]), id[:]))
	return f.fault(sim.Rule{Op: kvtest.OpGet, Key: key, Action: sim.FaultAction{Err: kv.ErrNotFound}})
}

// Heal lifts every fault: the store serves every call from now on.
func (f *Fixture) Heal() { f.faults.SetFault(nil) }

// fault replaces the store's faults with rules, delays on the fixture's
// clock, and answers a channel closed once the store first runs a write
// transaction under them.
func (f *Fixture) fault(rules ...sim.Rule) <-chan struct{} {
	f.t.Helper()
	s, err := sim.NewSchedule(1, f.Clock, rules...)
	if err != nil {
		f.t.Fatalf("fixture: fault schedule: %v", err)
	}
	e := &entered{Schedule: s, ch: make(chan struct{})}
	f.faults.SetFault(e)
	return e.ch
}

// entered is a schedule that closes its channel when the store first runs a
// write transaction under it.
type entered struct {
	*sim.Schedule
	once sync.Once
	ch   chan struct{}
}

func (e *entered) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpUpdate {
		e.once.Do(func() { close(e.ch) })
	}
	return e.Schedule.At(op, key)
}
