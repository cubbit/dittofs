package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
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

// loggedBuf is the tests' own log sink: the debug line a close writes lands
// here, so a test can assert what closed the connection reached the log.
type loggedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *loggedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *loggedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// budgets is a fresh set of the four budgets, wide enough for the tests.
func budgets() *resources.Budgets {
	return &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 4<<20),
		Reply:      resources.NewWaitBudget("reply", 4<<20),
		Connection: resources.NewBudget("connection", 4<<20),
		Hold:       resources.NewBudget("hold", 4<<20),
	}
}

// testServer is a server serving testproto's bare protocol under snap, its
// hand-over listener carrying the label accept counts under (§10.1): the
// listener a test hands to ServeConn is its own, not the server's clone.
func testServer(t *testing.T, d adapter.Deps, snap settings.Settings) (*core.Server, core.Listener) {
	t.Helper()
	srv, l := protoServer(t, d, snap, testproto.New(d).Bare())
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

// A compound within the record cap is answered with its members' payloads
// intact on a connection the server accepted: serveConn hands its pipeline
// the framer's record cap, which the compound bounds its members' replies
// within. A pipeline built with the cap zero, which is what omitting the
// framer's record from the link builds, refuses every compound whole as too
// big before any member runs.
func TestACompoundWithinTheRecordCapIsAnsweredOnAConnection(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	c := connPair(t, srv, l)
	err := c.Send(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 1, Tag: tagOf(1), Members: []testproto.Msg{
		{Op: testproto.OpEcho, Payload: []byte("hi")},
		{Op: testproto.OpEcho, Payload: []byte("ho")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Status != testproto.StatusOK {
		t.Fatalf("a compound within the record cap is answered %+v (%v)", rs, err)
	}
	hd, results, err := testproto.Results(rs[0].Body)
	if err != nil || !headIs(hd, testproto.StatusOK, tagOf(1)) || len(results) != 2 ||
		results[0].Status != testproto.StatusOK || !bytes.Equal(results[0].Body, []byte("hi")) ||
		results[1].Status != testproto.StatusOK || !bytes.Equal(results[1].Body, []byte("ho")) {
		t.Fatalf("the compound's reply is head %+v with %d results (%v), want its head ok echoing its own tag and each Echo's payload intact", hd, len(results), err)
	}
}

// An operation the settings switch off is refused not supported on a
// connection the server accepted: serveConn hands its pipeline the snapshot
// it accepted with, and the row checks answer the switched-off Echo with
// not supported. A pipeline built without its settings answers it, which is
// what dropping the snapshot from the link builds.
func TestASwitchedOffOperationIsRefusedOnAConnection(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := settings.DefaultNFS()
	snap.DisabledOperations = []string{"Echo"}
	srv, l := testServer(t, adapter.Deps{Budgets: b}, snap)
	c := connPair(t, srv, l)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	rs, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Status != testproto.StatusNotSupported {
		t.Fatalf("a switched-off Echo is answered %+v, want not supported", rs)
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
	core.Serving(srv) // the refusal is the stages', not the server's state
	conn, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	served := core.ServeConn(srv, conn, &l)
	// The connection closes before it reads: the teardown ends, and the peer's
	// read sees the end of the pipe, not its own deadline.
	select {
	case <-core.ConnDone(served):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection without an error table was not closed")
	}
	// No registry entry is left of it: a connection refused before its reader
	// starts is never registered.
	for range srv.Connections() {
		t.Error("a connection refused before it read is still registered")
	}
	buf := make([]byte, 1)
	if _, err := peer.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("the peer's read returns %v, want the connection's end", err)
	}
}

// A server built without budgets is refused where it is built, not at the
// first teardown.
func TestNewServerRefusesNoBudgets(t *testing.T) {
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	if _, err := core.NewServer(adapter.Deps{}, settings.DefaultNFS(), l); err == nil {
		t.Error("a server without budgets is built")
	}
}

// errlessProtocol is a protocol whose stages, on a real connection, name no
// error table: withDefaults refuses them at serve, and the connection closes
// before it reads, with nothing to answer its refusals. A nil Conn gets the
// bare stages whole, so the server's built-time check lets the server build.
type errlessProtocol struct{}

func (errlessProtocol) Framer() core.Framer { return testproto.New(adapter.Deps{}).Framer() }

func (errlessProtocol) NewConn(c *core.Conn) core.Stages {
	st := testproto.New(adapter.Deps{}).Bare().NewConn(c)
	if c != nil {
		st.Errors = nil
	}
	return st
}

// Stop closes every connection open and returns once each is torn down.
func TestStopClosesConnections(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
	for range 3 {
		connPair(t, srv, l)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
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

// The wait and OnClose share one grace counted from the close: a request that
// runs past the wait ends the teardown at the grace, and OnClose's context has
// already ended when it starts (RFC 20 section 4.7).
func TestOnClosesDeadlineIsTheWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		reached := make(chan struct{})
		release := make(chan struct{})
		started := make(chan error, 1)
		l := core.Listener{Services: []string{"test"}, Network: "tcp"}
		l.Protocol = withOnClose{
			withRouter{testproto.New(adapter.Deps{}).Bare(), func(rt core.Router) core.Router {
				return &stubborn{Router: rt, reached: reached, release: release}
			}},
			func(ctx context.Context) {
				started <- ctx.Err()
				<-ctx.Done()
			},
		}
		srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
		if err != nil {
			t.Fatal(err)
		}
		core.Serving(srv)
		conn, client := servedConn(t, srv, &l)
		c := testproto.NewClient(client)
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		<-reached
		// The client goes while the request still runs: the handler ignores its
		// context, so only the wait's grace ends it.
		_ = client.Close()
		synctest.Wait()
		time.Sleep(29 * time.Second)
		synctest.Wait()
		select {
		case err := <-started:
			t.Fatalf("OnClose ran at 29s: %v", err)
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case err := <-started:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("OnClose's context is %v when the wait's grace ends, want the deadline", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("OnClose did not run when the wait's grace ended")
		}
		select {
		case <-core.ConnDone(conn):
		case <-time.After(2 * time.Second):
			t.Fatal("the teardown did not end when the wait's grace ended")
		}
		close(release)
		synctest.Wait()
	})
}

// deadlines is the snapshot's two deadline settings, read off a test's own
// connection (RFC 13 Appendix B's values, 30 s and 15 min).
func deadlines(c *core.Conn) (handshake, idle time.Duration) {
	common := core.ConnSettings(c).Common()
	return common.HandshakeTimeout, common.IdleTimeout
}

// A Bare connection that never establishes is closed at the handshake
// deadline, counted on the server's clock from accept.
func TestABareConnectionIsClosedAtTheHandshakeTimeout(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	logged := &loggedBuf{}
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b, Logger: slog.New(slog.NewJSONHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	defer func() { _ = client.Close() }()
	handshake, _ := deadlines(conn)

	clk.Advance(handshake - time.Second)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection 1 s short of the handshake deadline closed")
	default:
	}
	clk.Advance(2 * time.Second)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection past the handshake deadline stayed open")
	}
	// The close's own error reached the log, at the debug level, beside the
	// remote address: a deadline closing healthy clients is seen.
	if got := logged.String(); !strings.Contains(got, "the handshake deadline passed") {
		t.Errorf("the close's error reached no log line: %q", got)
	}
}

// A full-protocol connection that establishes disarms the handshake deadline:
// past it, the connection answers an Echo.
func TestASessionDisarmsTheHandshakeDeadline(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	defer func() { _ = client.Close() }()
	handshake, _ := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}

	clk.Advance(handshake + 5*time.Second) // past the handshake deadline, short of idle
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a session's connection closed past the handshake deadline")
	default:
	}
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 3 {
		t.Fatalf("past the handshake deadline, an Echo is answered %+v (%v)", rs, err)
	}
}

// A connection whose last reply went out is closed at the idle deadline, which
// runs from that reply.
func TestAConnectionsIdleDeadlineRunsFromItsLastReply(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	// The clock moves between the session and the Echo, so a deadline armed at
	// the session and one armed at the Echo's reply are a whole second apart.
	clk.Advance(time.Second)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("the Echo is answered %+v (%v)", rs, err)
	}

	clk.Advance(idle - time.Millisecond)
	// The anti-check is a roundtrip: the deadline armed at the reply is 1 ms
	// away and the answer beats it, while one armed at the session, a second
	// earlier, would already have closed the connection and the send fail.
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 4, Session: 7, Payload: []byte("ho")}); err != nil {
		t.Fatalf("a connection 1 ms short of its idle deadline closed: %v", err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 4 {
		t.Fatalf("1 ms short of its idle deadline, a connection answers %+v (%v)", rs, err)
	}
	// The client goes, the reader at its end: the teardown runs with the
	// reply-re-armed idle timer still in the clock, and stops it, so nothing
	// is left armed.
	_ = client.Close()
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("the reader did not end at the client's close")
	}
	if open := clk.pending(); open != 0 {
		t.Errorf("a torn-down connection leaves %d timers armed", open)
	}
}

