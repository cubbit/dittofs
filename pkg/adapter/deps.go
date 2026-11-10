package adapter

import (
	"context"
	"log/slog"
	"net"
	"net/netip"

	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Deps is what an adapter is handed by the node's composition root; it builds
// no other component. A field is an interface where it has two
// implementations, a concrete type where it has one. It carries the budgets
// an adapter charges and no other: never the open-state budget or a share's
// cap. Only Service and Budgets are required; New refuses a missing one by
// name, and every other field may be nil.
type Deps struct {
	// Service is the adapter's only path to the filesystem: files, clients,
	// sign-in and principal spellings. It is the filesystem or the gateway,
	// and the adapter cannot tell which.
	Service vfs.Service
	// Budgets are the four on the request path: receive, reply, connection
	// state and hold.
	Budgets *resources.Budgets
	Metrics *metrics.Registry // nil: registers nothing
	Tracer  trace.Tracer      // spans; nil: the no-op tracer
	Logger  *slog.Logger      // nil: discards
	// Watch registers a loop with the node's liveness watchdog, which the
	// loop reports to until it closes the Loop at unbind. nil watches
	// nothing, as in a test.
	Watch func(loop string) Loop
	// Dial opens an outbound connection from a given local address: NLM's
	// callbacks to a host, from the floating address it used. nil: a
	// net.Dialer bound to from. A test supplies one returning a pipe.
	Dial func(ctx context.Context, from netip.Addr, to netip.AddrPort) (net.Conn, error)
}

// Loop is one watched loop: Turn reports a pass, Idle a wait with nothing to
// do (an accept loop at its accept deadline), Close unregisters it. The
// node's health loop satisfies it.
type Loop interface {
	Turn()
	Idle()
	Close()
}
