package core_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
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
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// The drain, Stop's serving half (RFC 20 sections 3.1 and 3.4): the listeners
// close first so no dial completes while draining, every pipeline is told the
// server drains, and Stop returns once nothing runs or the drain deadline
// ends the waiting, the earlier of its context's deadline and drain_timeout.
// The loopback tests move the server's clock to the deadline; the two
// deadlines test and the slow OnClose run in a synctest bubble on the
// server's real clock, where both deadlines run on the bubble's time. The
// move target's tests drain one connection in a bubble too, where a reply
// never sent shows once the bubble settles.

// drainSnap is testproto's shape with drain_timeout at 200 ms, the deadline
// every test here reaches: either the test advances the clock to it or the
// bubble's time runs out at it.
func drainSnap(addrs ...string) testproto.Settings {
	s := on(addrs...)
	s.DrainTimeout = 200 * time.Millisecond
	return s
}

// capturing is a protocol that keeps every connection it is handed, so a
// loopback test reaches the connections whose pipelines the drain counts.
type capturing struct {
	core.Protocol
	mu    sync.Mutex
	conns []*core.Conn
}

func newCapturing(p core.Protocol) *capturing { return &capturing{Protocol: p} }

func (p *capturing) NewConn(c *core.Conn) core.Stages {
	// NewServer's own check wires a nil connection once, at configured (the
	// listener's NewConn(nil) it probes); a capture is an accepted connection.
	if c != nil {
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.mu.Unlock()
	}
	return p.Protocol.NewConn(c)
}

// list is the connections handed over so far, one per accepted socket.
func (p *capturing) list() []*core.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

// pipelineRuns is what runs on a connection's pipeline, 0 where the
// connection was captured before its pipeline was built: at accept the
// protocol wires before the pipeline does.
func pipelineRuns(c *core.Conn) int {
	if p := core.ConnPipeline(c); p != nil {
		return core.Runs(p)
	}
	return 0
}

// registered waits until the registry holds every connection the protocol
// captured: a capture happens at accept, before the pipeline is built, and
// the registry only after, under the server's mutex, so a read of the
// pipeline past this wait is ordered behind its build.
func registered(t *testing.T, srv *core.Server, p *capturing) {
	t.Helper()
	waitsFor(t, "the captured connections to be registered", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n >= len(p.list())
	})
}

// theConn waits for the only connection the server ever accepts.
func theConn(t *testing.T, p *capturing) *core.Conn {
	t.Helper()
	waitsFor(t, "the connection to be handed over", func() bool {
		return len(p.list()) > 0
	})
	return p.list()[0]
}

// drainServer is a loopback server on testproto's bare protocol under snap,
// started on the settings' addresses, its clock the test's manual one. An
// OnClose the test declares rides on every connection it accepts; the server's
// own teardown still runs the pipeline's waits around it.
func drainServer(t *testing.T, b *resources.Budgets, snap testproto.Settings, onClose func(context.Context)) (*core.Server, *manualClock, *capturing) {
	t.Helper()
	p := newCapturing(testproto.New(adapter.Deps{}).Bare())
	if onClose != nil {
		p.Protocol = withOnClose{p.Protocol, onClose}
	}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, clk, p
}

// bubbleServer is a server serving on no address, for a pipe test in a
// synctest bubble: Stop runs on the same bubble time its deadlines do.
func bubbleServer(t *testing.T, b *resources.Budgets, snap testproto.Settings, onClose func(context.Context)) (*core.Server, core.Listener) {
	t.Helper()
	p := core.Protocol(testproto.New(adapter.Deps{}).Bare())
	if onClose != nil {
		p = withOnClose{p, onClose}
	}
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	return srv, l
}

// testAddr is the server's one bound address.
func testAddr(t *testing.T, srv *core.Server) netip.AddrPort {
	t.Helper()
	at := srv.Addrs()[key("test")]
	if len(at) != 1 {
		t.Fatalf("Addrs %v, want one address", at)
	}
	return at[0]
}

// clientAt dials at and wraps the client's end, under a ten-second deadline:
// a server that never answers fails the read, not the test.
func clientAt(t *testing.T, at netip.AddrPort) (*testproto.Client, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", at.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return testproto.NewClient(conn), conn
}

