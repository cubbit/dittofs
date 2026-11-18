package core

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// What the package's external tests reach of core's unexported parts. It is
// the standard library's export_test.go by another name, since
// exports_test.go here is the surface test.
//
// decision: each part of the pipeline is tested once, at its seam with the
// connection: a test builds a pipeline through these hooks, hands serve the
// records a reader would, and reads the replies from a recorder in out's
// place, which passes the suite of out the connection's writer passes too;
// what only a socket decides is tested on the connection itself. Overturned
// if a part can only be observed through a socket.

type (
	Shared      = shared
	TableSeries = tableSeries
	LabelSeries = labelSeries
)

var (
	NewShared    = newShared
	SplitReasons = splitReasons
)

// StageNames is the names of the stages a refusal counts under and of those
// adapter_wire_seconds observes, by their index for Refused and Observe.
func StageNames() (refusals, wire map[int]string) {
	refusals, wire = map[int]string{}, map[int]string{}
	for _, st := range refusing {
		refusals[int(st)] = stageNames[st]
	}
	for _, st := range timed {
		wire[int(st)] = stageNames[st]
	}
	return refusals, wire
}

// ServiceErrorNames is the error label's values, by sentinel.
func ServiceErrorNames() map[error]string {
	m := map[error]string{}
	for _, e := range serviceErrors {
		m[e.err] = e.name
	}
	return m
}

func (sh *shared) Table(t *OpTable) (*TableSeries, error) { return sh.table(t) }
func (sh *shared) Label(p string) *LabelSeries            { return sh.label(p) }
func (sh *shared) HoldPoolCharged(n int64)                { sh.holdPoolCharged(n) }

func (s *protocolSeries) Answered()                       { s.answered() }
func (s *protocolSeries) Refused(st int)                  { s.refused(stage(st)) }
func (s *protocolSeries) Observe(st int, d time.Duration) { s.observe(stage(st), d) }

func (s *labelSeries) Admitted()  { s.admitted() }
func (s *labelSeries) ReplayHit() { s.replayHit() }

func (t *tableSeries) Routed(from *LabelSeries)  { t.routed(from) }
func (s *protocolSeries) UnmappedErr()           { s.unmappedErr() }
func (t *tableSeries) HoldRefused()              { t.holdRefused() }
func (t *tableSeries) Split(reason int)          { t.split(split(reason)) }
func (t *tableSeries) Panicked(op *Op)           { t.panicked(op) }
func (s *protocolSeries) StatusMapped(err error) { s.statusMapped(err) }
func (t *tableSeries) Held(n int64)              { t.held(n) }
func (t *tableSeries) Released(n int64)          { t.released(n) }

// CountLookups makes n count each child sh resolves from here on, each a
// WithLabelValues on a registry.
func CountLookups(sh *Shared, n *atomic.Int64) {
	for i, f := range sh.fam {
		sh.fam[i] = func(lv ...string) adder {
			n.Add(1)
			return f(lv...)
		}
	}
	wire := sh.wire
	sh.wire = func(lv ...string) observer {
		n.Add(1)
		return wire(lv...)
	}
}

// Clock is the server's clock, which SetClock replaces with a test's, and
// Timer what its AfterFunc arms.
type (
	Clock = clock
	Timer = timer
)

var ErrPanicked = errPanicked

func SetClock(sh *Shared, c Clock) { sh.clock = c }

// SetFloor lowers the floor each connection's held requests count against
// before the hold pool.
func SetFloor(sh *Shared, requests int, bytes int64) {
	sh.floorRequests, sh.floorBytes = requests, bytes
}

func (sh *shared) Deadline(next Call) Call                     { return sh.deadline(next) }
func (sh *shared) Recovery(op *Op, ts *TableSeries) Middleware { return sh.recovery(op, ts) }

// WithAttemptStart and WithHoldDeadline keep in ctx what the pipeline keeps
// there before an attempt: the start in a request's own context.
func WithAttemptStart(ctx context.Context, t time.Time) context.Context {
	return &requestCtx{Context: ctx, start: t}
}

func WithHoldDeadline(ctx context.Context, t time.Time) context.Context {
	rc := *ctx.(*requestCtx)
	rc.hold = t
	return &rc
}

// Chain is op's chain, built with the series of its table.
func (t *tableSeries) Chain(op *Op) Call { return t.chains[op.Code] }

// SetInvoke and SetResolveStart keep with r what the pipeline keeps there
// before the chain; CallEnd is the call's end the chain marked.
func SetInvoke(r *Request, call Call)          { r.invoke = call }
func SetResolveStart(r *Request, at time.Time) { r.resolveStart = at }
func CallEnd(r *Request) time.Time             { return r.callEnd }

// OnTheDeadline reports whether ctx is the deadline's own context, as the
// handler's is when the deadline is the chain's innermost middleware.
func OnTheDeadline(ctx context.Context) bool {
	_, ok := ctx.(*clockCtx)
	return ok
}

