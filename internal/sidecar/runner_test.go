package sidecar_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// The runner (RFC 20 section 5.3's... section 3.5): each sidecar synced with
// the Serving of those of its adapters now serving, in the order around each
// adapter's Start, Reconfigure and Stop; a disabled one with the empty set;
// a failed Sync retried with its last set after a backoff from 1 s doubling
// to 30 s; Failed naming it until then; Close ending the retries. The tests
// drive real core servers over loopback, named nfs and smb, on testproto.

// budgets is a fresh set of the four budgets, wide enough for the tests.
func budgets() *resources.Budgets {
	return &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 4<<20),
		Reply:      resources.NewWaitBudget("reply", 4<<20),
		Connection: resources.NewBudget("connection", 4<<20),
		Hold:       resources.NewBudget("hold", 4<<20),
	}
}

// on is testproto's settings, binding each of addrs.
func on(addrs ...string) testproto.Settings {
	s := testproto.Settings{Adapter: settings.DefaultNFS().Adapter}
	for _, a := range addrs {
		s.Addrs = append(s.Addrs, netip.MustParseAddr(a))
	}
	return s
}

// needIPv6Loopback skips t where ::1 cannot be bound.
func needIPv6Loopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("::1 cannot be bound here: %v", err)
	}
	_ = ln.Close()
}

// waitsFor polls cond until it holds, failing the test at its deadline.
func waitsFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// manualClock is a clock the test moves: a retry's wait fires when Advance
// reaches it. It carries no Now: the waits are the only thing the runner
// asks its clock for.
type manualClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers map[int]manualTimer
	next   int
}

type manualTimer struct {
	at time.Duration
	f  func()
}

func newManualClock() *manualClock { return &manualClock{timers: map[int]manualTimer{}} }

func (c *manualClock) AfterFunc(d time.Duration, f func()) sidecar.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.timers[id] = manualTimer{c.now + d, f}
	return stopFunc(func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.timers[id]
		delete(c.timers, id)
		return ok
	})
}

// stopFunc is a timer whose Stop is the func.
type stopFunc func() bool