// An unestablished connection is the handshake deadline's to close, not the
// idle one's: a protocol whose requests run before its first establishing
// session, with idle_timeout set under handshake_timeout, is never idled
// while it is still negotiating.
func TestAnUnestablishedConnectionRunsToItsHandshakeDeadline(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	// Negotiate runs before any session, so it runs unestablished: idle_timeout
	// below handshake_timeout is what makes the two deadlines tell apart. NFS
	// promotes its Adapter, so the two fields are set on the snapshot itself.
	snap := settings.DefaultNFS()
	snap.IdleTimeout = 2 * time.Second
	snap.HandshakeTimeout = time.Minute
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	idleCurrent := core.WatchIdleArms(conn)
	defer func() { _ = client.Close() }()
	c := testproto.NewClient(client)
	if err := c.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: 1, ID: 1}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("the Negotiate is answered %+v (%v)", rs, err)
	}

	// Past its idle deadline, unestablished and quiet, the connection stays
	// open: only the handshake deadline governs it, and that is a minute out.
	clk.Advance(10 * time.Second)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("an unestablished connection closed at its idle deadline")
	default:
	}
	// Once it establishes, the idle deadline governs it: it closes there.
	if err := c.Send(testproto.Msg{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("the Session is answered %+v (%v)", rs, err)
	}
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	_, idle := deadlines(conn)
	clk.Advance(idle + time.Second)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("an established connection stayed open past its idle deadline")
	}
}

// Zero timeouts disarm their deadlines, as read_timeout's and write_timeout's
// zero values do: a connection whose settings carry zero stays open past any
// advance and answers.
func TestZeroTimeoutsLeaveNoDeadline(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	snap := settings.DefaultNFS()
	snap.IdleTimeout = 0
	snap.HandshakeTimeout = 0
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	defer func() { _ = client.Close() }()
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}

	clk.Advance(24 * time.Hour)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a zero-timeout connection closed at a deadline")
	default:
	}
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 3 {
		t.Fatalf("past every deadline, a zero-timeout Echo is answered %+v (%v)", rs, err)
	}
}

// A request that waits for its reply reservation past the idle deadline keeps
// the connection open, and once it is answered the deadline runs from the
// reply: the fire while it waited closed nothing.
func TestAnIdleDeadlineWaitsOutAReplyReservation(t *testing.T) {
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	idleCurrent := core.WatchIdleArms(conn)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	held, err := resources.NewBufPool(b.Reply).Get(context.Background(), netip.Addr{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 7, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool {
		return core.Runs(core.ConnPipeline(conn)) == 1
	})

	clk.Advance(2 * idle) // the armed deadline fires while the Echo still runs
	select {
	case <-core.ConnDone(conn):
		t.Fatal("an idle deadline closed a connection with a request running")
	default:
	}
	held.Release()
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 3 {
		t.Fatalf("the waiting Echo is answered %+v (%v)", rs, err)
	}
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	clk.Advance(idle - time.Millisecond)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection 1 ms short of its idle deadline closed")
	default:
	}
	clk.Advance(2 * time.Millisecond)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection past its idle deadline stayed open")
	}
}

