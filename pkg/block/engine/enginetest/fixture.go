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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
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

// fixtureSettings keeps each open's spare pool at 4 segments (2 content, 1
// headroom, 1 reserve) instead of the defaults' 18 of 256 MiB each.
var fixtureSettings = journal.Settings{Streams: 2, SpareTarget: 2}

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

	// refuse makes the engine's Open answer as a journal does for a
	// directory the platform profile refuses.
	refuse atomic.Bool

	// fake, once set by WithFakeJournal, is the journal every process start
	// opens in place of the one in Dir.
	fake *FakeJournal

	// cloneGate, once set by HoldNextClone, holds the next clone's phase 1.
	cloneGate atomic.Pointer[gate]
	// startGate, once set by HoldNextStart, holds the next engine start.
	startGate atomic.Pointer[gate]
	handles   sync.Map // journal.ShareTag → the handle the running engine opened on it

	kv      *failingKV                // the store's kv.KV, failing Updates when a test asks
	batches atomic.Pointer[batchHold] // holds every removal batch at its span while set
	ended   chan struct{}             // one value per removal batch span that ended
	spansMu sync.Mutex
	spans   []*Span // every span ended, in the order they ended

	compose func(*Fixture, *engine.Engine) Service // builds the service each process start composes; nil for none
	svc     Service                                // the service running now, built by compose
	serving bool                                   // svc was started and is not yet stopped
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

// HoldNextClone holds the next clone's phase 1 at the store, with both of
// its files registered, until release is called; entered is closed once it
// arrives. A test's cleanup releases it.
func (f *Fixture) HoldNextClone() (entered <-chan struct{}, release func()) {
	return f.holdNext(&f.cloneGate)
}

// HoldNextStart holds the next raise of the shards' incarnations at the
// store: the next engine start's, inside its start steps after its journal
// is open, or a first attach's if one comes first. It holds until release is
// called; entered is closed once it arrives. A test's cleanup releases it.
func (f *Fixture) HoldNextStart() (entered <-chan struct{}, release func()) {
	return f.holdNext(&f.startGate)
}

// holdNext sets a gate at p for the next store call that takes it.
func (f *Fixture) holdNext(p *atomic.Pointer[gate]) (entered <-chan struct{}, release func()) {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release = func() { once.Do(func() { close(g.release) }) }
	f.t.Cleanup(release)
	p.Store(g)
	return g.entered, release
}

// gate is a held store call: entered is closed when it arrives, and it waits
// for release.
type gate struct{ entered, release chan struct{} }

