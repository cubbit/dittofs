package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
)

// watchdog is the node's liveness watchdog as RFC 25 section 3.5 states it
// in miniature: a loop registers itself under Watch, reports its turns and
// idles in order, and unregisters at its close.
type watchdog struct {
	mu    sync.Mutex
	loops map[string][]string
}

func newWatchdog() *watchdog { return &watchdog{loops: map[string][]string{}} }

func (w *watchdog) Watch(loop string) adapter.Loop {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loops[loop] = nil
	return watched{w, loop}
}

// names are the loops registered now, a closed one gone from them.
func (w *watchdog) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Keys(w.loops))
}

// reports are one loop's, in order: a turn per connection it accepted, an
// idle at each accept deadline the clock reached.
func (w *watchdog) reports(name string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.loops[name])
}

type watched struct {
	w    *watchdog
	name string
}

func (l watched) Turn()  { l.w.report(l.name, "turn") }
func (l watched) Idle()  { l.w.report(l.name, "idle") }
func (l watched) Close() { l.w.unregister(l.name) }

func (w *watchdog) report(name, kind string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loops[name] = append(w.loops[name], kind)
}

func (w *watchdog) unregister(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.loops, name)
}

// livenessServer is a loopback server started on the settings' addresses
// under a Watch, its loops' timers armed on the test's clock, which the
// liveness tests advance.
func livenessServer(t *testing.T, b *resources.Budgets, w *watchdog, addrs ...string) (*core.Server, *manualClock) {
	t.Helper()
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b, Watch: w.Watch}, on(addrs...), l)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, clk
}