// A connection whose reader holds half a record is not idle: the deadline
// rests while the record is partly read, and runs again from its reply.
func TestAPartlyReadRecordIsNotIdle(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	idleCurrent := core.WatchIdleArms(conn)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	// Half an Echo: the record's length and the first bytes of its body, the
	// rest withheld.
	echo := testproto.Append(nil, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 3, Session: 7, Payload: []byte("hi")})
	if _, err := client.Write(echo[:8]); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the half record to be held", func() bool {
		return b.Receive.Reserved() > 0
	})

	clk.Advance(2 * idle)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection mid-record closed past its idle deadline")
	default:
	}
	if _, err := client.Write(echo[8:]); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != 3 {
		t.Fatalf("the finished Echo is answered %+v (%v)", rs, err)
	}
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	clk.Advance(idle - time.Millisecond)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection 1 ms short of its idle deadline closed")
	default:
	}
	clk.Advance(2 * time.Millisecond)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection past its idle deadline stayed open")
	}
}

// A session with keep set holds the connection past the idle deadline, and the
// logoff that ends it starts the deadline again: the connection closes
// idle_timeout later.
func TestASessionsKeepHoldsTheIdleDeadline(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	idleCurrent := core.WatchIdleArms(conn)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7, Keep: true},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	if kept := core.Kept(conn); kept != 1 {
		t.Fatalf("a session with keep set holds %d keeps, want one", kept)
	}

	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	clk.Advance(2 * idle)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a kept connection closed past its idle deadline")
	default:
	}
	if err := c.Send(testproto.Msg{Op: testproto.OpLogoff, Version: 1, ID: 3, Session: 7}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("the logoff is answered %+v (%v)", rs, err)
	}
	waitsFor(t, "the keep's release", func() bool { return core.Kept(conn) == 0 })
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	clk.Advance(idle - time.Millisecond)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a released connection closed 1 ms short of its idle deadline")
	default:
	}
	clk.Advance(2 * time.Millisecond)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("a released connection stayed open past its idle deadline")
	}
}

// A record that runs nothing still ends at the idle deadline: the deadline
// that expired while the reader held it looks again a deadline later, so the
// paths that end a record without the pipeline running it - a cancel, a
// refusal sent without running - cannot hold the connection open and keep its
// charge (section 6.4).
func TestARecordThatRunsNothingStillClosesAtIdle(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b, Logger: slog.New(slog.NewJSONHandler(&loggedBuf{}, nil))}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	idleCurrent := core.WatchIdleArms(conn)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	waitsFor(t, "the idle deadline to re-arm", func() bool {
		return idleCurrent() && core.Runs(core.ConnPipeline(conn)) == 0
	})
	// Half a cancel, the rest withheld: the deadline expires while the reader
	// holds it.
	cancel := testproto.Append(nil, testproto.Msg{Op: testproto.OpCancel, Version: 1, ID: 3, Session: 7, Target: 2})
	if _, err := client.Write(cancel[:8]); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the half record to be held", func() bool {
		return b.Receive.Reserved() > 0
	})
	clk.Advance(2 * idle)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection mid-record closed past its idle deadline")
	default:
	}
	// The record completes as one that runs nothing: a cancel ends without
	// the pipeline running it, so no done() re-arms the deadline for it.
	if _, err := client.Write(cancel[8:]); err != nil {
		t.Fatal(err)
	}
	// The buffer's release lands before the reader clears its held tail, so
	// the wait demands both: a deadline firing into the tail would re-arm,
	// where the record is done and it must close.
	waitsFor(t, "the cancel to end without running", func() bool {
		return b.Receive.Reserved() == 0 && !core.RecordHeld(conn)
	})
	clk.Advance(2 * idle)
	select {
	case <-core.ConnDone(conn):
	case <-time.After(2 * time.Second):
		t.Fatal("a connection whose record ran nothing stayed open past its idle deadline")
	}
}

// A release called twice releases nothing a second time: a second Keep still
// holds the connection.
func TestADoubleReleaseHoldsASecondKeep(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{})}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, client := servedConn(t, srv, &l)
	defer func() { _ = client.Close() }()
	_, idle := deadlines(conn)
	c := testproto.NewClient(client)
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7, Keep: true},
	} {
		if err := c.Send(m); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	second := conn.Keep()
	second()
	second()
	if kept := core.Kept(conn); kept != 1 {
		t.Fatalf("a release called twice leaves %d keeps, want the session's one", kept)
	}

	clk.Advance(2 * idle)
	select {
	case <-core.ConnDone(conn):
		t.Fatal("a connection a Keep still holds closed past its idle deadline")
	default:
	}
}

// establishedRefuser is a protocol whose NewConn establishes the connection
// and then answers stages without a Header, which core refuses at accept: the
// refusal comes last, with the idle deadline its protocol already armed. The
// configure-time probe NewConn(nil) reads the embedded protocol's wiring.
type establishedRefuser struct{ core.Protocol }

func (p establishedRefuser) NewConn(c *core.Conn) core.Stages {
	if c == nil {
		return p.Protocol.NewConn(nil)
	}
	c.Established()
	return core.Stages{}
}

// withRouter is a protocol whose connection's Router the test wraps.
type withRouter struct {
	core.Protocol
	wrap func(core.Router) core.Router
}

func (p withRouter) NewConn(c *core.Conn) core.Stages {
	st := p.Protocol.NewConn(c)
	st.Router = p.wrap(st.Router)
	return st
}