// holdsRoom fills the reply budget from a pool over b.Reply, holding n
// small-class charges, and returns what frees them: a request sent while the
// room is held waits for its reply reservation, counted as running, and what
// runs is what the drain waits out (RFC 20 sections 3.1 and 6.3).
func holdsRoom(t *testing.T, b *resources.Budgets, n int) (free func()) {
	t.Helper()
	pool := resources.NewBufPool(b.Reply)
	held := make([]*resources.Buf, 0, n)
	for range n {
		buf, err := pool.Get(context.Background(), netip.Addr{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, buf)
	}
	return func() {
		for _, h := range held {
			h.Release()
		}
	}
}

// An Echo, its role release, waiting for its reply reservation as Stop begins
// is answered once the test frees room before the deadline, and Stop then
// returns on the quiet the answer left, reaching for no deadline: the clock
// never advances. The connection's OnClose gate holds the teardown until the
// reply was read, so the answer is proven sent before Stop returns, not
// queued and later dropped by the writer's stop. A drain that closes
// connections at once fails here: the Echo would die with the client's read.
// An Echo of the none role would be refused at the same moment, as the role
// test tells.
func TestStopAnswersAnEchoThatFreesItsReplyRoomBeforeTheDeadline(t *testing.T) {
	b := budgets()
	// Room enough for one reply: the Echo waits until the test frees it.
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	gate := make(chan struct{})
	// The teardown waits for the reply's read, and whatever way the test
	// ends, success or failure, the gate opens so no cleanup is stranded
	// behind it.
	var open sync.Once
	defer open.Do(func() { close(gate) })
	srv, _, p := drainServer(t, b, drainSnap("127.0.0.1"), func(context.Context) { <-gate })
	at := testAddr(t, srv)
	c, _ := clientAt(t, at)
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	free()
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 7 || rs[0].Status != testproto.StatusOK {
		t.Fatalf("the Echo freed before the deadline is answered %+v (%v)", rs, err)
	}
	// The listeners closed at Stop's start: no dial completes while draining.
	dialRefused(t, at)
	open.Do(func() { close(gate) })
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain's quiet never ended it")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
}

// With room never freed and the drain deadline at 200 ms on the server's
// clock, Stop returns once the clock reaches the deadline, the parked Echo
// gets no reply, its connection is closed and every budget it charged is at
// zero. A drain that reads its deadline from nowhere never returns; a close
// that leaves the request running hangs Stop at the teardown; an answer sent
// for the abandoned Echo reaches the client's read (RFC 20 sections 3.1 and
// 3.4).
func TestTheDrainDeadlineAbandonsAnEchoStillWaiting(t *testing.T) {
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	srv, clk, p := drainServer(t, b, drainSnap("127.0.0.1"), nil)
	at := testAddr(t, srv)
	c, nc := clientAt(t, at)
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	clk.Advance(201 * time.Millisecond)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop at the drain deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain held no deadline")
	}
	var buf [64]byte
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
	if n, rerr := nc.Read(buf[:]); n > 0 {
		t.Fatalf("the abandoned Echo was answered with %d bytes at the deadline", n)
	} else if rerr == nil || (!errors.Is(rerr, io.EOF) && !errors.Is(rerr, syscall.ECONNRESET)) {
		t.Fatalf("the closed connection's read answers %d, %v; want its end, no reply of it", n, rerr)
	}
	free() // the test's held room, kept to the end and given back after
	if r := b.Receive.Reserved(); r != 0 {
		t.Errorf("the closed connection's receive charge is still %d bytes", r)
	}
	if r := b.Connection.Reserved(); r != 0 {
		t.Errorf("the closed connection's state charge is still %d bytes", r)
	}
}

// parkedAccept is a listener whose accept, once Stop closes the listener,
// parks until the test lets it go, holding the accept loop's end - and with
// it Stop's second lock - open past the moment a reader of draining lands in.
type parkedAccept struct {
	net.Listener
	gate   chan struct{}
	closed atomic.Bool
}

func (a *parkedAccept) Accept() (net.Conn, error) {
	nc, err := a.Listener.Accept()
	if a.closed.Load() {
		<-a.gate
	}
	return nc, err
}

func (a *parkedAccept) Close() error {
	a.closed.Store(true)
	return a.Listener.Close()
}