// Each accept loop seen through a Watch the test declares: one loop per
// bound socket, registered before it serves; a Turn per connection it
// accepts; an Idle at each accept deadline the clock reaches with no
// connection waiting; each loop unregistered by Stop. A Watch the loops
// never reach, a loop that reports nothing, or a watch Stop leaves
// registered fails here (RFC 20 sections 3.4, 3.7 and 6.4).
func TestTheAcceptLoopsReportTheirLivenessToTheWatch(t *testing.T) {
	needIPv6Loopback(t)
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	w := newWatchdog()
	srv, clk := livenessServer(t, b, w, "127.0.0.1", "::1")

	// One loop per bound socket: two addresses, two loops.
	waitsFor(t, "the two loops to register", func() bool { return len(w.names()) == 2 })
	// Each loop's name follows RFC 20: accept <services> on <address>, the
	// services spaced, never bracketed.
	want := map[string]bool{}
	for _, at := range srv.Addrs()[key("test")] {
		want["accept test on "+at.String()] = true
	}
	got := map[string]bool{}
	for _, name := range w.names() {
		got[name] = true
	}
	if !maps.Equal(got, want) {
		t.Errorf("the loops' names are %q, want the services joined by spaces: %q", w.names(), slices.Sorted(maps.Keys(want)))
	}

	// A Turn per accepted connection: one client to each address.
	for _, at := range srv.Addrs()[key("test")] {
		c, err := net.Dial("tcp", at.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
	}
	waitsFor(t, "each loop to report its connection", func() bool {
		for _, name := range w.names() {
			if rs := w.reports(name); len(rs) != 1 || rs[0] != "turn" {
				return false
			}
		}
		return true
	})

	// An Idle at each 10 s of no connection, both loops at the one advance.
	waitsFor(t, "the accept deadlines to arm", func() bool { return clk.pending() > 0 })
	clk.Advance(10 * time.Second)
	waitsFor(t, "each loop to report its idle wake", func() bool {
		for _, name := range w.names() {
			if rs := w.reports(name); len(rs) != 2 || rs[1] != "idle" {
				return false
			}
		}
		return true
	})

	// Each loop unregistered by Stop.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitsFor(t, "the loops to unregister", func() bool { return len(w.names()) == 0 })
}

// With no client for 120 s on the server's clock, each loop reports an Idle
// at each 10 s, never an Accept failure: Health, read after each wake,
// raises no adapter_listener_lost, and a dial after is accepted at once, a
// Turn, with the clock not advanced. A loop blocked in Accept with no
// deadline fails the Idle reports; a wake counted as a failure fails the
// Health reads or the dial, which waits on a backoff the clock never runs
// (RFC 20 sections 3.7 and 6.4).
func TestTheIdleWakesNeverLoseTheListener(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	w := newWatchdog()
	srv, clk := livenessServer(t, b, w, "127.0.0.1")

	waitsFor(t, "the loop to register", func() bool { return len(w.names()) == 1 })
	name := w.names()[0]
	for i := range 12 {
		// The wake's Idle before the next span: a loop that never re-arms
		// stalls here, and a wake treated as a failure reports nothing.
		waitsFor(t, "the accept deadline to arm", func() bool { return clk.pending() > 0 })
		clk.Advance(10 * time.Second)
		waitsFor(t, "the wake's idle", func() bool {
			return len(w.reports(name)) == i+1 && w.reports(name)[i] == "idle"
		})
		if h := srv.Health(); len(h.Conditions) != 0 {
			t.Fatalf("after %d s of idle wakes Health reports %+v, want no condition: a wake counted as an Accept failure", 10*(i+1), h)
		}
	}
	// A dial is accepted with the clock never moved again: the wake left no
	// deadline past and no backoff behind it.
	c, err := net.Dial("tcp", srv.Addrs()[key("test")][0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	waitsFor(t, "the dial to be accepted and reported", func() bool {
		rs := w.reports(name)
		return len(rs) == 13 && rs[12] == "turn"
	})
}

// failingListener fails Accept with EMFILE while its cue is set and passes to
// its inner listener once it is not, refusing a connection already waiting in
// its backlog the same way once the cue is back: a listener at its
// file-descriptor limit, on cue. The real socket stays bound the whole time,
// so a client's connection waits in its backlog and is accepted once the fault
// lifts.
type failingListener struct {
	net.Listener
	cue      atomic.Bool
	failures atomic.Int64
	accepted atomic.Int64
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.cue.Load() {
		l.failures.Add(1)
		return nil, syscall.EMFILE
	}
	c, err := l.Listener.Accept()
	if err == nil && l.cue.Load() {
		// A connection the backlog held while the loop sat in Accept fails
		// it too, so a fault that starts while the loop waits records its
		// first failure rather than hanging on the next dial.
		_ = c.Close()
		l.failures.Add(1)
		return nil, syscall.EMFILE
	}
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
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

// stopNow stops the server on a live context and fails the test if Stop
// reports an error: a stop that waits its loops out, mid-backoff included,
// must return nil.
func stopNow(t *testing.T, s *core.Server) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

// A socket the connection-state budget cannot charge is closed at once, before
// any byte of it is read: nothing is written to it and no budget moves.
func TestAChargedRefusalClosesBeforeItReads(t *testing.T) {
	b := budgets()
	b.Connection = resources.NewBudget("connection", core.ConnCharge()-1)
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	stopNow(t, srv)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	core.AddBound(srv, &l, inner)

	client, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte{0, 0, 0, 8, 9, 0, 0, 2}); err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	// A refused socket ends at once, before its read deadline: an end, the
	// unread bytes of what it wrote reset away, not a silence left open.
	_, rerr := client.Read(buf[:])
	if rerr == nil {
		t.Error("a socket the budget refused was written to")
	} else if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, syscall.ECONNRESET) {
		t.Errorf("a socket the budget refused reads %v, want its end at once", rerr)
	}
	for _, c := range []struct {
		name string
		held int64
	}{
		{"connection", b.Connection.Reserved()},
		{"receive", b.Receive.Reserved()},
	} {
		if c.held != 0 {
			t.Errorf("a refused socket leaves %d bytes on the %s budget", c.held, c.name)
		}
	}
}

// Two connections hold their charges, 128 KiB together, and give them back
// when they close.
func TestTwoConnectionsHoldTheirCharges(t *testing.T) {
	b := budgets()
	b.Connection = resources.NewBudget("connection", 2*core.ConnCharge())
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	stopNow(t, srv)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	core.AddBound(srv, &l, inner)

	clients := make([]net.Conn, 2)
	for i := range clients {
		clients[i], err = net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = clients[i].Close() }()
	}
	waitsFor(t, "two charges to be held", func() bool {
		return b.Connection.Reserved() == 2*core.ConnCharge()
	})
	for _, c := range clients {
		_ = c.Close()
	}
	waitsFor(t, "two charges to be released", func() bool {
		return b.Connection.Reserved() == 0
	})
}

// After one Accept failure the next connection is accepted at the backoff's
// base, 10 ms, counted on the server's clock.
func TestAReleasedFaultAcceptsAtTenMilliseconds(t *testing.T) {
	_, fl, clk := faultedServer(t)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()

	waitsFor(t, "the first backoff to arm", func() bool { return clk.pending() > 0 })
	fl.cue.Store(false)

	// The loop's first failure has armed its 10 ms sleep: the release below is
	// accepted at it, and not before.
	clk.Advance(9 * time.Millisecond)
	if fl.accepted.Load() != 0 {
		t.Fatal("a connection was accepted before the backoff's 10 ms passed")
	}
	clk.Advance(2 * time.Millisecond)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
}

// A second failure doubles the backoff to 20 ms: the connection waits one step
// out.
func TestTheBackoffDoublesToTwentyMilliseconds(t *testing.T) {
	_, fl, clk := faultedServer(t)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()

	// The first backoff is the 10 ms base; firing it doubles the second to
	// 20 ms, which the release times.
	waitsFor(t, "the first backoff to arm", func() bool { return clk.pending() > 0 })
	if d := clk.nextIn(); d != 10*time.Millisecond {
		t.Errorf("the first backoff is %v, want the 10 ms base", d)
	}
	clk.Advance(10 * time.Millisecond)
	waitsFor(t, "the doubled backoff to arm", func() bool { return clk.pending() > 0 })
	if d := clk.nextIn(); d != 20*time.Millisecond {
		t.Errorf("the second backoff is %v, want the doubled 20 ms", d)
	}
	fl.cue.Store(false)

	// The released fault accepts at the doubled 20 ms, and not before.
	clk.Advance(19 * time.Millisecond)
	if fl.accepted.Load() != 0 {
		t.Fatal("a connection was accepted before the doubled 20 ms backoff passed")
	}
	clk.Advance(2 * time.Millisecond)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
}

// A backoff that has grown through 5 s of failures is capped at 1 s: a
// connection queued after the fault lifts is accepted within 1 s of it.
func TestTheBackoffCapsAtOneSecond(t *testing.T) {
	_, fl, clk := faultedServer(t)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()

	// The loop fails through 8 s of steps, past 5 s of them: after them a
	// fresh failure arms one more backoff, the cap's width exactly. Every
	// step is one second: a longer jump would end the run, two cap-widths
	// without a failure reading as failures that stopped.
	for range 8 {
		waitsFor(t, "the backoff to arm", func() bool { return clk.pending() > 0 })
		clk.Advance(time.Second)
	}
	waitsFor(t, "the backoff to arm", func() bool { return clk.pending() > 0 })
	clk.Advance(time.Second) // another step at the cap, the streak still one
	waitsFor(t, "the fresh backoff to arm", func() bool { return clk.pending() > 0 })
	if d := clk.nextIn(); d != time.Second {
		t.Errorf("the backoff of a long streak is %v, want the 1 s cap", d)
	}
	fl.cue.Store(false)

	// A connection is accepted within 1 s of the fault lifting.
	clk.Advance(999 * time.Millisecond)
	if fl.accepted.Load() != 0 {
		t.Fatal("a connection was accepted before the backoff's 1 s cap passed")
	}
	clk.Advance(2 * time.Millisecond)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
}

// A listener that keeps failing for 60 s of them is reported lost, and not at
// 59 s; the next accepted connection clears it. A listener that stopped
// failing is never reported, however long it has not accepted.
func TestALostListenerIsReportedAtSixtySeconds(t *testing.T) {
	srv, fl, clk := faultedServer(t)
	// The loop fails every second, each backoff firing the next failure, so
	// its last failure is always a second old.
	for range 59 {
		waitsFor(t, "the next backoff to arm", func() bool { return clk.pending() > 0 })
		clk.Advance(time.Second)
	}
	waitsFor(t, "the 59th backoff to arm", func() bool { return clk.pending() > 0 })
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a listener 59 s into its fault is reported %+v", h)
	}
	clk.Advance(time.Second)
	waitsFor(t, "the 60th backoff to arm", func() bool { return clk.pending() > 0 })
	if h := srv.Health(); len(h.Conditions) != 1 || h.Conditions[0] != "adapter_listener_lost" {
		t.Errorf("a listener 60 s into its fault is reported %+v", h)
	}

	fl.cue.Store(false)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()
	clk.Advance(2 * time.Second)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a listener that accepted a connection again is still reported %+v", h)
	}
}

