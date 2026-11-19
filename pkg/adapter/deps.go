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
// cap. Only VFS and Budgets are required; New refuses Deps missing
// either, naming every one missing, and every other field may be nil.
type Deps struct {
	// VFS is the adapter's only path to the filesystem: files, clients,
	// sign-in and principal spellings. It is the filesystem or the gateway,
	// and the adapter cannot tell which.
	VFS vfs.Service
	// Budgets are the four on the request path, receive, reply, connection
	// state and hold, and the node's connection count, Conns: the limit and
	// each address's quarter of it span every adapter of the node, each accept
	// taking from the one count.
	Budgets *resources.Budgets
	Metrics *metrics.Registry // nil: registers nothing
	Tracer  trace.Tracer      // spans; nil: the no-op tracer
	// SampleRatio is RFC 25 §6.3's tracing.sample_ratio, which the root
	// passes beside Tracer: the adapter samples a request on the trace ID it
	// draws and opens spans only for one it sampled. 0 samples none.
	SampleRatio float64
	// TraceIDs draws each request's trace ID (RFC 20 §10.2). nil draws from
	// math/rand/v2's global source; a test passes its own (RFC 29 §4.11).
	// No caller passes a source fed by a request's bytes. Every request
	// calls it, concurrently, so it is safe for concurrent use: a test's
	// source over a *rand.Rand, which takes no lock, holds a mutex.
	TraceIDs func() trace.TraceID
	Logger   *slog.Logger // nil: discards
	// Watch registers a loop with the node's liveness watchdog, which the
	// loop reports to until it closes the Loop at unbind. nil watches
	// nothing, as in a test.
	Watch func(loop string) Loop
	// DialClient opens a connection back to a client, for a callback (an
	// NFSv4.0 recall, NLM's GRANTED), from the local address the client
	// reached, so a floating address keeps working after a takeover. nil: a
	// net.Dialer bound to from. A test supplies one returning a pipe.
	DialClient func(ctx context.Context, from netip.Addr, to netip.AddrPort) (net.Conn, error)
}

// Loop is one watched loop: Turn reports a pass, Idle a wait with nothing to
// do (an accept loop at its accept deadline), Close unregisters it. The
// node's health loop satisfies it.
type Loop interface {
	Turn()
	Idle()
	Close()
}
