package core_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// budgets is a fresh set of the four budgets, wide enough for the tests.
func budgets() *resources.Budgets {
	return &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 4<<20),
		Reply:      resources.NewWaitBudget("reply", 4<<20),
		Connection: resources.NewBudget("connection", 4<<20),
		Hold:       resources.NewBudget("hold", 4<<20),
	}
}

// testServer is a server serving testproto's bare protocol under snap.
func testServer(t *testing.T, d adapter.Deps, snap settings.Settings) (*core.Server, core.Listener) {
	t.Helper()
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(d).Bare()}
	srv, err := core.NewServer(d, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	return srv, l
}

// connPair is a pipe pair handed to the server, and the client's end.
func connPair(t *testing.T, srv *core.Server, l core.Listener) *testproto.Client {
	t.Helper()
	conn, peer := net.Pipe()
	core.ServeConn(srv, conn, &l)
	t.Cleanup(func() { _ = peer.Close() })
	return testproto.NewClient(peer)
}

// servedConn is a pipe pair handed to the server, its connection and the
// client's end.
func servedConn(t *testing.T, srv *core.Server, l *core.Listener) (conn *core.Conn, peer net.Conn) {
	t.Helper()
	nc, peer := net.Pipe()
	conn = core.ServeConn(srv, nc, l)
	t.Cleanup(func() { _ = peer.Close() })
	return conn, peer
}

// An Echo over the bare protocol is answered on its connection.
func TestAnEchoIsAnswered(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	c := connPair(t, srv, l)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	rs, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ID != 1 {
		t.Fatalf("an Echo is answered %+v", rs)
	}
}

// A connection whose stages name no error table closes before it reads.
func TestStagesWithoutErrorsClose(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: errlessProtocol{}}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	conn, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	core.ServeConn(srv, conn, &l)
	// A read of the pipe's far end returns at once: the connection closed
	// before reading anything.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := peer.Read(buf); err == nil {
		t.Fatal("a connection without an error table read a record")
	}
}

// errlessProtocol is a protocol whose stages name no error table.
type errlessProtocol struct{}

func (errlessProtocol) Framer() core.Framer            { return testproto.New(adapter.Deps{}).Framer() }
func (errlessProtocol) NewConn(*core.Conn) core.Stages { return core.Stages{} }

// Stop closes every connection open and returns once each is torn down.
func TestStopClosesConnections(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	var clients []*testproto.Client
	for range 3 {
		clients = append(clients, connPair(t, srv, l))
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, c := range clients {
		_ = c
		_ = i
	}
	// Every connection's teardown ended before Stop returned: the registry is
	// empty and no budget holds a byte.
	for range srv.Connections() {
		t.Error("a connection is still registered after Stop")
	}
	if r := b.Receive.Reserved(); r != 0 {
		t.Errorf("the receive budget still holds %d bytes", r)
	}
}

// A connection whose client closes runs OnClose after everything it ran ended,
// under a context of its own.
func TestCloseRunsOnCloseAfterTheWait(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	seen := make(chan error, 1)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	l.Protocol = withOnClose{testproto.New(adapter.Deps{}).Bare(), func(ctx context.Context) { seen <- ctx.Err() }}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	conn, peer := servedConn(t, srv, &l)
	_ = peer.Close() // the client goes; the reader's next cut fails
	select {
	case err := <-seen:
		if err != nil {
			t.Errorf("OnClose's context is ended: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose did not run")
	}
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown did not end")
	}
	for range srv.Connections() {
		t.Error("a connection is still registered after its close")
	}
}

// withOnClose is a protocol whose stages carry an OnClose the test declares.
type withOnClose struct {
	core.Protocol
	onClose func(context.Context)
}

func (p withOnClose) NewConn(c *core.Conn) core.Stages {
	st := p.Protocol.NewConn(c)
	st.OnClose = p.onClose
	return st
}

// The connection's snapshot is the one it was served with: testproto's
// Negotiate reads its cap through Conn.Settings.
func TestNegotiateReadsTheConnectionsSnapshot(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	// The full protocol, not Bare: Negotiate is answered by the version's own
	// table, and Bare serves version 1 alone.
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, testproto.Settings{MaxVersion: 1}, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	connA, clientA := servedConn(t, srv, &l)
	_ = clientA
	c := testproto.NewClient(clientA)
	if err := c.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: 2, ID: 1}); err != nil {
		t.Fatal(err)
	}
	rs, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || len(rs[0].Body) != 1 || rs[0].Body[0] != 1 {
		t.Fatalf("Negotiate answered %+v, want version 1 in one byte", rs)
	}
	// The server's snapshot moves on, and the connection keeps its own: a second
	// connection of the same server caps at 2, and this one would still cap at
	// 1 were it to negotiate again.
	core.SetServerSettings(srv, testproto.Settings{MaxVersion: 2})
	_, clientB := servedConn(t, srv, &l)
	b2 := testproto.NewClient(clientB)
	if err := b2.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: 2, ID: 1}); err != nil {
		t.Fatal(err)
	}
	rs2, err := b2.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs2) != 1 || len(rs2[0].Body) != 1 || rs2[0].Body[0] != 2 {
		t.Fatalf("the second connection negotiates %+v, want version 2 from the server's new snapshot", rs2)
	}
	// This connection's own snapshot is unchanged, though the server's is not.
	if set, ok := core.ConnSettings(connA).(testproto.Settings); !ok || set.MaxVersion != 1 {
		t.Errorf("the first connection's snapshot is %+v, want its own MaxVersion 1", core.ConnSettings(connA))
	}
}