// Watches is how many functions ctx, the deadline's own context, still runs
// when it ends: those AfterFunc registered and no stop has removed.
func Watches(ctx context.Context) int {
	c, ok := ctx.(*clockCtx)
	if !ok {
		return -1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.funcs)
}

// NewCapped is the FlowControl a connection gets where its protocol has none.
func NewCapped() FlowControl { return newCapped() }

// NewCappedN is that FlowControl with n places.
func NewCappedN(n int) FlowControl { return make(capped, n) }

var (
	ErrReplyWait  = errReplyWait
	ErrStatusWait = errStatusWait
)

func (sh *shared) ReserveReply(ctx context.Context, key netip.Addr, bound int) (*resources.Buf, error) {
	return sh.reserveReply(ctx, key, bound)
}

func (sh *shared) ReserveStatus(ctx context.Context, key netip.Addr, close func(error)) *resources.Buf {
	return sh.reserveStatus(ctx, key, 0, time.Time{}, close)
}

// TestReader is one connection's record cut, reached by the reader's tests.
type TestReader struct{ r *reader }

// NewTestReader returns a record cut over nc, its records charged to key from
// recv, with timeout as read_timeout.
func NewTestReader(nc net.Conn, f Framer, recv *resources.BufPool, key netip.Addr, timeout time.Duration) *TestReader {
	return &TestReader{r: newReader(nc, f, recv, key, timeout)}
}

// Read returns the next record.
func (t *TestReader) Read(ctx context.Context) (*resources.Buf, error) { return t.r.Read(ctx) }

// ErrFrame is the framing refusal every record-level error wraps, so a close
// can name its reason.
var ErrFrame = errFrame

// The pipeline at its seam: a test builds one over a link of its own parts
// and hands serve the records a reader would.
type Pipeline = pipeline

var (
	WithDefaults     = withDefaults
	ErrUnwrapNeither = errUnwrapNeither
	ErrNoHold        = errNoHold
)

func NewPipeline(sh *Shared, st Stages, ctx context.Context, remote netip.Addr, set settings.Settings, record int, out func(*resources.Buf, uint64, uint64, uint64) error, close func(error)) *Pipeline {
	return newPipeline(sh, st, link{ctx: ctx, remote: remote, set: set, record: record, out: out, close: close})
}

func (p *pipeline) Serve(rec *resources.Buf)       { p.serve(rec) }
func (p *pipeline) Drain()                         { p.drain() }
func (p *pipeline) Wait(ctx context.Context) error { return p.wait(ctx) }
func (p *pipeline) Close()                         { p.close() }

// HoldStatusWait takes the connection's one status wait, as a status waiting
// for its buffer holds it, and returns what gives it back: a holder that
// outlasts the statuses waiting behind it.
func (p *pipeline) HoldStatusWait() (give func()) {
	p.status <- struct{}{}
	return func() { <-p.status }
}

// Runs is how many requests and refusals the pipeline counts as running.
func (p *pipeline) Runs() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// WriteBound is the writer's queue's bound, by messages.
const WriteBound = writeBound

// ChainPadding is the room a chain's joined record keeps between two replies.
const ChainPadding = chainPadding

// TestWriter is a connection's writer, reached by its tests.
type TestWriter struct{ w *writer }

// NewTestWriter starts a writer over nc, its gathered writes bounded by
// timeout and closeConn its connection's close.
func NewTestWriter(nc net.Conn, f Framer, timeout time.Duration, closeConn func(error)) *TestWriter {
	return &TestWriter{w: newWriter(nc, f, timeout, closeConn)}
}

// Out queues a message.
func (t *TestWriter) Out(msg *resources.Buf, lo, hi, before uint64) error {
	return t.w.out(msg, lo, hi, before)
}

// Stop ends the writer.
func (t *TestWriter) Stop() { t.w.stop() }

// OnBatch has the writer report each gathered write's message count.
func OnBatch(t *TestWriter, fn func(n int)) { t.w.onBatch = fn }

// Pause is the writer's bound as the reader sees it: a channel closed once the
// messages held fall below it, and whether they are at it.
func Pause(t *TestWriter) (<-chan struct{}, bool) { return t.w.pause() }

// Track opens and ends one request's tracking over rec, as serve and a
// request that sent nothing do.
func (p *pipeline) Track(rec *resources.Buf) { p.end(p.begin(rec), false, 0, 0, nil) }

// Sampled is the pipeline's verdict on a request whose trace ID is id, under
// a SampleRatio of ratio.
func Sampled(id trace.TraceID, ratio float64) bool { return sampled(id, sampleBound(ratio)) }

