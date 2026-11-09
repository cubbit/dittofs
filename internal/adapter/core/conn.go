package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Conn is one accepted connection. Its reader, writer and pipeline are core's;
// a protocol reaches it through these methods only.
type Conn struct{}

// Send queues a message on the writer: a callback, or an interim reply. core
// passes it through Security.Wrap under h first, so a lease break on an
// encrypting session is sealed. before is the HoldCodec.Key of the request
// whose reply it must precede, 0 for none; core keys every request of a table
// that Holds that way.
func (c *Conn) Send(h Header, msg *resources.Buf, before uint64) error {
	return fmt.Errorf("core: send: %w", errors.ErrUnsupported)
}

// Established ends the handshake deadline: SMB at its first authenticated
// session, NFS at its first routed call.
func (c *Conn) Established() {
	panic("not implemented")
}

// Keep marks state the client relies on (an SMB session's durable open, an
// NFSv4.1 back channel) so the idle timeout passes the connection over until
// every release is called.
func (c *Conn) Keep() (release func()) {
	panic("not implemented")
}

// Settings is the settings snapshot this connection loaded at accept, the
// protocol's whole shape; NewConn reads its own from it.
func (c *Conn) Settings() config.Settings {
	panic("not implemented")
}

// Close ends the connection for a protocol violation the protocol finds: an
// SMB message that fails decryption, a second NEGOTIATE, a moved continuously
// available share's trees. The reader stops, every hold and park ends as on a
// closed connection (cancel called, replies dropped), and OnClose runs. err is
// logged and counted.
func (c *Conn) Close(err error) {
	panic("not implemented")
}

// Dispatcher is what a connection hands each framed record to. The pipeline
// is the one implementation; a connection's own tests supply a stub.
type Dispatcher interface {
	Dispatch(ctx context.Context, c *Conn, rec *resources.Buf)
	// Drain switches to draining: from now on only the requests a drain
	// serves run, and each other one gets ErrMoved or ErrDraining, decided
	// per request by asking the service about the handle it names.
	Drain()
}