// Violate closes the connection from its handler and returns; the connection's
// teardown follows.
func TestViolateClosesTheConnection(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	if err := c.Send(testproto.Msg{Op: testproto.OpViolate, Version: 1, ID: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("the connection did not close after Violate")
	}
	for range srv.Connections() {
		t.Error("a connection is still registered after Violate")
	}
}

// A client that closes with an Echo waiting for its reply reservation ends the
// Echo: OnClose runs only once nothing runs, the reply is never sent, and the
// budgets hold nothing.
func TestAClientCloseEndsAPendingEcho(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		// The reply budget holds one small class, so the Echo waits.
		b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
		adaptertest.CheckLeaks(t, b)
		seen := make(chan int, 1)
		var conn *core.Conn
		l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: withOnClose{testproto.New(adapter.Deps{}).Bare(), func(context.Context) {
			seen <- core.Runs(core.ConnPipeline(conn))
		}}}
		srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)
		var client net.Conn
		conn, client = servedConn(t, srv, &l)
		held, err := resources.NewBufPool(b.Reply).Get(context.Background(), netip.Addr{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		c := testproto.NewClient(client)
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the Echo waits in its reply reservation
		_ = client.Close()
		select {
		case runs := <-seen:
			if runs != 0 {
				t.Errorf("OnClose ran with %d requests still running", runs)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OnClose did not run")
		}
		held.Release()
		if r := b.Receive.Reserved(); r != 0 {
			t.Errorf("the receive budget holds %d bytes after the close", r)
		}
		if r := b.Reply.Reserved(); r != 0 {
			t.Errorf("the reply budget holds %d bytes after the close", r)
		}
	})
}

// chargeOfReply is what a reply of n bytes reserves, learned from a pool of its
// own.
func chargeOfReply(t *testing.T, n int) int64 {
	t.Helper()
	b := resources.NewWaitBudget("probe", 1<<30)
	buf, err := resources.NewBufPool(b).Get(context.Background(), netip.Addr{}, n)
	if err != nil {
		t.Fatal(err)
	}
	c := b.Reserved()
	buf.Release()
	return c
}

