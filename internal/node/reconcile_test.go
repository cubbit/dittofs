package node_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Two remote stores a share's namespace can be created on.
var (
	storeA = [16]byte{0: 'a', 15: 1}
	storeB = [16]byte{0: 'b', 15: 1}
)

// cfgGen is the settings generation's key, as a FaultKV rule names it: every
// pass's counters read gets it.
var cfgGen = keys.Describe([]byte{keys.KindCFGGEN})

// createShare creates a share named name in a namespace of its own on
// store, both in one write, and returns it as read back.
func createShare(t *testing.T, h *nodetest.Harness, name string, store [16]byte) entity.Share {
	t.Helper()
	return createShares(t, h, store, name)[0]
}

// createShares creates a share of each name, each in a namespace of its own
// on store, all in one write, and returns them as read back.
func createShares(t testing.TB, h *nodetest.Harness, store [16]byte, names ...string) []entity.Share {
	t.Helper()
	shares := make([]entity.Share, len(names))
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		for i, name := range names {
			ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: store}
			if _, err := tx.PutNamespace(ns, 0); err != nil {
				return err
			}
			shares[i] = entity.Share{ID: entity.NewShareID(), Name: name, Path: "/" + name, Root: entity.NewFileID(),
				Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}}
			if _, err := tx.PutShare(shares[i], 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("create shares %v: %v", names, err)
	}
	for i, sh := range shares {
		if shares[i], _, err = h.Store().ShareIndex().Share(t.Context(), sh.ID); err != nil {
			t.Fatalf("read share %s back: %v", sh.Name, err)
		}
	}
	return shares
}

// configure gives store a setting, which makes it a configured remote store.
func configure(t testing.TB, h *nodetest.Harness, store [16]byte) {
	t.Helper()
	s := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "endpoint", Value: []byte("https://store.invalid")}
	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
		t.Fatalf("configure the remote store: %v", err)
	}
}

// inEngine reports whether the engine's stats name share id's tag as an
// attached share's, which only a binding puts there.
func inEngine(h *nodetest.Harness, id entity.ShareID) bool {
	for _, j := range h.Node().Engine().Stats().Journals {
		for _, sid := range j.TagShares {
			if sid == id {
				return true
			}
		}
	}
	return false
}

// counters reads the counters as another node would.
func counters(t testing.TB, h *nodetest.Harness) kvstore.Counters {
	t.Helper()
	c, err := h.Store().Nodes().Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}
	return c
}

func covers(pos, c kvstore.Counters) bool {
	return pos.Settings >= c.Settings && pos.ShareList >= c.ShareList && pos.Principals >= c.Principals
}

// caughtUp advances the node's clock a second at a time until the node has
// reconciled through the counters as they are now. After each second it
// waits for a pass to end, or a second of real time for a loop waiting on a
// longer backoff, so the clock never runs ahead of a pass's store calls.
func caughtUp(t *testing.T, h *nodetest.Harness) {
	t.Helper()
	c := counters(t, h)
	// The backstop bounds the wait for progress, not the work: a pass
	// binding many shares on a loaded host can take far longer than an idle
	// one, and each move of the positions toward c restarts the backstop. A
	// pass that ends without such a move, as one whose shares fail does,
	// does not, so only a reconcile that stopped catching up fails here.
	last, best := time.Now(), toward(h.Node().Positions(), c)
	for !covers(h.Node().Positions(), c) {
		if time.Since(last) > passStall {
			t.Fatalf("the positions did not move toward the counters in %v of turns: positions %+v, want them to cover %+v", passStall, h.Node().Positions(), c)
		}
		turn(h)
		if got := toward(h.Node().Positions(), c); got > best {
			last, best = time.Now(), got
		}
	}
}

// toward is how far pos has come toward c: the sum of each position, capped
// at c's, that covers counts.
func toward(pos, c kvstore.Counters) uint64 {
	return min(pos.Settings, c.Settings) + min(pos.ShareList, c.ShareList) + min(pos.Principals, c.Principals)
}

// passStall is how long caughtUp waits for the positions to move before it
// fails: a generous backstop, as a pass ends within a second on an idle host.
const passStall = 2 * time.Minute

