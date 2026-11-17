package node_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
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
	sh := entity.Share{ID: entity.NewShareID(), Name: name, Path: "/" + name, Root: entity.NewFileID()}
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: store}
		if _, err := tx.PutNamespace(ns, 0); err != nil {
			return err
		}
		sh.Namespaces = []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}
		_, err := tx.PutShare(sh, 0)
		return err
	})
	if err != nil {
		t.Fatalf("create share %s: %v", name, err)
	}
	got, _, err := h.Store().ShareIndex().Share(t.Context(), sh.ID)
	if err != nil {
		t.Fatalf("read share %s back: %v", name, err)
	}
	return got
}

// configure gives store a setting, which makes it a configured remote store.
func configure(t *testing.T, h *nodetest.Harness, store [16]byte) {
	t.Helper()
	s := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "endpoint", Value: []byte("https://store.invalid")}
	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
		t.Fatalf("configure the remote store: %v", err)
	}
}

// inEngine reports whether the engine's stats count share id among a
// journal's shares, which only a binding puts there.
func inEngine(h *nodetest.Harness, id entity.ShareID) bool {
	for _, j := range h.Node().Engine().Stats().Journals {
		if _, ok := j.Shares[id]; ok {
			return true
		}
	}
	return false
}

// counters reads the counters as another node would.
func counters(t *testing.T, h *nodetest.Harness) kvstore.Counters {
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
	deadline := time.Now().Add(30 * time.Second)
	for !covers(h.Node().Positions(), c) {
		if time.Now().After(deadline) {
			t.Fatalf("positions %+v after 30 s of turns, want them to cover %+v", h.Node().Positions(), c)
		}
		turn(h)
	}
}

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

func started(t *testing.T, h *nodetest.Harness) {
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
// quiesced, its binding is quiesced, and resumed, serving again. A grant
// raises no counter, so it leaves the positions where they were.
func TestReconcileBindsQuiescesAndResumes(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
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

	before := h.Node().Positions()
	if _, err := cp.SetGrant(t.Context(), entity.ShareGrant{Share: sh.ID, Principal: entity.Everyone, Access: entity.AccessRead}, 0); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := counters(t, h); got != before {
		t.Errorf("counters after a grant: got %+v, want the positions reconciled before it, %+v", got, before)
	}
}

// Shares spanning more than one page of the store's listing are all bound.
func TestReconcileBindsEveryPage(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	started(t, h)
	configure(t, h, storeA)
	var shares []entity.Share
	for i := range 300 {
		shares = append(shares, createShare(t, h, fmt.Sprintf("s%d", i), storeA))
	}
	caughtUp(t, h)
	for _, sh := range shares {
		if !inEngine(h, sh.ID) {
			t.Fatalf("share %s of 300: not bound once reconcile caught up", sh.Name)
		}
	}
}

// A share whose namespace's remote store is not configured is not bound and
// is refused, naming it, beside a share that is bound; once its store is
// configured it is bound and no longer refused.
func TestReconcileRefusesAShareWithNoRemoteStore(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	started(t, h)
	configure(t, h, storeA)
	a := createShare(t, h, "a", storeA)
	b := createShare(t, h, "nowhere", storeB)
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
		t.Errorf("refused records: got %+v, want share nowhere at generation %d, its error naming it", refused, b.Position)
	}

	configure(t, h, storeB)
	caughtUp(t, h)
	if !inEngine(h, b.ID) {
		t.Errorf("share once its store is configured: not bound, want bound")
	}
	if got := h.Node().Refused(); len(got) != 0 {
		t.Errorf("refused records once the store is configured: got %+v, want none", got)
	}
}

// faulted builds a harness whose node runs over a FaultKV.
func faulted(t *testing.T, opts ...func(node.Config) (*node.Node, error)) (*nodetest.Harness, *kvtest.FaultKV) {
	var f *kvtest.FaultKV
	newNode := node.New
	if len(opts) > 0 {
		newNode = opts[0]
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return newNode(cfg)
	}))
	return h, f
}

// heldUntil advances the node's clock a second at a time until a store
// call waits on held.
func heldUntil(t *testing.T, h *nodetest.Harness, held *sim.TestClock, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for held.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 10 s of turns", what)
		}
		h.Clock().Advance(time.Second)
		runtime.Gosched()
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

// A store call held past its bound ends at the bound on the node's clock,
// and reconcile turns again, so the node stays live past the watchdog's
// stall; the held call is released only by its own clock.
func TestReconcileBoundsAStoreCall(t *testing.T) {
	reads := make(chan kvstore.Counters, 256)
	h, f := faulted(t, node.NewPausing(func(_ *node.Node, c kvstore.Counters) {
		select {
		case reads <- c:
		default:
		}
	}))
	started(t, h)
	held := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, held, sim.Rule{Op: kvtest.OpGet, Key: cfgGen, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
	defer held.Advance(time.Hour)
	heldUntil(t, h, held, "counters read held")
	for len(reads) > 0 {
		<-reads
	}
	heldAt := h.Clock().Elapsed()
	h.Clock().Advance(21 * time.Second)
	again := time.Now().Add(10 * time.Second)
	for passed := false; !passed; {
		if time.Now().After(again) {
			t.Fatalf("no reconcile pass once the held store call passed its 20 s bound on the node's clock")
		}
		h.Clock().Advance(time.Second)
		select {
		case <-reads:
			passed = true
		case <-time.After(100 * time.Millisecond):
		}
	}
	for h.Clock().Elapsed() < heldAt+66*time.Second {
		turn(h)
	}
	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/live"); got != (answer{http.StatusOK, "live"}) {
		t.Errorf("liveness 66 s after a store call was held: got %+v, want 200 live", got)
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
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewPausing(func(_ *node.Node, c kvstore.Counters) {
		reads <- c
		<-release
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
	type result struct {
		pos kvstore.Counters
		err error
	}
	applied := func(ctx context.Context) <-chan result {
		ch := make(chan result, 1)
		go func() {
			pos, err := h.Node().Applied(ctx)
			ch <- result{pos, err}
		}()
		return ch
	}
	recv := func(ch <-chan result, what string) result {
		t.Helper()
		select {
		case a := <-ch:
			return a
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no answer within 10 s", what)
			return result{}
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

	first := applied(t.Context())
	pass("the pass the first Applied woke")
	if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: entity.NewUser(), Name: "alice"}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	want := counters(t, h)
	second := applied(t.Context())
	wait(t, "the second Applied's wake", h.Node().Woken)
	release <- struct{}{}
	if a := recv(first, "the first Applied"); a.err != nil {
		t.Errorf("first Applied: %v", a.err)
	}
	pass("a pass after the one running when the second Applied woke reconcile")
	select {
	case a := <-second:
		t.Errorf("second Applied returned %+v on the pass running when it woke, want it to wait for the next", a)
	default:
	}
	release <- struct{}{}
	if a := recv(second, "the second Applied"); a.err != nil || a.pos.Settings < want.Settings || a.pos.Principals < want.Principals {
		t.Errorf("second Applied: got positions %+v (%v), want them to cover the counters read after the write, %+v", a.pos, a.err, want)
	}

	if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: entity.NewUser(), Name: "bob"}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("create a user: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	third := applied(ctx)
	pass("the pass the third Applied woke")
	cancel()
	if a := recv(third, "Applied under a cancelled context"); !errors.Is(a.err, context.Canceled) {
		t.Errorf("Applied with its pass held and its context cancelled: got %v, want context.Canceled", a.err)
	}
	release <- struct{}{}
}