// OnClose's context is its own, live when it starts and ended thirty seconds
// after the close on the server's clock.
func TestOnClosesContextEndsAfterTheGrace(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	live := make(chan bool, 1)
	ended := make(chan error, 1)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: withOnClose{testproto.New(adapter.Deps{}).Bare(), func(ctx context.Context) {
		live <- ctx.Err() == nil
		<-ctx.Done()
		ended <- ctx.Err()
	}}}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	_ = client.Close()
	select {
	case ok := <-live:
		if !ok {
			t.Fatal("OnClose's context is ended when it starts")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose did not run")
	}
	clk.Advance(29 * time.Second)
	select {
	case err := <-ended:
		t.Fatalf("OnClose's context ended at 29s: %v", err)
	default:
	}
	clk.Advance(time.Second)
	select {
	case err := <-ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("OnClose's context ends with %v, want the deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnClose's context did not end at 30s")
	}
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown did not end")
	}
}

// A client that reads nothing is not read past the writer's bound: its next
// write does not complete, the reader holds no record while it waits, another
// connection is served meanwhile, and both drain once it reads.
func TestASilentClientStopsTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
		client := testproto.NewClient(nil)
		_ = client
		connA, peerA := servedConn(t, srv, &l)
		_ = connA
		a := testproto.NewClient(peerA)
		const sent = 200
		sending := make(chan error, 1)
		go func() {
			for i := range sent {
				if err := a.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1), Payload: []byte("x")}); err != nil {
					sending <- err
					return
				}
			}
			sending <- nil
		}()
		synctest.Wait()
		//// The bound is reached: the sender waits in a write, and the reader,
		//// paused, holds no record.
		select {
		case err := <-sending:
			t.Fatalf("every write completed with the client reading nothing: %v", err)
		default:
		}
		if r := b.Receive.Reserved(); r != 0 {
			t.Errorf("a paused reader holds %d receive bytes", r)
		}
		//// A second connection is served meanwhile.
		_, peerB := servedConn(t, srv, &l)
		bm := testproto.NewClient(peerB)
		if err := bm.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("y")}); err != nil {
			t.Fatal(err)
		}
		rs, err := bm.Recv()
		if err != nil || len(rs) != 1 {
			t.Fatalf("the second connection answered %+v: %v", rs, err)
		}
		//// Reading A drains both: every reply, in order, then the sender ends.
		// Every request is answered exactly once: the replies are not in the
		// requests' order, which a protocol without ordering leaves to xids.
		answered := make(map[uint64]bool, sent)
		for i := range sent {
			rs, err := a.Recv()
			if err != nil || len(rs) != 1 {
				t.Fatalf("reply %d: %+v (%v)", i, rs, err)
			}
			answered[rs[0].ID] = true
		}
		if len(answered) != sent {
			t.Errorf("%d replies for %d requests: some answered twice, some never", len(answered), sent)
		}
		select {
		case err := <-sending:
			if err != nil {
				t.Fatalf("the sender: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the sender never finished after the client read")
		}
	})
}

// A session spans connections: the first connection's close leaves it to the
// second, and the second's ends it.
func TestASessionSurvivesOneConnectionsClose(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	join := func(t *testing.T) (conn *core.Conn, client net.Conn, c *testproto.Client) {
		t.Helper()
		var clientConn net.Conn
		conn, clientConn = servedConn(t, srv, &l)
		c = testproto.NewClient(clientConn)
		for _, m := range []testproto.Msg{
			{Op: testproto.OpNegotiate, Version: 1, ID: 1},
			{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
		} {
			if err := c.Send(m); err != nil {
				t.Fatal(err)
			}
			if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
				t.Fatalf("setting the session up: %+v (%v)", rs, err)
			}
		}
		return conn, clientConn, c
	}
	connA, clientA, _ := join(t)
	connB, clientB, bClient := join(t)
	// The first connection goes; the session stays with the second.
	_ = clientA.Close()
	select {
	case <-core.ConnDone(connA):
	case <-time.After(2 * time.Second):
		t.Fatal("the first connection's teardown did not end")
	}
	if err := bClient.Send(testproto.Msg{Op: testproto.OpTree, Version: 1, ID: 3, Session: 7, Tree: 1}); err != nil {
		t.Fatal(err)
	}
	if rs, err := bClient.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("a tree on the session the second connection carries: %+v (%v)", rs, err)
	}
	// The second goes, and the session with it.
	_ = clientB.Close()
	select {
	case <-core.ConnDone(connB):
	case <-time.After(2 * time.Second):
		t.Fatal("the second connection's teardown did not end")
	}
	// The second's close ends the session, which a third connection would find
	// gone. That half cannot be asserted at this base: core's row checks, which
	// compare a row's Needs with the request's level, are the epic 5 stack's and
	// are not in this branch, so a request that needs a session runs anyway. The
	// registry's leave runs all the same, on the teardown's OnClose.
}