func (f stopFunc) Stop() bool { return f() }

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []func()
	for id, tm := range c.timers {
		if tm.at <= c.now {
			due = append(due, tm.f)
			delete(c.timers, id)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// pending is how many waits are still armed.
func (c *manualClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// nextIn is how far the nearest armed wait sits from now.
func (c *manualClock) nextIn() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Duration
	for _, tm := range c.timers {
		if d := tm.at - c.now; next == 0 || d < next {
			next = d
		}
	}
	return next
}

// holder is the sidecar the tests declare: what a Sync hands it it holds, so
// a test reads the sets the runner synced (RFC 20 section 3.5).
type holder struct {
	name string
	owns []string
	mu   sync.Mutex
	held []adapter.Serving
}

func newHolder(name string, owns ...string) *holder { return &holder{name: name, owns: owns} }

func (h *holder) Name() string       { return h.name }
func (h *holder) Adapters() []string { return h.owns }

func (h *holder) Sync(_ context.Context, serving []adapter.Serving) error {
	h.mu.Lock()
	h.held = serving
	h.mu.Unlock()
	return nil
}

// holding is what its last Sync left it with.
func (h *holder) holding() []adapter.Serving {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

// faulting is a sidecar whose Sync fails while its cue is set: a sidecar
// that cannot be brought to match, on cue.
type faulting struct {
	adapter.Sidecar
	cue atomic.Bool
}

// errSyncFault is what the faulting sidecar's Sync fails with.
var errSyncFault = errors.New("the sidecar cannot be synced")

func (f *faulting) Sync(ctx context.Context, serving []adapter.Serving) error {
	if f.cue.Load() {
		return errSyncFault
	}
	return f.Sidecar.Sync(ctx, serving)
}

// gating is a holder that parks one driven set's build on demand: its Adapters
// read counts down from armN, and the read that reaches zero parks on the
// gate, entered telling the test the build it wanted held is held, with the
// runner free to move whatever else the test moves in the meantime.
type gating struct {
	*holder
	armN    atomic.Int32
	entered chan struct{}
	gate    chan struct{}
}

func (g *gating) Adapters() []string {
	if g.armN.Add(-1) == 0 {
		g.entered <- struct{}{}
		<-g.gate
	}
	return g.holder.Adapters()
}

// hooked is a real adapter with one reading folded in: each lifecycle call
// the runner drives reads what the sidecar holds, and what the adapter it
// serves has bound, before it carries the call. RFC 20 section 3.5's order
// shows itself in what those readings find.
type hooked struct {
	adapter.Adapter
	read func(call string)
}

func (h hooked) Start(ctx context.Context) error {
	h.read("start")
	return h.Adapter.Start(ctx)
}

func (h hooked) Reconfigure(ctx context.Context, s settings.Settings) error {
	h.read("reconfigure")
	return h.Adapter.Reconfigure(ctx, s)
}

func (h hooked) Stop(ctx context.Context) error {
	h.read("stop")
	return h.Adapter.Stop(ctx)
}

// mark is one reading: where the call found the sidecar, and what the
// adapter it serves reported bound at that moment.
type mark struct {
	call string
	held []adapter.Serving
	srvs []adapter.Serving
}

// buildServer is one of the runner's adapters: a real core server on
// testproto over loopback, one listener named after its adapter, stopped by
// the test's own end.
func buildServer(t *testing.T, b *resources.Budgets, service string, snap testproto.Settings) *core.Server {
	t.Helper()
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, core.Listener{Services: []string{service}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}

// servingOf is the adapter's own Serving, built as the runner builds them
// from Addrs.
func servingOf(name string, a adapter.Adapter) []adapter.Serving {
	return []adapter.Serving{{Adapter: name, Listeners: a.Addrs()}}
}

// sameServs tells whether two serving sets match, adapter by adapter, every
// listener's addresses included.
func sameServs(a, b []adapter.Serving) bool {
	if len(a) != len(b) {
		return false
	}
	return maps.EqualFunc(servMap(a), servMap(b), func(x, y adapter.Serving) bool {
		return maps.EqualFunc(x.Listeners, y.Listeners, slices.Equal)
	})
}

// servMap keys a set's adapters, one Serving per adapter.
func servMap(set []adapter.Serving) map[string]adapter.Serving {
	m := make(map[string]adapter.Serving, len(set))
	for _, s := range set {
		m[s.Adapter] = s
	}
	return m
}

// minus is the set with each of drop's addresses left out of every
// listener's slice: the expectation of a Sync before a removal.
func minus(set []adapter.Serving, drop ...netip.Addr) []adapter.Serving {
	out := make([]adapter.Serving, len(set))
	for i, s := range set {
		ls := make(map[adapter.Socket][]netip.AddrPort, len(s.Listeners))
		for k, addrs := range s.Listeners {
			keep := make([]netip.AddrPort, 0, len(addrs))
			for _, at := range addrs {
				if !slices.Contains(drop, at.Addr()) {
					keep = append(keep, at)
				}
			}
			ls[k] = keep
		}
		out[i] = adapter.Serving{Adapter: s.Adapter, Listeners: ls}
	}
	return out
}

// The order the runner syncs in, read from what the sidecar holds at each
// adapter call: after Start the adapter's own set, at a Reconfigure that
// removes an address that address already left out, after every
// Reconfigure whatever Addrs reports then, a refused one included, and
// before Stop nothing, though the adapter still answers. A Sync that runs
// after the removal, or none before the drain, or one that carries the
// sidecar no longer its own set, fails the readings (RFC 20 section 3.5).
func TestTheRunnerSyncsAroundEachAdapterCall(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	h := newHolder("holder", "nfs")
	r := sidecar.New(h)
	t.Cleanup(r.Close)

	snfs := buildServer(t, b, "nfs", on("127.0.0.1", "::1"))
	var timeline []mark
	read := func(adapterName string) func(string) {
		return func(call string) {
			timeline = append(timeline, mark{adapterName + " " + call, h.holding(), servingOf("nfs", snfs)})
		}
	}
	nfs := hooked{snfs, read("nfs")}
	smb := hooked{buildServer(t, b, "smb", on("127.0.0.1")), read("smb")}

	if err := r.Start(t.Context(), "nfs", nfs); err != nil {
		t.Fatal(err)
	}
	afterStart := servingOf("nfs", snfs)
	if got := h.holding(); !sameServs(got, afterStart) {
		t.Fatalf("after Start the sidecar holds %v, want nfs's set %v", got, afterStart)
	}

	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); !sameServs(got, afterStart) {
		t.Errorf("across smb's Start the sidecar holds %v, want nfs's set untouched %v", got, afterStart)
	}

	// A Reconfigure refusing its settings still comes around: the removed
	// address is left out before the call, and back after it, since the
	// refusal changed nothing.
	refused := on("127.0.0.1")
	refused.MaxVersion = 3
	if err := r.Reconfigure(t.Context(), "nfs", nfs, refused); err == nil {
		t.Fatal("the settings Validate refuses were accepted")
	}
	if got := h.holding(); !sameServs(got, afterStart) {
		t.Errorf("after the refused Reconfigure the sidecar holds %v, want what Addrs still reports %v", got, afterStart)
	}

	if err := r.Reconfigure(t.Context(), "nfs", nfs, on("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	afterRemoval := servingOf("nfs", snfs)
	if got := h.holding(); !sameServs(got, afterRemoval) {
		t.Errorf("after the removal the sidecar holds %v, want what Addrs reports %v", got, afterRemoval)
	}

	// Before Stop the sidecar holds nothing, though nfs still answers on
	// 127.0.0.1: the advertisement is withdrawn while the adapter can still
	// be reached, which is the point of syncing before the drain.
	beforeStop := servingOf("nfs", snfs)
	if err := r.Stop(t.Context(), "nfs", nfs); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); len(got) != 0 {
		t.Errorf("after Stop the sidecar holds %v, want nothing", got)
	}

	if len(timeline) != 5 {
		t.Fatalf("the readings number %d, want the five calls the test drove", len(timeline))
	}
	if m := timeline[0]; m.call != "nfs start" || m.held != nil {
		t.Errorf("at its start the sidecar already holds %v, want no set handed it before its adapter serves", m.held)
	}
	if m := timeline[1]; m.call != "smb start" || !sameServs(m.held, m.srvs) {
		t.Errorf("at smb's start the sidecar holding nfs alone holds %v, want nfs's set %v: smb is not its adapter", m.held, m.srvs)
	}
	for i, m := range timeline[2:4] {
		if m.call != "nfs reconfigure" {
			t.Errorf("the reading between the two Reconfigures is %q", m.call)
		}
		want := minus(m.srvs, netip.IPv6Loopback())
		if !sameServs(m.held, want) {
			t.Errorf("before Reconfigure %d the sidecar holds %v, want the set with ::1 left out %v", i+1, m.held, want)
		}
	}
	if m := timeline[4]; m.call != "nfs stop" {
		t.Errorf("the last reading is %q, want nfs's stop", m.call)
	} else if len(m.held) != 0 {
		t.Errorf("before Stop the sidecar holds %v, want nothing until the drain", m.held)
	} else if !sameServs(m.srvs, beforeStop) {
		t.Errorf("at Stop nfs reported %v, want it still bound on %v: the sync ran after the drain", m.srvs, beforeStop)
	}
}

// A disabled sidecar holds nothing, is synced by nothing while it is
// disabled, and holds its adapter's set again once re-enabled; a name no
// sidecar carries is refused. A disable that leaves the serving set in, or a
// lifecycle sync that reaches a disabled one, fails here (RFC 20 section 3.5).
func TestADisabledSidecarIsNotSynced(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	h := newHolder("holder", "nfs")
	r := sidecar.New(h)
	t.Cleanup(r.Close)
	nfs := buildServer(t, b, "nfs", on("127.0.0.1"))
	smb := buildServer(t, b, "smb", on("127.0.0.1"))

	if err := r.Start(t.Context(), "nfs", nfs); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); !sameServs(got, servingOf("nfs", nfs)) {
		t.Fatalf("after both Starts the sidecar holds %v, want nfs's set alone %v", got, servingOf("nfs", nfs))
	}

	if err := r.SetEnabled(t.Context(), "holder", false); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); len(got) != 0 {
		t.Errorf("disabled, the sidecar holds %v, want the empty set", got)
	}
	// The settings change is live and reaches the enabled sidecars of smb
	// alone: none serve it, and the disabled one is synced by nothing.
	next := on("127.0.0.1")
	next.MaxVersion = 1
	if err := r.Reconfigure(t.Context(), "smb", smb, next); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); len(got) != 0 {
		t.Errorf("across smb's reconfigure and stop a disabled sidecar holding nfs was synced: %v", got)
	}

	if err := r.SetEnabled(t.Context(), "holder", true); err != nil {
		t.Fatal(err)
	}
	if got := h.holding(); !sameServs(got, servingOf("nfs", nfs)) {
		t.Errorf("enabled again, the sidecar holds %v, want nfs's set %v", got, servingOf("nfs", nfs))
	}
	if err := r.SetEnabled(t.Context(), "bogus", false); err == nil {
		t.Error("SetEnabled named no sidecar it carries, and returned nil")
	}
}

// A sidecar whose Sync fails on cue fails only itself: Failed names it, the
// other sidecar is synced across an address change, both adapters kept
// serving and ready, and the retry is armed behind the failure. A Sync
// failure that fails the adapter's Start, stops the other sidecar's syncs or
// never names the sidecar fails here (RFC 20 sections 3.5 and 3.7).
func TestAFailingSidecarFailsOnlyItself(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	good := newHolder("good", "nfs")
	gone := &faulting{Sidecar: newHolder("gone", "smb")}
	r := sidecar.New(good, gone)
	t.Cleanup(r.Close)
	clk := newManualClock()
	sidecar.SetClock(r, clk)
	nfs := buildServer(t, b, "nfs", on("127.0.0.1", "::1"))
	smb := buildServer(t, b, "smb", on("127.0.0.1"))

	gone.cue.Store(true)
	if err := r.Start(t.Context(), "nfs", nfs); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatalf("a failing Sync moved into the adapter's Start: %v", err)
	}
	if f := r.Failed(); len(f) != 1 || f[0] != "gone" {
		t.Fatalf("after its failed Sync Failed() = %q, want gone alone", f)
	}
	for _, srv := range []*core.Server{nfs, smb} {
		if !srv.Health().Ready() {
			t.Errorf("a failing sidecar left %s unready", srv.Health().State)
		}
	}
	if got, exp := good.holding(), servingOf("nfs", nfs); !sameServs(got, exp) {
		t.Errorf("the other sidecar holds %v, want nfs's full set %v", got, exp)
	}

	// The other sidecar is synced across the removal, the failing one left
	// untouched: nothing else failed with it.
	if err := r.Reconfigure(t.Context(), "nfs", nfs, on("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if got, exp := good.holding(), servingOf("nfs", nfs); !sameServs(got, exp) {
		t.Errorf("across a failing sidecar, the other sidecar holds %v, want the synced set %v", got, exp)
	}
	if h := gone.Sidecar.(*holder); len(h.holding()) != 0 {
		t.Errorf("the failing sidecar was handed its sync as if it had matched: %v", h.holding())
	}
	waitsFor(t, "the retry behind the failed Sync to arm", func() bool { return clk.pending() > 0 })
}

// A failed Sync is retried with its last set, after a backoff from 1 s
// doubling to 30 s, until one succeeds: the clock moved by the test shows
// the first retry a second after the failure and the second doubled to two,
// Failed naming it until the cue lifts, and the retry carrying the last set
// rather than the serving set, which a disabled window tells apart. Close
// ends the retries: a failing one is not called again, and no wait it armed
// is left running (RFC 20 section 3.5).
func TestAFailedSyncIsRetriedUntilItSucceeds(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	h := newHolder("holder", "nfs", "smb")
	f := &faulting{Sidecar: h}
	r := sidecar.New(f)
	t.Cleanup(r.Close)
	clk := newManualClock()
	sidecar.SetClock(r, clk)
	nfs := buildServer(t, b, "nfs", on("127.0.0.1"))
	smb := buildServer(t, b, "smb", on("127.0.0.1"))

	f.cue.Store(true)
	if err := r.Start(t.Context(), "nfs", nfs); err != nil {
		t.Fatal(err)
	}
	if f := r.Failed(); len(f) != 1 || f[0] != "holder" {
		t.Fatalf("after the failed Sync Failed() = %q, want holder alone", f)
	}
	// The retry runs a second after the failure, no fixed delay the test
	// could not pin: its next wait sits a second out.
	waitsFor(t, "the retry's first wait to arm", func() bool { return clk.pending() > 0 })
	if d := clk.nextIn(); d != time.Second {
		t.Fatalf("the retry waits %v, want the 1 s base", d)
	}
	clk.Advance(time.Second)
	waitsFor(t, "the failed retry's second wait to arm", func() bool { return clk.pending() > 0 })
	if d := clk.nextIn(); d != 2*time.Second {
		t.Fatalf("the second retry waits %v, want the doubled 2 s", d)
	}
	if f := r.Failed(); len(f) != 1 || f[0] != "holder" {
		t.Errorf("before the cue lifts Failed() = %q, want holder still named", f)
	}

	// The retry carries the last set, not a rebuilt serving set: the disable
	// failed while the cue held, so the last set is the empty one, and an
	// adapter starting while the sidecar is disabled changes the serving set
	// without another sync.
	if err := r.SetEnabled(t.Context(), "holder", false); err != nil {
		t.Fatal(err)
	}
	f.cue.Store(false)
	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Second)
	waitsFor(t, "the retry to succeed against the lifted cue", func() bool { return len(r.Failed()) == 0 })
	if got := h.holding(); len(got) != 0 {
		t.Errorf("the successful retry synced %v, want the last set it was given, the empty one", got)
	}

	// Enabled again the sidecar is synced with the serving set, once, by the
	// call that enabled it.
	if err := r.SetEnabled(t.Context(), "holder", true); err != nil {
		t.Fatal(err)
	}
	full := append(servingOf("nfs", nfs), servingOf("smb", smb)...)
	if got := h.holding(); !sameServs(got, full) {
		t.Fatalf("enabled again the sidecar holds %v, want both adapters' sets %v", got, full)
	}

	// Close ends the retries: the Sync that fails after it is called no
	// more, its wait never armed again.
	f.cue.Store(true)
	next := on("127.0.0.1")
	next.MaxVersion = 1
	if err := r.Reconfigure(t.Context(), "nfs", nfs, next); err != nil {
		t.Fatal(err)
	}
	if f := r.Failed(); len(f) != 1 || f[0] != "holder" {
		t.Fatalf("after the post-Close failure Failed() = %q, want holder alone", f)
	}
	r.Close()
	waitsFor(t, "the cancelled retry's wait to clear", func() bool { return clk.pending() == 0 })
	f.cue.Store(false)
	clk.Advance(time.Minute) // past the base, the doubling and the cap
	if f := r.Failed(); len(f) != 1 || f[0] != "holder" {
		t.Errorf("after Close a retry ran against the lifted cue and cleared Failed(): %q", f)
	}
	if got := h.holding(); !sameServs(got, full) {
		t.Errorf("after Close the sidecar holds %v, want the pre-Close set %v: a retry was synced past Close", got, full)
	}
}

