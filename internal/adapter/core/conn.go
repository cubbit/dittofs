package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
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
	series     *labelSeries // its connection series, resolved at accept from its listener's label (§10.1)
	remote     netip.Addr
	remoteAddr netip.AddrPort         // the client's address as it reached this node
	accept     *bound                 // the bound socket that accepted it, whose removal closes it; a hook's connection names none
	service    string                 // the first service its listener names, which the client list reports
	since      time.Time              // accepted at
	charge     *resources.Reservation // the connection-state charge the accept loop took; nil for none
	count      *resources.ConnLimit   // the node's connection count admission took it from, given back at the teardown; nil where admission took none, a hook's socket

	ctx    context.Context
	cancel context.CancelFunc

	err         atomic.Pointer[error] // the first close's error, which later ones leave
	closed      atomic.Bool
	sendFirst   atomic.Bool // the teardown lets the writer send what it holds before stopping it
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
// that Holds that way. Once Close has run, or before the connection's
// pipeline exists (a Send inside NewConn), Send writes nothing: it drops msg
// and returns net.ErrClosed, as the writer's out does from the close on.
//
// decision: Send takes msg's reference whatever it returns, a Wrap refusal
// included, as the writer's out does, so its caller never releases it.
// Overturned if RFC 20 section 4.7 leaves a refused message with its caller.
func (c *Conn) Send(h Header, msg *resources.Buf, before uint64) error {
	if c.p == nil || c.p.ctx.Err() != nil {
		msg.Release()
		return net.ErrClosed
	}
	if err := c.p.st.Security.Wrap(h, msg); err != nil {
		msg.Release()
		return err
	}
	return c.w.out(msg, 0, 0, before)
}

// Established ends the handshake deadline: SMB at its first authenticated
// session, NFS at its first routed call. It is idempotent, and the first call
// starts the idle deadline's watch (RFC 20 section 6.4) and moves
// adapter_connections from handshake to established, once (§10.1).
func (c *Conn) Established() {
	if !c.established.CompareAndSwap(false, true) {
		return
	}
	c.series.established()
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
// decision: Close records the error, cancels the connection's context, ends
// what the writer takes, so a Send or a reply past its closed check is
// dropped, and then stops the reader's read, in that order, so a reader
// re-arming its deadline either sees the ended context or has its new
// deadline overwritten by the stop; it then ends the pipeline and returns, so
// a close from a handler or from the reader never waits on itself. The
// reader's goroutine runs the rest of the teardown once its loop ends.
// Overturned if the teardown needs its own goroutine.
func (c *Conn) Close(err error) { c.close(err, false) }

// closeAfterSent closes the connection as Close does, but its teardown lets
// the writer send what it holds first, within the close's grace: a chain's
// bad step, whose records before it go out before the connection closes (RFC
// 20 section 4.7). A close that came first keeps its own teardown.
func (c *Conn) closeAfterSent(err error) { c.close(err, true) }

// close is Close, the teardown sending what the writer holds first where
// sendFirst; only the first close sets it, as only its error is kept.
func (c *Conn) close(err error, sendFirst bool) {
	if err == nil {
		err = errConnClosed
	}
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.sendFirst.Store(sendFirst)
	c.err.Store(&err)
	c.closedAt.CompareAndSwap(0, c.sh.clock.Now().UnixNano())
	c.cancel()
	if c.w != nil {
		c.w.refuse()
	}
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

// errAtRead wraps the errors the reader's own read fails a connection with,
// errAtWrite the ones the writer's write does, so a close tells the side its
// socket failed on apart from a protocol's own error (§10.1): a read failed is
// the client's or its read_timeout's reason, a write its write or
// write_timeout's.
var (
	errAtRead  = errors.New("core: the read failed")
	errAtWrite = errors.New("core: the write failed")
)

// closeReason is the reason a connection's close counts under (§10.1):
// core's own ends by their errors, a framing error by the reader's, a
// peer that closed or reset, or a read that failed as keepalive or
// TCP_USER_TIMEOUT ended a vanished one with ETIMEDOUT, the client's - told
// from read_timeout by os.ErrDeadlineExceeded, never by Timeout(), which
// syscall.Errno also reports for ETIMEDOUT (Go 1.26's syscall_unix.go) - and
// a close nothing of core's closed, which protocol code asked for.
func closeReason(err error) reason {
	switch {
	case errors.Is(err, errAtWrite):
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return rsnWriteTimeout
		}
		return rsnWrite
	case errors.Is(err, errFrame):
		return rsnFrame
	case errors.Is(err, errWalk):
		return rsnWalk
	case errors.Is(err, ErrDecode):
		return rsnDecode
	case errors.Is(err, errUnwrapClose):
		return rsnUnwrap
	case errors.Is(err, errWiring):
		return rsnWiring
	case errors.Is(err, errAtRead):
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return rsnReadTimeout
		}
		return rsnClient
	case errors.Is(err, errHandshake):
		return rsnHandshake
	case errors.Is(err, errIdle):
		return rsnIdle
	case errors.Is(err, errStatusWait):
		return rsnStatusWait
	case errors.Is(err, errReplyWait):
		return rsnReplyWait
	case errors.Is(err, errDrain):
		return rsnDrain
	case errors.Is(err, errRemoved):
		return rsnRemoved
	case errors.Is(err, errStop):
		return rsnStop
	}
	return rsnViolation
}