// A live connection is listed, and one whose server is not serving closes
// before it reads.
func TestTheRegistryListsLiveConnections(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	conn, client := servedConn(t, srv, &l)
	c := testproto.NewClient(client)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recv(); err != nil {
		t.Fatal(err)
	}
	var listed int
	for info := range srv.Connections() {
		listed++
		if info.Protocol != "testproto" {
			t.Errorf("a connection lists protocol %q", info.Protocol)
		}
	}
	if listed != 1 {
		t.Errorf("%d connections listed for one, want 1", listed)
	}
	_ = client.Close()
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown did not end")
	}
	for range srv.Connections() {
		t.Error("a closed connection is still listed")
	}

	// A socket handed to a stopped server closes before it reads.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn2, client2 := servedConn(t, srv, &l)
	if err := client2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client2.Write([]byte{0, 0, 0, 4}); err == nil {
		if _, err := testproto.NewClient(client2).Recv(); err == nil {
			t.Error("a stopped server served a connection")
		}
	}
	select {
	case <-core.ConnDone(conn2):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection served after Stop did not close")
	}
}

// A connection answers an Echo over the full protocol, session and tree set up.
func TestAnEchoOverTheFullProtocol(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	_, client := servedConn(t, srv, &l)
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
		{Op: testproto.OpTree, Version: 1, ID: 3, Session: 7, Tree: 1},
		{Op: testproto.OpEcho, Version: 1, ID: 4, Session: 7, Tree: 1, Payload: []byte("hi")},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
}

// A close ends the connection's context, which every wait inside it reads.
func TestACloseEndsTheConnectionsContext(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	conn, client := servedConn(t, srv, &l)
	ctx := core.ConnContext(conn)
	if ctx.Err() != nil {
		t.Fatalf("a live connection's context is ended: %v", ctx.Err())
	}
	conn.Close(nil)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a close did not end the connection's context")
	}
	_ = client.Close()
	<-core.ConnDone(conn)
}

// Keeps are held one apiece: a release called twice frees one, and the other
// keeps the connection.
func TestKeepsAreHeldOneEach(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	conn, client := servedConn(t, srv, &l)
	first, second := conn.Keep(), conn.Keep()
	if got := core.Kept(conn); got != 2 {
		t.Errorf("two Keeps hold %d", got)
	}
	first()
	first() // a release called twice frees one
	if got := core.Kept(conn); got != 1 {
		t.Errorf("after one release, Keeps hold %d, want 1", got)
	}
	second()
	if got := core.Kept(conn); got != 0 {
		t.Errorf("after both releases, Keeps hold %d", got)
	}
	_ = client.Close()
	<-core.ConnDone(conn)
}

// The connection-state charge the accept loop took is released by the
// teardown.
func TestTheChargeIsReleasedAtTeardown(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	charge, err := b.Connection.Reserve(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	// The accept loop charges the connection-state budget and hands the
	// reservation to serveConn, whose teardown releases it.
	nc, client2 := net.Pipe()
	charged := core.ServeConnCharged(srv, nc, &l, &charge)
	if r := b.Connection.Reserved(); r != 64<<10 {
		t.Fatalf("the charge is %d bytes, want 64 KiB", r)
	}
	_ = client2.Close()
	select {
	case <-core.ConnDone(charged):
	case <-time.After(2 * time.Second):
		t.Fatal("the teardown did not end")
	}
	if r := b.Connection.Reserved(); r != 0 {
		t.Errorf("the connection-state budget holds %d bytes after the teardown", r)
	}
}