// stubborn is a router whose Echo runs past its context: the handler holds
// until the test releases it, so the request outlives the wait's grace.
type stubborn struct {
	core.Router
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	table   *core.OpTable
}

func (r *stubborn) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	tb, req, err := r.Router.Route(h, prev)
	if tb == nil || err != nil {
		return tb, req, err
	}
	// One copy of the table, labelled as its own: a table per route would open
	// a series per request.
	r.once.Do(func() {
		ops := make(map[uint32]*core.Op, len(tb.Ops))
		for code, op := range tb.Ops {
			op := *op
			decode := op.Decode
			op.Decode = func(body []byte, v core.Version) (core.Invocation, int, error) {
				inv, n, err := decode(body, v)
				if err == nil && code == testproto.OpEcho {
					inv.Call = func(context.Context, *core.Request) error {
						close(r.reached)
						<-r.release
						return nil
					}
				}
				return inv, n, err
			}
			ops[code] = &op
		}
		t := *tb
		t.Ops = ops
		r.table = &t
	})
	return r.table, req, nil
}

// A client that reads nothing is not read past the writer's bound: its next
// write does not complete, the reader holds no record while it waits, another
// connection is served meanwhile, and both drain once it reads.
func TestASilentClientStopsTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		srv, l := testServer(t, adapter.Deps{Budgets: b}, settings.DefaultNFS())
		_, peerA := servedConn(t, srv, &l)
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
		// The bound is reached: the sender waits in a write, and the reader,
		// paused, holds no record.
		select {
		case err := <-sending:
			t.Fatalf("every write completed with the client reading nothing: %v", err)
		default:
		}
		if r := b.Receive.Reserved(); r != 0 {
			t.Errorf("a paused reader holds %d receive bytes", r)
		}
		// A second connection is served meanwhile.
		_, peerB := servedConn(t, srv, &l)
		bm := testproto.NewClient(peerB)
		if err := bm.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("y")}); err != nil {
			t.Fatal(err)
		}
		rs, err := bm.Recv()
		if err != nil || len(rs) != 1 {
			t.Fatalf("the second connection answered %+v: %v", rs, err)
		}
		// Reading A drains both: every reply, in order, then the sender ends.
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
	// The second's close ends the session: re-created by a third connection's
	// join, its trees gone with the leave, it answers no tree - the row
	// checks refuse a tree-needing PutFH naming the tree the session once had
	// with StatusNoTree, while a session latched with its trees would let the
	// join preserve them and answer instead.
	connC, clientC, cClient := join(t)
	if err := cClient.Send(testproto.Msg{Op: testproto.OpPutFH, Version: 1, ID: 3, Session: 7, Tree: 1, Handle: []byte("h")}); err != nil {
		t.Fatal(err)
	}
	if rs, err := cClient.Recv(); err != nil || len(rs) != 1 || rs[0].Status != testproto.StatusNoTree {
		t.Fatalf("a tree-needing operation on the re-created session: %+v (%v)", rs, err)
	}
	_ = clientC.Close()
	select {
	case <-core.ConnDone(connC):
	case <-time.After(2 * time.Second):
		t.Fatal("the third connection's teardown did not end")
	}
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
		if info.Protocol != "test" {
			t.Errorf("a connection lists protocol %q, want the first service its listener names", info.Protocol)
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
	// The server closed its end at once: setting a deadline on the closed
	// pipe is that, not a failure to answer.
	if err := client2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
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

// A connection refused after its protocol established is closed with its
// timers stopped: the idle deadline a protocol armed inside NewConn has no
// pipeline to read at its expiry, and would read c.p as nil there.
func TestARefusedConnectionStopsItsTimers(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: establishedRefuser{testproto.New(adapter.Deps{}).Bare()}}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	servedConn(t, srv, &l)
	// ServeConn returned already, the connection refused: the deadline its
	// protocol armed must be stopped with it, none left to fire.
	if pending := clk.pending(); pending != 0 {
		t.Fatalf("a refused connection left %d timer(s) armed", pending)
	}
	clk.Advance(2 * time.Minute)
}

// The client list's row over a listener naming two services: the connection
// is listed with its Protocol, the first service its listener names, its
// remote address, an empty Session and its accept time, and no longer once
// it closed. A registry entry left after a close, or a Protocol read from a
// service other than the first or from the stages' own label, fails here
// (RFC 20 section 3.4).
func TestConnectionsListTheFirstServiceAndDropClosedOnes(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"one", "two"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, on("127.0.0.1"), l)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	_, client := clientAt(t, srv.Addrs()[key("one")][0])

	waitsFor(t, "the connection to be listed", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 1
	})
	for info := range srv.Connections() {
		if info.Protocol != "one" {
			t.Errorf("a connection over a listener naming one and two lists protocol %q, want the first service", info.Protocol)
		}
		if info.Remote != client.LocalAddr().(*net.TCPAddr).AddrPort() {
			t.Errorf("a connection lists remote %s, want the client's own address %s", info.Remote, client.LocalAddr())
		}
		if info.Session != "" {
			t.Errorf("a connection lists session %q, want none: no protocol names one yet", info.Session)
		}
		if info.Since.Before(before) || info.Since.After(time.Now()) {
			t.Errorf("a connection lists its accept at %v, outside the accept's own window", info.Since)
		}
	}
	// A closed connection is listed no longer.
	_ = client.Close()
	waitsFor(t, "the closed connection to be dropped", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 0
	})
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

// heldDeadline is a pipe end whose read-deadline writes hold: each one enters,
// telling the test, and leaves on its release. A deadline in the past, the
// close's own stop, passes through: the close is what the test races.
type heldDeadline struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (h *heldDeadline) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		return h.Conn.SetReadDeadline(t)
	}
	h.entered <- struct{}{}
	<-h.release
	return h.Conn.SetReadDeadline(t)
}

