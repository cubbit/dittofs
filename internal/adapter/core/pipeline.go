package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
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
	sample   uint64       // Deps.SampleRatio as sampled compares it
	reply    *resources.BufPool
}

// newShared is a server's shared state over reply, the pool every reply
// buffer is taken from: its vectors registered on d.Metrics, or recording
// nothing without one, on the runtime's clock, d.Logger, or a logger that
// discards, and d.Tracer.
func newShared(d adapter.Deps, reply *resources.BufPool) *shared {
	sh := &shared{clock: runtimeClock{}, log: d.Logger, tracer: d.Tracer, sample: sampleBound(d.SampleRatio), reply: reply, panics: map[[2]string]*lineLimiter{}}
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

// reserveStatus takes the reply buffer a status alone goes out in, of n
// bytes, its header's ReplyLen, for key: at once when the pool has room with
// no waiter ahead, otherwise
// waiting within ctx, the connection's, so that a close ends the wait at
// once, for at most statusWait from when it starts. Past that it calls close
// with errReplyWait. It returns nil, with nothing reserved, when the wait
// ended. Its caller keeps the request's record, and so its receive charge,
// until the status goes out or the wait ends (RFC 24 §6.1).
func (sh *shared) reserveStatus(ctx context.Context, key netip.Addr, n int, close func(error)) *resources.Buf {
	// A status is encoded by appending to B[:0], within n's class.
	if buf, ok := sh.reply.TryGet(key, n); ok {
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
	buf, err := sh.reply.Get(wait, key, n)
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
// request's derives from; the remote address, the budgets' key; the settings
// snapshot loaded at accept; the writer's enqueue; and Conn.Close, which the
// pipeline calls where a stage says close. out never blocks its caller, takes
// msg's reference whatever it returns, fails only once the connection is
// closed, and sends a message handed with before ahead of the reply of that
// key; key is the request's HoldCodec.Key in a table that holds, else 0.
type link struct {
	ctx    context.Context
	remote netip.Addr
	set    settings.Settings // nil: none, so no operation is switched off
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
	label  *labelSeries
	name   string          // the request span's, until Route names its operation
	off    map[string]bool // the operations the snapshot switched off, by name

	misbehaved atomic.Bool // a refusal at Unwrap was logged on this connection

	ctx    context.Context // l.ctx's, ended by close
	cancel context.CancelFunc

	draining atomic.Bool
	status   chan struct{} // the one wait for a status's buffer a connection has

	mu   sync.Mutex
	runs int           // requests running and statuses waiting
	idle chan struct{} // made by a wait while runs is above 0, closed when it falls to 0
}

// newPipeline builds a connection's pipeline over stages withDefaults filled.
func newPipeline(sh *shared, st Stages, l link) *pipeline {
	p := &pipeline{sh: sh, st: st, l: l, status: make(chan struct{}, 1)}
	_, p.noHold = st.Hold.(noHold)
	p.label, p.name = sh.label(st.Protocol), "adapter."+st.Protocol
	// decision: a switched-off operation is matched by Op.Name, case aside,
	// so the spelling the op label shows works too, read once per connection
	// from the snapshot it loaded at accept, so a change reaches connections
	// set up after it, as every setting does. Overturned if an operator must
	// switch an operation off on connections already open.
	if l.set != nil {
		for _, name := range l.set.Common().DisabledOperations {
			if p.off == nil {
				p.off = map[string]bool{}
			}
			p.off[strings.ToLower(name)] = true
		}
	}
	p.ctx, p.cancel = context.WithCancel(l.ctx)
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
//
// decision: the reader runs a record's first message as far as Admit, so a
// connection at its bound is not read, and everything after runs on the
// request's own goroutine. Overturned if a stage after Admit must run on the
// reader.
//
// decision: an error raised before Route has chosen a table, at Decode,
// Unwrap or Admit, is mapped by the stages' own table; a header Decode
// returns with its error is answered and not passed to Wrap, an Unwrap
// refusal is answered wrapped under the header it returned, and Unwrap's nil
// header closes the connection, answered nothing (§5.5). Overturned if a
// protocol must answer a refusal before Route by a table it routes to.
//
// ponytail: unwrap on the reader. One connection's receive rate is capped by
// one core's decryption, about 2 GB/s for AES-GCM; multichannel and NFS
// nconnect spread a client over several readers. Move decryption to the
// worker, with admission re-checked after it, when a single-connection
// encrypted write benchmark shows the reader CPU-bound.
func (p *pipeline) serve(rec *resources.Buf) {
	tr := p.begin(rec)
	h, err := p.st.Header.Decode(rec, 0)
	if h == nil {
		p.drop(tr, rec, fmt.Errorf("%w: %v", ErrDecode, err))
		return
	}
	tr.version, tr.versioned = h.Version(), true
	if err != nil {
		// The refusal of a header that failed Decode is not passed to Wrap:
		// nothing proved the session it names, so a signature under that
		// session would hand a peer a valid checksum under a key it does not
		// hold (§5.5).
		p.refuse(tr, rec, h, stageDecodeHeader, fmt.Errorf("%w: %v", ErrDecode, err), false, h.Next() != 0)
		return
	}
	if h.Next() != 0 {
		p.drop(tr, rec, errChain)
		return
	}
	u, err := p.st.Security.Unwrap(h, rec, nil)
	if u == nil {
		if err == nil {
			// Unwrap returns a header or an error, never neither (§4.7):
			// one that breaks that closes the connection as a nil header
			// does, under core's own error.
			err = errUnwrapNeither
		}
		p.drop(tr, rec, err)
		return
	}
	if err != nil {
		p.refuse(tr, rec, u, stageUnwrap, err, true, false)
		return
	}
	if _, ok := p.st.Hold.Cancels(u); ok {
		// ponytail: the key a cancel names is dropped, since nothing holds a
		// request yet. End the hold it names, and keep an early cancel for
		// HoldCodec's while, once the hold registry lands.
		p.end(tr, false, 0, 0, nil)
		rec.Release()
		return
	}
	release, err := p.st.Flow.Admit(tr.ctx, u)
	if err != nil {
		if p.endedByClose(err) {
			p.end(tr, false, 0, 0, nil)
			rec.Release()
			return
		}
		p.refuse(tr, rec, u, stageAdmit, err, true, false)
		return
	}
	// Admit tries once under a context close ended, and may still grant: a
	// request admitted after close is not run. It counts as running before
	// close is checked, so a wait entered after a close, from any goroutine,
	// either waits for it or never sees it start.
	p.add()
	if p.ctx.Err() != nil {
		p.done()
		release()
		p.end(tr, false, 0, 0, nil)
		rec.Release()
		return
	}
	tr.label.admitted()
	tr.flight = true
	go p.run(tr, rec, u, release)
}

// drop ends a record whose stage says close: nothing is answered, and the
// connection closes with err.
func (p *pipeline) drop(tr *track, rec *resources.Buf, err error) {
	p.end(tr, false, 0, 0, err)
	if rec != nil {
		rec.Release()
	}
	p.l.close(err)
}

// errChain closes a connection whose record chains more than one message.
var errChain = errors.New("core: a record of more than one message")

// errUnwrapNeither closes a connection whose Unwrap returned no header and
// no error.
var errUnwrapNeither = errors.New("core: Unwrap returned no header and no error")

// refuse answers on the reader a refusal at stage st, before Route, as the
// stages' table maps it, under h, wrapped where wrap, and closes the
// connection after where closeAfter. Its status's buffer is tried at once;
// when the pool has no room it waits on a goroutine of its own, which keeps
// rec, and so its receive charge, until the status goes out, and a
// connection has one such wait at a time: a second refusal waits here, on
// the reader, for the first to end.
func (p *pipeline) refuse(tr *track, rec *resources.Buf, h Header, st stage, err error, wrap, closeAfter bool) {
	p.refused(tr, st)
	s, send := p.mapped(tr, p.st.Errors, err, nil, h.Version(), st != stageUnwrap)
	if st == stageUnwrap && p.misbehaved.CompareAndSwap(false, true) {
		// A client that sends what Unwrap refuses is logged once per
		// connection, not once per request (§10.3).
		p.sh.log.LogAttrs(tr.ctx, slog.LevelWarn, "a message the connection's security refused", tr.attrs(err)...)
	}
	if !send {
		p.end(tr, false, 0, 0, err)
		rec.Release()
		if closeAfter {
			p.l.close(err)
		}
		return
	}
	if buf, ok := p.sh.reply.TryGet(p.l.remote, p.st.Header.ReplyLen(h)); ok {
		p.answer(tr, rec, h, s, buf, wrap, closeAfter, err)
		return
	}
	select {
	case p.status <- struct{}{}:
	case <-p.ctx.Done():
		p.end(tr, false, 0, 0, err)
		rec.Release()
		return
	}
	// A close can make both cases ready, and the select picks one at random:
	// the refusal counts as running before close is checked, as serve's
	// request does, so a wait entered after the close never sees it start.
	p.add()
	if p.ctx.Err() != nil {
		<-p.status
		p.done()
		rec.Release()
		return
	}
	go func() {
		defer p.done()
		defer func() { <-p.status }()
		if buf := p.sh.reserveStatus(p.ctx, p.l.remote, p.st.Header.ReplyLen(h), p.l.close); buf != nil {
			p.answer(tr, rec, h, s, buf, wrap, closeAfter, err)
			return
		}
		p.end(tr, false, 0, 0, err)
		rec.Release()
	}()
}

// answer sends a refusal's status in buf, then releases rec, and closes the
// connection after where closeAfter.
func (p *pipeline) answer(tr *track, rec *resources.Buf, h Header, s Status, buf *resources.Buf, wrap, closeAfter bool, cause error) {
	defer rec.Release()
	buf.B = buf.B[:0] // a buffer comes back as long as its reservation
	if err := p.st.Header.Encode(h, s, buf); err != nil {
		buf.Release()
		p.end(tr, false, 0, 0, err)
		p.l.close(err)
		return
	}
	n := len(buf.B)
	if !wrap {
		p.out(buf, 0)
	} else {
		p.send(h, buf, 0)
	}
	p.end(tr, true, s, n, cause)
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
func (p *pipeline) run(tr *track, rec *resources.Buf, h Header, release func()) {
	defer p.done()
	defer rec.Release()
	ctx := tr.ctx
	cached, err := p.st.Replay.Lookup(ctx, h)
	if cached != nil {
		tr.label.replayHit()
		tr.cached = true
		release()
		n := len(cached.B)
		p.send(h, cached, 0)
		p.end(tr, true, 0, n, nil)
		return
	}
	if err != nil {
		if p.endedByClose(err) {
			release()
			p.end(tr, false, 0, 0, err)
			return
		}
		p.refused(tr, stageReplayCheck)
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
		// decision: a table that holds, routed on stages whose Hold was nil,
		// is a fault of the protocol's wiring, since nothing could key or
		// cancel its holds: the connection is closed naming the table, not
		// served. Overturned if a protocol must hold with no cancel.
		if ts, serr = p.sh.table(tb); serr == nil && tb.Holds && p.noHold {
			serr = fmt.Errorf("%w: the table of %s", errNoHold, opNames(tb))
		}
		if serr != nil {
			release()
			p.drop(tr, nil, serr)
			return
		}
		p.routed(tr, ts, h.Op())
		errs = tb.Errors
	}
	if r != nil {
		r.trace = tr.id
	}
	if err == nil {
		op = tb.Ops[h.Op()]
		err = p.check(op, r)
	}
	if err != nil && !tr.refused {
		p.refused(tr, stageRoute)
	}
	tr.op = op
	if err == nil {
		args := p.st.Header.Args(rec, h)
		var n int
		inv, n, err = op.Decode(args, h.Version())
		if err == nil && op.Members != nil && (n < 0 || n > len(args)) {
			err = fmt.Errorf("a compound's arguments of %d bytes, %d in its message", n, len(args))
		}
		if err != nil {
			err = fmt.Errorf("%w: %v", ErrDecode, err)
			p.refused(tr, stageDecodeArgs)
		} else if op.Members != nil {
			p.compound(tr, h, r, tb, op, inv, args[n:], release)
			return
		}
	}
	var reply *resources.Buf
	if err == nil {
		// ponytail: ctx is the connection's, which has no deadline, so the
		// reservation can wait as long as the pool stays full. Wait under the
		// attempt's deadline once the request context lands.
		start := p.sh.clock.Now()
		if reply, err = p.sh.reserveReply(ctx, p.l.remote, inv.Bound()+p.st.Header.ReplyLen(h)); err != nil {
			if st, ok := reservationStage(err); ok {
				p.refused(tr, st)
			}
		} else {
			r.resolveStart = p.sh.clock.Now()
			ts.observe(stageReplyWait, r.resolveStart.Sub(start))
		}
	}
	if err == nil {
		if err = inv.Resolve(r); err != nil {
			p.refused(tr, stageResolveIDs)
		}
	}
	if err == nil {
		// The role is asked only while draining or on an expired credential,
		// so a request served normally pays for no second lookup.
		draining, expired := p.draining.Load(), p.expired(r)
		role := op.Role
		if (draining || expired) && inv.Role != nil {
			role = inv.Role(r)
		}
		switch {
		case expired && !op.Expired && role != RoleRelease:
			err = ErrExpired
		case draining && role == RoleNone:
			err = drainErr(ctx, handlesOf(inv))
		}
		if err != nil {
			p.refused(tr, stageResolveIDs)
		}
	}
	if err == nil {
		r.invoke = inv.Call
		tr.ran = true
		err = ts.chains[op.Code](ctx, r)
	}
	p.finish(tr, h, tb, errs, op, inv, reply, err, release)
}

// finish answers a request whose call, or refusal, gave err: its status
// mapped by errs, encoded into reply or, with none reserved, a status's
// buffer, stored where its row is cacheable, and sent; or, for a NoReply
// row, nothing, or an interim where the table holds.
func (p *pipeline) finish(tr *track, h Header, tb *OpTable, errs *ErrorTable, op *Op, inv Invocation, reply *resources.Buf, err error, release func()) {
	ctx := tr.ctx
	if err != nil && p.endedByClose(err) {
		if reply != nil {
			reply.Release()
		}
		release()
		p.end(tr, false, 0, 0, err)
		return
	}
	var s Status
	send := true
	switch {
	case errors.Is(err, errPanicked):
		// decision: a recovered panic is answered the fault status without
		// Map, so it is counted once, as a panic, and never also as an error
		// no row maps. Overturned if a protocol must answer a panic otherwise.
		s = errs.fault
	case err != nil:
		s, send = p.mapped(tr, errs, err, op, h.Version(), true)
	}
	if !send {
		p.unanswered(tr, h, tb, reply, err, release)
		return
	}
	if reply == nil {
		if reply = p.sh.reserveStatus(ctx, p.l.remote, p.st.Header.ReplyLen(h), p.l.close); reply == nil {
			release()
			p.end(tr, false, 0, 0, err)
			return
		}
	}
	reply.B = reply.B[:0] // a buffer comes back as long as its reservation, for a read
	if encErr := p.encode(h, s, reply, inv); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, encErr)
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
	n := len(reply.B)
	p.send(h, reply, key)
	p.end(tr, true, s, n, err)
}

// track is what the pipeline keeps of one request for its span, its log
// lines and its counts, from serve taking its record to its reply going to
// out or being dropped.
type track struct {
	ctx       context.Context // carries the request span, where there is one
	span      trace.Span      // nil on a server with no tracer
	id        trace.TraceID   // drawn even when the request is not traced
	label     *labelSeries
	table     *tableSeries // from Route on
	protocol  string       // the label it records under: the stages', then the table's
	flight    bool         // counted in flight
	bytes     int          // the record's
	version   Version      // its header's, where one decoded
	versioned bool
	cached    bool // a replay hit, whose copy's status the pipeline never reads
	ran       bool // the service's call started, so its result is the span's
	op        *Op
	refused   bool
	stage     stage // the stage that refused it
}

// begin draws a request's trace ID as serve takes its record, which its log
// lines carry (RFC 25 §5.3), and samples the request on it (RFC 20 §10.2).
// Only a sampled request calls the tracer, so an unsampled one allocates
// nothing for tracing (RFC 25 §6.3). Its span, named after the connection's
// label until Route names its operation, takes the drawn ID through the
// root's ID generator; with a provider that draws its own, the request's
// lines carry the span's.
func (p *pipeline) begin(rec *resources.Buf) *track {
	tr := &track{ctx: p.ctx, label: p.label, protocol: p.st.Protocol, bytes: len(rec.B), id: newTraceID()}
	if p.sh.tracer != nil && sampled(tr.id, p.sh.sample) {
		tr.ctx, tr.span = p.sh.tracer.Start(adapter.ContextWithTraceID(p.ctx, tr.id), p.name)
		if id := tr.span.SpanContext().TraceID(); id.IsValid() {
			tr.id = id
		}
	}
	return tr
}

// series is what the request records into now: its connection's label's
// before Route, its table's from it.
func (tr *track) series() *protocolSeries {
	if tr.table != nil {
		return &tr.table.protocolSeries
	}
	return &tr.label.protocolSeries
}

// refused counts the request's refusal under the stage that refused it.
func (p *pipeline) refused(tr *track, st stage) {
	tr.refused, tr.stage = true, st
	tr.series().refused(st)
}

// routed moves the request to its table's series and names its span after
// the operation its header names.
func (p *pipeline) routed(tr *track, ts *tableSeries, code uint32) {
	if tr.flight {
		ts.routed(tr.label)
	}
	tr.table, tr.protocol = ts, ts.protocol
	if tr.span != nil {
		if name, ok := ts.names[code]; ok {
			tr.span.SetName(name)
		}
	}
}

// mapped is err's status in errs, counted and logged: an error no row maps at
// error, as a bug; one a row answers at debug, where log, and counted under
// its sentinel when it is the service's.
//
// decision: error level is for what no row maps and for a panic; a refusal
// or a service error a row answers is expected, and the service logs its own
// faults. RFC 20 §4.5 gives examples of each level, not a closed set.
// Overturned if an operator needs a mapped error at a level that pages.
func (p *pipeline) mapped(tr *track, errs *ErrorTable, err error, op *Op, v Version, log bool) (Status, bool) {
	s, reply, matched := errs.mapped(err, op, v)
	if !matched {
		tr.series().unmappedErr()
		p.sh.log.LogAttrs(tr.ctx, slog.LevelError, "an error no row maps", tr.attrs(err)...)
		return s, reply
	}
	tr.series().statusMapped(err)
	if log && p.sh.log.Enabled(tr.ctx, slog.LevelDebug) {
		p.sh.log.LogAttrs(tr.ctx, slog.LevelDebug, "an error its row answers", tr.attrs(err)...)
	}
	return s, reply
}

// unanswered ends a request refused with a NoReply row, err: no final
// reply, and where tb holds, an interim in reply's place.
func (p *pipeline) unanswered(tr *track, h Header, tb *OpTable, reply *resources.Buf, err error, release func()) {
	if tb != nil && tb.Holds {
		p.interim(tr.ctx, h, reply)
	} else if reply != nil {
		reply.Release()
	}
	release()
	p.end(tr, false, 0, 0, err)
}

// attrs is a request's log line's keys: its trace ID, its span ID where a
// span has one (RFC 25 §5.3), its protocol and operation, and err.
func (tr *track) attrs(err error) []slog.Attr {
	a := []slog.Attr{slog.String("trace_id", tr.id.String())}
	if tr.span != nil {
		if id := tr.span.SpanContext().SpanID(); id.IsValid() {
			a = append(a, slog.String("span_id", id.String()))
		}
	}
	a = append(a, slog.String("protocol", tr.protocol))
	if tr.op != nil {
		a = append(a, slog.String("op", tr.op.Name))
	}
	return append(a, slog.Any("error", err))
}

// end takes the request out of flight and ends its span with its
// attributes: what was sent, if anything, its status and reply size, the
// service's result, and the stage that refused it.
func (p *pipeline) end(tr *track, sent bool, s Status, reply int, err error) {
	if tr.flight {
		tr.series().answered()
		tr.flight = false
	}
	if tr.span == nil {
		return
	}
	if tr.span.IsRecording() {
		a := []attribute.KeyValue{
			attribute.String("protocol", tr.protocol),
			attribute.Int("request_bytes", tr.bytes),
		}
		if tr.versioned {
			a = append(a, attribute.Int64("version", int64(tr.version)))
		}
		if tr.op != nil {
			a = append(a, attribute.String("op", strings.ToLower(tr.op.Name)))
		}
		if sent {
			a = append(a, attribute.Int("reply_bytes", reply))
			if !tr.cached {
				a = append(a, attribute.Int64("status", int64(s)))
			}
		}
		// A result is the service's: the call ran, and returned ok or an error
		// its closed set names. A refusal before it, a cancel, a replay hit
		// and a request admitted after close ran nothing, whatever error they
		// carry.
		if i := serviceError(err); tr.ran && (err == nil || i >= 0) {
			result := "ok"
			if err != nil {
				result = serviceErrors[i].name
			}
			a = append(a, attribute.String("result", result))
		}
		if tr.refused {
			a = append(a, attribute.String("stage", stageNames[tr.stage]))
		}
		tr.span.SetAttributes(a...)
	}
	tr.span.End()
}

// endedByClose reports whether err is the connection's close ending a wait
// or a call under its context. That is no error: nothing can be sent, and the
// request is neither counted as a refusal or an error no row maps nor logged
// at error (§10.1).
func (p *pipeline) endedByClose(err error) bool {
	return p.ctx.Err() != nil && errors.Is(err, context.Canceled)
}

// reservationStage is the stage the request's reply reservation's refusal
// counts under (§10.1): reply_wait for its ErrDelay of DelayResources at the
// attempt's deadline, reply_too_big for a reply past the pool's ceiling. A
// wait a close ended is no refusal.
func reservationStage(err error) (stage, bool) {
	switch {
	case errors.Is(err, ErrReplyTooBig):
		return stageReplyTooBig, true
	case errors.Is(err, vfs.ErrDelay):
		return stageReplyWait, true
	}
	return 0, false
}

// sampleBound is ratio as sampled compares it: the tracing API's ratio rule
// scaled to 2^63, every ID at 1 or more, none at 0 or less.
func sampleBound(ratio float64) uint64 {
	switch {
	case ratio >= 1:
		return 1 << 63
	case ratio > 0:
		return uint64(ratio * (1 << 63))
	}
	return 0
}

// sampled says whether the request whose trace ID is id is traced, by the
// rule the tracing API's ratio sampler applies to the same ID: its low eight
// bytes, shifted right by one, below bound.
func sampled(id trace.TraceID, bound uint64) bool {
	return binary.BigEndian.Uint64(id[8:])>>1 < bound
}

// newTraceID draws a request's trace ID from math/rand/v2's own source, a
// per-thread generator seeded by the runtime, which every connection shares
// without a lock.
func newTraceID() (id trace.TraceID) {
	binary.BigEndian.PutUint64(id[:8], rand.Uint64())
	binary.BigEndian.PutUint64(id[8:], rand.Uint64())
	return id
}

// check is the row's checks between Route and Decode, in their order: an
// operation the table lacks or the operator switched off, its versions, its
// needs from the lowest precondition up, then the current filehandle
// (§4.2). A request it refuses never has its arguments decoded.
func (p *pipeline) check(op *Op, r *Request) error {
	if op == nil || len(p.off) > 0 && p.off[strings.ToLower(op.Name)] {
		return ErrNotSupported
	}
	if r.Version < op.Versions.First || r.Version > op.Versions.Last {
		return ErrVersion
	}
	need, level := op.Needs&^NeedsFH, r.Level&^NeedsFH
	for _, step := range []struct {
		needs Needs
		err   error
	}{{NeedsNegotiated, ErrNotNegotiated}, {NeedsSession, ErrNoSession}, {NeedsTree, ErrNoTree}} {
		if need >= step.needs && level < step.needs {
			return step.err
		}
	}
	if op.Needs&NeedsFH != 0 && r.Level&NeedsFH == 0 {
		return ErrNoFH
	}
	return nil
}

// expired says r's credential has ended, by the server's clock.
func (p *pipeline) expired(r *Request) bool {
	end := r.Call.Identity.Evidence.Expires
	return !end.IsZero() && !p.sh.clock.Now().Before(end)
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
// grants the request's credits back, in the request's own reply buffer while
// it still holds one, or else in a status's buffer, which waits for room:
// releasing the reservation first could hand its bytes to a request waiting
// on the pool and leave the interim, and so the client's credits, behind it.
func (p *pipeline) interim(ctx context.Context, h Header, buf *resources.Buf) {
	if buf == nil {
		if buf = p.sh.reserveStatus(ctx, p.l.remote, p.st.Header.ReplyLen(h), p.l.close); buf == nil {
			return
		}
	}
	buf.B = buf.B[:0]
	if err := p.st.Hold.Interim(h, true, nil, buf); err != nil {
		buf.Release()
		p.l.close(err)
		return
	}
	p.send(h, buf, 0)
}

// handlesOf is the filehandle inv's arguments name, if any.
func handlesOf(inv Invocation) []vfs.Handle {
	if inv.Handle != nil {
		if h, ok := inv.Handle(); ok {
			return []vfs.Handle{h}
		}
	}
	return nil
}

// drainErr is what a request the drain does not serve is refused with,
// decided once from every filehandle it names.
//
// decision: ErrDraining, the service's answer while no topology names a
// target for the request's share; asking the service for a move target, and
// answering ErrMoved where it names one, comes with the drain. Withdraw it
// then.
func drainErr(context.Context, []vfs.Handle) error { return ErrDraining }

// drain begins the connection's drain: from here on a request whose role is
// none is refused with drainErr.
func (p *pipeline) drain() { p.draining.Store(true) }

// wait returns nil once nothing runs and no status waits, or ctx's error.
func (p *pipeline) wait(ctx context.Context) error {
	p.mu.Lock()
	if p.runs == 0 {
		p.mu.Unlock()
		return nil
	}
	if p.idle == nil {
		p.idle = make(chan struct{})
	}
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
	p.runs++
	p.mu.Unlock()
}

func (p *pipeline) done() {
	p.mu.Lock()
	if p.runs--; p.runs == 0 && p.idle != nil {
		close(p.idle)
		p.idle = nil
	}
	p.mu.Unlock()
}