// turn advances the node's clock a second and waits for a pass to end, or a
// second of real time.
func turn(h *nodetest.Harness) {
	moved := h.Node().Moved()
	h.Clock().Advance(time.Second)
	select {
	case <-moved:
	case <-time.After(time.Second):
	}
}

// ended turns h's node until operation id has ended, and returns it.
func ended(t *testing.T, h *nodetest.Harness, id entity.OperationID) entity.Operation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		op := operation(t, h, id)
		if op.State == entity.OpSucceeded || op.State == entity.OpFailed {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s within 30 s: %v at step %s, want it ended", op.Kind, op.State, op.Step)
		}
		turn(h)
	}
}

func started(t testing.TB, h *nodetest.Harness) {
	t.Helper()
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

// A share written through the store is bound within a turn of reconcile;
// quiesced, its binding is quiesced, and resumed, serving again: one bind
// each, as a pass over records that did not change binds nothing again, a
// settings change included. A grant raises no counter, so it leaves the
// positions where they were.
func TestReconcileBindsQuiescesAndResumes(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	var mu sync.Mutex
	var binds []entity.ShareState
	h.Node().OnBind(func(s entity.Share) {
		mu.Lock()
		defer mu.Unlock()
		binds = append(binds, s.State)
	})
	started(t, h)
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	caughtUp(t, h)
	if !inEngine(h, sh.ID) {
		t.Fatalf("share created after Start: not bound in the engine once reconcile caught up")
	}
	cp := h.Store().ControlPlane()
	for _, s := range []struct{ from, to entity.ShareState }{{entity.ShareEnabled, entity.ShareQuiesced}, {entity.ShareQuiesced, entity.ShareEnabled}} {
		if err := cp.SetShareState(t.Context(), sh.ID, s.from, s.to); err != nil {
			t.Fatalf("share state %v to %v: %v", s.from, s.to, err)
		}
		caughtUp(t, h)
		if got, ok := h.Node().Bound(sh.ID); !ok || got != s.to {
			t.Errorf("binding after the share went %v: got %v (bound %v), want %v", s.to, got, ok, s.to)
		}
	}
	configure(t, h, storeB)
	caughtUp(t, h)
	mu.Lock()
	got := slices.Clone(binds)
	mu.Unlock()
	if want := []entity.ShareState{entity.ShareEnabled, entity.ShareQuiesced, entity.ShareEnabled}; !slices.Equal(got, want) {
		t.Errorf("binds after a settings change over the same shares: got states %v, want %v, one per change", got, want)
	}

	before := h.Node().Positions()
	if _, err := cp.SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: entity.Everyone, Access: entity.AccessRead}, 0); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := counters(t, h); got != before {
		t.Errorf("counters after a grant: got %+v, want the positions reconciled before it, %+v", got, before)
	}
}

// Shares spanning more than one page of the store's listing, 256 rows, are
// all bound. Their binds, not their creation, take most of the test's time.
func TestReconcileBindsEveryPage(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	started(t, h)
	configure(t, h, storeA)
	names := make([]string, 257)
	for i := range names {
		names[i] = fmt.Sprintf("s%d", i)
	}
	shares := createShares(t, h, storeA, names...)
	caughtUp(t, h)
	for _, sh := range shares {
		if !inEngine(h, sh.ID) {
			t.Fatalf("share %s of %d: not bound once reconcile caught up", sh.Name, len(shares))
		}
	}
}