// A retry reads whether the sidecar is still failed, and the set it carries,
// behind the sidecar's mutex, which every driven sync holds from its own read
// to its Sync's return: no driven set lands between the retry's read and its
// Sync, so the retry never carries an older set over a newer one (RFC 20
// section 3.5). A retry that reads ahead of the mutex reads with it free.
func TestARetryReadsBehindTheSidecarsMutex(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	h := newHolder("gone", "smb")
	gone := &faulting{Sidecar: h}
	r := sidecar.New(gone)
	held := make(chan bool, 1)
	sidecar.SetRetryRead(r, func() { held <- sidecar.Held(r, "gone") })
	t.Cleanup(r.Close)
	clk := newManualClock()
	sidecar.SetClock(r, clk)
	smb := buildServer(t, b, "smb", on("127.0.0.1"))

	gone.cue.Store(true)
	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the failed sync's retry to arm", func() bool { return clk.pending() > 0 })
	gone.cue.Store(false)
	clk.Advance(time.Second)
	select {
	case ok := <-held:
		if !ok {
			t.Fatal("the retry read the failed answer and its set with the sidecar's mutex free")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the retry never read")
	}
	waitsFor(t, "the retry's sync to succeed", func() bool { return len(r.Failed()) == 0 })
	if got, want := h.holding(), servingOf("smb", smb); !sameServs(got, want) {
		t.Errorf("after the retry the sidecar holds %v, want %v", got, want)
	}
}

// A Start's set builds behind the sidecar's mutex, where its enabled answer
// is read, so a disable that flips while the build runs queues its sync
// behind it and lands its nil last: the disabled sidecar advertises nothing
// (RFC 20 section 3.5). A build ahead of the mutex runs with it free, and a
// disable's nil landing meanwhile is spoken over by the Start's full set.
func TestAStartSyncBuildingPastADisableLandsNone(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	gate := &gating{holder: newHolder("hold", "smb", "nfs"), entered: make(chan struct{}, 1), gate: make(chan struct{})}
	r := sidecar.New(gate)
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		select {
		case <-gate.gate:
		default:
			close(gate.gate)
		}
	})
	smb := buildServer(t, b, "smb", on("127.0.0.1"))
	nfs := buildServer(t, b, "nfs", on("127.0.0.1"))
	if err := r.Start(t.Context(), "smb", smb); err != nil {
		t.Fatal(err)
	}
	// The next Start's set parks in its build.
	gate.armN.Store(2)
	started := make(chan error, 1)
	go func() { started <- r.Start(t.Context(), "nfs", nfs) }()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the driven set's build never parked")
	}
	if !sidecar.Held(r, "hold") {
		t.Fatal("the Start's set builds with the sidecar's mutex free")
	}
	// Whenever the disable flips, its sync lands after the Start's: queued
	// on the parked build, or past its return.
	disabled := make(chan error, 1)
	go func() { disabled <- r.SetEnabled(t.Context(), "hold", false) }()
	close(gate.gate)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatal(err)
	}
	if got := gate.holding(); len(got) != 0 {
		t.Errorf("a Start whose set built past the disable landed %v over the disable's nil, want the enabled answer the sync itself rereads", got)
	}
}