// A close racing the reader's deadline writes still stops it. Held mid-write
// at each of the three - the arming at the record's first bytes, the arming
// the receive grant starts, and the disarm at the record's end - the close
// runs to its end first, its stop set under the held write and overwritten by
// it once released; the reader stops all the same, before a backstop shorter
// than read_timeout, each time (RFC 20 section 6.4, as the amendment states
// the check after each deadline write).
func TestACloseRacingEachDeadlineWriteStillStopsTheReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := budgets()
		adaptertest.CheckLeaks(t, b)
		snap := settings.DefaultNFS()
		snap.ReadTimeout = 3 * time.Second
		srv, l := testServer(t, adapter.Deps{Budgets: b}, snap)
		for write := 1; write <= 3; write++ {
			nc, peer := net.Pipe()
			held := &heldDeadline{Conn: nc, entered: make(chan struct{}), release: make(chan struct{})}
			conn := core.ServeConn(srv, held, &l)
			t.Cleanup(func() { _ = peer.Close() })
			c := testproto.NewClient(peer)
			sent := make(chan error, 1)
			go func() {
				sent <- c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")})
			}()
			for i := 1; i <= write; i++ {
				<-held.entered
				if i == write {
					// The close runs to its end while the reader is held
					// mid-write: past the cancel, its stop lands.
					conn.Close(nil)
				}
				held.release <- struct{}{}
			}
			select {
			case <-core.ConnDone(conn):
			case <-held.entered: // the reader read on past a stopped connection
				t.Fatalf("held at write %d: the reader went on to the next deadline write after the close", write)
			case <-time.After(2 * time.Second):
				t.Fatalf("held at write %d: the reader did not stop once its deadline overwrote the close's stop", write)
			}
			select {
			case err := <-sent:
				if err != nil {
					t.Fatalf("held at write %d: the client's send: %v", write, err)
				}
			default:
			}
		}
	})
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

// protoServer is testServer's builder over the until-then serving flip: the
// protocol a test declares, the full one where a reason needs its stages,
// security among them. A test that Starts it leaves it configured here.
func protoServer(t *testing.T, d adapter.Deps, snap settings.Settings, p core.Protocol) (*core.Server, core.Listener) {
	t.Helper()
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p}
	srv, err := core.NewServer(d, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	core.Labeled(&l, testproto.Name)
	return srv, l
}

// waitClosed waits for a connection's teardown: its close counted and logged.
func waitClosed(t *testing.T, c *core.Conn) {
	t.Helper()
	select {
	case <-core.ConnDone(c):
	case <-time.After(2 * time.Second):
		t.Fatal("the connection's teardown never ended")
	}
}

// counted asserts the close moved one reason's counter by one and no other
// reason's at all, and the connections gauge fell back with it: handshake and
// established both at zero, whatever state the close found (§10.1). A close
// counted under another reason, not at all, or leaving the gauge raised,
// fails here (R52, R53).
func counted(t *testing.T, m *metrics.Registry, reason string) {
	t.Helper()
	values, _ := scrape(t, m)
	for _, name := range core.ReasonNames() {
		k := "adapter_connection_closes_total{protocol=" + testproto.Name + ",reason=" + name + "}"
		switch v, want := values[k], 0.0; {
		case name == reason && v != 1:
			t.Errorf("the close counts %v under %s, want 1", v, name)
		case name != reason && v != want:
			t.Errorf("a close counts %v under %s, want it not moved", v, name)
		}
	}
	for _, state := range []string{"handshake", "established"} {
		if k := "adapter_connections{protocol=" + testproto.Name + ",state=" + state + "}"; values[k] != 0 {
			t.Errorf("after the close the %s state still holds %v connections", state, values[k])
		}
	}
}

// syncLog is a log sink shared by the server's goroutines and the test: lines
// land whole under one lock, and the test reads what landed so far.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLog) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

func (s *syncLog) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(strings.TrimSpace(s.buf.String()), "\n")
}

// faultedConn is a socket whose reads or writes fail on cue with the error a
// test faults it with, everything else passing through to the socket it
// wraps: fault injection, the reads failing like a vanished peer's would and
// the writes like an unreachable one's.
type faultedConn struct {
	net.Conn
	reads, writes bool
	err           error
}

func (c *faultedConn) Read(b []byte) (int, error) {
	if c.reads {
		return 0, c.err
	}
	return c.Conn.Read(b)
}

func (c *faultedConn) Write(b []byte) (int, error) {
	if c.writes {
		return 0, c.err
	}
	return c.Conn.Write(b)
}

// countingConn is a socket a test reads its bytes through: everything passes
// to the socket it wraps, and the totals tell what the adapter counted.
type countingConn struct {
	net.Conn
	read, written atomic.Int64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.written.Add(int64(n))
	return n, err
}

// A client that closes between records is counted under client: the peer
// closed the connection (§10.1). A close counted under another reason, or not
// at all, fails here (R52, R53).
func TestTheClientsCloseCountsUnderClient(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	conn, peer := servedConn(t, srv, &l)
	_ = peer.Close()
	waitClosed(t, conn)
	counted(t, m, "client")
}

// A read that fails with ETIMEDOUT, as keepalive or TCP_USER_TIMEOUT ends a
// peer that vanished, counts under client: it is told from read_timeout by
// os.ErrDeadlineExceeded, never by Timeout(), which syscall.Errno also
// reports for ETIMEDOUT (§10.1, Go 1.26's syscall_unix.go). A reason chosen
// by Timeout() counts a vanished peer under read_timeout and fails here.
func TestAVanishedPeersETIMEDOUTReadCountsUnderClient(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	nc, clientEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close() })
	conn := core.ServeConn(srv, &faultedConn{Conn: nc, reads: true, err: syscall.ETIMEDOUT}, &l)
	waitClosed(t, conn)
	counted(t, m, "client")
}