// A share whose namespace's remote store is not configured is not bound at
// Start nor by reconcile, and is refused, naming it, beside a share that is
// bound; once its store is configured it is bound and no longer refused.
func TestReconcileRefusesAShareWithNoRemoteStore(t *testing.T) {
	h, f := faulted(t)
	configure(t, h, storeA)
	a := createShare(t, h, "a", storeA)
	b := createShare(t, h, "nowhere", storeB)
	started(t, h)
	caughtUp(t, h)
	if !inEngine(h, a.ID) {
		t.Errorf("share on a configured store: not bound, want bound")
	}
	if inEngine(h, b.ID) {
		t.Errorf("share on a store with no configuration: bound, want refused")
	}
	refused := h.Node().Refused()
	if len(refused) != 1 || refused[0].Record.Kind != entity.ResourceShare || entity.ShareID(refused[0].Record.ID) != b.ID ||
		refused[0].Generation != b.Position || !strings.Contains(string(refused[0].Error), "share nowhere") {
		t.Errorf("refused records: got %s, want share nowhere at generation %d, its error naming it", describe(refused), b.Position)
	}

	// The refusal is not retried: with the namespaces' scan held, turns
	// that move no counter run no share pass, and the settings change that
	// configures the store does.
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour) // a failed test's Stop finds no scan held
	for range 5 {
		turn(h)
	}
	if n := held.Pending(); n != 0 {
		t.Fatalf("a share pass ran with no counter moved since the refusal (%d scans held), want the refusal to wait for its record", n)
	}
	configure(t, h, storeB)
	heldUntil(t, h, held, "share pass once the store is configured")
	held.Advance(time.Hour)
	caughtUp(t, h)
	if !inEngine(h, b.ID) {
		t.Errorf("share once its store is configured: not bound, want bound")
	}
	if got := h.Node().Refused(); len(got) != 0 {
		t.Errorf("refused records once the store is configured: got %s, want none", describe(got))
	}
}

// describe renders refused records with each error as the text it is.
func describe(refused []entity.RefusedRecord) string {
	var b strings.Builder
	for _, r := range refused {
		fmt.Fprintf(&b, "{%s %x generation %d error %s} ", r.Record.Kind, r.Record.ID, r.Generation, r.Error)
	}
	return b.String()
}

// faulted builds a harness whose node runs over a FaultKV, each prep
// changing the node's Config first.
func faulted(t *testing.T, prep ...func(*node.Config)) (*nodetest.Harness, *kvtest.FaultKV) {
	var f *kvtest.FaultKV
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		for _, p := range prep {
			p(&cfg)
		}
		return node.New(cfg)
	}))
	return h, f
}

// setFault has f answer each call by rules, delays waiting on held.
func setFault(t *testing.T, f *kvtest.FaultKV, held *sim.TestClock, rules ...sim.Rule) {
	t.Helper()
	sched, err := sim.NewSchedule(1, held, rules...)
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
}

// nsScan is the namespaces' scan, as a FaultKV rule names it: a share
// pass's read of which remote store each namespace names.
var nsScan = keys.Describe([]byte{keys.KindNSR})

// floorRead matches the version floor reads of share id, the first store
// call of the engine's Bind of it.
func floorRead(id entity.ShareID) string {
	return keys.Describe(append(keys.ID([]byte{keys.KindS}, id[:]), keys.SVf)) + "*"
}

// logLines is a log's sink, each write a line sent to it, dropped when the
// channel is full.
type logLines chan string

func (l logLines) Write(p []byte) (int, error) {
	select {
	case l <- string(p):
	default:
	}
	return len(p), nil
}

// heldUntil advances the node's clock a second at a time until a store
// call waits on held. After each second it polls held for 100 ms of real
// time, so the clock does not run past the bound of a pass's store call
// before the pass reaches the call held.
func heldUntil(t *testing.T, h *nodetest.Harness, held *sim.TestClock, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for held.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 20 s of turns", what)
		}
		h.Clock().Advance(time.Second)
		for step := time.Now().Add(100 * time.Millisecond); held.Pending() == 0 && time.Now().Before(step); {
			runtime.Gosched()
		}
	}
}

