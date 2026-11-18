package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// onCloseGrace is how long OnClose runs: its own context, which the close does
// not cancel, ending this long after the close (RFC 20 section 4.7).
const onCloseGrace = 30 * time.Second

// Conn is one accepted connection. Its reader, writer and pipeline are core's;
// a protocol reaches it through these methods only.
type Conn struct {
	nc         net.Conn
	srv        *Server
	snap       settings.Settings // the snapshot loaded at accept
	sh         *shared
	p          *pipeline
	w          *writer
	rd         *reader
	remote     netip.Addr
	remoteAddr netip.AddrPort         // the client's address as it reached this node
	since      time.Time              // accepted at
	charge     *resources.Reservation // the connection-state charge the accept loop took; nil for none

	ctx    context.Context
	cancel context.CancelFunc

	err         atomic.Pointer[error] // the first close's error, which later ones leave
	closed      atomic.Bool
	established atomic.Bool
	done        chan struct{} // closed once the teardown ended
	kept        atomic.Int32  // the idle pass-over's count, one per Keep, released one apiece
	closedAt    atomic.Int64  // the close's time, in Unix nanoseconds; 0 before one

	timersMu   sync.Mutex // guards the two deadlines below
	handshakeT timer      // armed at accept for handshake_timeout, ended by Established
	idleT      timer      // armed whenever nothing runs, resting under a Keep or a record
}

// Send queues a message on the writer: a callback, or an interim reply. core
// passes it through Security.Wrap under h first, so a lease break on an
// encrypting session is sealed. before is the HoldCodec.Key of the request
// whose reply it must precede, 0 for none; core keys every request of a table
// that Holds that way.
func (c *Conn) Send(h Header, msg *resources.Buf, before uint64) error {
	return fmt.Errorf("core: send: %w", errors.ErrUnsupported)
}

// Established ends the handshake deadline: SMB at its first authenticated
// session, NFS at its first routed call. It is idempotent, and the first call
// starts the idle deadline's watch (RFC 20 section 6.4).
func (c *Conn) Established() {
	if !c.established.CompareAndSwap(false, true) {
		return
	}
	c.timersMu.Lock()
	if c.handshakeT != nil {
		c.handshakeT.Stop()
		c.handshakeT = nil
	}
	c.timersMu.Unlock()
	c.idle()
}

// Keep marks state the client relies on (an SMB session's durable open, an
// NFSv4.1 back channel) so the idle timeout passes the connection over until
// every release is called. A release is its own: calling it twice releases
// nothing a second time, and the last one may start the idle deadline's
// watch again.
func (c *Conn) Keep() (release func()) {
	c.kept.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			if c.kept.Add(-1) == 0 {
				c.idle()
			}
		})
	}
}

// idle arms the idle deadline: the pipeline calls it at each moment nothing
// runs any more, and so do Established and a Keep's last release. The
// deadline runs on the server's clock from this call, so a connection whose
// last reply went out is closed idle_timeout after it.
func (c *Conn) idle() {
	if c.closed.Load() {
		return
	}
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	if c.idleT != nil {
		c.idleT.Stop()
		c.idleT = nil
	}
	// A zero idle_timeout disarms it, as it disarms read_timeout and
	// write_timeout (RFC 13 Appendix B's zero value).
	if d := c.snap.Common().IdleTimeout; d > 0 {
		c.idleT = c.sh.clock.AfterFunc(d, c.idleExpired)
	}
}

// idleExpired closes the connection at its idle deadline, once everything the
// deadline waits on still rests: the connection is established, nothing runs,
// no record is partly read and no Keep is held. A deadline that expired
// while that no longer holds leaves the connection alone: the next moment
// nothing runs arms it afresh.
func (c *Conn) idleExpired() {
	if !c.established.Load() || c.closed.Load() || c.kept.Load() != 0 || !c.p.quiet() {
		return
	}
	if c.rd.holding() {
		// A record in hand may run nothing, and so call no done(): the
		// deadline looks again a deadline later instead of resting on one.
		c.idle()
		return
	}
	c.Close(errIdle)
}

// armHandshake arms the handshake deadline at accept, for the snapshot's
// handshake_timeout (RFC 20 section 6.4).
func (c *Conn) armHandshake() {
	// A zero handshake_timeout disarms it, as it disarms read_timeout and
	// write_timeout (RFC 13 Appendix B's zero value).
	if d := c.snap.Common().HandshakeTimeout; d <= 0 {
		return
	}
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	c.handshakeT = c.sh.clock.AfterFunc(c.snap.Common().HandshakeTimeout, func() { c.Close(errHandshake) })
}

// Settings is the settings snapshot this connection loaded at accept, the
// protocol's whole shape; NewConn reads its own from it.
func (c *Conn) Settings() settings.Settings { return c.snap }

