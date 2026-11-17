package core_test

import (
	"context"
	"net"
	"testing"
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
	c := connPair(t, srv, l)
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
}
