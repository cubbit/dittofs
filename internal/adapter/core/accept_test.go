package core_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// failingListener fails Accept with EMFILE while its cue is set and passes to
// its inner listener once it is not: a listener at its file-descriptor limit,
// on cue. The real socket stays bound the whole time, so a client's connection
// waits in its backlog and is accepted once the fault lifts.
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
	// instant, on the minute's shoulder.
	clk.Advance(clk.nextIn())
	fails := fl.failures.Load()
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