// Close ends the connection for a protocol violation the protocol finds: an
// SMB message that fails decryption, a second NEGOTIATE, a moved continuously
// available share's trees. The reader stops, every hold and park ends as on a
// closed connection (cancel called, replies dropped), and OnClose runs. err is
// logged at the debug level, named with the connection's remote address.
//
// decision: Close records the error, cancels the connection's context and then
// stops the reader's read, in that order, so a reader re-arming its deadline
// either sees the ended context or has its new deadline overwritten by the
// stop; it then ends the pipeline and returns, so a close from a handler or
// from the reader never waits on itself. The reader's goroutine runs the rest
// of the teardown once its loop ends. Overturned if the teardown needs its own
// goroutine.
func (c *Conn) Close(err error) {
	if err == nil {
		err = errConnClosed
	}
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.err.Store(&err)
	c.closedAt.CompareAndSwap(0, c.sh.clock.Now().UnixNano())
	c.cancel()
	_ = c.nc.SetReadDeadline(time.Now().Add(-time.Second))
	if c.p != nil {
		c.p.close()
	}
}

// errConnClosed is a close a protocol asked for and named no cause of.
var errConnClosed = errors.New("core: the connection closed")

// errHandshake is a connection closed at its handshake deadline.
var errHandshake = errors.New("core: the handshake deadline passed")

// errIdle is a connection closed at its idle deadline.
var errIdle = errors.New("core: the idle deadline passed")

// serveConn serves one accepted socket until it closes: the snapshot, the
// protocol's stages, the writer, the pipeline and the reader, with the teardown
// on the reader once its loop ends (RFC 20 section 3.2). charge is the
// connection-state reservation the accept loop took, which the teardown
// releases; nil for none.
func (s *Server) serveConn(nc net.Conn, l *Listener, charge *resources.Reservation) *Conn {
	snap := *s.settings.Load()
	remote, port := remoteOf(nc)
	c := &Conn{nc: nc, srv: s, snap: snap, sh: s.sh, remote: remote, remoteAddr: port, since: time.Now(), charge: charge, done: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	st, err := withDefaults(l.Protocol.NewConn(c))
	if err != nil {
		s.sh.log.ErrorContext(c.ctx, "connection refused", "err", err)
		_ = nc.Close()
		c.release()
		close(c.done)
		return c
	}
	c.armHandshake()
	f := l.Protocol.Framer()
	common := snap.Common()
	c.w = newWriter(nc, f, common.WriteTimeout, c.Close)
	c.p = newPipeline(c.sh, st, link{ctx: c.ctx, remote: c.remote, set: snap, out: c.w.out, close: c.Close, idle: c.idle})
	c.rd = newReader(nc, f, s.recv, c.remote, common.ReadTimeout)
	registered := s.register(c)
	if !registered {
		// The server is not serving: the connection closes before its reader
		// is started, so not one record is cut.
		c.Close(errStop)
	}
	go c.readLoop()
	return c
}

// teardown is the reader's, once its loop ends: the requests still running, then
// OnClose under a context of its own, the writer, the charge and the registry
// (RFC 20 section 3.2's order).
func (c *Conn) teardown() {
	defer close(c.done)
	c.timersMu.Lock()
	if c.handshakeT != nil {
		c.handshakeT.Stop()
		c.handshakeT = nil
	}
	if c.idleT != nil {
		c.idleT.Stop()
		c.idleT = nil
	}
	c.timersMu.Unlock()
	if ep := c.err.Load(); ep != nil {
		c.sh.log.Debug("core: connection closed", "remote", c.remoteAddr, "err", *ep)
	}
	// OnClose's context is the server's clock's, not the connection's: the
	// close has cancelled that one (RFC 20 section 4.7).
	// The wait and OnClose share one grace, counted from the close: the teardown
	// ends at most that long after it, so OnClose runs once nothing runs, and its
	// context ends at the grace's end (RFC 20 section 4.7).
	at := c.sh.clock.Now().Add(onCloseGrace)
	if closedAt := c.closedAt.Load(); closedAt != 0 {
		at = time.Unix(0, closedAt).Add(onCloseGrace)
	}
	wait, cancelWait := withClockDeadline(context.Background(), c.sh.clock, at)
	defer cancelWait()
	if err := c.p.wait(wait); err != nil {
		c.sh.log.ErrorContext(wait, "connection teardown: requests still running", "err", err)
	}
	if c.p.st.OnClose != nil {
		ctx, cancel := withClockDeadline(context.Background(), c.sh.clock, at)
		defer cancel()
		c.p.st.OnClose(ctx)
	}
	c.w.stop()
	c.release()
	c.srv.unregister(c)
	_ = c.nc.Close()
}

// protocol is the connection's protocol label, what a request records before
// Route.
func (c *Conn) protocol() string {
	if c.p == nil {
		return ""
	}
	return c.p.st.Protocol
}

// release returns the connection's charge to the connection-state budget.
func (c *Conn) release() {
	if c.charge != nil {
		c.charge.Release()
	}
}

// remoteOf is nc's remote address, which its records are charged to, and the
// address as the client's list reports it; the zero ones for a socket that has
// none, as a test's pipe.
func remoteOf(nc net.Conn) (netip.Addr, netip.AddrPort) {
	if a, ok := nc.RemoteAddr().(*net.TCPAddr); ok {
		ap := a.AddrPort()
		return ap.Addr().Unmap(), ap
	}
	return netip.Addr{}, netip.AddrPort{}
}
