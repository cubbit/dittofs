package core

import (
	"context"
	"errors"
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

	timersMu   sync.Mutex    // guards the two deadlines below
	handshakeT timer         // armed at accept for handshake_timeout, ended by Established
	idleT      timer         // armed whenever nothing runs, resting under a Keep or a record
	onIdleArm  func(uint64)  // a test's hook, set from hooks_test.go: each arm's generation, under timersMu; nil in production
	idleGen    atomic.Uint64 // the generation an arm carries: each arm, and each request a connection starts or ends, advances it, so a timer the current generation superseded closes nothing
}

// Send queues a message on the writer: a callback, or an interim reply. core
// passes it through Security.Wrap under h first, so a lease break on an
// encrypting session is sealed. before is the HoldCodec.Key of the request
// whose reply it must precede, 0 for none; core keys every request of a table
// that Holds that way.
//
// decision: Send takes msg's reference whatever it returns, a Wrap refusal
// included, as the writer's out does, so its caller never releases it.
// Overturned if RFC 20 section 4.7 leaves a refused message with its caller.
func (c *Conn) Send(h Header, msg *resources.Buf, before uint64) error {
	if err := c.p.st.Security.Wrap(h, msg); err != nil {
		msg.Release()
		return err
	}
	return c.w.out(msg, 0, 0, before)
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
	// closed is read under timersMu, which the teardown's stopTimers takes
	// after the close sets it, so no arm lands past that stop.
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	if c.closed.Load() {
		return
	}
	if c.idleT != nil {
		c.idleT.Stop()
		c.idleT = nil
	}
	// A zero idle_timeout disarms it, as it disarms read_timeout and
	// write_timeout (RFC 13 Appendix B's zero value).
	if d := c.snap.Common().IdleTimeout; d > 0 {
		g := c.idleGen.Add(1)
		c.idleT = c.sh.clock.AfterFunc(d, func() { c.idleExpired(g) })
		if c.onIdleArm != nil {
			c.onIdleArm(g)
		}
	}
}

// idleExpired closes the connection at its idle deadline, once everything the
// deadline waits on still rests: the connection is established, nothing runs,
// no record is partly read and no Keep is held. A record in hand is checked
// before the quiet, so one the reader serves between the two reads - the
// quiet it read numbering no request yet, the record no longer held, its
// request now running - cannot close a connection that is serving: the
// deadline looks again a deadline later instead. A deadline that expired while
// that no longer holds leaves the connection alone: the next moment nothing
// runs arms it afresh. The arm's generation is checked under timersMu: a
// timer a later arm, or a request a connection started or ended since,
// superseded closes nothing.
func (c *Conn) idleExpired(g uint64) {
	if !c.established.Load() || c.closed.Load() || c.kept.Load() != 0 {
		return
	}
	if c.rd.holding() {
		// A record in hand may run nothing, and so call no done(): the
		// deadline looks again a deadline later instead of resting on one.
		c.idle()
		return
	}
	if !c.p.quiet() {
		return
	}
	c.timersMu.Lock()
	stale := g != c.idleGen.Load()
	c.timersMu.Unlock()
	if stale {
		return
	}
	c.Close(errIdle)
}

// stopTimers stops the connection's two deadlines and forgets them, under
// timersMu: the teardown at its end, and a connection refused at accept, which
// a protocol may leave armed from inside NewConn.
func (c *Conn) stopTimers() {
	c.timersMu.Lock()
	defer c.timersMu.Unlock()
	if c.handshakeT != nil {
		c.handshakeT.Stop()
		c.handshakeT = nil
	}
	if c.idleT != nil {
		c.idleT.Stop()
		c.idleT = nil
	}
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
		// A protocol that armed a deadline inside NewConn leaves it armed
		// here: the connection is closed and its timers stopped, as no
		// reader ever starts to stop them.
		c.closed.Store(true)
		c.stopTimers()
		s.sh.log.ErrorContext(c.ctx, "connection refused", "err", err)
		_ = nc.Close()
		c.release()
		close(c.done)
		return c
	}
	f := l.Protocol.Framer()
	record, _ := f.MaxRecord()
	common := snap.Common()
	c.w = newWriter(nc, f, common.WriteTimeout, c.Close)
	c.p = newPipeline(c.sh, st, link{ctx: c.ctx, remote: c.remote, set: snap, record: record, out: c.w.out, close: c.Close, idle: c.idle, turn: func() { c.idleGen.Add(1) }})
	c.rd = newReader(nc, f, s.recv, c.remote, common.ReadTimeout)
	// Armed only once the writer, pipeline and reader exist, so a deadline
	// that fires at once never races their assignment.
	c.armHandshake()
	if !s.register(c) {
		// The server is not serving: the connection closes before its reader
		// is started, so not one record is cut, and its teardown runs here,
		// on the accept loop's goroutine, which Stop waits on: a Stop this
		// races returns only once its OnClose has and its charge is back
		// (RFC 20 section 3.2).
		c.Close(errStop)
		c.teardown()
		return c
	}
	go c.readLoop()
	return c
}

// teardown is the reader's, once its loop ends: the requests still running, then
// OnClose under a context of its own, the writer, the charge and the registry
// (RFC 20 section 3.2's order).
func (c *Conn) teardown() {
	defer close(c.done)
	c.stopTimers()
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
// release gives the connection's charge back once. A register refusal
// releases it at once, so a Stop it races never returns holding it; the
// teardown's later release then gives nothing back, and neither waits for
// the other, the refusal running before the reader starts.
func (c *Conn) release() {
	if c.charge != nil {
		r := c.charge
		c.charge = nil
		r.Release()
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