// A read that runs past read_timeout, a client that trickles the rest of its
// record, counts under read_timeout: the socket's own deadline, which
// os.ErrDeadlineExceeded names (§10.1).
func TestAReadPastItsDeadlineCountsUnderReadTimeout(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := settings.DefaultNFS()
	snap.ReadTimeout = 50 * time.Millisecond
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, snap)
	conn, peer := servedConn(t, srv, &l)
	if _, err := peer.Write([]byte{0, 0, 0}); err != nil {
		t.Fatal(err)
	} // a third of a record mark, and nothing behind it
	waitClosed(t, conn)
	counted(t, m, "read_timeout")
}

// A connection that never establishes closes at its handshake deadline and
// counts under handshake (§10.1).
func TestTheHandshakeDeadlineCountsUnderHandshake(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, peer := servedConn(t, srv, &l)
	defer func() { _ = peer.Close() }()
	handshake, _ := deadlines(conn)
	clk.Advance(handshake + time.Second)
	waitClosed(t, conn)
	counted(t, m, "handshake")
}

// An established connection that goes quiet closes at its idle deadline and
// counts under idle, its close lowering the established state's gauge (§10.1).
func TestTheIdleDeadlineCountsUnderIdle(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	p := testproto.New(adapter.Deps{})
	srv, l := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS(), p)
	core.Serving(srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	for _, msg := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1, ID: 1},
		{Op: testproto.OpSession, Version: 1, ID: 2, Session: 7},
	} {
		if err := c.Send(msg); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("over the full protocol: %+v (%v)", rs, err)
		}
	}
	_, idle := deadlines(conn)
	values, _ := scrape(t, m)
	if got := values["adapter_connections{protocol="+testproto.Name+",state=established}"]; got != 1 {
		t.Fatalf("the established state holds %v connections, want 1", got)
	}
	clk.Advance(idle + time.Second)
	waitClosed(t, conn)
	counted(t, m, "idle")
}

// A reply that waits past write_timeout, a client that never reads its
// replies, closes under write_timeout: the writer's own deadline, which the
// writer's close carries to the teardown (§10.1).
func TestAWritePastItsDeadlineCountsUnderWriteTimeout(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := settings.DefaultNFS()
	snap.WriteTimeout = 50 * time.Millisecond
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, snap)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn) // the reply never read, its write runs past the deadline
	counted(t, m, "write_timeout")
}

// A write that fails counts under write, told from write_timeout by the
// deadline the error carries or lacks (§10.1).
func TestAFailedWriteCountsUnderWrite(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	nc, clientEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close() })
	// The server's end fails its writes on cue; the client sends through its
	// own end, which the pipe writes whole to a reader that works.
	conn := core.ServeConn(srv, &faultedConn{Conn: nc, writes: true, err: syscall.EPIPE}, &l)
	c := testproto.NewClient(clientEnd)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn)
	counted(t, m, "write")
}

// A record whose mark claims more than the protocol's cap is a framing error:
// the close counts under frame (§10.1). The reader's framing refusals all wrap
// the sentinel a close tells the reason by.
func TestAFramingErrorCountsUnderFrame(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	conn, peer := servedConn(t, srv, &l)
	// Two megabytes claimed of a one-megabyte cap: a bad record mark.
	if _, err := peer.Write([]byte{0, 0x20, 0, 0}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn)
	counted(t, m, "frame")
}

// A record whose header does not decode far enough to address a reply - a
// body shorter than testproto's message header - closes under decode
// (§10.1).
func TestAShortHeaderCountsUnderDecode(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	conn, peer := servedConn(t, srv, &l)
	if _, err := peer.Write([]byte{0, 0, 0, 4, 1, 2, 3, 4}); err != nil {
		t.Fatal(err) // four bytes of body, where a header alone is forty
	}
	waitClosed(t, conn)
	counted(t, m, "decode")
}

// A message sealed under the session of the member before it, with none
// before it, has no header to answer under: the close counts under unwrap
// (§10.1), whatever testproto's own error for it is.
func TestAnUnwrapWithoutAHeaderCountsUnderUnwrap(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	p := testproto.New(adapter.Deps{})
	srv, l := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS(), p)
	core.Serving(srv)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	// All ones: the session field of a member that takes the one before's,
	// which the record's first message has none of.
	if err := c.Send(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 1, Session: ^uint32(0)}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn)
	counted(t, m, "unwrap")
}

// A connection whose stages withDefaults refuses at accept never reads: its
// close counts under wiring, the gauge the accept raised is lowered, and its
// line goes at the error level, one a minute for two closes of one reason
// (§10.1, §10.3; D2: nothing but the teardown counts a close). A tear-down
// path that counts nothing, or outside the teardown, leaves the gauge raised
// and the reason unmoved and fails here.
func TestTheStagesRefusalAtAcceptCountsUnderWiring(t *testing.T) {
	logged := &syncLog{}
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: errlessProtocol{}}
	srv, err := core.NewServer(adapter.Deps{Metrics: m, Budgets: b, Logger: slog.New(slog.NewJSONHandler(logged, nil))}, settings.DefaultNFS(), l)
	if err != nil {
		t.Fatal(err)
	}
	core.Labeled(&l, testproto.Name)
	core.Serving(srv)
	for range 2 {
		nc, _ := net.Pipe()
		conn := core.ServeConn(srv, nc, &l)
		waitClosed(t, conn)
	}
	values, _ := scrape(t, m)
	if n := values["adapter_connection_closes_total{protocol="+testproto.Name+",reason=wiring}"]; n != 2 {
		t.Errorf("two refused connections count %v under wiring, want 2", n)
	}
	for _, state := range []string{"handshake", "established"} {
		if k := "adapter_connections{protocol=" + testproto.Name + ",state=" + state + "}"; values[k] != 0 {
			t.Errorf("a refused connection still holds the %s state raised", state)
		}
	}
	for _, name := range core.ReasonNames() {
		if name == "wiring" {
			continue
		}
		if k := "adapter_connection_closes_total{protocol=" + testproto.Name + ",reason=" + name + "}"; values[k] != 0 {
			t.Errorf("a refused connection counts under %s, want wiring alone", name)
		}
	}
	lines := logged.lines()
	var line struct{ Level, Reason, Protocol string }
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
		t.Fatalf("two closes of one reason within a minute logged %d lines, want one: %q", len(lines), logged.lines())
	}
	if line.Level != "ERROR" || line.Reason != "wiring" || line.Protocol != testproto.Name {
		t.Errorf("the close '%s' at %s under %s carries reason protocol %s; want an error line under wiring", lines[0], line.Level, line.Reason, line.Protocol)
	}
}