// serveConn serves one accepted socket until it closes: the snapshot, the
// protocol's stages, the writer, the pipeline and the reader, with the teardown
// on the reader once its loop ends, or here, on the accept loop, for a socket
// the registry refuses (RFC 20 section 3.2). The socket carries the TCP
// options (section 6.7), set before its stages are built, and the connection
// counts into its series (§10.1), both from the snapshot it loads here. b is
// the bound socket that accepted it, whose removal by a Reconfigure closes
// this connection; nil for a hook's connection, which no removal closes.
// count is the node's connection count admission took the socket from and
// charge the connection-state reservation it charged, both of which the
// teardown gives back; each nil for none. A connection whose stages
// withDefaults refuses never starts its reader, so serveConn runs its
// teardown itself: the close is counted under wiring, the gauge lowered, the
// error-level line rate-limited (§10.1, §10.3).
func (s *Server) serveConn(nc net.Conn, l *Listener, b *bound, count *resources.ConnLimit, charge *resources.Reservation) *Conn {
	snap := *s.settings.Load()
	remote, port := remoteOf(nc)
	c := &Conn{nc: nc, srv: s, snap: snap, sh: s.sh, series: s.sh.label(l.label), remote: remote, remoteAddr: port, accept: b, service: l.Services[0], since: time.Now(), charge: charge, count: count, done: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.series.accepted()
	common := snap.Common()
	userTimeout(nc, common.WriteTimeout)
	if s.accepted != nil {
		s.accepted(nc)
	}
	st, err := withDefaults(l.Protocol.NewConn(c))
	if err != nil {
		c.Close(err)
		c.teardown()
		return c
	}
	// The connection's series are its own Stages.Protocol's from here, the
	// listener's only while its stages were unbuilt (RFC 20 section 10.1).
	if ls := s.sh.label(st.Protocol); ls != c.series {
		c.series.states[stHandshake].Add(-1)
		ls.accepted()
		c.series = ls
	}
	f := l.Protocol.Framer()
	record, _ := f.MaxRecord()
	c.w = newWriter(nc, f, common.WriteTimeout, func(err error) { c.Close(fmt.Errorf("%w: %w", errAtWrite, err)) }, c.series.out)
	c.p = newPipeline(c.sh, st, link{ctx: c.ctx, remote: c.remote, set: snap, record: record, out: c.w.out, close: c.Close, closeAfterSent: c.closeAfterSent, idle: c.idle, turn: func() { c.idleGen.Add(1) }})
	c.rd = newReader(nc, f, s.recv, c.remote, common.ReadTimeout, c.series.in)
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

// teardown runs once, on the reader as its loop ends, or on the accept loop for
// a connection the registry or its stages refused: the close counted and its
// gauge lowered, here alone, then the requests still running, OnClose
// under a context of its own, the writer, the charge, the count and the
// registry (RFC 20 section 3.2's order). A teardown with no pipeline is a
// close at accept: the stages' refusal, whose line the minute rate-limits
// (§10.3).
func (c *Conn) teardown() {
	defer close(c.done)
	c.stopTimers()
	err := c.closeErr()
	r := closeReason(err)
	// The flag is claimed here, not read: a request still running as the
	// teardown counted its close can call Established after it, and the
	// gauge must not drift to a handshake below zero with an established
	// above it for good (§10.1).
	c.series.closed(r, !c.established.CompareAndSwap(false, true))
	c.sh.closeLine(c.series, r, err, c.p == nil)
	// OnClose's context is the server's clock's, not the connection's: the
	// close has cancelled that one (RFC 20 section 4.7).
	// The wait and OnClose share one grace, counted from the close: the teardown
	// ends at most that long after it. OnClose runs once nothing runs, or at the
	// grace's end with a request still running, logged at error, and its context
	// ends at the grace's end (RFC 20 sections 4.7 and 6.5).
	//
	// decision: past the grace OnClose runs beside a request still running
	// rather than wait on it: a request that ignores its cancelled context
	// would otherwise hold the teardown for good, and with it the Stop that
	// waits on it, which RFC 20 section 3.1 bounds at 30 s past its drain
	// deadline. Overturned if OnClose must never run beside a request.
	at := c.sh.clock.Now().Add(onCloseGrace)
	if closedAt := c.closedAt.Load(); closedAt != 0 {
		at = time.Unix(0, closedAt).Add(onCloseGrace)
	}
	if c.p != nil {
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
		// decision: past a bad step's close, the writer, which takes nothing
		// past the close, sends what it holds before it is stopped, as RFC 20
		// section 4.7 has the records before the bad step go first: after
		// OnClose, which keeps the grace it shares with the requests' wait, and
		// within that grace, so the teardown stays bounded by it; a write the
		// peer does not take fails at write_timeout, if that comes first.
		// Overturned if a close must wait past the grace for its records.
		if c.sendFirst.Load() {
			if sent := c.w.flushed(); sent != nil {
				select {
				case <-sent:
				case <-wait.Done():
				}
			}
		}
	}
	if c.w != nil {
		c.w.stop()
	}
	c.release()
	c.srv.unregister(c)
	_ = c.nc.Close()
}

// closeErr is the error the connection's close carries: the first close's,
// which later ones leave (RFC 20 section 4.7), or core's own where none was
// stored.
func (c *Conn) closeErr() error {
	if err := c.err.Load(); err != nil {
		return *err
	}
	return errConnClosed
}

// release gives the connection's charge back to the connection-state budget
// and its count to the node's connection count, in that order, each only
// where admission took it. Each path a connection ends by calls it once: the
// teardown, or the refusal of a NewConn that failed.
func (c *Conn) release() {
	if c.charge != nil {
		r := c.charge
		c.charge = nil
		r.Release()
	}
	if c.count != nil {
		c.count.Release(c.remote)
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