// A lost listener no client ever clears still goes quiet by time alone: the
// report holds one second past the streak's last failure and leaves at two
// seconds and a clock tick past it, whether an Accept ever succeeds again
// (RFC 20 section 6.4).
func TestALostListenerWithNoClientClearsByTime(t *testing.T) {
	srv, fl, clk := faultedServer(t)
	// A minute of failures raises it (the reported-and-cleared test proves the
	// minute's own edges).
	for range 60 {
		waitsFor(t, "the backoff to arm", func() bool { return clk.pending() > 0 })
		clk.Advance(time.Second)
	}
	waitsFor(t, "the next backoff to arm", func() bool { return clk.pending() > 0 })
	if h := srv.Health(); len(h.Conditions) != 1 || h.Conditions[0] != "adapter_listener_lost" {
		t.Fatalf("a listener a minute into its fault is reported %+v", h)
	}
	// One failure more, the streak's last: it lands at the armed backoff's own
	// instant, on the minute's shoulder. The count is read before the
	// advance: it releases the backoff, and the failure it records may land
	// before a later read, which would then wait for one that never comes.
	fails := fl.failures.Load()
	clk.Advance(clk.nextIn())
	waitsFor(t, "the last failure to be recorded", func() bool { return fl.failures.Load() > fails })
	fl.cue.Store(false) // the fault ends, and no client dials
	// The capped backoff fires and the loop parks in Accept, the last failure
	// one second behind it: the report holds.
	clk.Advance(time.Second)
	if h := srv.Health(); len(h.Conditions) != 1 || h.Conditions[0] != "adapter_listener_lost" {
		t.Errorf("a listener one second past its last failure is not reported: %+v", h)
	}
	// Two seconds less two ticks past the last failure it still holds: the
	// evidence window is twice the backoff's cap, the RFC's own two seconds.
	clk.Advance(time.Second - 2*time.Millisecond)
	if h := srv.Health(); len(h.Conditions) != 1 || h.Conditions[0] != "adapter_listener_lost" {
		t.Errorf("a listener under two seconds past its last failure is not reported: %+v", h)
	}
	// Two seconds and a clock tick past the last failure, the report is gone.
	clk.Advance(3 * time.Millisecond)
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a listener two seconds past its last failure is still reported: %+v", h)
	}
}