// The accept loop's end is parked in its closed listener's accept until the
// test lets it go, so the span between draining's publication and the loop's
// end is held open: a reader that observes draining the moment the state
// publishes, advances the server's clock past the drain deadline, and only
// then lets the loop go, still sees Stop return at the deadline. A deadline
// taken only past the loop's end reads the advanced clock, arms one that
// never moves again, and Stop never returns (RFC 20 sections 3.1 and 3.4).
func TestTheDrainDeadlineIsTakenWhereDrainingIsPublished(t *testing.T) {
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	listen := func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &parkedAccept{Listener: ln, gate: gate}, nil
	}
	p := newCapturing(testproto.New(adapter.Deps{}).Bare())
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, drainSnap("127.0.0.1"), core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	core.SetListen(srv, listen)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	at := testAddr(t, srv)
	c, nc := clientAt(t, at)
	conn := theConn(t, p)
	registered(t, srv, p)
	free := sync.OnceFunc(holdsRoom(t, b, 1))
	// Registered past stopAtEnd, so it runs before that Stop: a failure below
	// cannot leave the running Stop parked on the gate, the room or the clock.
	t.Cleanup(func() { open(); free(); clk.Advance(time.Second) })
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	clk.Advance(201 * time.Millisecond)
	open()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop at the drain deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain held no deadline")
	}
	var buf [64]byte
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
	if n, rerr := nc.Read(buf[:]); n > 0 {
		t.Fatalf("the abandoned Echo was answered with %d bytes at the deadline", n)
	} else if rerr == nil || (!errors.Is(rerr, io.EOF) && !errors.Is(rerr, syscall.ECONNRESET)) {
		t.Fatalf("the closed connection's read answers %d, %v; want its end, no reply of it", n, rerr)
	}
	free() // the test's held room, kept to the end and given back after
	if r := b.Receive.Reserved(); r != 0 {
		t.Errorf("the closed connection's receive charge is still %d bytes", r)
	}
	if r := b.Connection.Reserved(); r != 0 {
		t.Errorf("the closed connection's state charge is still %d bytes", r)
	}
}

// While the drain waits, Health reports draining and not ready, and no dial
// completes: the listeners closed before the first pipeline was told. A drain
// that leaves its listeners accepting fails the dial; one that closes the
// connections at once answers nothing on the connection the finish serves.
func TestWhileDrainingADialIsRefusedAndHealthIsNotReady(t *testing.T) {
	b := budgets()
	// Room for two replies: one Echo holds the drain, and the connection the
	// free ends answers too.
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 16))
	adaptertest.CheckLeaks(t, b)
	srv, _, p := drainServer(t, b, drainSnap("127.0.0.1"), nil)
	at := testAddr(t, srv)
	c, _ := clientAt(t, at)
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	dialRefused(t, at)
	h := srv.Health()
	if h.State != adapter.StateDraining || h.Ready() || len(h.Conditions) != 0 {
		t.Errorf("while draining Health is %+v, want draining, not ready, no condition", h)
	}
	free()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain's quiet never ended it")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
}

