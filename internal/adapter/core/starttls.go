package core

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
)

var (
	// errTLSAgain is a StartTLS on a connection that has an upgrade pending or
	// already switched to TLS.
	errTLSAgain = errors.New("core: the connection already asked for TLS")
	// errTLSClosing is a StartTLS on a connection that began closing.
	errTLSClosing = errors.New("core: the connection is closing")
	// errTLSNoConfig is a StartTLS with no TLS configuration.
	errTLSNoConfig = errors.New("core: StartTLS without a TLS configuration")
	// errTLSOutside is a StartTLS made outside the decode of a call's first header.
	errTLSOutside = errors.New("core: StartTLS outside the decode of a call's first header")
)

// upgrade is one switch of a connection to TLS, from the call that asked in
// its header's Decode to the end of that call: the reader reads no further
// record while it is pending.
type upgrade struct {
	cfg  *tls.Config
	done chan struct{}
	conn net.Conn
	once sync.Once

	// queued: the call's reply went to the writer as the last plaintext
	// message; that call's goroutine alone
	queued bool
}

// finish lets the reader go on, once.
func (u *upgrade) finish() {
	u.once.Do(func() {
		close(u.done)
	})
}

// The windows StartTLS is accepted in, which decoding a header opens.
const (
	decodeNone   int32 = iota // no header is being decoded
	decodeFirst               // a call's first header, which the reader decodes
	decodeMember              // a later member of a chain, which the chain's goroutine decodes
)

// openDecode starts the decode of a header: first for a call's first header, on
// the reader, else a chain member's. Decodes of one connection run one at a
// time, each holding the connection's one token, so StartTLS, wherever it is
// entered from, is taken as the ask of the first-header decode open now, and
// refused while a member's decode is open and while none is.
func (c *Conn) openDecode(first bool) {
	c.decodeSem <- struct{}{}

	if first {
		c.decoding.Store(decodeFirst)
		return
	}

	c.decoding.Store(decodeMember)
}

// shutDecode ends the decode openDecode started and returns the upgrade it
// asked for, nil for none. Only a first header's decode can have asked, so the
// chain's members, which ignore it, never take one.
func (c *Conn) shutDecode() *upgrade {
	defer func() {
		<-c.decodeSem
	}()

	c.decoding.Store(decodeNone)
	c.upMu.Lock()
	defer c.upMu.Unlock()

	return c.up
}

// withdrawUpgrade ends an upgrade that will not happen, so the reader reads on
// in plaintext and a later call may ask again.
func (c *Conn) withdrawUpgrade(up *upgrade) {
	c.upMu.Lock()
	if c.up == up {
		c.up = nil
	}
	c.upMu.Unlock()
	up.finish()
}

// awaitUpgrade is the reader's wait after a record is served, on the upgrade
// its own decode asked for (up, nil for none): where the call's header asked
// for TLS, no further record is read until that call ended, and a switched
// connection is read through its TLS conn. The wait is on the upgrade the
// reader's decode returned, never on the connection's current one: the
// handshake runs on the writer's goroutine and may already have ended and
// cleared it by the time the reader waits, and a reader that missed it would
// read the switched socket in plaintext.
func (c *Conn) awaitUpgrade(up *upgrade) error {
	if up == nil {
		return nil
	}

	select {
	case <-up.done:
	case <-c.ctx.Done():
		return c.ctx.Err()
	}

	if up.conn != nil {
		c.rd.swap(up.conn)
	}

	return nil
}

// handoff runs on the writer's goroutine once the reply that asked for TLS is
// written in plaintext: it hands the socket, and the bytes the reader already
// held, to crypto/tls under a fresh handshake deadline. It returns the TLS
// conn the writer sends on from here, or nil where the connection closed or the
// handshake failed, which closes the connection.
func (c *Conn) handoff(up *upgrade) net.Conn {
	if c.closed.Load() {
		c.withdrawUpgrade(up)
		return nil
	}

	tc := tls.Server(&heldConn{Conn: c.nc, held: c.rd.takeHeld()}, up.cfg)

	var deadline timer
	if d := c.snap.Common().HandshakeTimeout; d > 0 {
		deadline = c.sh.clock.AfterFunc(d, func() {
			c.Close(errHandshake)
		})
	}
	// Stopped before the reader goes on, so the deadline does not close a
	// connection whose handshake already ended; only a fire that raced the
	// Stop and won does, the handshake's own reason still.
	err := tc.Handshake()

	if deadline != nil {
		deadline.Stop()
	}

	if err != nil {
		c.Close(fmt.Errorf("%w: tls handshake: %w", errAtRead, err))
		c.withdrawUpgrade(up)

		return nil
	}

	c.upMu.Lock()
	c.up, c.tlsOn = nil, true
	c.upMu.Unlock()

	up.conn = tc
	up.finish()

	return tc
}

// heldConn is the socket with the bytes the reader had already read past the
// call that asked for TLS put back in front of it, so crypto/tls reads them
// first and they are never cut as records.
type heldConn struct {
	net.Conn
	held []byte
}

func (h *heldConn) Read(p []byte) (int, error) {
	if len(h.held) > 0 {
		n := copy(p, h.held)
		h.held = h.held[n:]

		return n, nil
	}

	return h.Conn.Read(p)
}