// A protocol's own close, testproto's Violate, counts under violation: a
// close nothing of core's started (§10.1).
func TestAProtocolViolationCountsUnderViolation(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	p := testproto.New(adapter.Deps{})
	srv, l := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS(), p)
	core.Serving(srv)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	if err := c.Send(testproto.Msg{Op: testproto.OpViolate, Version: 1, ID: 1}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn)
	counted(t, m, "violation")
}

// A connection whose request still runs at the drain deadline is closed as
// one the drain abandoned and counts under drain; nothing running is closed
// under stop (§10.1). A drain close counted as a stop, or a Stop that closes
// a still-running request under it, fails here.
func TestTheDrainDeadlineAbandonedRequestCountsUnderDrain(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	b.Reply = resources.NewWaitBudget("reply", chargeOfReply(t, 8))
	adaptertest.CheckLeaks(t, b)
	p := newCapturing(testproto.New(adapter.Deps{}).Bare())
	snap := on("127.0.0.1")
	snap.DrainTimeout = 200 * time.Millisecond
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: p}
	srv, err := core.NewServer(adapter.Deps{Metrics: m, Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, _ := clientAt(t, testAddr(t, srv))
	conn := theConn(t, p)
	registered(t, srv, p)
	free := holdsRoom(t, b, 1)
	if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	waitsFor(t, "the Echo to wait for its reply room", func() bool { return pipelineRuns(conn) == 1 })
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	waitsFor(t, "the server to drain", func() bool { return srv.Health().State == adapter.StateDraining })
	clk.Advance(251 * time.Millisecond)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop at the drain deadline: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned: the drain held no deadline")
	}
	free()
	counted(t, m, "drain")
}

// A connection on an address a Reconfigure removes is closed as removed and
// counts under removed (§10.1).
func TestARemovedAddressCountsRemoved(t *testing.T) {
	needIPv6Loopback(t)
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, _ := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, on("127.0.0.1", "::1"), testproto.New(adapter.Deps{}).Bare())
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	dropped := srv.Addrs()[key("test")][1] // ::1, the address the Reconfigure removes
	c, err := net.Dial("tcp", dropped.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	waitsFor(t, "the removed-to-be address's connection to be accepted", func() bool {
		n := 0
		for range srv.Connections() {
			n++
		}
		return n == 1
	})
	if err := srv.Reconfigure(t.Context(), on("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	// Reconfigure returns once the removed address's connections are torn
	// down: the close is counted, on both of its states' gauges level.
	values, _ := scrape(t, m)
	if n := values["adapter_connection_closes_total{protocol="+testproto.Name+",reason=removed}"]; n != 1 {
		t.Errorf("%v connections closed at the removed address, want its one", n)
	}
	for _, state := range []string{"handshake", "established"} {
		if k := "adapter_connections{protocol=" + testproto.Name + ",state=" + state + "}"; values[k] != 0 {
			t.Errorf("after the reconfigure the %s state still holds %v connections", state, values[k])
		}
	}
}

// A quiet connection closed by Stop counts under stop (§10.1): nothing runs
// on it when the drain's end reaches its close.
func TestStopCountsQuietConnectionsUnderStop(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
	conn, peer := servedConn(t, srv, &l)
	defer func() { _ = peer.Close() }()
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn)
	counted(t, m, "stop")
}

// A status that waits the thirty seconds for a reply buffer and finds none
// closes under reply_wait: RFC 24's wait ends and the connection goes
// (§10.1). The wait runs on the bubble's time, where nothing frees the pool.
func TestAStatusWithoutAReplyBufferCountsUnderReplyWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		b := budgets()
		b.Reply = resources.NewWaitBudget("reply", 0)
		adaptertest.CheckLeaks(t, b)
		srv, l := testServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS())
		conn, peer := servedConn(t, srv, &l)
		rec := testproto.Append(nil, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1})
		// A next pointing past the record: a header that decoded far enough
		// to be answered, refused under the header it returned.
		binary.BigEndian.PutUint32(rec[4:], 1<<30)
		if _, err := peer.Write(rec); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the refusal's status waits, and the wait runs out
		synctest.Wait() // the close the wait runs to, and its teardown
		waitClosed(t, conn)
		counted(t, m, "reply_wait")
	})
}