// A listener that failed once and then went quiet is not lost: its last
// failure is evidence too, and evidence 60 s old says the fault ended.
func TestAListenerThatStoppedFailingIsNotLost(t *testing.T) {
	srv, fl, clk := faultedServer(t)

	waitsFor(t, "the first backoff to arm", func() bool { return clk.pending() > 0 })
	fl.cue.Store(false)
	clk.Advance(20 * time.Millisecond) // the backoff runs out; the loop waits in Accept

	clk.Advance(60 * time.Second)
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a listener that stopped failing is reported %+v", h)
	}

	// One stray failure long after the quiet period starts a NEW run, not
	// the minute-old one: the report that would have joined it stays down,
	// and the backoff runs from its base again.
	fl.cue.Store(true)
	fails := fl.failures.Load()
	// A client dials, so the Accept the loop waits in returns a connection the
	// cue refuses: the stray failure records, rather than waiting for a dial
	// the test never makes.
	stray := dial(t, fl)
	defer func() { _ = stray.Close() }()
	waitsFor(t, "the stray failure to be recorded", func() bool { return fl.failures.Load() > fails })
	fl.cue.Store(false)
	waitsFor(t, "the backoff to arm", func() bool { return clk.pending() > 0 })
	// The new run's backoff runs from its base again, not the capped value
	// the old run ended on.
	if got := clk.nextIn(); got > 20*time.Millisecond {
		t.Errorf("the stray failure resumed at its old backoff %v", got)
	}
	clk.Advance(time.Second)
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a stray failure after a quiet run is reported: %+v", h)
	}
}

// stopMidAccept runs Stop as its Accept returns a socket, and hands the
// socket over beside it: a connection accepted in flight, as Stop runs.
type stopMidAccept struct {
	net.Listener
	srv     *core.Server
	once    sync.Once
	stopped chan error
}

func (l *stopMidAccept) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	l.once.Do(func() { go func() { l.stopped <- l.srv.Stop(context.Background()) }() })
	return c, nil
}

// Stop waits a connection its accept loop accepts as Stop runs: the loop ends
// only once it has handed the socket over, so the connection is in the
// registry's snapshot and its charge is back at zero when Stop returns, not
// on a teardown of its own Stop never waited for. A round races one Stop
// against one accept in flight; the rounds give the race every ordering.
func TestStopWaitsAConnectionAcceptedAsItEnds(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	for round := range 8 {
		l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
		srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)

		inner, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		sl := &stopMidAccept{Listener: inner, srv: srv, stopped: make(chan error, 1)}
		core.AddBound(srv, &l, sl)
		queued, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		if err, ok := <-sl.stopped; !ok || err != nil {
			t.Fatalf("round %d: Stop: %v", round, err)
		}
		if held := b.Connection.Reserved(); held != 0 {
			_ = queued.Close()
			t.Fatalf("round %d: Stop returned with %d bytes still charged to a connection it never waited for", round, held)
		}
		_ = queued.Close()
		_ = inner.Close()
	}
}

