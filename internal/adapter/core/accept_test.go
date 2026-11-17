package core_test

import (
	"net"
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

// A socket the connection-state budget cannot charge is closed at once, before
// any byte of it is read: nothing is written back to it and no budget moves
// (R7).
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
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	core.AddBound(srv, &l, inner)
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })

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
	if _, err := client.Read(buf[:]); err == nil {
		t.Error("a socket the budget refused was written to")
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
// when they close (R7).
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
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	core.AddBound(srv, &l, inner)
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })

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
// base, 10 ms, counted on the server's clock (R10, D3).
func TestAReleasedFaultAcceptsAtTenMilliseconds(t *testing.T) {
	srv, fl, clk := faultedServer(t)
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })
	fl.cue.Store(true)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()
	waitsFor(t, "the first failure", func() bool { return fl.failures.Load() > 0 })

	fl.cue.Store(false)
	clk.Advance(9 * time.Millisecond)
	if fl.accepted.Load() != 0 {
		t.Fatal("a connection was accepted before the backoff's 10 ms passed")
	}
	clk.Advance(2 * time.Millisecond)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
}

// A backoff that has grown through failures past five seconds of them is
// capped at 1 s: a connection is accepted within 1 s of the fault lifting
// (R10).
func TestTheBackoffCapsAtOneSecond(t *testing.T) {
	srv, fl, clk := faultedServer(t)
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })
	fl.cue.Store(true)
	queued := dial(t, fl)
	defer func() { _ = queued.Close() }()

	// The backoff doubles per failure until it caps: eight of them, twice
	// over, leaves it at the cap well past 5 s of failures.
	for fl.failures.Load() < 10 {
		before := fl.failures.Load()
		clk.Advance(time.Second)
		waitsFor(t, "the next failure", func() bool { return fl.failures.Load() > before })
		time.Sleep(2 * time.Millisecond) // the loop's next timer arms
	}
	fl.cue.Store(false)
	clk.Advance(999 * time.Millisecond)
	if fl.accepted.Load() != 0 {
		t.Fatal("a connection was accepted before the backoff's 1 s cap passed")
	}
	clk.Advance(2 * time.Millisecond)
	waitsFor(t, "the connection to be accepted", func() bool { return fl.accepted.Load() > 0 })
}

// A listener that fails for 60 s with no connection accepted since is
// reported lost, and not at 59 s; the next accepted connection clears it
// (R1, R10).
func TestALostListenerIsReportedAtSixtySeconds(t *testing.T) {
	srv, fl, clk := faultedServer(t)
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })
	fl.cue.Store(true)
	waitsFor(t, "the first failure", func() bool { return fl.failures.Load() > 0 })

	clk.Advance(59 * time.Second)
	if h := srv.Health(); len(h.Conditions) != 0 {
		t.Errorf("a listener 59 s into its fault is reported %+v", h)
	}
	clk.Advance(time.Second)
	if h := srv.Health(); len(h.Conditions) != 1 || h.Conditions[0] != "adapter_listener_lost" {
		t.Fatalf("a listener 60 s into its fault is reported %+v", h)
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

// faultedServer is a serving server with one bound socket whose listener fails
// Accept on cue, on a clock the test advances (D3).
func faultedServer(t *testing.T) (*core.Server, *failingListener, *manualClock) {
	t.Helper()
	b := budgets()
	adaptertest.CheckLeaks(t, b)
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
	t.Cleanup(func() { _ = inner.Close() })
	fl := &failingListener{Listener: inner}
	core.AddBound(srv, &l, fl)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	return srv, fl, clk
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
