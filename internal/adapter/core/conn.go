package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	nc     net.Conn
	srv    *Server
	snap   settings.Settings // the snapshot loaded at accept
	sh     *shared
	p      *pipeline
	w      *writer
	rd     *reader
	remote netip.Addr
	charge *resources.Reservation // the connection-state charge the accept loop took; nil for none

	ctx    context.Context
	cancel context.CancelFunc

	err         atomic.Pointer[error] // the first close's error, which later ones leave
	closed      atomic.Bool
	established atomic.Bool
	done        chan struct{} // closed once the teardown ended
	kept        atomic.Bool   // T4.7a's idle pass-over
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
// session, NFS at its first routed call. It is idempotent.
func (c *Conn) Established() { c.established.Store(true) }

// Keep marks state the client relies on (an SMB session's durable open, an
// NFSv4.1 back channel) so the idle timeout passes the connection over until
// every release is called.
func (c *Conn) Keep() (release func()) {
	c.kept.Store(true)
	return func() { c.kept.Store(false) }
}

// Settings is the settings snapshot this connection loaded at accept, the
// protocol's whole shape; NewConn reads its own from it.
func (c *Conn) Settings() settings.Settings { return c.snap }

// Close ends the connection for a protocol violation the protocol finds: an
// SMB message that fails decryption, a second NEGOTIATE, a moved continuously
// available share's trees. The reader stops, every hold and park ends as on a
// closed connection (cancel called, replies dropped), and OnClose runs. err is
// logged and counted.
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
	c.cancel()
	_ = c.nc.SetReadDeadline(time.Now().Add(-time.Second))
	if c.p != nil {
		c.p.close()
	}
}

// errConnClosed is a close a protocol asked for and named no cause of.
var errConnClosed = errors.New("core: the connection closed")

// serveConn serves one accepted socket until it closes: the snapshot, the
// protocol's stages, the writer, the pipeline and the reader, with the teardown
// on the reader once its loop ends (RFC 20 section 3.2).
func (s *Server) serveConn(nc net.Conn, l *Listener) *Conn {
	snap := *s.settings.Load()
	c := &Conn{nc: nc, srv: s, snap: snap, sh: s.sh, remote: remoteOf(nc), done: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	st, err := withDefaults(l.Protocol.NewConn(c))
	if err != nil {
		s.sh.log.ErrorContext(c.ctx, "connection refused", "err", err)
		_ = nc.Close()
		c.release()
		close(c.done)
		return c
	}
	f := l.Protocol.Framer()
	common := snap.Common()
	c.w = newWriter(nc, f, common.WriteTimeout, c.Close)
	c.p = newPipeline(c.sh, st, link{ctx: c.ctx, remote: c.remote, out: c.w.out, close: c.Close})
	c.rd = newReader(nc, f, s.recv, c.remote, common.ReadTimeout)
	s.register(c)
	go c.readLoop()
	return c
}

// teardown is the reader's, once its loop ends: the requests still running, then
// OnClose under a context of its own, the writer, the charge and the registry
// (RFC 20 section 3.2's order).
func (c *Conn) teardown() {
	defer close(c.done)
	// OnClose's context is the server's clock's, not the connection's: the
	// close has cancelled that one (RFC 20 section 4.7).
	ctx, cancel := withClockDeadline(context.Background(), c.sh.clock, c.sh.clock.Now().Add(onCloseGrace))
	defer cancel()
	_ = c.p.wait(ctx)
	if c.p.st.OnClose != nil {
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

// remoteOf is nc's remote IP address, the key its records are charged to; the
// zero address for a socket that has none, as a test's pipe.
func remoteOf(nc net.Conn) netip.Addr {
	if a, ok := nc.RemoteAddr().(*net.TCPAddr); ok {
		return a.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}