// While draining, what a request does to state decides whether it runs: an
// Echo whose role is none, sent once Health names draining and while the
// accept loops are still waited, is refused as the table answers the drain's
// error, which over version 1 is no reply at all, and its connection closes
// without a byte of it; one whose role is release is answered. The Echo that
// holds the drain is itself a release, freed once both are in: whichever of
// the two the room reaches, the none is refused and the release answered, so
// the room's race costs the test nothing. The listener's closed accept holds
// its loop past the publication, so the none lands strictly inside the window
// a drain told only past the accept loops' end serves it in: told with the
// publication, the refusal meets it where it arrives and the none gets no
// reply; told late, it answers and the none Echo reads its answer back.
func TestWhileDrainingARequestIsServedByRole(t *testing.T) {
	b := budgets()
	// Room enough for the two Echoes the drain holds and answers one of.
	b.Reply = resources.NewWaitBudget("reply", 2*chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	// The listener's accept is parked past the publication until the test
	// lets it go, so the none Echo below arrives while its loop is waited.
	loop := make(chan struct{})
	letLoop := sync.OnceFunc(func() { close(loop) })
	listen := func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &parkedAccept{Listener: ln, gate: loop}, nil
	}
	p := newCapturing(testproto.New(adapter.Deps{}).Bare())
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, drainSnap("127.0.0.1"), core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p})
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	core.SetServerClock(srv, newManualClock())
	core.SetListen(srv, listen)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	at := testAddr(t, srv)
	rel, _ := clientAt(t, at)
	none, _ := clientAt(t, at)
	waitsFor(t, "both connections to be accepted", func() bool {
		return len(p.list()) == 2
	})
	registered(t, srv, p)
	free := sync.OnceFunc(holdsRoom(t, b, 2))
	// Registered past stopAtEnd, so it runs before that Stop: a failure below
	// cannot leave the running Stop parked on the loop or the rooms.
	t.Cleanup(func() { letLoop(); free() })
	if err := rel.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Role: core.RoleRelease, Payload: []byte("r")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the release Echo to wait for its reply room", func() bool {
		for _, c := range p.list() {
			if pipelineRuns(c) == 1 {
				return true
			}
		}
		return false
	})
	// runs is what the two pipelines run among them.
	runs := func() int {
		n := 0
		for _, c := range p.list() {
			n += pipelineRuns(c)
		}
		return n
	}
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	// The none Echo, sent while the accept loops are still waited, has the
	// drain's refusal waiting for it; it reaches the pipeline before the room
	// it shares with the release is freed, so its end is the refusal's, not
	// a record never read. The rooms are freed and both Echoes end before the
	// loop's end is let go, so a drain told only past the accept loops' end
	// cannot refuse the none anymore: its answer sits in the socket, and the
	// assertion reading it back fails against it.
	if err := none.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2, Payload: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "both Echoes to run on the drain", func() bool { return runs() == 2 })
	free()
	waitsFor(t, "both Echoes to end", func() bool { return runs() == 0 })
	if rs, err := rel.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 1 || rs[0].Status != testproto.StatusOK {
		t.Errorf("the release Echo sent under the drain is answered %+v (%v)", rs, err)
	}
	letLoop()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain's quiet never ended it")
	}
	// The none Echo was refused with no reply: the closed connection ends as
	// it was, and nothing it sent washes back as its answer.
	rs, err := none.Recv()
	if len(rs) != 0 || (err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("the none Echo, sent after the drain began, got %+v (%v), want no reply and the connection's end", rs, err)
	}
	if err == nil {
		t.Error("the none Echo's connection still reads: it never closed")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
}

// Two Stop calls that overlap in the drain both return after stopped, and by
// then every OnClose has run: the second parks on the first's end, and an
// Echo holding its reply room keeps every connection out of its teardown
// until the test frees it. A second call that returns before the teardowns
// returns while the room is still held, before any OnClose can have run.
func TestTwoConcurrentStopsReturnOnceEveryOnCloseHasRun(t *testing.T) {
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	fired := make(chan struct{})
	srv, _, p := drainServer(t, b, drainSnap("127.0.0.1"), func(context.Context) { close(fired) })
	at := testAddr(t, srv)
	c, _ := clientAt(t, at)
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return pipelineRuns(conn) == 1
	})
	stops := make(chan error, 2)
	go func() { stops <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool {
		return srv.Health().State == adapter.StateDraining
	})
	// The drain is held, so the first Stop cannot end yet: a Stop that
	// arrives now returns only once it has torn every connection down,
	// OnClose included. Whatever returns inside the window returned early,
	// before anything could have been torn down, and the window is short real
	// time: the drain itself, held by the room the test frees after it, keeps
	// the rest deterministic.
	early := make(chan error, 1)
	go func() { early <- srv.Stop(context.Background()) }()
	select {
	case err := <-early:
		select {
		case <-fired:
		default:
			t.Error("a concurrent Stop returned before OnClose had run")
		}
		if err != nil {
			t.Errorf("a concurrent Stop that returned early: %v", err)
		} else {
			t.Error("a concurrent Stop returned while the drain held, before the first had stopped the server")
		}
		free()
		select {
		case err := <-stops:
			if err != nil {
				t.Errorf("the first Stop: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the first Stop never returned")
		}
	case <-time.After(300 * time.Millisecond):
		free()
		select {
		case err := <-early:
			if err != nil {
				t.Fatalf("the parked Stop at the first's end: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the parked Stop never returned")
		}
		if err := <-stops; err != nil {
			t.Fatalf("the first Stop: %v", err)
		}
		if h := srv.Health(); h.State != adapter.StateStopped {
			t.Errorf("after both Stops the server is %s, want stopped", h.State)
		}
		// Every OnClose has run: the teardown closed it before Stop
		// returned. The fired check inside the early case above is the
		// other side of the same proof.
		select {
		case <-fired:
		default:
			t.Error("no OnClose had run at the stopped server")
		}
	}
}

// The server is not stopped while a connection's OnClose still runs (RFC 20
// section 3.1): with the OnClose held, Health reads draining, and stopped once
// it returns and Stop with it. A Stop that names the server stopped before it
// closes its connections reads stopped while the OnClose is held.
func TestTheServerIsNotStoppedWhileAnOnCloseStillRuns(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	entered := make(chan struct{})
	gate := make(chan struct{})
	srv, _, p := drainServer(t, b, drainSnap("127.0.0.1"), func(context.Context) {
		close(entered)
		<-gate
	})
	// Opened before the cleanup's Stop runs, so a failure below cannot park
	// it on the held OnClose.
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open)
	clientAt(t, testAddr(t, srv))
	theConn(t, p)
	registered(t, srv, p)
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never ran the connection's OnClose")
	}
	if h := srv.Health(); h.State != adapter.StateDraining {
		t.Fatalf("while an OnClose runs the server is %s, want draining", h.State)
	}
	open()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned after the OnClose did")
	}
	if h := srv.Health(); h.State != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", h.State)
	}
}