// pass takes the gate set at p, if one is, and waits for its release or for
// ctx to end.
func pass(ctx context.Context, p *atomic.Pointer[gate]) error {
	g := p.Swap(nil)
	if g == nil {
		return nil
	}
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FailNextSync makes the journal's next fsync of a segment fail, so the sync
// window it belongs to fails. A spare's sync as it is made is not one.
func (f *Fixture) FailNextSync() {
	f.t.Helper()
	f.journalOnly("FailNextSync")
	if err := f.Seam.Schedule(kit.Failing(kit.OpSync, "*.seg", 1, errors.New("fixture: an injected sync failure"))); err != nil {
		f.t.Fatalf("fixture: schedule a sync failure: %v", err)
	}
}

// FailNextWindow makes the journal's next fsync of a segment fail and every
// segment in the journal's directory serve its durable image to a read, so
// the window's read-back does not verify and the window fails with loss
// records instead of being re-appended. The stale reads last for the
// fixture's life; segments created after the call read as written. Over the
// fake journal it is the fake's FailNextWindow.
func (f *Fixture) FailNextWindow() {
	f.t.Helper()
	if f.fake != nil {
		f.fake.FailNextWindow()
		return
	}
	f.FailNextSync()
	names, err := f.Seam.List()
	if err != nil {
		f.t.Fatalf("fixture: list the journal's directory: %v", err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".seg") {
			f.Seam.StaleReads(name)
		}
	}
}

// HoldFirstRepack holds the first repack pass mid-way, its segments selected
// and none yet returned, until release; reached is closed once it is there.
// Over the journal it holds the pass where it removes a segment it retired;
// over the fake journal, after it selected. A test's cleanup releases it.
func (f *Fixture) HoldFirstRepack() (reached <-chan struct{}, release func()) {
	f.t.Helper()
	if f.fake == nil {
		h := f.Seam.HoldFirst(f.t, "remove", "*")
		return h.Reached(), h.Release
	}
	in, out := make(chan struct{}), make(chan struct{})
	var first, rel sync.Once
	f.fake.AtRepackSelected(func(ctx context.Context) {
		held := false
		first.Do(func() { held = true; close(in) })
		if !held {
			return
		}
		select {
		case <-out:
		case <-ctx.Done():
		}
	})
	release = func() { rel.Do(func() { close(out) }) }
	f.t.Cleanup(release)
	return in, release
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

// FailUpdatesSetting fails every Update whose transaction sets a key match
// accepts, as a store refusing one file's records does, and lets every
// other through. A nil match, or the next Crash, clears it.
func (f *Fixture) FailUpdatesSetting(match func(key []byte) bool) {
	if match == nil {
		f.kv.setting.Store(nil)
		return
	}
	f.kv.setting.Store(&match)
}

// FailUpdatesAfter lets the store's next n Updates through and fails every
// one after them, as a store that stopped between two batches of a removal
// does. The next Crash clears it.
func (f *Fixture) FailUpdatesAfter(n int) {
	f.t.Helper()

	f.kv.left.Store(int64(n))
}

// Span is a span the fixture's Trace was handed: its name, its attributes and
// the span the context it was opened in carried, nil for a root.
type Span struct {
	Name   string
	Attrs  []slog.Attr
	Parent *Span
}

// spanKey is the context key under which Trace carries the span it opened.
type spanKey struct{}

// Trace is the engine's Tracer, and opens a test's own spans: it opens a span
// named name as a child of the one ctx carries and keeps it once ended, for
// Spans. It also holds a removal batch while a hold is set and counts the
// removal batch spans that end.
func (f *Fixture) Trace(
	ctx context.Context,
	name string,
	attrs ...slog.Attr,
) (context.Context, func(error)) {
	parent, _ := ctx.Value(spanKey{}).(*Span)
	sp := &Span{Name: name, Attrs: attrs, Parent: parent}
	batch := name == "engine.removal_batch"
	if h := f.batches.Load(); batch && h != nil {
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}

	return context.WithValue(ctx, spanKey{}, sp), func(error) {
		f.spansMu.Lock()
		f.spans = append(f.spans, sp)
		f.spansMu.Unlock()
		if !batch {
			return
		}
		select {
		case f.ended <- struct{}{}:
		default:
		}
	}
}

// Spans is every span ended so far, in the order they ended, across restarts.
func (f *Fixture) Spans() []*Span {
	f.spansMu.Lock()
	defer f.spansMu.Unlock()

	return slices.Clone(f.spans)
}

// New creates the journal and the store, then starts a process over them: the
// open-state table and the engine, the engine started with the share bound.
// Everything is closed when the test ends.
func New(t *testing.T, opts ...Option) *Fixture {
	t.Helper()

	return newFixture(t, nil, opts...)
}

// Option changes what New composes.
type Option func(*Fixture)

// WithFakeJournal has every process start open one in-memory journal, kept
// across Restart and Crash, in place of the journal in Dir: for a check of the
// composition's own order of work that the journal's unbuilt parts would stop
// first. The fake runs its segment model, segments of fakeSegmentCap bytes
// under a maximum of fakeMaxBytes, so repack has segments to free. It is held
// to the journal for Share and Since alone (TestShareAgreesWithTheJournal,
// TestSinceYieldsOverAsTheJournalDoes), so nothing else it answers here is
// checked against the journal. FailNextWindow and HoldFirstRepack act on the
// fake; the other journal-side controls (FailNextSync, Reinit, Crash's
// variants, HandleOf) act on the journal in Dir, which nothing reads then, so
// each fails the test.
//
// decision: a check run this way proves nothing about the journal, only what
// the engine and the service above it do in what order; the fake stands in
// where the logic under test is theirs. Its Crash is a Reopen, since the fake
// holds every operation durable at once. Withdraw it, and run those checks
// over the journal alone, once a restart reopens a journal that holds
// segments.
func WithFakeJournal() Option {
	return func(f *Fixture) {
		f.fake = NewFakeJournal()
		f.fake.SegmentCap(fakeSegmentCap, fakeMaxBytes)
	}
}

// The fake journal's segment model under WithFakeJournal: 1 MiB segments
// under a maximum well above the share's limit. The rows need the model on,
// so repack has segments to free, not these sizes: a capacity pass seals the
// active segment.
const (
	fakeSegmentCap = 1 << 20
	fakeMaxBytes   = 64 << 20
)

// journalOnly fails a control that acts on the journal in Dir, which
// WithFakeJournal leaves unread.
func (f *Fixture) journalOnly(what string) {
	f.t.Helper()
	if f.fake != nil {
		f.t.Fatalf("fixture: %s acts on the journal in Dir, which WithFakeJournal leaves unread", what)
	}
}

// Service is what a compose hook builds over the fixture's open-state table
// and engine: the table's Sink, with Admission its Order, started after the
// engine and drained and stopped before it.
type Service interface {
	openstate.Sink
	Admission(f entity.FileID) (unlock func())
	Start(ctx context.Context) error
	Drain(ctx context.Context) error
	Stop() error
}

// WithCompose has every process start, New's, Restart's and Crash's, build a
// service with compose over the open-state table it built (f.Opens) and
// engine e, both not yet started. The fixture then starts the table with the
// service as its Sink and Order, then the engine, then the service, which
// serves the shards itself; a stop drains and stops the service, then closes
// the engine, then stops the table. Without it the table's Sink hears no one.
//
// decision: the hook builds a service and nothing around it, no node and no
// protocol adapter, since the tier it composes runs the service over the
// engine alone. Withdraw it once the node's own constructor composes these
// pieces and the tier runs over that instead.
func WithCompose(compose func(f *Fixture, e *engine.Engine) Service) Option {
	return func(f *Fixture) { f.compose = compose }
}

// Logger is the logger the fixture hands the engine and open state.
func (f *Fixture) Logger() *slog.Logger { return f.logger }

// StopService drains and stops the composed service, leaving the open-state
// table and the engine running until the fixture's own stop.
func (f *Fixture) StopService() {
	f.t.Helper()
	f.stopService(true)
}

// stopService drains and stops the composed service, if one was built.
// clean is false after a crash, when a failure is only logged.
func (f *Fixture) stopService(clean bool) {
	f.t.Helper()
	if f.svc == nil {
		return
	}
	report := f.t.Errorf
	if !clean {
		report = f.t.Logf
	}
	if f.serving {
		if err := f.svc.Drain(context.Background()); err != nil {
			report("fixture: service drain: %v", err)
		}
	}
	if err := f.svc.Stop(); err != nil {
		report("fixture: service stop: %v", err)
	}
	f.svc, f.serving = nil, false
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
// one; a negative left fails none. While setting holds a match, it also
// fails each Update whose transaction sets a key the match accepts.
type failingKV struct {
	kv.KV
	left    atomic.Int64
	setting atomic.Pointer[func(key []byte) bool]
}

func (k *failingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if k.left.Load() >= 0 && k.left.Add(-1) < 0 {
		k.left.Store(0)

		return errors.New("fixture: an injected store failure")
	}

	match := k.setting.Load()
	if match == nil {
		return k.KV.Update(ctx, fn)
	}

	return k.KV.Update(ctx, func(txn kv.Txn) error {
		w := &settingTxn{Txn: txn, match: *match}
		if err := fn(w); err != nil {
			return err
		}
		if w.hit {
			return errors.New("fixture: an injected store failure of a matched key")
		}

		return nil
	})
}

// settingTxn is a transaction that notes whether it set a key match accepts.
type settingTxn struct {
	kv.Txn
	match func(key []byte) bool
	hit   bool
}

func (t *settingTxn) Set(key, value []byte) error {
	if t.match(key) {
		t.hit = true
	}

	return t.Txn.Set(key, value)
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

func newFixture(t *testing.T, wrap func(kv.KV) kv.KV, opts ...Option) *Fixture {
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
	for _, o := range opts {
		o(f)
	}

	if f.Journal, err = journal.Init(t.Context(), dir, fixtureInstall,
		journal.WithClock(clock), journaltest.SuitableMount(), journal.WithStorage(seam.FS())); err != nil {
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

	// The share's tag and shard are the ones the store minted when it put
	// the share; the share value the FS kept from before the put has neither.
	f.Share = ShareOf(t, f.FS.Store, f.FS.Share.ID, 0)

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

// RefuseOpens makes every Open of the engine, those of a process started
// later included, answer the refusal a journal gives for a directory the
// platform profile refuses, naming the rule and the directory (RFC 1 §3), or
// stops doing so. The journal's own check is not run.
func (f *Fixture) RefuseOpens(on bool) {
	f.refuse.Store(on)
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
	f.journalOnly("Reinit")

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
		journaltest.SuitableMount(),
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
	if len(opts) > 0 {
		f.journalOnly("a crash variant")
	}
	f.crashed.Store(true) // before the crash, so nothing the old engine runs meanwhile reaches the store
	f.kv.left.Store(-1)
	f.kv.setting.Store(nil)
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
// journal directory and store, and the composed service over both when one
// is asked for, then starts them in that order.
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

	e, err := engine.New(engine.Deps{
		Node:    f.Node,
		Devices: []string{f.Dir},
		Open: func(ctx context.Context, dir string, floor journal.Version, expect journal.JournalID) (engine.Journal, error) {
			if f.refuse.Load() {
				return nil, fmt.Errorf(
					"journal: directory %s refused: mount has no barriers: %w",
					dir, journal.ErrUnsuitableStorage,
				)
			}
			if f.fake != nil {
				f.fake.Reopen()
				return f.fake, nil
			}
			j, err := journal.Open(ctx, dir, floor, expect, nil,
				journal.WithClock(f.Clock), journaltest.SuitableMount(), journal.WithStorage(f.Seam.FS()),
				journal.WithSettings(fixtureSettings))
			if err != nil {
				return nil, err
			}
			return wrapJournal(j, func(tag journal.ShareTag, h engine.Handle) { f.handles.Store(tag, h) }), nil
		},
		Existence: fencedExistence{Existence: f.FS.Store.Existence(), crashed: f.crashed, stalled: &f.stalled, cloneGate: &f.cloneGate},
		Content:   fencedContent{Content: f.FS.Store.Content(), crashed: f.crashed},
		Lifecycle: gatedLifecycle{Lifecycle: f.FS.Store.Lifecycle(), startGate: &f.startGate},
		Holds:     Holds(f.Opens, f.FS.Store.Opens()),
		Clock:     f.Clock,
		Logger:    f.logger,
		Tracer:    f.Trace,
	})
	if err != nil {
		t.Fatalf("fixture: engine: %v", err)
	}

	var sink openstate.Sink = quietSink{}
	order := openstate.Order(noOrder)
	if f.compose != nil {
		f.svc = f.compose(f, e)
		sink, order = f.svc, f.svc.Admission
	}
	if err := f.Opens.Start(ctx, sink, order); err != nil {
		t.Fatalf("fixture: open state start: %v", err)
	}
	if f.svc == nil {
		f.starts++
		if err := f.Opens.Recovery().ServeShard(ctx, f.Share.ID, f.Share.Shard, f.starts); err != nil {
			t.Fatalf("fixture: serve shard: %v", err)
		}
	}

	if err := e.Apply(Settings); err != nil {
		t.Fatalf("fixture: engine settings: %v", err)
	}
	if err := e.Start(ctx, []engine.Share{f.Share}); err != nil {
		t.Fatalf("fixture: engine start: %v", err)
	}
	f.E = e

	if f.svc != nil {
		if err := f.svc.Start(ctx); err != nil {
			t.Fatalf("fixture: service start: %v", err)
		}
		f.serving = true
	}
}

// stop stops the composed service, if one was built, closes the engine
// running now, if one started, and stops the open-state table. clean is false
// after a crash, when the engine's close is expected to fail and is only
// logged.
func (f *Fixture) stop(clean bool) {
	f.t.Helper()
	f.stopService(clean)
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

// fencedExistence is the store's existence view, keeping every change from
// the store once crashed is set: a crashed process's engine closing must not
// commit what only recovery may, or a row after the crash would pass without
// recovery.
type fencedExistence struct {
	engine.Existence
	crashed   *atomic.Bool
	stalled   *atomic.Bool
	cloneGate *atomic.Pointer[gate]
}

// Commit answers a crashed process's existence commit as done and writes
// nothing: the store never sees it, as with a refusal, but the old engine's
// close, whose last group commit waits for every pending file, ends rather
// than retrying until a stop bound that runs on the fixture's clock.
func (x fencedExistence) Commit(ctx context.Context, pending []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	// decision: a crashed process's existence commit answers done, not
	// refused, so the old engine's close ends; nothing reaches the store.
	// Withdraw it if a row reads an answer the crashed engine gave.
	if x.crashed.Load() {
		return nil, nil
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
	if err := pass(ctx, x.cloneGate); err != nil {
		return err
	}
	return x.Existence.Clone(ctx, op, pending)
}

// gatedLifecycle is the store's lifecycle view, holding the raise of the
// shards' incarnations at start while HoldNextStart's gate is set.
type gatedLifecycle struct {
	engine.Lifecycle
	startGate *atomic.Pointer[gate]
}

func (l gatedLifecycle) RaiseAtStart(ctx context.Context, shards []entity.ShardID) (map[entity.ShardID]uint64, error) {
	if err := pass(ctx, l.startGate); err != nil {
		return nil, err
	}
	return l.Lifecycle.RaiseAtStart(ctx, shards)
}

// fencedContent is the store's content view, refusing its changes once
// crashed is set, as fencedExistence's Remove and Clone do.
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