// Reads that fail for three turns leave the running set as it was and bind
// nothing; the share created meanwhile is bound by the first read that
// works, not before.
func TestReconcileAfterFailedReads(t *testing.T) {
	h, f := faulted(t)
	configure(t, h, storeA)
	a := createShare(t, h, "a", storeA)
	started(t, h)
	held := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, held, sim.Rule{Op: kvtest.OpGet, Key: cfgGen, Count: 3, Action: sim.FaultAction{Delay: time.Hour, Err: errInjected}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
	b := createShare(t, h, "b", storeA)
	for i := range 3 {
		heldUntil(t, h, held, fmt.Sprintf("counters read %d", i+1))
		if inEngine(h, b.ID) {
			t.Fatalf("share created while reads fail: bound before read %d, want it bound only once a read works", i+1)
		}
		if st, ok := h.Node().Bound(a.ID); !ok || st != entity.ShareEnabled {
			t.Errorf("running set at failed read %d: share a got %v (bound %v), want it kept enabled", i+1, st, ok)
		}
		held.Advance(time.Hour)
	}
	caughtUp(t, h)
	if !inEngine(h, b.ID) {
		t.Errorf("share created while reads failed: not bound once a read worked")
	}
}

// A store call held past its bound ends exactly at the bound on the node's
// clock, 20 s, failing the pass, and reconcile turns again; the held call is
// released only by its own clock. A write's wake starts the pass, so the
// node's clock stands still from the call's start.
func TestReconcileBoundsAStoreCall(t *testing.T) {
	logs := make(logLines, 256)
	h, f := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	started(t, h)
	held := sim.NewTestClock(time.Unix(0, 0))
	// The first read is Applied's own, the second the pass its wake starts.
	setFault(t, f, held, sim.Rule{Op: kvtest.OpGet, Key: cfgGen, From: 2, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = h.Node().Applied(ctx) }()
	wait(t, "the woken pass's counters read held", func() bool { return held.Pending() > 0 })
	timedOut := func(within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case l := <-logs:
				if strings.Contains(l, "did not answer within") {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	h.Clock().Advance(19 * time.Second)
	// No clock reaches a pass that has not failed, so a bounded wait is the
	// assertion: a pass failed by then would have logged at once.
	if timedOut(200 * time.Millisecond) {
		t.Fatalf("the held store call failed its pass 19 s after it started, want it held until 20 s")
	}
	h.Clock().Advance(time.Second)
	if !timedOut(10 * time.Second) {
		t.Fatalf("the held store call did not fail its pass 20 s after it started on the node's clock")
	}
	moved := h.Node().Moved()
	for range 10 {
		select {
		case <-moved:
			return
		default:
		}
		turn(h)
	}
	t.Errorf("no reconcile pass ended within 10 s of turns after the bound")
}

// A pass's own store calls do not count toward the watchdog's stall with the
// backoff before them: after a 30 s backoff, a pass whose two store calls
// take 15 s and 16 s leaves the node live, over 60 s since its last pass
// ended.
func TestReconcileStaysLiveThroughTheBackoffAndAPass(t *testing.T) {
	h, f := faulted(t)
	started(t, h)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held,
		sim.Rule{Op: kvtest.OpGet, Key: cfgGen, Count: 6, Action: sim.FaultAction{Delay: time.Hour, Err: errInjected}},
		sim.Rule{Op: kvtest.OpGet, Key: cfgGen, From: 7, Count: 1, Action: sim.FaultAction{Delay: time.Hour}},
		sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}},
	)
	defer held.Advance(time.Hour)
	var failedAt time.Duration
	for i := range 6 { // waits of 1, 2, 4, 8 and 16 s, then 30 s
		heldUntil(t, h, held, fmt.Sprintf("failed counters read %d", i+1))
		failedAt = h.Clock().Elapsed()
		held.Advance(time.Hour)
	}
	heldUntil(t, h, held, "counters read after the 30 s backoff")
	// heldUntil may pass the read's start by a second, never fall short of it.
	if got := h.Clock().Elapsed() - failedAt; got < 30*time.Second {
		t.Fatalf("wait after six failed reads: got %v, want the backoff's cap, 30 s", got)
	}
	h.Clock().Advance(15 * time.Second)
	held.Advance(time.Hour)
	wait(t, "the namespaces' scan held", func() bool { return held.Pending() > 0 })
	h.Clock().Advance(16 * time.Second)
	api, _ := h.Node().Addrs()
	wait(t, "liveness over 60 s after the last pass ended, 31 s into a pass", func() bool {
		return get(t, api, "/health/live") == answer{http.StatusOK, "live"}
	})
}

// A refusal of the engine's Bind is retried on the backoff, with no counter
// moving: the share is bound once the engine takes it.
func TestReconcileRetriesABindTheEngineRefused(t *testing.T) {
	h, f := faulted(t)
	started(t, h)
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	setFault(t, f, nil, sim.Rule{Op: kvtest.OpBefore, Key: floorRead(sh.ID), Count: 1, Action: sim.FaultAction{Err: errInjected}})
	caughtUp(t, h)
	if inEngine(h, sh.ID) {
		t.Fatalf("share whose bind the engine refused: bound, want refused")
	}
	if got := h.Node().Refused(); len(got) != 1 || entity.ShareID(got[0].Record.ID) != sh.ID {
		t.Fatalf("refused records after the engine refused the bind: got %s, want share a", describe(got))
	}
	pos := h.Node().Positions()
	for range 5 {
		turn(h)
		if inEngine(h, sh.ID) {
			break
		}
	}
	if !inEngine(h, sh.ID) {
		t.Errorf("share whose bind the engine refused once: not bound 5 s later, want it retried on the backoff")
	}
	if got := h.Node().Positions(); got != pos {
		t.Errorf("positions while the bind was retried: got %+v, want %+v, no counter moved", got, pos)
	}
}

// A pass whose read failed has the next pass act on every kind, a share
// pass included, though no counter moved.
func TestReconcileActsOnEveryKindAfterAFailedRead(t *testing.T) {
	h, f := faulted(t)
	started(t, h)
	caughtUp(t, h)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held,
		sim.Rule{Op: kvtest.OpGet, Key: cfgGen, Count: 1, Action: sim.FaultAction{Err: errInjected}},
		sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}},
	)
	defer held.Advance(time.Hour)
	heldUntil(t, h, held, "share pass after the failed read")
}