// In a pipe-test bubble on the server's real clock, both deadlines run on the
// bubble's time: with drain_timeout at 200 ms and Stop's context ending at
// 100 ms, the drain ends at the earlier one and the echo parked on its reply
// room is abandoned there. A drain that reads its deadline from one of them
// alone ends at the other here: the context's, gone as soon as Stop's own
// wait reaches it late.
func TestTheDrainEndsAtTheEarlierOfTheTwoDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
		adaptertest.CheckLeaks(t, b)
		srv, l := bubbleServer(t, b, drainSnap(), nil)
		conn, peer := servedConn(t, srv, &l)
		defer func() { _ = peer.Close() }()
		c := testproto.NewClient(peer)
		free := holdsRoom(t, b, 1)
		defer free()
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the reader back in its pipe, the Echo on its room
		if got := pipelineRuns(conn); got != 1 {
			t.Fatalf("the Echo runs %d, want it parked on its reply room", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		stopped := make(chan error, 1)
		go func() { stopped <- srv.Stop(ctx) }()
		if err := <-stopped; err != nil {
			t.Fatalf("Stop at the earlier deadline: %v", err)
		}
		elapsed := time.Since(start)
		if elapsed < 100*time.Millisecond {
			t.Fatalf("the drain ended %s in, before either deadline", elapsed)
		}
		if elapsed >= 200*time.Millisecond {
			t.Fatalf("the drain waited the whole drain_timeout (%s in, %s under it); want Stop's earlier context deadline", elapsed, 200*time.Millisecond)
		}
		if h := srv.Health(); h.State != adapter.StateStopped {
			t.Errorf("after Stop the server is %s, want stopped", h.State)
		}
	})
}

// A request that answers while the server drains has its reply written before
// Stop closes its connection (RFC 20 section 3.1): in a pipe-test bubble whose
// client reads nothing yet, an Echo freed from its reply room answers into a
// writer the client holds, and Stop has not returned; once the client reads,
// the reply is the Echo's and Stop returns. A drain that reads the connection
// quiet once the reply is queued closes it with the reply unsent.
func TestADrainSendsTheReplyOfARequestItLetFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
		adaptertest.CheckLeaks(t, b)
		srv, l := bubbleServer(t, b, drainSnap(), nil)
		conn, peer := servedConn(t, srv, &l)
		defer func() { _ = peer.Close() }() // a failure below still ends the write
		c := testproto.NewClient(peer)
		free := sync.OnceFunc(holdsRoom(t, b, 1))
		defer free()
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Role: core.RoleRelease, Payload: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the Echo on its reply room
		if got := pipelineRuns(conn); got != 1 {
			t.Fatalf("the Echo runs %d, want it parked on its reply room", got)
		}
		stopped := make(chan error, 1)
		go func() { stopped <- srv.Stop(context.Background()) }()
		synctest.Wait() // the drain waits on the Echo
		free()
		synctest.Wait() // the Echo answered into a writer the client holds
		select {
		case err := <-stopped:
			t.Fatalf("Stop returned (%v) with the answered Echo's reply unsent", err)
		default:
		}
		rs, err := c.Recv()
		if err != nil || len(rs) != 1 || rs[0].ID != 7 || rs[0].Status != 0 {
			t.Fatalf("the answered Echo's reply: %+v (%v)", rs, err)
		}
		if err := <-stopped; err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