// A disable racing an enable cannot land its nil over the enabled answer: a
// SetEnabled sync builds its set behind the sidecar's mutex with the flag
// re-read at the front, so whichever of the two lands last speaks to the
// flag's last answer (RFC 20 section 3.5). A disable whose flip the enable
// follows re-reads enabled once its sync lands and advertises, where the nil
// a branch builds ahead of the flag would speak the last override and the
// enabled sidecar advertise nothing. A start parked on its build holds the
// mutex, so both SetEnabled calls queue their syncs behind it and land once
// it lets go, the flag already enabled.
func TestADisableRacingAnEnableSpeaksTheEnabledAnswer(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	gate := &gating{holder: newHolder("hold", "smb"), entered: make(chan struct{}, 1), gate: make(chan struct{})}
	spokeNil := &nilNoting{gating: gate, none: make(chan struct{}, 1)}
	r := sidecar.New(spokeNil)
	t.Cleanup(r.Close)
	t.Cleanup(func() {
		select {
		case <-gate.gate:
		default:
			close(gate.gate)
		}
	})
	smb := buildServer(t, b, "smb", on("127.0.0.1"))

	// The start parks in its own build, holding the sidecar's mutex before
	// either SetEnabled lands: both queue their syncs against it.
	gate.armN.Store(2)
	started := make(chan error, 1)
	go func() { started <- r.Start(t.Context(), "smb", smb) }()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the start's build never parked")
	}
	// The disable flips first, the enable past it: the flag settles enabled
	// while both syncs still queue.
	disabled := make(chan error, 1)
	go func() { disabled <- r.SetEnabled(t.Context(), "hold", false) }()
	waitsFor(t, "the disable's flip to land", func() bool { return !sidecar.Enabled(r, "hold") })
	enabled := make(chan error, 1)
	go func() { enabled <- r.SetEnabled(t.Context(), "hold", true) }()
	waitsFor(t, "the enable's flip to land", func() bool { return sidecar.Enabled(r, "hold") })

	// The build lets go and every queued sync lands in order, each reading
	// the enabled answer behind the mutex it waited on.
	close(gate.gate)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatal(err)
	}
	if err := <-enabled; err != nil {
		t.Fatal(err)
	}
	select {
	case <-spokeNil.none:
		t.Fatal("a disable's sync spoke an empty set while the flag reads enabled")
	default:
	}
	full := servingOf("smb", smb)
	if got := gate.holding(); !sameServs(got, full) {
		t.Fatalf("the sidecar holds %v after a disable raced its own enable, want the enabled set %v", got, full)
	}
}

// nilNoting is a gating wrapped so a Sync handed an empty set tells the test
// once: in a race where every set is meant to name what serves, an empty set
// spoken is a disable's build answering ahead of the enabled flag the sync
// re-reads.
type nilNoting struct {
	*gating
	none chan struct{}
}

func (n *nilNoting) Sync(ctx context.Context, serving []adapter.Serving) error {
	if len(serving) == 0 {
		select {
		case n.none <- struct{}{}:
		default:
		}
	}
	return n.gating.Sync(ctx, serving)
}