// A share whose removal is marked while a share pass that listed it runs,
// and which the removal drops before the pass binds it, is not bound: the
// late bind is refused, and the share is not listed among the refusals, as
// it is gone. Dropping it again answers nil.
func TestReconcileDoesNotBindAShareDroppedMidPass(t *testing.T) {
	h, f := faulted(t)
	started(t, h)
	configure(t, h, storeA)
	sh := createShare(t, h, "a", storeA)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	heldUntil(t, h, held, "the namespaces' scan of the pass that listed the share")
	if err := h.Store().ControlPlane().DeleteShare(t.Context(), sh.ID, entity.SnapshotsRefuse, 1); err != nil {
		t.Fatalf("mark the share removed: %v", err)
	}
	if err := h.Node().Engine().Drop(t.Context(), sh.ID, journal.ShareTag(sh.Tag)); err != nil {
		t.Fatalf("drop the share: %v", err)
	}
	moved := h.Node().Moved()
	held.Advance(time.Hour)
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatalf("the held pass did not end once its scan was released")
	}
	if inEngine(h, sh.ID) {
		t.Errorf("share dropped while the pass that listed it ran: bound in the engine, want refused")
	}
	if _, ok := h.Node().Bound(sh.ID); ok {
		t.Errorf("share dropped while the pass that listed it ran: in the running set, want not")
	}
	if refusedShare(h.Node().Refused(), sh.ID) {
		t.Errorf("share dropped while the pass that listed it ran: among the refusals, want not, as it is gone")
	}
	if err := h.Node().Engine().Drop(t.Context(), sh.ID, journal.ShareTag(sh.Tag)); err != nil {
		t.Errorf("a second drop of the share the engine never bound: got %v, want nil, as the journal holds nothing of its tag", err)
	}
}