// Stop waits for OnClose but no longer than its 30 s grace: with a connection
// whose OnClose returns only when its context ends, and the drain held to its
// 200 ms deadline by an Echo it abandons, Stop returns at the grace's end, 30 s
// past the drain deadline and none later. A Stop that returns before the
// teardowns, or a teardown that holds OnClose past its grace, misses the
// window; nothing is waited for that a protocol's registry handed to
// Deps.Retry, whose calls never reach Stop at all.
func TestStopWaitsOutASlowOnClosePastItsGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
		adaptertest.CheckLeaks(t, b)
		srv, l := bubbleServer(t, b, drainSnap(), func(ctx context.Context) { <-ctx.Done() })
		conn, peer := servedConn(t, srv, &l)
		defer func() { _ = peer.Close() }()
		c := testproto.NewClient(peer)
		free := holdsRoom(t, b, 1)
		defer free()
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 7, Payload: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the reader back in its pipe, the Echo on its room
		if got := pipelineRuns(conn); got != 1 {
			t.Fatalf("the Echo runs %d, want it parked on its reply room", got)
		}
		start := time.Now()
		stopped := make(chan error, 1)
		go func() { stopped <- srv.Stop(context.Background()) }()
		if err := <-stopped; err != nil {
			t.Fatalf("Stop behind a slow OnClose: %v", err)
		}
		grace := 30*time.Second + 200*time.Millisecond
		if elapsed := time.Since(start); elapsed < grace || elapsed >= grace+time.Second {
			t.Fatalf("Stop returned %s from its start, want the 30 s grace past the 200 ms drain deadline (%s to %s)", elapsed, grace, grace+time.Second)
		}
		if h := srv.Health(); h.State != adapter.StateStopped {
			t.Errorf("after Stop the server is %s, want stopped", h.State)
		}
	})
}

// moving is the filesystem over memory with its one share moving to gateway,
// a fault wrapper: MoveTarget names gateway for every handle the filesystem's
// StatFS places in share, and passes any other to the filesystem, which
// names none; every other call passes through.
type moving struct {
	vfs.Service
	share vfs.ShareID
}

// gateway is the node the moving share moves to.
var gateway = netip.MustParseAddr("192.0.2.7")

func (m moving) MoveTarget(ctx context.Context, c vfs.Call, h vfs.Handle) (netip.Addr, error) {
	if info, err := m.StatFS(ctx, c, h); err == nil && info.Share == m.share {
		return gateway, nil
	}
	return m.Service.MoveTarget(ctx, c, h)
}

// drained is a connection a server on testproto serves over faults over
// moving, in a synctest bubble, set up to session 7 and tree 1 and then told
// to drain. A non-nil outer replaces the error table of every table the
// router returns, a compound row's members keeping their own, and none, where
// not zero, is an operation of those tables the drain refuses.
type drained struct {
	t            *testing.T
	svc          *memory.Service
	faults       *vfstest.FaultService
	root, pseudo vfs.Handle // the share's root; the pseudo-root, in no share
	c            *testproto.Client
	peer         net.Conn
}

