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
// cap.
type Deps struct {
	// Service is the adapter's only path to the filesystem: files, clients,
	// sign-in and principal spellings. It is the filesystem or the gateway,
	// and the adapter cannot tell which.
	Service vfs.Service
	// Pools are the four budgets on the request path: receive, reply,
	// connection state and hold.
	Pools   *resources.Pools
	Metrics *metrics.Registry
	Tracer  trace.Tracer // spans; the no-op tracer when tracing is off
	Logger  *slog.Logger
	// Watch registers a loop with the node's liveness watchdog: turn reports
	// a pass, idle a wait with nothing to do (an accept loop at its accept
	// deadline), stop unregisters it at unbind. nil watches nothing, as in a
	// test.
	Watch func(loop string) (turn, idle, stop func())
	// Dial opens an outbound connection from a given local address: NLM's
	// callbacks to a host, from the floating address it used. A test supplies
	// one returning a pipe.
	Dial func(ctx context.Context, from netip.Addr, to netip.AddrPort) (net.Conn, error)
}