// A socket the connection-state budget refuses is closed at accept and counts
// under pool, the listener's own label: the gauge never rose for it. Two
// closes within a minute log one line, warning, and a third after the clock
// passes the minute logs a second carrying the count held back (§10.1,
// §10.3; RFC 25 §5.2). A line per close fails here: a client in a reconnect
// loop turns one fault into a line per attempt, and one that drops the
// held-back count hides the volume.
func TestThePoolsCloseAtAcceptCountsAndItsLineHoldsTheMinute(t *testing.T) {
	logged := &syncLog{}
	m := metrics.New("v", "c")
	b := budgets()
	b.Connection = resources.NewBudget("connection", core.ConnCharge()-1)
	adaptertest.CheckLeaks(t, b)
	srv, _ := protoServer(t, adapter.Deps{Metrics: m, Budgets: b, Logger: slog.New(slog.NewJSONHandler(logged, nil))}, on("127.0.0.1"), testproto.New(adapter.Deps{}).Bare())
	stopAtEnd(t, srv)
	clk := newManualClock()
	core.SetServerClock(srv, clk)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	at := testAddr(t, srv)
	// A socket the budget refused sees the connection's end at once: the
	// dial, the accept and the close, then no more bytes of it.
	refused := func() {
		c, err := net.Dial("tcp", at.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := c.Read(make([]byte, 1)); err != nil {
				return
			}
		}
	}
	for range 2 {
		refused()
	}
	// Wait on the line, not the counter: the second close holds its line
	// back, which the count alone cannot see, and the line is written by the
	// server's own goroutine, in flight past the count.
	waitsFor(t, "the refused sockets' close lines to land", func() bool { return len(logged.lines()) == 1 })
	if values, _ := scrape(t, m); values["adapter_connection_closes_total{protocol="+testproto.Name+",reason=pool}"] != 2 {
		t.Fatalf("two refused sockets did not both close within the minute")
	}
	logged.reset()
	clk.Advance(61 * time.Second)
	refused()
	waitsFor(t, "the third close's line, past the minute", func() bool { return len(logged.lines()) == 1 })
	lines := logged.lines()
	var line struct {
		Level, Reason string
		Suppressed    int64
	}
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil {
		t.Fatalf("a third close past the minute logged %d lines, want one: %q", len(lines), logged.lines())
	}
	if line.Level != "WARN" || line.Reason != "pool" || line.Suppressed != 1 {
		t.Errorf("the minute's line '%s': level %s, reason %s, %d held back; want a warning under pool carrying one", lines[0], line.Level, line.Reason, line.Suppressed)
	}
	// The refused sockets raised no connection: both states stay level, and
	// no other reason moved.
	values, _ := scrape(t, m)
	for _, state := range []string{"handshake", "established"} {
		if k := "adapter_connections{protocol=" + testproto.Name + ",state=" + state + "}"; values[k] != 0 {
			t.Errorf("a socket the budget refused holds a connection in %s", state)
		}
	}
	for _, name := range core.ReasonNames() {
		if name == "pool" {
			continue
		}
		if k := "adapter_connection_closes_total{protocol=" + testproto.Name + ",reason=" + name + "}"; values[k] != 0 {
			t.Errorf("a socket the budget refused closed under %s, want pool alone", name)
		}
	}
}

// A full-protocol connection that sends its Session moves the connections
// gauge to established, once for two Sessions, and its close lowers both
// states again (§10.1). An Established that leaves the gauge at handshake
// fails here (R52).
func TestASessionMovesTheConnectionsGauge(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	p := testproto.New(adapter.Deps{})
	srv, l := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, settings.DefaultNFS(), p)
	core.Serving(srv)
	conn, peer := servedConn(t, srv, &l)
	c := testproto.NewClient(peer)
	if err := c.Send(testproto.Msg{Op: testproto.OpNegotiate, Version: 1, ID: 1}); err != nil {
		t.Fatal(err)
	}
	if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
		t.Fatalf("the Negotiate is answered %+v (%v)", rs, err)
	}
	for _, id := range []uint64{2, 3} {
		if err := c.Send(testproto.Msg{Op: testproto.OpSession, Version: 1, ID: id, Session: 7}); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].Status != 0 {
			t.Fatalf("the Session is answered %+v (%v)", rs, err)
		}
	}
	waitsFor(t, "the gauge to move", func() bool {
		values, _ := scrape(t, m)
		return values["adapter_connections{protocol="+testproto.Name+",state=handshake}"] == 0 &&
			values["adapter_connections{protocol="+testproto.Name+",state=established}"] == 1
	})
	_ = peer.Close()
	waitClosed(t, conn)
	counted(t, m, "client")
}

// Bytes in and out equal what the client wrote and read, framing included:
// the record marks and the message headers count with the payloads (§10.1). A
// count that leaves a header out fails here.
func TestBytesTotalCountFramingIncluded(t *testing.T) {
	m := metrics.New("v", "c")
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	srv, _ := protoServer(t, adapter.Deps{Metrics: m, Budgets: b}, on("127.0.0.1"), testproto.New(adapter.Deps{}).Bare())
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, err := net.Dial("tcp", testAddr(t, srv).String())
	if err != nil {
		t.Fatal(err)
	}
	client := &countingConn{Conn: raw}
	t.Cleanup(func() { _ = client.Close() })
	c := testproto.NewClient(client)
	for i := range 3 {
		payload := []byte("hi, this is the byte-count test's payload")
		if err := c.Send(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: uint64(i + 1), Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if rs, err := c.Recv(); err != nil || len(rs) != 1 || rs[0].ID != uint64(i+1) {
			t.Fatalf("the Echo is answered %+v (%v)", rs, err)
		}
	}
	waitsFor(t, "the bytes to be counted", func() bool {
		values, _ := scrape(t, m)
		return values["adapter_bytes_total{direction=in,protocol="+testproto.Name+"}"] == float64(client.written.Load()) &&
			values["adapter_bytes_total{direction=out,protocol="+testproto.Name+"}"] == float64(client.read.Load())
	})
	// The three vectors carry only the labels this RFC gives them, each its
	// closed set of values, and none of them an address: a remote address
	// used as a label value, or a value carrying one, fails here (§10.1;
	// RFC 25 section 2.4).
	values, _ := scrape(t, m)
	for _, vector := range []struct {
		name  string
		carry []string // the labels the vector carries, all closed sets
	}{
		{"adapter_connections", []string{"protocol", "state"}},
		{"adapter_connection_closes_total", []string{"protocol", "reason"}},
		{"adapter_bytes_total", []string{"protocol", "direction"}},
	} {
		for _, label := range vector.carry {
			for _, v := range valuesOf(values, vector.name, label) {
				if strings.Contains(v, ":") || strings.Contains(v, ".") {
					t.Errorf("%s's %s label takes %q, which an address shape is", vector.name, label, v)
				}
			}
		}
	}
}