func newDrained(t *testing.T, outer *core.ErrorTable, none uint32) *drained {
	t.Helper()
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	svc := memory.New(t)
	ctx, admin := t.Context(), svc.Admin()
	root, _, err := svc.Root(ctx, admin, vfs.ShareRef{Name: svc.Store.Share.Name})
	if err != nil {
		t.Fatal(err)
	}
	pseudo, _, err := svc.Root(ctx, admin, vfs.ShareRef{})
	if err != nil {
		t.Fatal(err)
	}
	faults := vfstest.Faults(moving{Service: svc, share: svc.Store.Share.ID})
	d := adapter.Deps{Budgets: b, VFS: faults}
	var p core.Protocol = testproto.New(d).WithCall(admin)
	if outer != nil {
		r := retable(p, func(op *core.Op, member bool) {
			if !member && op.Code == none {
				op.Role = core.RoleNone
			}
		})
		for _, tb := range r.Tables() {
			tb.Errors = outer
		}
		p = r
	}
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p}
	srv, err := core.NewServer(d, drainSnap(), l)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	core.Serving(srv)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
		{Op: testproto.OpTree, Version: 1, ID: 3, Session: 7, Tree: 1},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusOK {
			t.Fatalf("setting up the connection: %+v (%v)", rs, err)
		}
	}
	core.ConnPipeline(conn).Drain()
	return &drained{t: t, svc: svc, faults: faults, root: root, pseudo: pseudo, c: c, peer: peer}
}

// compound sends a version 1 compound of members and lets the bubble settle.
func (d *drained) compound(members ...testproto.Msg) {
	d.t.Helper()
	if err := d.c.Send(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 4, Session: 7, Tree: 1, Members: members}); err != nil {
		d.t.Fatal(err)
	}
	synctest.Wait()
}

// sentNothing reports whether nothing came back, then closes the client's end.
func (d *drained) sentNothing() bool {
	read := start(func() { _, _ = d.c.Recv() })
	synctest.Wait()
	sent := read.returned()
	_ = d.peer.Close()
	<-read.done
	return !sent
}

// made reports whether the share's root holds name.
func (d *drained) made(name string) bool {
	_, _, err := d.svc.Lookup(d.t.Context(), d.svc.Admin(), d.root, []byte(name))
	return err == nil
}

// While draining, the drain's error a compound's refused members get is
// decided once, from every filehandle the compound names: ErrMoved, which
// version 1 answers on Create with its move signal, only when the compound
// names one at least and the service names a target for the share of each,
// and ErrDraining, which answers nothing and runs nothing, otherwise, a
// MoveTarget that fails included (RFC 20 sections 3.1 and 4.7). The moved
// compound runs to its Create, which names no handle of its own, and is
// answered there, the Create not applied. A drain that answers ErrMoved
// without asking, asks about its first or last handle alone, or moves a
// compound naming none answers one of the others, and one that answers
// MoveTarget's error answers the failing one; one that decides per member,
// or refuses every drained compound whole, answers the moved one nothing.
func TestADrainedCompoundMovesOnlyWhenEveryHandleItNamesHasATarget(t *testing.T) {
	for _, c := range []struct {
		name    string
		handles []string // a PutFH of each, before the Create
		fails   bool     // the service's MoveTarget answers an error
		moved   bool
	}{
		{"a handle in the moving share", []string{"share"}, false, true},
		{"the pseudo-root, in no share", []string{"pseudo"}, false, false},
		{"a handle in the share, then the pseudo-root", []string{"share", "pseudo"}, false, false},
		{"the pseudo-root, then a handle in the share", []string{"pseudo", "share"}, false, false},
		{"a handle in the moving share, its MoveTarget failing", []string{"share"}, true, false},
		{"no handle", nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDrained(t, nil, 0)
				if c.fails {
					d.faults.Fail(vfs.OpMoveTarget, vfs.ErrAccess)
				}
				fh := map[string]vfs.Handle{"share": d.root, "pseudo": d.pseudo}
				var members []testproto.Msg
				for _, n := range c.handles {
					members = append(members, testproto.Msg{Op: testproto.OpPutFH, Handle: fh[n]})
				}
				d.compound(append(members, testproto.Msg{Op: testproto.OpCreate, Name: "f"})...)
				if !c.moved {
					if !d.sentNothing() {
						t.Error("the compound was answered, want nothing: the drain's error is ErrDraining")
					}
				} else {
					rs, err := d.c.Recv()
					if err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusOK {
						t.Fatalf("the moved compound is answered %+v (%v)", rs, err)
					}
					hd, res, err := testproto.Results(rs[0].Body)
					if err != nil {
						t.Fatal(err)
					}
					if hd.Status != testproto.StatusMoved || len(res) != 2 || res[0].Status != testproto.StatusOK || res[1].Op != testproto.OpCreate || res[1].Status != testproto.StatusMoved {
						t.Errorf("the moved compound is answered %d with %+v, want the PutFH run and the Create's move signal", hd.Status, res)
					}
				}
				if d.made("f") {
					t.Error("the drained Create made its file")
				}
			})
		})
	}
}