// A socket its loop hands over as Stop runs is refused at its registration
// and torn down on that loop, OnClose included, so Stop, which waits its
// loops, returns only once that OnClose has (RFC 20 section 3.2). A teardown
// left to a reader of its own runs OnClose outside every wait Stop makes.
func TestStopWaitsARefusedConnectionsOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		entered, gate := make(chan struct{}), make(chan struct{})
		open := sync.OnceFunc(func() { close(gate) })
		defer open() // a failure below still lets the bubble's goroutines end
		l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: withOnClose{testproto.New(adapter.Deps{}).Bare(), func(context.Context) {
			close(entered)
			<-gate
		}}}
		srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)
		nc, peer := net.Pipe()
		defer func() { _ = peer.Close() }()
		core.AddBound(srv, &l, &handedAtClose{conn: nc, closed: make(chan struct{})})
		stopped := make(chan error, 1)
		go func() { stopped <- srv.Stop(context.Background()) }()
		<-entered
		synctest.Wait() // every goroutine blocked: Stop has returned or waits
		select {
		case err := <-stopped:
			t.Fatalf("Stop returned (%v) while a refused connection's OnClose still runs", err)
		default:
		}
		open()
		if err := <-stopped; err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

// handedAtClose is a listener that hands over its one socket only once it is
// closed: Stop closes it after it publishes draining, so the socket reaches
// serveConn at a server no longer serving.
type handedAtClose struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
	handed atomic.Bool
}

func (l *handedAtClose) Accept() (net.Conn, error) {
	<-l.closed
	if l.handed.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	return nil, net.ErrClosed
}

func (l *handedAtClose) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *handedAtClose) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// Stop, entered while a loop sleeps out a backoff it cannot age on the test's
// clock, returns nil once every socket's loop has ended: the sleep ends with
// the socket and the loop follows it.
func TestStopEndsALoopInItsBackoff(t *testing.T) {
	srv, _, clk, loop := faultedLoop(t)

	waitsFor(t, "the first backoff to arm", func() bool { return clk.pending() > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop, mid backoff: %v", err)
	}
	select {
	case <-loop:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop returned with its accept loop still running")
	}
}

// blockedAccept is a listener whose Accept answers only on its release
// channel, and as a closed listener then: a loop parked in Accept, where no
// close it can see moves it.
type blockedAccept struct {
	net.Listener
	release chan struct{}
}

func (l *blockedAccept) Accept() (net.Conn, error) {
	<-l.release
	return nil, net.ErrClosed
}

func (l *blockedAccept) Close() error { return nil }

// Stop waits its accept loops out: with one parked in an Accept no close would
// move, Stop returns only once the test moves it, and then at once.
func TestStopWaitsALoopParkedInAccept(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	stopNow(t, srv)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	bl := &blockedAccept{Listener: inner, release: make(chan struct{})}
	loop := core.AddBound(srv, &l, bl)

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned with its accept loop still parked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(bl.release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-loop:
	case <-time.After(2 * time.Second):
		t.Fatal("the released accept loop did not end")
	}
}

// A Stop parked at an accept loop that outlives its socket keeps the server
// draining, one of the four states (RFC 20 section 3.1): register refuses
// (a socket handed over closes before it reads, and is never served) and
// Health reports the state; the Stop ends once the park releases, closing
// every connection, and a later Stop returns nil.
func TestAStopParkedAtAnAcceptLoopKeepsTheServerDraining(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	conn, _ := servedConn(t, srv, &l)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	bl := &blockedAccept{Listener: inner, release: make(chan struct{})}
	loop := core.AddBound(srv, &l, bl)

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	// The Stop is at its park: the state moved under its first lock, and
	// register refuses what the parked loop's socket hands over.
	waitsFor(t, "the Stop to move the server to draining", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	conn2, client2 := servedConn(t, srv, &l)
	c2 := testproto.NewClient(client2)
	if err := c2.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err == nil {
		if _, err := c2.Recv(); err == nil {
			t.Error("a socket handed to a draining server was served")
		}
	}
	close(bl.release)
	if err := <-stopped; err != nil {
		t.Fatalf("Stop, released from its park: %v", err)
	}
	for _, c := range [2]*core.Conn{conn, conn2} {
		select {
		case <-core.ConnDone(c):
		case <-time.After(2 * time.Second):
			t.Fatal("the Stop left a connection open")
		}
	}
	select {
	case <-loop:
	case <-time.After(2 * time.Second):
		t.Fatal("the released accept loop did not end")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Fatalf("the Stop left the state %v, want stopped", h.State)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("a later Stop: %v", err)
	}
}

// faultedServer is a serving server with one bound socket whose listener is
// already failing Accept on cue, on a clock the test advances: the loop starts
// against the fault, never a step ahead of the cue.
func faultedServer(t *testing.T) (*core.Server, *failingListener, *manualClock) {
	srv, fl, clk, _ := faultedLoop(t)
	return srv, fl, clk
}

// faultedLoop is faultedServer, with the bound socket's loop-end channel.
func faultedLoop(t *testing.T) (*core.Server, *failingListener, *manualClock, <-chan struct{}) {
	t.Helper()
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	stopNow(t, srv)

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	fl := &failingListener{Listener: inner}
	fl.cue.Store(true)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	loop := core.AddBound(srv, &l, fl)
	return srv, fl, clk, loop
}

// dial connects a client to the faulted listener's real socket, which waits in
// its backlog while the fault is on.
func dial(t *testing.T, fl *failingListener) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", fl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// loop1 and loop2 are the two loopback addresses the limit's cases connect
// from: a case needing a second source skips where the host has only 127.0.0.1
// aliased.
var (
	loop1 = netip.MustParseAddr("127.0.0.1")
	loop2 = netip.MustParseAddr("127.0.0.2")
)

// need127_0_0_2 skips t where 127.0.0.2 cannot be a connection's source: the
// probe binds the address before it connects, so the local bind's refusal is
// the host's own answer.
func need127_0_0_2(t *testing.T) {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: loop2.AsSlice()}}
	c, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:0")
	if err == nil {
		_ = c.Close()
		return
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Skipf("127.0.0.2 cannot be a source here: %v", err)
	}
}