// A write's wait wakes reconcile and returns only on a pass that read the
// counters after the write: a wake sent while a pass runs runs one more pass
// after it, and the wait does not return on the one running. A wait whose
// context ends returns its error. The clock never moves, so only a wake
// starts a pass.
func TestAppliedWaitsForAPassAfterTheWrite(t *testing.T) {
	reads := make(chan kvstore.Counters)
	release := make(chan struct{})
	done := make(chan struct{}) // closed as the test ends, so a failed test's Stop finds no pass held
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewPausing(func(c kvstore.Counters) {
		select {
		case reads <- c:
		case <-done:
			return
		}
		select {
		case <-release:
		case <-done:
		}
	})))
	pass := func(what string) kvstore.Counters {
		t.Helper()
		select {
		case c := <-reads:
			return c
		case <-time.After(10 * time.Second):
			t.Fatalf("no reconcile pass: %s", what)
			return kvstore.Counters{}
		}
	}
	applied := func(ctx context.Context) <-chan error {
		ch := make(chan error, 1)
		go func() { ch <- h.Node().Applied(ctx) }()
		return ch
	}
	recv := func(ch <-chan error, what string) error {
		t.Helper()
		select {
		case err := <-ch:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no answer within 10 s", what)
			return nil
		}
	}

	startErr := make(chan error, 1)
	go func() { startErr <- h.Node().Start(t.Context()) }()
	pass("Start's first full reconcile")
	release <- struct{}{}
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	defer close(done)

	first := applied(t.Context())
	pass("the pass the first Applied woke")
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: entity.NewUser(), Name: "alice"}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	want := counters(t, h)
	second := applied(t.Context())
	wait(t, "the second Applied's wake", h.Node().Woken)
	release <- struct{}{}
	if err := recv(first, "the first Applied"); err != nil {
		t.Errorf("first Applied: %v", err)
	}
	pass("a pass after the one running when the second Applied woke reconcile")
	select {
	case err := <-second:
		t.Errorf("second Applied returned (%v) on the pass running when it woke, want it to wait for the next", err)
	default:
	}
	release <- struct{}{}
	if err := recv(second, "the second Applied"); err != nil {
		t.Errorf("second Applied: %v", err)
	}
	if pos := h.Node().Positions(); pos.Settings < want.Settings || pos.Principals < want.Principals {
		t.Errorf("positions once the second Applied returned: got %+v, want them to cover the counters read after the write, %+v", pos, want)
	}

	if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: entity.NewUser(), Name: "bob"}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	third := applied(ctx)
	pass("the pass the third Applied woke")
	cancel()
	if err := recv(third, "Applied under a cancelled context"); !errors.Is(err, context.Canceled) {
		t.Errorf("Applied with its pass held and its context cancelled: got %v, want context.Canceled", err)
	}
	release <- struct{}{}
}

// A share pass that fails still has apply run in the same pass, so a
// principals change does not wait for the share store: the pass publishes
// the principals position, so Applied returns, and keeps the share list and
// settings positions; it then fails and is retried, applying again with no
// counter moving.
func TestReconcileAppliesWhenTheSharePassFails(t *testing.T) {
	h, f := faulted(t)
	applies := make(chan kvstore.Counters, 64)
	h.Node().OnApply(func(c kvstore.Counters) {
		select {
		case applies <- c:
		default:
		}
	})
	started(t, h)
	before := h.Node().Positions()
	setFault(t, f, nil, sim.Rule{Op: kvtest.OpScan, Key: nsScan, Action: sim.FaultAction{Err: errInjected}})
	createShare(t, h, "a", storeA)
	u := entity.User{Name: "alice"}
	u.Principal = entity.NewUser()
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), u, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	want := counters(t, h).Principals
	appliedThrough := func(what string, reached func(kvstore.Counters) bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			select {
			case c := <-applies:
				if reached(c) {
					return
				}
				continue
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no apply that far within 30 s of turns while the share pass fails", what)
			}
			turn(h)
		}
	}

	principals := func(c kvstore.Counters) bool { return c.Principals >= want }
	appliedThrough("the pass that read the principals change", principals)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a backstop: Applied returns on the position moving
	defer cancel()
	if err := h.Node().Applied(ctx); err != nil {
		t.Fatalf("Applied while the share pass fails: %v, want it to return once principals are applied", err)
	}
	pos, now := h.Node().Positions(), counters(t, h)
	if pos.Principals < want {
		t.Errorf("principals position after a pass whose share pass failed: got %d, want %d, the generation it applied", pos.Principals, want)
	}
	if pos.ShareList >= now.ShareList || pos.Settings != before.Settings {
		t.Errorf("share list and settings positions after a failed share pass: got %d and %d, want below %d and still %d", pos.ShareList, pos.Settings, now.ShareList, before.Settings)
	}
	appliedThrough("the pass retried after the share pass failed", principals)

	// A settings change also configures a share's remote store, which only
	// the share pass acts on, so its position stays while that pass fails.
	configure(t, h, storeA)
	settings := counters(t, h).Settings
	appliedThrough("the pass that read the settings change", func(c kvstore.Counters) bool { return c.Settings >= settings })
	if got := h.Node().Positions().Settings; got >= settings {
		t.Errorf("settings position after applying generation %d while the share pass fails: got %d, want below it", settings, got)
	}
}