// While draining, a compound any of whose members would be refused with a
// NoReply row answers nothing and runs nothing, though every handle it names
// moves: an Echo the drain refuses after a Create it moves, whose row has no
// move signal, the same where the compound's own table moves every
// operation, and a member that does not decode (RFC 20 section 4.7). Run to
// the first member refused instead, the first is answered the Create's move
// signal and the third the refusal of an operation its table lacks; refused
// whole with the drain's error rather than ErrDraining, the second is
// answered its own table's move signal.
func TestADrainedCompoundWithANoReplyRefusalAnswersNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		rest  []testproto.Msg // after a PutFH of the share's root
		outer *core.ErrorTable
	}{
		{"an Echo the drain refuses after the Create", []testproto.Msg{{Op: testproto.OpCreate, Name: "f"}, {Op: testproto.OpEcho, Role: core.RoleNone, Payload: []byte("e")}}, nil},
		{"the same, the compound's own table moving every operation", []testproto.Msg{{Op: testproto.OpCreate, Name: "f"}, {Op: testproto.OpEcho, Role: core.RoleNone, Payload: []byte("e")}}, movesEverywhere()},
		{"a member that does not decode", []testproto.Msg{{Op: testproto.OpEcho, Role: core.RoleRelease, Payload: []byte("e")}, {Op: 99}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDrained(t, c.outer, 0)
				d.compound(append([]testproto.Msg{{Op: testproto.OpPutFH, Handle: d.root}}, c.rest...)...)
				if !d.sentNothing() {
					t.Error("the compound was answered, want nothing")
				}
				if d.made("f") {
					t.Error("the drained Create made its file")
				}
			})
		})
	}
}

// While draining, a request alone the drain refuses gets ErrMoved, which its
// table answers with the move signal, when the service names a target for the
// share of the handle it names, and ErrDraining, which answers nothing,
// otherwise (RFC 20 section 3.1): a PutFH the drain refuses, its table moving
// every operation. A drain that never asks answers the first nothing; one
// that answers ErrMoved without asking answers the second.
func TestADrainedRequestMovesOnlyWhenItsHandleHasATarget(t *testing.T) {
	for _, c := range []struct {
		name   string
		handle string // "share" or "pseudo"
		moved  bool
	}{
		{"a handle in the moving share", "share", true},
		{"the pseudo-root, in no share", "pseudo", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDrained(t, movesEverywhere(), testproto.OpPutFH)
				fh := map[string]vfs.Handle{"share": d.root, "pseudo": d.pseudo}
				if err := d.c.Send(testproto.Msg{Op: testproto.OpPutFH, Version: 1, ID: 4, Session: 7, Tree: 1, Handle: fh[c.handle]}); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if !c.moved {
					if !d.sentNothing() {
						t.Error("the PutFH was answered, want nothing: the drain's error is ErrDraining")
					}
					return
				}
				if rs, err := d.c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusMoved {
					t.Errorf("the moved PutFH is answered %+v (%v), want the move signal", rs, err)
				}
			})
		})
	}
}

// movesEverywhere answers ErrMoved with the move signal on every operation,
// as a table whose ErrMoved row names none does, and ErrDraining nothing.
func movesEverywhere() *core.ErrorTable {
	return core.NewErrorTable(testproto.StatusFault,
		core.ErrorRow{Err: core.ErrDecode, Status: testproto.StatusDecode},
		core.ErrorRow{Err: core.ErrTooLong, Status: testproto.StatusTooLong},
		core.ErrorRow{Err: core.ErrReplyTooBig, Status: testproto.StatusReplyTooBig},
		core.ErrorRow{Err: core.ErrDraining, NoReply: true},
		core.ErrorRow{Err: core.ErrMoved, Status: testproto.StatusMoved},
	)
}
