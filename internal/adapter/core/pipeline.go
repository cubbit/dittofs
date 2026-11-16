package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// shared is what every connection of one server shares: the request path's
// series, resolved once per server, its clock, its logger and its tracer.
//
// decision: the series are kept here, never in a table: two servers may
// share a table, and a node's servers share its one registry. A table's are
// resolved at the first Route that returns it and a label's at the first
// accept that names it, into copy-on-write maps that every request reads with
// no lock and no label lookup, and that mu fills. Overturned if a profile
// shows a first resolution's copy to matter, which it cannot at a handful of
// tables.
type shared struct {
	fam      [vecs]family
	wire     histFamily
	holdPool adder // no protocol label
	mu       sync.Mutex
	tables   atomic.Pointer[map[*OpTable]*tableSeries]
	labels   atomic.Pointer[map[string]*labelSeries]
	clock    clock
	log      *slog.Logger
	tracer   trace.Tracer // nil: no spans
}

// newShared is a server's shared state, its vectors registered on
// d.Metrics, or recording nothing without one, on the runtime's clock,
// d.Logger, or a logger that discards, and d.Tracer.
func newShared(d adapter.Deps) *shared {
	sh := &shared{clock: runtimeClock{}, log: d.Logger, tracer: d.Tracer}
	if sh.log == nil {
		sh.log = slog.New(slog.DiscardHandler)
	}
	sh.fam, sh.wire = newFamilies(d.Metrics)
	sh.holdPool = sh.fam[vHoldPoolBytes]()
	sh.tables.Store(&map[*OpTable]*tableSeries{})
	sh.labels.Store(&map[string]*labelSeries{})
	return sh
}

// flowCap is how many requests a connection has in flight, counted at admit,
// where its protocol has no flow control of its own (RFC 20 §6.3).
//
// ponytail: a fixed cap of 64 per connection for protocols without flow
// control. One NFSv3 client on one connection is limited to the cap however
// idle the node is. Raise it, or size it from the receive pool, when a
// single-client NFSv3 benchmark on the reference box is limited by it.
const flowCap = 64

// capped is the FlowControl of a connection whose protocol has none: a
// counting semaphore of flowCap places.
type capped chan struct{}

// newCapped is one connection's cap, every place free.
func newCapped() capped { return make(capped, flowCap) }

// Admit takes a place, waiting within ctx for one to free. Under a ctx
// already ended it tries once: it takes a free place, and otherwise returns
// ctx's error at once. Its release frees the place once, however often it is
// called.
//
// ponytail: each admission allocates twice, the release closure and the flag
// it captures. One allocation is the floor while release stays idempotent
// after its place is reused: give each place a generation the closure captures
// by value and compares and swaps. Do that when the pipeline's allocation count
// is lowered past these two.
func (c capped) Admit(ctx context.Context, _ Header) (release func(), err error) {
	select {
	case c <- struct{}{}:
	default:
		select {
		case c <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			<-c
		}
	}, nil
}

// reserveReply takes a reply buffer of bound bytes from pool for key, waiting
// within ctx, the attempt's. A wait that ends at ctx's deadline is the
// adapter's own refusal for a node memory pool, ErrDelay of DelayResources
// (RFC 20 §6.3), and a charge past the pool's ceiling, which the pool refuses
// at once under any ctx, is ErrReplyTooBig (RFC 20 §4.3); a cancellation, and
// any other error, is returned as it is. On an error nothing is reserved.
//
// ponytail: every reply is reserved from the reply pool, the small class
// included, so a small request is charged 12 KiB twice while it runs and at
// 64 requests in flight a connection over-reserves 768 KiB. Give the pool an
// unreserved small buffer, or set one aside per connection at accept for its
// statuses, when the reply budget is shown to bind before the receive budget
// does.
func reserveReply(ctx context.Context, pool *resources.BufPool, key netip.Addr, bound int) (*resources.Buf, error) {
	buf, err := pool.Get(ctx, key, bound)
	switch {
	case err == nil:
		return buf, nil
	case errors.Is(err, resources.ErrTooLarge):
		return nil, fmt.Errorf("%w: %v", ErrReplyTooBig, err)
	case errors.Is(err, context.DeadlineExceeded):
		return nil, &vfs.Delay{Cause: vfs.DelayResources}
	}
	return nil, err
}

// statusWait bounds how long a reply carrying a status alone, which holds no
// reply reservation yet, waits for its buffer (RFC 20 §6.4).
const statusWait = 30 * time.Second

// errReplyWait is what a connection is closed with when a status found no
// reply buffer within statusWait; its close is counted as reply_wait
// (RFC 20 §10.1).
var errReplyWait = errors.New("core: no reply buffer for a status within " + statusWait.String())

// reserveStatus takes the small-class reply buffer a status alone goes out
// in, for key: at once when the pool has room with no waiter ahead, otherwise
// waiting within ctx, the connection's, so that a close ends the wait at
// once, for at most statusWait from when it starts. Past that it calls close
// with errReplyWait. It returns nil, with nothing reserved, when the wait
// ended. Its caller keeps the request's record, and so its receive charge,
// until the status goes out or the wait ends (RFC 24 §6.1).
func reserveStatus(ctx context.Context, pool *resources.BufPool, key netip.Addr, close func(error)) *resources.Buf {
	// A status is encoded by appending to B[:0], within the small class.
	if buf, ok := pool.TryGet(key, 0); ok {
		return buf
	}
	// decision: the wait is bounded, and past it the connection closes,
	// because the status can go out no other way: under the attempt's
	// deadline, which may have passed, the pool refuses unless it has room at
	// once and the status would never go out; under the request's context
	// alone the wait would have no end while parks, which have no deadline,
	// hold the pool full. The 30 s is a constant where RFC 20 §6.4's other
	// timeouts are settings; make it one when a status is seen waiting past
	// it. Its ceiling: a pool held full for 30 s by parks across connections
	// closes the connection whose status waited, not those whose parks fill
	// the pool, so an SMB client there loses its non-durable opens, and a
	// status can arrive up to 30 s past its own deadline. Withdraw it when
	// RFC 24 §4.1's turns bound each client's share of the pool, or when
	// parks' reply reservations are capped per client.
	wait, cancel := context.WithTimeout(ctx, statusWait)
	defer cancel()
	buf, err := pool.Get(wait, key, 0)
	if err != nil {
		if ctx.Err() == nil {
			close(errReplyWait)
		}
		return nil
	}
	return buf
}
