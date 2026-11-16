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
// series, resolved once per server, its reply pool, its clock, its logger
// and its tracer.
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
	panics   map[[2]string]*lineLimiter // by protocol and operation, under mu
	clock    clock
	log      *slog.Logger
	tracer   trace.Tracer // nil: no spans
	reply    *resources.BufPool
}

// newShared is a server's shared state over reply, the pool every reply
// buffer is taken from: its vectors registered on d.Metrics, or recording
// nothing without one, on the runtime's clock, d.Logger, or a logger that
// discards, and d.Tracer.
func newShared(d adapter.Deps, reply *resources.BufPool) *shared {
	sh := &shared{clock: runtimeClock{}, log: d.Logger, tracer: d.Tracer, reply: reply, panics: map[[2]string]*lineLimiter{}}
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

// reserveReply takes a reply buffer of bound bytes from the reply pool for
// key, waiting within ctx, the attempt's. A wait that ends at ctx's deadline
// is the adapter's own refusal for a node memory pool, ErrDelay of
// DelayResources (RFC 20 §6.3), and a charge past the pool's ceiling, which
// the pool refuses at once under any ctx, is ErrReplyTooBig (RFC 20 §4.3); a
// cancellation, and any other error, is returned as it is. On an error
// nothing is reserved.
//
// ponytail: every reply is reserved from the reply pool, the small class
// included, so a small request is charged 12 KiB twice while it runs and at
// 64 requests in flight a connection over-reserves 768 KiB. Give the pool an
// unreserved small buffer, or set one aside per connection at accept for its
// statuses, when the reply budget is shown to bind before the receive budget
// does.
func (sh *shared) reserveReply(ctx context.Context, key netip.Addr, bound int) (*resources.Buf, error) {
	buf, err := sh.reply.Get(ctx, key, bound)
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
func (sh *shared) reserveStatus(ctx context.Context, key netip.Addr, close func(error)) *resources.Buf {
	// A status is encoded by appending to B[:0], within the small class.
	if buf, ok := sh.reply.TryGet(key, 0); ok {
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
	buf, err := sh.reply.Get(wait, key, 0)
	if err != nil {
		if ctx.Err() == nil {
			close(errReplyWait)
		}
		return nil
	}
	return buf
}

// link is what the pipeline reads of its connection, handed to newPipeline at
// accept and never read from Conn: the connection's context, which every
// request's derives from; the remote address, the budgets' key; the writer's
// enqueue; and Conn.Close, which the pipeline calls where a stage says close. out never blocks its caller, takes
// msg's reference whatever it returns, fails only once the connection is
// closed, and sends a message handed with before ahead of the reply of that
// key; key is the request's HoldCodec.Key in a table that holds, else 0.
type link struct {
	ctx    context.Context
	remote netip.Addr
	out    func(msg *resources.Buf, key, before uint64) error
	close  func(err error)
}

// The stages a protocol leaves nil, filled once at accept so the pipeline
// never checks for nil.
type (
	passSecurity struct{} // messages pass unchanged
	noReplay     struct{} // no replay check
	noHold       struct{} // cancels nothing; a table that holds is never run on it
)

func (passSecurity) Unwrap(h Header, _ *resources.Buf, _ Header) (Header, error) { return h, nil }
func (passSecurity) Wrap(Header, *resources.Buf) error                           { return nil }

func (noReplay) Lookup(context.Context, Header) (*resources.Buf, error) { return nil, nil }
func (noReplay) Store(Header, *resources.Buf)                           {}

func (noHold) Key(Header) uint64                                    { return 1 << 63 }
func (noHold) Interim(Header, bool, []Header, *resources.Buf) error { return errNoHold }
func (noHold) Final(h Header, _ bool) Header                        { return h }
func (noHold) Cancels(Header) (uint64, bool)                        { return 0, false }
func (noHold) Pends(Header, error) bool                             { return false }

// errNoHold closes a connection whose stages had no Hold when Route returns a
// table that holds: the protocol's wiring is at fault, and nothing could key
// the request's hold.
var errNoHold = errors.New("core: a table that holds, on stages with no Hold")

// withDefaults fills st's nil stages: Security passes messages unchanged,
// Replay checks nothing, Flow is the fixed cap of 64, Hold cancels nothing
// and OnClose does nothing. It refuses stages without Header, Router, Errors
// or Protocol, naming the field, and accept then closes the connection before
// it reads.
func withDefaults(st Stages) (Stages, error) {
	for _, req := range []struct {
		name string
		nil  bool
	}{{"Header", st.Header == nil}, {"Router", st.Router == nil}, {"Errors", st.Errors == nil}, {"Protocol", st.Protocol == ""}} {
		if req.nil {
			return Stages{}, fmt.Errorf("core: stages without %s", req.name)
		}
	}
	if st.Security == nil {
		st.Security = passSecurity{}
	}
	if st.Flow == nil {
		st.Flow = newCapped()
	}
	if st.Replay == nil {
		st.Replay = noReplay{}
	}
	if st.Hold == nil {
		st.Hold = noHold{}
	}
	if st.OnClose == nil {
		st.OnClose = func(context.Context) {}
	}
	return st, nil
}

// pipeline is one connection's request pipeline: what serve runs on, and
// what drain, wait and close act on.
//
// decision: a Conn holds one pipeline, which is handed everything it reads of
// its connection through link and reads no field of Conn, so the connection
// (its reader, writer and Conn) and the pipeline are built and tested apart,
// joined by the suite of out every writer passes. Overturned if a stage the
// pipeline runs comes to need the connection itself.
type pipeline struct {
	sh     *shared
	st     Stages
	l      link
	noHold bool // st.Hold is the default: a table that holds closes the connection

	ctx    context.Context // l.ctx's, ended by close
	cancel context.CancelFunc

	draining atomic.Bool
	status   chan struct{} // the one wait for a status's buffer a connection has

	mu   sync.Mutex
	runs int           // requests running and statuses waiting
	idle chan struct{} // closed while runs is 0
}

// newPipeline builds a connection's pipeline over stages withDefaults filled.
func newPipeline(sh *shared, st Stages, l link) *pipeline {
	p := &pipeline{sh: sh, st: st, l: l, status: make(chan struct{}, 1), idle: make(chan struct{})}
	_, p.noHold = st.Hold.(noHold)
	p.ctx, p.cancel = context.WithCancel(l.ctx)
	close(p.idle)
	return p
}

// serve takes one record, whose one reference it owns and releases exactly
// once, on every path. On the reader it decodes the first header, unwraps it,
// asks whether it cancels a hold, and admits it, blocking in Admit, so a
// connection at its bound is not read; the request runs on a goroutine of its
// own from there.
//
// decision: a record is served by its first message alone. One whose first
// header names a next, a chain, closes the connection, and one whose next is
// not a valid step is answered as a decode error and then closes it, since
// nothing past it can be read. The ceiling: a client that chains is
// disconnected. Withdraw it when the chain runner serves every member.
func (p *pipeline) serve(rec *resources.Buf) {
	h, err := p.st.Header.Decode(rec, 0)
	if h == nil {
		rec.Release()
		p.l.close(fmt.Errorf("%w: %v", ErrDecode, err))
		return
	}
	if err != nil {
		// The refusal of a header that failed Decode is not passed to Wrap:
		// nothing proved the session it names, so a signature under that
		// session would hand a peer a valid checksum under a key it does not
		// hold (§5.5).
		p.refuse(rec, h, fmt.Errorf("%w: %v", ErrDecode, err), false, h.Next() != 0)
		return
	}
	if h.Next() != 0 {
		rec.Release()
		p.l.close(errChain)
		return
	}
	u, err := p.st.Security.Unwrap(h, rec, nil)
	if u == nil {
		rec.Release()
		p.l.close(err)
		return
	}
	if err != nil {
		p.refuse(rec, u, err, true, false)
		return
	}
	if _, ok := p.st.Hold.Cancels(u); ok {
		rec.Release()
		return
	}
	release, err := p.st.Flow.Admit(p.ctx, u)
	if err != nil {
		p.refuse(rec, u, err, true, false)
		return
	}
	p.add()
	go p.run(rec, u, release)
}

// errChain closes a connection whose record chains more than one message.
var errChain = errors.New("core: a record of more than one message")

// refuse answers on the reader a refusal before Route, as the stages' table
// maps it, under h, wrapped where wrap, and closes the connection after where
// closeAfter. Its status's buffer is tried at once; when the pool has no room
// it waits on a goroutine of its own, which keeps rec, and so its receive
// charge, until the status goes out, and a connection has one such wait at a
// time: a second refusal waits here, on the reader, for the first to end.
func (p *pipeline) refuse(rec *resources.Buf, h Header, err error, wrap, closeAfter bool) {
	s, send := p.st.Errors.Map(err, nil, h.Version())
	if !send {
		rec.Release()
		if closeAfter {
			p.l.close(err)
		}
		return
	}
	if buf, ok := p.sh.reply.TryGet(p.l.remote, 0); ok {
		p.answer(rec, h, s, buf, wrap, closeAfter, err)
		return
	}
	select {
	case p.status <- struct{}{}:
	case <-p.ctx.Done():
		rec.Release()
		return
	}
	p.add()
	go func() {
		defer p.done()
		defer func() { <-p.status }()
		if buf := p.sh.reserveStatus(p.ctx, p.l.remote, p.l.close); buf != nil {
			p.answer(rec, h, s, buf, wrap, closeAfter, err)
			return
		}
		rec.Release()
	}()
}

// answer sends a refusal's status in buf, then releases rec, and closes the
// connection after where closeAfter.
func (p *pipeline) answer(rec *resources.Buf, h Header, s Status, buf *resources.Buf, wrap, closeAfter bool, cause error) {
	defer rec.Release()
	if err := p.st.Header.Encode(h, s, buf); err != nil {
		buf.Release()
		p.l.close(err)
		return
	}
	if !wrap {
		p.out(buf, 0)
	} else {
		p.send(h, buf, 0)
	}
	if closeAfter {
		p.l.close(cause)
	}
}

// out hands a message to the writer, which takes its reference, or, once the
// pipeline is closed, drops it: a closed connection's replies are not sent.
func (p *pipeline) out(msg *resources.Buf, key uint64) {
	if p.ctx.Err() != nil {
		msg.Release()
		return
	}
	_ = p.l.out(msg, key, 0)
}

// run is one admitted request from the replay check to out, on its own
// goroutine, in §4.7's order. A refusal is mapped by the stages' table until
// Route returns one, and by that table after.
func (p *pipeline) run(rec *resources.Buf, h Header, release func()) {
	defer p.done()
	defer rec.Release()
	ctx := p.ctx
	cached, err := p.st.Replay.Lookup(ctx, h)
	if cached != nil {
		release()
		p.send(h, cached, 0)
		return
	}
	errs := p.st.Errors
	var (
		tb  *OpTable
		ts  *tableSeries
		r   *Request
		op  *Op
		inv Invocation
	)
	if err == nil {
		tb, r, err = p.st.Router.Route(h, nil)
	}
	if tb != nil {
		var serr error
		if ts, serr = p.sh.table(tb); serr == nil && tb.Holds && p.noHold {
			serr = errNoHold
		}
		if serr != nil {
			release()
			p.l.close(serr)
			return
		}
		errs = tb.Errors
	}
	if err == nil {
		if op = tb.Ops[h.Op()]; op == nil {
			err = ErrNotSupported
		}
	}
	if err == nil {
		if inv, _, err = op.Decode(p.st.Header.Args(rec, h), h.Version()); err != nil {
			err = fmt.Errorf("%w: %v", ErrDecode, err)
		}
	}
	var reply *resources.Buf
	if err == nil {
		reply, err = p.sh.reserveReply(ctx, p.l.remote, inv.Bound()+p.st.Header.ReplyLen(h))
	}
	if err == nil {
		r.resolveStart = p.sh.clock.Now()
		err = inv.Resolve(r)
	}
	if err == nil && p.draining.Load() {
		role := op.Role
		if inv.Role != nil {
			role = inv.Role(r)
		}
		if role == RoleNone {
			err = drainErr(ctx, r, inv)
		}
	}
	if err == nil {
		r.invoke = inv.Call
		err = ts.chains[op.Code](ctx, r)
	}

	var s Status
	send := true
	switch {
	case errors.Is(err, errPanicked):
		s = errs.fault
	case err != nil:
		s, send = errs.Map(err, op, h.Version())
	}
	if !send {
		if reply != nil {
			reply.Release()
		}
		if tb != nil && tb.Holds {
			p.interim(h)
		}
		release()
		return
	}
	if reply == nil {
		if reply = p.sh.reserveStatus(ctx, p.l.remote, p.l.close); reply == nil {
			release()
			return
		}
	}
	reply.B = reply.B[:0] // a buffer comes back as long as its reservation, for a read
	if err := p.encode(h, s, reply, inv); err != nil {
		reply.Release()
		release()
		p.l.close(err)
		return
	}
	release()
	if op != nil && op.Cacheable {
		p.st.Replay.Store(h, reply)
	}
	var key uint64
	if tb != nil && tb.Holds {
		key = p.st.Hold.Key(h)
	}
	p.send(h, reply, key)
}

// encode appends the reply header with status s and, where the arguments
// decoded, the row's reply, which encodes the status alone for a request
// refused before its call.
func (p *pipeline) encode(h Header, s Status, reply *resources.Buf, inv Invocation) error {
	if err := p.st.Header.Encode(h, s, reply); err != nil {
		return err
	}
	if inv.Encode != nil {
		return inv.Encode(reply, h.Version())
	}
	return nil
}

// send wraps reply under h and hands it to out.
func (p *pipeline) send(h Header, reply *resources.Buf, key uint64) {
	if err := p.st.Security.Wrap(h, reply); err != nil {
		reply.Release()
		p.l.close(err)
		return
	}
	p.out(reply, key)
}

// interim sends the interim a NoReply row gets in a table that holds, which
// grants the request's credits back; with no buffer for it at once, none.
func (p *pipeline) interim(h Header) {
	buf, ok := p.sh.reply.TryGet(p.l.remote, 0)
	if !ok {
		return
	}
	if err := p.st.Hold.Interim(h, true, nil, buf); err != nil {
		buf.Release()
		p.l.close(err)
		return
	}
	p.send(h, buf, 0)
}

// drainErr is what a request the drain does not serve is refused with.
//
// decision: ErrDraining, the service's answer while no topology names a
// target for the request's share; asking the service for a move target, and
// answering ErrMoved where it names one, comes with the drain. Withdraw it
// then.
func drainErr(context.Context, *Request, Invocation) error { return ErrDraining }

// drain begins the connection's drain: from here on a request whose role is
// none is refused with drainErr.
func (p *pipeline) drain() { p.draining.Store(true) }

// wait returns nil once nothing runs and no status waits, or ctx's error.
func (p *pipeline) wait(ctx context.Context) error {
	p.mu.Lock()
	idle := p.idle
	p.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close starts the teardown and returns at once, so it may be entered from a
// handler's own goroutine and from the reader: it ends the pipeline's
// context, which ends every wait a request or a status is in.
func (p *pipeline) close() { p.cancel() }

// add counts a request or a status wait starting; done, one ending.
func (p *pipeline) add() {
	p.mu.Lock()
	if p.runs == 0 {
		p.idle = make(chan struct{})
	}
	p.runs++
	p.mu.Unlock()
}

func (p *pipeline) done() {
	p.mu.Lock()
	if p.runs--; p.runs == 0 {
		close(p.idle)
	}
	p.mu.Unlock()
}