// dialFrom connects to at with local as the connection's source, so the
// address the server counts is the test's own.
func dialFrom(t *testing.T, local netip.Addr, at netip.AddrPort) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: local.AsSlice()}}
	c, err := d.DialContext(context.Background(), "tcp", at.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

// refusesFast fails t unless c, a socket refused at accept, sees its end at
// once: no byte of it is read, so nothing is written back (R8).
func refusesFast(t *testing.T, c net.Conn) {
	t.Helper()
	defer func() { _ = c.Close() }()
	var buf [1]byte
	if _, err := c.Read(buf[:]); err == nil {
		t.Error("a socket refused at accept was written to")
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("a socket refused at accept reads %v, want its end at once", err)
	}
}

// limitServer starts a serving loopback server over the bare protocol on the
// settings' addresses, its Budgets and every other field of d the test set.
func limitServer(t *testing.T, snap settings.Settings, d adapter.Deps) (*core.Server, netip.AddrPort) {
	t.Helper()
	srv, _ := protoServer(t, d, snap, testproto.New(adapter.Deps{}).Bare())
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, testAddr(t, srv)
}

// perAddress is the value of the per_address close counter over the test
// protocol's label, and limitCloses the limit's.
func perAddress(t *testing.T, m *metrics.Registry) float64 {
	t.Helper()
	values, _ := scrape(t, m)
	return values[closesKey("per_address")]
}

func limitCloses(t *testing.T, m *metrics.Registry) float64 {
	t.Helper()
	values, _ := scrape(t, m)
	return values[closesKey("limit")]
}

// closesKey is a close counter's label, named by its reason.
func closesKey(reason string) string {
	return "adapter_connection_closes_total{protocol=" + testproto.Name + ",reason=" + reason + "}"
}

// Over loopback with a limit of 8: one address holds two connections and its
// third is closed under per_address before any byte of it is read, and a
// second address still connects (R8, R9). Each refusal's line names its own
// address, so two addresses refused in the same minute log two lines, not
// one and a suppressed count between them (RFC 20 section 10.3).
func TestTheQuarterHoldsOneAddressWhileAnotherConnects(t *testing.T) {
	need127_0_0_2(t)
	m := metrics.New("v", "c")
	logged := &syncLog{}
	b := budgets()
	b.Conns = resources.NewConnLimit(8)
	adaptertest.CheckLeaks(t, b)
	srv, at := limitServer(t, on("127.0.0.1"), adapter.Deps{Metrics: m, Budgets: b, Logger: slog.New(slog.NewJSONHandler(logged, nil))})

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	// 127.0.0.1 holds its quarter of two; its third is closed under
	// per_address, never under limit.
	for range 2 {
		held = append(held, dialFrom(t, loop1, at))
	}
	waitsFor(t, "the first address's two connections", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 2
	})
	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the per_address close to count", func() bool {
		return perAddress(t, m) == 1
	})
	if v := limitCloses(t, m); v != 0 {
		t.Errorf("the quarter's close counts %v under limit too, want per_address alone", v)
	}

	// The second address still connects: it holds two beside the first
	// address's two, and its third, at its own quarter, is closed with a
	// line of its own.
	for range 2 {
		held = append(held, dialFrom(t, loop2, at))
	}
	waitsFor(t, "the second address's two connections", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 4
	})
	refusesFast(t, dialFrom(t, loop2, at))
	waitsFor(t, "the second address's close to count", func() bool {
		return perAddress(t, m) == 2
	})

	// Two addresses refused in one minute, one line each, each naming its
	// own address: a line rate-limited per reason alone would hold the
	// second one back.
	waitsFor(t, "the two addresses' lines to land", func() bool { return len(logged.lines()) == 2 })
	clients := []string{}
	for _, ln := range logged.lines() {
		var line struct {
			Level, Reason, Client string
			Suppressed            int64
		}
		if json.Unmarshal([]byte(ln), &line) != nil || line.Level != "WARN" || line.Reason != "per_address" || line.Suppressed != 0 {
			t.Fatalf("an address's refused close logged '%s', want a warning under per_address carrying none held back", ln)
		}
		clients = append(clients, line.Client)
	}
	slices.Sort(clients)
	if len(clients) != 2 || clients[0] != "127.0.0.1" || clients[1] != "127.0.0.2" {
		t.Errorf("the two lines name %v, want one line per address", clients)
	}
}