// Members walks a compound's members, as the runner does: how many decoded,
// the index of the first that did not (-1 for the compound refused whole),
// and the error.
func Members(row *Op, inv Invocation, rest []byte, v Version) (n, at int, err error) {
	ms, at, _, err := members(row, inv, rest, v)
	return len(ms), at, err
}

// Later is the headers a chain's interim is handed after the member at off,
// whose header is h, read through st's HeaderCodec.
func Later(st Stages, rec *resources.Buf, off int, h Header) []Header {
	c := &chain{p: &pipeline{st: st}, rec: rec, cur: entry{off: off, h: h}}
	return c.later(nil)
}

// Judge is judge over the members rest holds, decoded as the runner does.
func Judge(row *Op, inv Invocation, rest []byte, v Version, r *Request) (whole bool, handles []vfs.Handle) {
	ms, _, _, err := members(row, inv, rest, v)
	return judge(ms, err == nil, r)
}

// SetListen sets the socket opener s binds with, on s alone: no real bind
// can be made to answer on demand with what a Start test needs - an
// in-use refusal, or a listener whose address is no TCP one - so the test
// injects it, without any other server's bind meeting the same hook.
func SetListen(s *Server, f func(ctx context.Context, network, addr string) (net.Listener, error)) {
	s.listen = f
}

// The connection and accept seams: what the connection's and the accept
// loop's tests reach.

// ServeConn hands the server a socket, which its accept loop does in
// production, and returns the connection it serves on it.
func ServeConn(s *Server, nc net.Conn, l *Listener) *Conn { return s.serveConn(nc, l, nil) }

// ServeConnCharged hands the server a socket with the connection-state charge
// its accept loop would take, and returns the connection.
func ServeConnCharged(s *Server, nc net.Conn, l *Listener, charge *resources.Reservation) *Conn {
	return s.serveConn(nc, l, charge)
}

// SetServerSettings replaces the server's current snapshot, as Reconfigure
// does once it lands, so a test can tell a connection's own from it.
func SetServerSettings(s *Server, snap settings.Settings) { s.settings.Store(&snap) }

// SetServerClock replaces the server's shared clock with a test's.
func SetServerClock(s *Server, c Clock) { s.sh.clock = c }

// Serving moves a server to serving, as Start does once its listeners are
// bound: a test hands the server sockets itself and needs no listener.
func Serving(s *Server) {
	s.mu.Lock()
	s.state = adapter.StateServing
	s.mu.Unlock()
}

// ConnContext is a connection's context, for a test that proves a close ends
// it.
func ConnContext(c *Conn) context.Context { return c.ctx }

// ConnDone is a connection's teardown end, for a test that waits for it.
func ConnDone(c *Conn) <-chan struct{} { return c.done }

// ConnPipeline is a connection's pipeline, for a test that reads what runs.
func ConnPipeline(c *Conn) *Pipeline { return c.p }

// ConnSettings is a connection's snapshot, as Settings reports it.
func ConnSettings(c *Conn) settings.Settings { return c.Settings() }

// Kept is how many Keeps a connection holds unreleased.
func Kept(c *Conn) int32 { return c.kept.Load() }

// ConnCharge is what the accept loop charges a connection, its fixed cost to
// the connection-state budget (RFC 24 section 4.4).
func ConnCharge() int64 { return connCharge }

// Runs is how many requests or status waits the pipeline has running.
func Runs(p *Pipeline) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// AddBound appends a bound socket over ln to a serving server and starts its
// accept loop: the seam a test's listener, a failing one say, runs on. The
// socket closes and its loop ends with Stop, as Start's own do, and loopDone
// reports the loop's end.
func AddBound(s *Server, l *Listener, ln net.Listener) (loopDone <-chan struct{}) {
	b := &bound{l: l, addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0), ln: ln, stopped: make(chan struct{})}
	b.loopDone = make(chan struct{})
	s.mu.Lock()
	s.bound = append(s.bound, b)
	s.mu.Unlock()
	go s.acceptLoop(b)
	return b.loopDone
}

// WatchIdleArms hands c's idle arms to the test from now on, and returns
// what reports whether the deadline armed last is the current one, armed
// since the connection's last request started or ended: a test waits on it
// before advancing, so its advance lands after that arm and no earlier one,
// however many arms a step makes.
func WatchIdleArms(c *Conn) (current func() bool) {
	var last uint64 // under timersMu, as the hook runs
	c.timersMu.Lock()
	c.onIdleArm = func(g uint64) { last = g }
	c.timersMu.Unlock()
	return func() bool {
		c.timersMu.Lock()
		defer c.timersMu.Unlock()
		return c.idleT != nil && last == c.idleGen.Load()
	}
}

// RecordHeld reports whether the connection's reader still holds bytes of a
// record: a test waits it out before advancing, so a deadline firing into the
// record's tail closes instead of re-arming.
func RecordHeld(c *Conn) bool {
	return c.rd.holding()
}