// The per_address close's line names the address its quarter refused, at most
// once a minute: two refused connections over the first address log one line
// carrying the address, and once the server's clock passes the minute the
// next refusal logs a second, carrying the count it held back (R8; RFC 25
// section 5.2; D3). A line per close, one that names no address, or one that
// drops the held-back count, fails here.
func TestTheQuarterCloseLineNamesTheAddressOnceAMinute(t *testing.T) {
	logged := &syncLog{}
	m := metrics.New("v", "c")
	b := budgets()
	b.Conns = resources.NewConnLimit(8)
	adaptertest.CheckLeaks(t, b)
	snap := on("127.0.0.1")
	snap.HandshakeTimeout = 0 // the connections held past the minute stay: no deadline closes them
	srv, _ := protoServer(t, adapter.Deps{Metrics: m, Budgets: b, Logger: slog.New(slog.NewJSONHandler(logged, nil))}, snap, testproto.New(adapter.Deps{}).Bare())
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	at := testAddr(t, srv)

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for range 2 {
		held = append(held, dialFrom(t, loop1, at))
	}
	waitsFor(t, "the address's two connections", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 2
	})
	// Wait on the line, not the counter: the second close holds its line
	// back, which the count alone cannot see, and the line is written by
	// the server's own goroutine, in flight past the count.
	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the first refused close's line", func() bool { return len(logged.lines()) == 1 })
	var first struct {
		Level, Reason, Client string
		Suppressed            int64
	}
	if json.Unmarshal([]byte(logged.lines()[0]), &first) != nil || first.Level != "WARN" || first.Reason != "per_address" || first.Client != "127.0.0.1" || first.Suppressed != 0 {
		t.Fatalf("the first refused close logged '%s', want a warning under per_address naming 127.0.0.1 carrying none held back", logged.lines()[0])
	}
	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the close to count unheld", func() bool { return perAddress(t, m) == 2 })

	logged.reset()
	clk.Advance(61 * time.Second)
	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the second line, past the minute", func() bool {
		return len(logged.lines()) == 1 && perAddress(t, m) == 3
	})
	var last struct {
		Level, Reason, Client string
		Suppressed            int64
	}
	if json.Unmarshal([]byte(logged.lines()[0]), &last) != nil || last.Level != "WARN" || last.Reason != "per_address" || last.Client != "127.0.0.1" || last.Suppressed != 1 {
		t.Fatalf("the first close past the minute logged '%s', want a warning under per_address naming the address carrying one held back", logged.lines()[0])
	}
	if v := limitCloses(t, m); v != 0 {
		t.Errorf("the quarter's closes count %v under limit too, want per_address alone", v)
	}
}

// With a limit of 1: a connection from 127.0.0.1 holds it, and one from
// 127.0.0.2, below its own quarter, is closed under limit; once the first
// closes the second address connects, its count given back at the teardown
// (R60, D2). Two sources are needed because at 2 the quarter is 1, so every
// refusal from an address holding one would be the quarter's too; here the
// second address holds none, so the limit's refusal is its own. A count the
// teardown never releases fails the last dial, which CheckLeaks cannot read
// (G13).
func TestALimitOfOneClosesASecondAddressUntilTheFirstCloses(t *testing.T) {
	need127_0_0_2(t)
	m := metrics.New("v", "c")
	b := budgets()
	b.Conns = resources.NewConnLimit(1)
	adaptertest.CheckLeaks(t, b)
	srv, at := limitServer(t, on("127.0.0.1"), adapter.Deps{Metrics: m, Budgets: b})

	first := dialFrom(t, loop1, at)
	defer func() { _ = first.Close() }()
	waitsFor(t, "the first address's connection", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 1
	})
	refusesFast(t, dialFrom(t, loop2, at))
	waitsFor(t, "the limit's close to count", func() bool { return limitCloses(t, m) == 1 })
	values, _ := scrape(t, m)
	if v := values[closesKey("per_address")]; v != 0 {
		t.Errorf("a socket below its quarter counts %v under per_address, want limit alone", v)
	}

	_ = first.Close()
	waitsFor(t, "the held connection to leave", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 0
	})
	second := dialFrom(t, loop2, at)
	defer func() { _ = second.Close() }()
	waitsFor(t, "the second address to connect once the first closed", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 1
	})
}

// With the connection-state budget one byte short of a connection's charge
// and a limit of 2: a dial from 127.0.0.1 and one from 127.0.0.2 are each
// closed under pool; once the budget has room, one from each address is
// admitted, and a third is refused at the limit the two now hold: the
// refused accepts used no slot, and the count was back at zero (R60). A slot
// kept by a refused accept fails the second dial of this test, under which
// connection_limit such refusals shut every adapter of the node to new
// connections.
func TestAChargeRefusedAfterAdmissionGivesItsSlotBack(t *testing.T) {
	need127_0_0_2(t)
	m := metrics.New("v", "c")
	b := budgets()
	b.Conns = resources.NewConnLimit(2)
	b.Connection = resources.NewBudget("connection", 4*core.ConnCharge())
	adaptertest.CheckLeaks(t, b)
	srv, at := limitServer(t, on("127.0.0.1"), adapter.Deps{Metrics: m, Budgets: b})
	// The bytes the budget has left are one short of a connection's charge.
	block, err := b.Connection.Reserve(3*core.ConnCharge() + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Release()

	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the first refused accept's close", func() bool { return poolCloses(t, m) == 1 })
	refusesFast(t, dialFrom(t, loop2, at))
	waitsFor(t, "the second refused accept's close", func() bool { return poolCloses(t, m) == 2 })
	if v := limitCloses(t, m); v != 0 {
		t.Errorf("a socket the pool refused counts %v under limit, want pool alone", v)
	}
	if v := perAddress(t, m); v != 0 {
		t.Errorf("a socket the pool refused counts %v under per_address, want pool alone", v)
	}

	block.Release()
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for _, local := range []netip.Addr{loop1, loop2} {
		held = append(held, dialFrom(t, local, at))
	}
	waitsFor(t, "one connection from each address", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 2
	})
	// The admitted two are the limit exactly: a third dial, from either
	// address, is refused under limit, the count the refused accepts left
	// behind would have spent.
	refusesFast(t, dialFrom(t, loop1, at))
	waitsFor(t, "the third dial's refusal at the full limit", func() bool { return limitCloses(t, m) == 1 })

	// Once the two close, the count is back at zero: a further connection is
	// admitted, which a slot either teardown kept would refuse.
	for _, c := range held {
		_ = c.Close()
	}
	held = held[:0]
	waitsFor(t, "the two connections to leave", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 0
	})
	held = append(held, dialFrom(t, loop1, at))
	waitsFor(t, "a connection after the two closed", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 1
	})
}

// poolCloses is the pool close counter over the test protocol's label.
func poolCloses(t *testing.T, m *metrics.Registry) float64 {
	t.Helper()
	values, _ := scrape(t, m)
	return values[closesKey("pool")]
}

// Two servers over one Budgets share the count: an address holding two
// connections on one is refused on the other (R60). A count per server,
// under which NFS's and SMB's connections would each get the whole limit,
// fails here.
func TestTwoServersShareOneConnectionCount(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	b.Conns = resources.NewConnLimit(8)
	adaptertest.CheckLeaks(t, b)
	d := adapter.Deps{Metrics: m, Budgets: b}
	srv1, at1 := limitServer(t, freePort(), d)
	srv2, at2 := limitServer(t, freePort(), d)

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for range 2 {
		held = append(held, dialFrom(t, loop1, at1))
	}
	waitsFor(t, "the two connections on the first server", func() bool {
		n := 0
		for range srv1.Connections() {
			n++
		}
		return n == 2
	})
	refusesFast(t, dialFrom(t, loop1, at2))
	waitsFor(t, "the second server's refusal to count", func() bool { return perAddress(t, m) == 1 })
	if v := limitCloses(t, m); v != 0 {
		t.Errorf("the second server's refusal counts %v under limit, want per_address alone", v)
	}
	if n := len(slices.Collect(srv2.Connections())); n != 0 {
		t.Errorf("the second server holds %d connections, want it to have refused the address's third", n)
	}
}

// freePort is the settings of a loopback server on a port the system picks,
// so two servers of one test bind apart.
func freePort() settings.Settings {
	s := on("127.0.0.1")
	s.Port = 0
	return s
}
