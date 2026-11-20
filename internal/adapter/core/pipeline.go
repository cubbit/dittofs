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
// series, resolved once per server, its reply pool, its clock, its logger,
// its tracer and the service a drain asks for a move target.
//
// decision: the series are kept here, never in a table: two servers may
// share a table, and a node's servers share its one registry. A table's are
// resolved at NewServer, for every table its listeners' protocols list, into
// a map no request writes; a label's at the first accept that names it, into
// a copy-on-write map that mu fills. Every request reads both with no lock
// and no label lookup. Overturned if a profile shows a first label
// resolution's copy to matter, which it cannot at a handful of labels.
type shared struct {
	fam          [vecs]family
	wire         histFamily
	holdPool     adder                     // no protocol label
	tables       map[*OpTable]*tableSeries // filled at NewServer, then only read
	panics       map[[2]string]*frameLines // by protocol and operation, filled with tables
	mu           sync.Mutex
	labels       atomic.Pointer[map[string]*labelSeries]
	closes       map[reason]*lineLimiter     // a close at accept's line, by reason, under mu
	addressLines map[netip.Addr]*lineLimiter // the per_address close at accept's line, by address, under mu
	clock        clock
	log          *slog.Logger
	tracer       trace.Tracer         // nil: no spans
	sample       uint64               // Deps.SampleRatio as sampled compares it
	traceIDs     func() trace.TraceID // Deps.TraceIDs, or newTraceID where it is nil
	reply        *resources.BufPool
	shares       vfs.Shares        // Deps.VFS
	hold         *resources.Budget // the node's hold pool; nil: none, so the floor bounds every connection's holds
	// The floor each connection's held requests count against before the
	// hold pool, in requests and in their records' receive charge (§4.6).
	floorRequests int
	floorBytes    int64
}

// newShared is a server's shared state over reply, the pool every reply
// buffer is taken from: its vectors registered on d.Metrics, or recording
// nothing without one, on the runtime's clock, d.Logger, or a logger that
// discards, d.Tracer, d.TraceIDs and d.VFS; held requests past a connection's
// floor of 256 requests and 32 MiB go to d.Budgets' hold pool (§4.6).
func newShared(d adapter.Deps, reply *resources.BufPool) *shared {
	sh := &shared{clock: runtimeClock{}, log: d.Logger, tracer: d.Tracer, shares: d.VFS, sample: sampleBound(d.SampleRatio), traceIDs: d.TraceIDs, reply: reply, tables: map[*OpTable]*tableSeries{}, panics: map[[2]string]*frameLines{}, closes: map[reason]*lineLimiter{}, addressLines: map[netip.Addr]*lineLimiter{}, floorRequests: 256, floorBytes: 32 << 20}
	if d.Budgets != nil {
		sh.hold = d.Budgets.Hold
	}
	if sh.log == nil {
		sh.log = slog.New(slog.DiscardHandler)
	}
	if sh.traceIDs == nil {
		sh.traceIDs = newTraceID
	}
	sh.fam, sh.wire = newFamilies(d.Metrics)
	sh.holdPool = sh.fam[vHoldPoolBytes]()
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
// key: at once when the pool has room with no waiter ahead, otherwise
// waiting until the deadline of ctx's attempt, so a pool held full ends the
// wait (RFC 20 §6.4; RFC 24 §3.3). A wait that ends at that deadline is the
// adapter's own refusal for a node memory pool, ErrDelay of DelayResources
// (RFC 20 §6.3), and a charge past the pool's ceiling, which the pool
// refuses at once under any ctx, is ErrReplyTooBig (RFC 20 §4.3); a
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
	// A reply that fits at once arms no deadline.
	if buf, ok := sh.reply.TryGet(key, bound); ok {
		return buf, nil
	}
	ctx, cancel := sh.attempt(ctx)
	defer cancel()
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
// reply reservation yet, waits for the connection's one status wait, where it
// takes it, and then for its buffer: one 30 s from when it starts waiting
// (RFC 20 §6.4; RFC 24 §2.6).
const statusWait = 30 * time.Second

// errStatusWait is what a connection is closed with when a status still
// waited for the connection's one status wait at statusWait; its close is
// counted as status_wait (RFC 20 §10.1).
var errStatusWait = errors.New("core: no status wait for a status within " + statusWait.String())

// errReplyWait is what a connection is closed with when a status found no
// reply buffer within statusWait; its close is counted as reply_wait
// (RFC 20 §10.1).
var errReplyWait = errors.New("core: no reply buffer for a status within " + statusWait.String())

// takeStatus takes the connection's one status wait for a status: at once
// when it is free, otherwise waiting within ctx, the connection's, for at
// most statusWait. Past that, still waiting, it calls close with
// errStatusWait. It reports whether it took the wait, and the deadline the
// status's wait for its buffer keeps: statusWait from when this wait
// started, or zero where it took the wait at once.
//
// decision: a status that finds the status wait free reads no clock and
// arms no timer, so its 30 s starts at its wait for its buffer, which for a
// refusal alone or a chain's first member is a goroutine's start after it
// found the pool full. Overturned if that start is seen to matter: read the
// clock where the status finds no room.
func (p *pipeline) takeStatus(ctx context.Context) (at time.Time, ok bool) {
	select {
	case p.status <- struct{}{}:
		return time.Time{}, true
	default:
	}
	at = p.sh.clock.Now().Add(statusWait)
	wait, cancel := withClockDeadline(ctx, p.sh.clock, at)
	defer cancel()
	select {
	case p.status <- struct{}{}:
		return at, true
	case <-wait.Done():
		if ctx.Err() == nil {
			p.l.close(errStatusWait)
		}
		return time.Time{}, false
	}
}

// reserveStatus takes the reply buffer a status alone goes out in, of n
// bytes, its header's ReplyLen, for key: at once when the pool has room with
// no waiter ahead, otherwise
// waiting within ctx, the connection's, so that a close ends the wait at
// once, until at, the deadline takeStatus returned, or for at most
// statusWait from when it starts where at is zero. Past that it calls close
// with errReplyWait. It returns nil, with nothing reserved, when the wait
// ended. Its caller keeps the request's record, and so its receive charge,
// until the status goes out or the wait ends (RFC 24 §6.1).
func (sh *shared) reserveStatus(ctx context.Context, key netip.Addr, n int, at time.Time, close func(error)) *resources.Buf {
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
	if at.IsZero() {
		at = sh.clock.Now().Add(statusWait)
	}
	wait, cancel := withClockDeadline(ctx, sh.clock, at)
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
// closed, and sends a message handed with before ahead of the first queued
// message whose keys span it, lo <= before <= hi. lo and hi are the least and
// greatest HoldCodec.Key of the replies a message carries, in a table that
// holds: equal for a reply alone, 0 for none.
//
// decision: a message is queued under the span of its replies' keys rather
// than each key, so a chain's record costs no allocation. A callback whose
// before falls inside a record's span without naming one of its members goes
// ahead of that record too, earlier than it must, which breaks no order
// RFC 20 section 6.2 keeps. Overturned if a callback must ever stay behind a
// reply it does not name.
type link struct {
	ctx    context.Context
	remote netip.Addr
	set    settings.Settings // nil: none, so no operation is switched off
	record int               // the protocol's record cap, Framer.MaxRecord's bytes: a chain's record and a compound's reply stay within it
	out    func(msg *resources.Buf, lo, hi, before uint64) error
	close  func(err error)
	// closeAfterSent is close, the records already handed to out sent
	// first: a chain's bad step's (§4.7).
	closeAfterSent func(err error)
	idle           func() // called at each moment nothing runs any more
	turn           func() // called when a request starts or ends, under the pipeline's mutex, so a connection's idle deadline knows its arm was superseded
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

// errWiring is what every wiring fault wraps, the faults about a protocol's
// shape core finds at accept or while serving: stages without a required
// field, a Hold a holding table needs, a stage that fails where its stage
// list hands it work (§4.7, §10.1's wiring).
var errWiring = errors.New("core: a wiring fault")

// errNoHold closes a connection whose stages had no Hold when Route returns a
// table that holds: the protocol's wiring is at fault, and nothing could key
// the request's hold.
var errNoHold = fmt.Errorf("%w: a table that holds, on stages with no Hold", errWiring)

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
			return Stages{}, fmt.Errorf("%w: stages without %s", errWiring, req.name)
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
	times  [stages]bool    // the stages the connection times: timed, but those its protocol left nil

	misbehaved atomic.Bool // a refusal at Unwrap was logged on this connection

	ctx    context.Context // l.ctx's, ended by close
	cancel context.CancelFunc

	draining atomic.Bool
	status   chan struct{} // the one wait for a status's buffer a connection has

	mu   sync.Mutex
	runs int           // requests running, held or parked, and statuses waiting
	idle chan struct{} // made by a wait while runs is above 0, closed when it falls to 0

	pend pends
}

// newPipeline builds a connection's pipeline over stages withDefaults filled.
func newPipeline(sh *shared, st Stages, l link) *pipeline {
	p := &pipeline{sh: sh, st: st, l: l, status: make(chan struct{}, 1)}
	_, p.noHold = st.Hold.(noHold)
	for _, s := range timed {
		p.times[s] = true
	}
	// A stage the protocol left nil runs core's default, which is neither
	// timed nor observed (§10.1).
	_, defaultUnwrap := st.Security.(passSecurity)
	_, defaultAdmit := st.Flow.(capped)
	_, defaultReplay := st.Replay.(noReplay)
	p.times[stageUnwrap], p.times[stageAdmit], p.times[stageReplayCheck] = !defaultUnwrap, !defaultAdmit, !defaultReplay
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
// A record whose first header names a next is a chain, which serveChain
// takes from there (§4.7).
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
	p.lap(tr, stageDecodeHeader)
	if h == nil {
		p.drop(tr, rec, fmt.Errorf("%w: %v", ErrDecode, err))
		return
	}
	tr.version, tr.versioned = h.Version(), true
	if h.Next() != 0 {
		p.serveChain(tr, rec, h, err)
		return
	}
	if err != nil {
		// The refusal of a header that failed Decode is not passed to Wrap:
		// nothing proved the session it names, so a signature under that
		// session would hand a peer a valid checksum under a key it does not
		// hold (§5.5).
		p.refuse(tr, rec, h, stageDecodeHeader, fmt.Errorf("%w: %v", ErrDecode, err), false, false)
		return
	}
	u, err := p.st.Security.Unwrap(h, rec, nil)
	p.lap(tr, stageUnwrap)
	if u == nil {
		if err == nil {
			// Unwrap returns a header or an error, never neither (§4.7):
			// one that breaks that closes the connection as a nil header
			// does, under core's own error.
			err = errUnwrapNeither
		} else {
			err = fmt.Errorf("%w: %w", errUnwrapClose, err)
		}
		p.drop(tr, rec, err)
		return
	}
	if err != nil {
		p.refuse(tr, rec, u, stageUnwrap, err, true, false)
		return
	}
	if key, ok := p.st.Hold.Cancels(u); ok {
		// A cancel gets no reply of its own (§4.7).
		p.end(tr, false, 0, 0, nil)
		rec.Release()
		p.endHold(key)
		return
	}
	release, err := p.st.Flow.Admit(tr.ctx, u)
	p.lap(tr, stageAdmit)
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

// errUnwrapClose wraps what a connection closed for an Unwrap that returned
// no header carries, so the close counts under unwrap whatever the protocol's
// own error is: the error Unwrap returned with it, or core's own for one that
// returned neither (§10.1).
var errUnwrapClose = errors.New("core: Unwrap returned no header")

// errUnwrapNeither is the error core closes with where Unwrap returned no
// header and no error at all.
var errUnwrapNeither = fmt.Errorf("%w and no error", errUnwrapClose)

// stageFault wraps a stage's own failure where its stage list handed it work,
// so the connection it closes counts under wiring (§10.1).
func stageFault(err error) error { return fmt.Errorf("%w: %v", errWiring, err) }

// mapRefusal counts a refusal before Route at stage st, a request alone's or
// a chain member's, and maps err under h as the stages' table maps it, its
// map_error timed to here: its status, and whether it is sent.
func (p *pipeline) mapRefusal(tr *track, h Header, st stage, err error) (Status, bool) {
	p.refused(tr, st)
	s, send, _ := p.mapped(tr, p.st.Errors, err, nil, h.Version(), st != stageUnwrap)
	if st == stageUnwrap && p.misbehaved.CompareAndSwap(false, true) {
		// A client that sends what Unwrap refuses is logged once per
		// connection, not once per request (§10.3).
		p.sh.log.LogAttrs(tr.ctx, slog.LevelWarn, "a message the connection's security refused", tr.attrs(err)...)
	}
	p.lap(tr, stageMapError)
	return s, send
}

// refuse answers on the reader a refusal at stage st, before Route, as the
// stages' table maps it, under h, wrapped where wrap, and closes the
// connection after where closeAfter. Its status's buffer is tried at once;
// when the pool has no room it waits on a goroutine of its own, which keeps
// rec, and so its receive charge, until the status goes out, and a
// connection has one such wait at a time: a second refusal waits here, on
// the reader, for the first to end, its statusWait running from there on
// through its wait for its buffer.
func (p *pipeline) refuse(tr *track, rec *resources.Buf, h Header, st stage, err error, wrap, closeAfter bool) {
	s, send := p.mapRefusal(tr, h, st, err)
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
	at, ok := p.takeStatus(p.ctx)
	if !ok {
		p.end(tr, false, 0, 0, err)
		rec.Release()
		return
	}
	// The status wait can be taken after a close: the refusal counts as
	// running before close is checked, as serve's request does, so a wait
	// entered after the close never sees it start.
	p.add()
	if p.ctx.Err() != nil {
		<-p.status
		p.end(tr, false, 0, 0, err)
		p.done()
		rec.Release()
		return
	}
	go func() {
		defer p.done()
		defer func() { <-p.status }()
		if buf := p.sh.reserveStatus(p.ctx, p.l.remote, p.st.Header.ReplyLen(h), at, p.l.close); buf != nil {
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
		p.l.close(stageFault(err))
		return
	}
	p.lap(tr, stageEncode)
	n := len(buf.B)
	var sent bool
	if !wrap {
		sent = p.out(buf, 0, 0)
		p.lap(tr, stageSend)
	} else {
		sent = p.send(tr, h, buf, 0)
	}
	p.end(tr, sent, s, n, cause)
	if closeAfter {
		p.l.close(cause)
	}
}

// out hands a message to the writer, which takes its reference, or, once the
// pipeline is closed, drops it: a closed connection's replies are not sent.
// lo and hi are the least and greatest keys of the replies it carries. It
// reports whether the writer took it, so a dropped reply's span is not ended
// as sent (§10.2).
func (p *pipeline) out(msg *resources.Buf, lo, hi uint64) bool {
	if p.ctx.Err() != nil {
		msg.Release()
		return false
	}
	return p.l.out(msg, lo, hi, 0) == nil
}

// run is one admitted request alone, on its own goroutine, from the replay
// check to out.
func (p *pipeline) run(tr *track, rec *resources.Buf, h Header, release func()) {
	defer p.done()
	defer rec.Release()
	p.message(tr, rec, h, nil, release, alone{p})
}

// answerer is where one message's answer goes, and how its reply is
// reserved: a request alone goes out at once, and a chain's member into its
// chain's record (§4.7).
type answerer interface {
	// reserve takes the reply buffer of n bytes the message's call answers in.
	reserve(ctx context.Context, n int) (*resources.Buf, error)
	// status takes the buffer of n bytes a status alone goes out in, or
	// none once its wait has ended.
	status(ctx context.Context, n int) *resources.Buf
	// lookup asks the replay cache for h's reply.
	lookup(tr *track, h Header) (*resources.Buf, error)
	// halted says the message is to run no further: its chain stopped, its
	// connection closed by a split, while it looked up or reserved.
	halted() bool
	// answered takes the message's encoded reply, its key, its status and
	// whether that status is a failure (RFC 20 §4.3).
	answered(tr *track, h Header, reply *resources.Buf, key uint64, s Status, failed bool, err error)
	// unanswered ends a message a NoReply row answered.
	unanswered(tr *track, h Header, tb *OpTable, reply *resources.Buf, err error, release func())
	// pended sends the interim of a message that went pending, keyed key,
	// with the replies answered before it.
	pended(h Header, interim *resources.Buf, key uint64)
	// resume runs answer, the end of a message held between attempts, and
	// what comes after it: for a chain's member, the members after it.
	resume(answer func())
}

// alone answers a request that is no chain's member.
type alone struct{ p *pipeline }

func (a alone) reserve(ctx context.Context, n int) (*resources.Buf, error) {
	return a.p.sh.reserveReply(ctx, a.p.l.remote, n)
}

func (a alone) status(ctx context.Context, n int) *resources.Buf {
	return a.p.sh.reserveStatus(ctx, a.p.l.remote, n, time.Time{}, a.p.l.close)
}

// lookup asks the replay cache under an ended context first, which tries its
// copy's reservation once; where that would wait, it asks again under the
// attempt's deadline, which the copy's reservation waits under (§4.7, §6.4).
//
// decision: a request alone asks first under an ended context too, as a
// chain's member does, so a request whose copy fits at once, or that has
// none, arms no deadline: every request asks, on a protocol with no cache
// too. Overturned if a protocol's cache must be asked once per request.
func (a alone) lookup(tr *track, h Header) (*resources.Buf, error) {
	cached, err := a.p.st.Replay.Lookup(tr.ended(), h)
	if !copyWaits(err) {
		return cached, err
	}
	ctx, cancel := a.p.sh.attempt(tr.ctx)
	defer cancel()
	return a.p.st.Replay.Lookup(ctx, h)
}

// copyWaits says a Lookup under an ended context refused because its copy's
// reservation would wait (§4.7).
func copyWaits(err error) bool {
	var d *vfs.Delay
	return errors.As(err, &d) && d.Cause == vfs.DelayResources
}

func (alone) halted() bool { return false }

func (a alone) answered(tr *track, h Header, reply *resources.Buf, key uint64, s Status, _ bool, err error) {
	n := len(reply.B)
	a.p.end(tr, a.p.send(tr, h, reply, key), s, n, err)
}

func (a alone) unanswered(tr *track, h Header, tb *OpTable, reply *resources.Buf, err error, release func()) {
	a.p.unanswered(tr, h, tb, reply, err, release)
}

func (a alone) pended(h Header, interim *resources.Buf, key uint64) { a.p.send(nil, h, interim, key) }

func (alone) resume(answer func()) { answer() }

// message runs one admitted message from the replay check to its answer, in
// §4.7's order, under prev, the request of the member before a related one,
// and answers it through a. A refusal is mapped by the stages' table until
// Route returns one, and by that table after. It returns the message's
// request, for a related member after it, and whether it is held between
// attempts, which its answer then ends.
func (p *pipeline) message(tr *track, rec *resources.Buf, h Header, prev *Request, release func(), a answerer) (*Request, bool) {
	ctx := tr.ctx
	cached, err := a.lookup(tr, h)
	p.lap(tr, stageReplayCheck)
	if cached != nil {
		tr.label.replayHit()
		tr.cached = true
		release()
		a.answered(tr, h, cached, 0, 0, false, nil)
		return nil, false
	}
	if err != nil {
		if p.endedByClose(err) {
			release()
			p.end(tr, false, 0, 0, err)
			return nil, false
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
		tb, r, err = p.st.Router.Route(h, prev)
	}
	if tb != nil {
		var serr error
		// decision: a table that holds, routed on stages whose Hold was nil,
		// is a fault of the protocol's wiring, since nothing could key or
		// cancel its holds: the connection is closed naming the table, not
		// served. Overturned if a protocol must hold with no cancel.
		if ts, serr = p.sh.table(tb, h.Version()); serr == nil && tb.Holds && p.noHold {
			serr = fmt.Errorf("%w: the table of %s", errNoHold, opNames(tb))
		}
		if serr != nil {
			release()
			p.drop(tr, nil, serr)
			return nil, false
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
		p.lap(tr, stageRoute)
	}
	tr.op = op
	if err == nil {
		// route's run ends here: observed below, or by a compound with its
		// members' runs, its decode_args going on through their walk (§10.1).
		route := p.since(tr)
		args := p.st.Header.Args(rec, h)
		var n int
		inv, n, err = op.Decode(args, h.Version())
		if err == nil && op.Members != nil && (n < 0 || n > len(args)) {
			err = fmt.Errorf("a compound's arguments of %d bytes, %d in its message", n, len(args))
		}
		if _, ok := a.(alone); ok && err == nil && op.Members != nil {
			p.compound(tr, h, r, tb, op, inv, args[n:], route, release)
			return r, false
		}
		tr.series().observe(stageRoute, route)
		p.lap(tr, stageDecodeArgs)
		if err != nil {
			err = fmt.Errorf("%w: %v", ErrDecode, err)
			p.refused(tr, stageDecodeArgs)
		} else if op.Members != nil {
			// decision: a compound row reached as a chain's member is
			// refused not supported: its one reply cannot join the chain's
			// record, and no protocol chains one (NFSv4 does not chain, and
			// SMB has no compound row). Overturned if a protocol must.
			err = fmt.Errorf("%w: a compound row as a chain's member", ErrNotSupported)
			inv = Invocation{} // refused whole, as compound refuses it: its status alone
			p.refused(tr, stageRoute)
		}
	}
	var reply *resources.Buf
	if err == nil {
		if tb.Holds {
			// Each call carries the clone wait's hold: the deadline the row
			// mapping it returns, evaluated at the request's arrival with the
			// hold's start at it, or none where that row has no Hold (§4.6).
			tr.req.holds = true
			if bound := tb.Errors.hold(cloneWait, op, h.Version()); bound != nil {
				tr.req.clone = bound(cloneWait, tr.req.start)
			}
		}
		reply, err = p.reserve(tr, a, inv, h, r)
	}
	if a.halted() {
		// Its chain stopped while it looked up or reserved, its connection
		// closed: it neither calls nor answers.
		if reply != nil {
			reply.Release()
		}
		release()
		p.end(tr, false, 0, 0, nil)
		return r, false
	}
	if err == nil {
		err = p.resolve(tr, r, op, inv, p.draining.Load())
	}
	var pd *pending
	if err == nil {
		r.invoke = inv.Call
		tr.ran = true
		if tb.Holds {
			pd = p.watch(tr, rec, h, r, tb, ts, op, inv, a)
			pd.reply = reply
		}
		err = ts.chains[op.Code](ctx, r)
		p.called(tr, r)
	}
	if pd != nil {
		if p.parked(pd, release) {
			return r, false
		}
		var held bool
		if held, err = p.after(pd, err, reply, release); held {
			return r, true
		}
	}
	p.finish(tr, h, tb, errs, op, inv, reply, err, release, a, pd)
	if pd != nil {
		p.unpend(pd)
	}
	return r, false
}

// reserve takes the reply buffer of h's attempt through a, its wait timed
// and its refusal counted. resolve_ids starts where it ends.
func (p *pipeline) reserve(tr *track, a answerer, inv Invocation, h Header, r *Request) (*resources.Buf, error) {
	n, _ := inv.Bound(r, p.l.record-p.st.Header.ReplyLen(h))
	reply, err := a.reserve(tr.ctx, n+p.st.Header.ReplyLen(h))
	if err != nil {
		if st, ok := reservationStage(err); ok {
			p.refused(tr, st)
			p.lapAttempt(tr, st)
		}
		return nil, err
	}
	p.lapAttempt(tr, stageReplyWait)
	return reply, nil
}

// resolve runs inv's Resolve, then the expired credential's check and, where
// draining, the drain's, each refusal counted and timed under resolve_ids,
// which on no refusal the call's start ends (§4.4, §3.1, §10.1).
func (p *pipeline) resolve(tr *track, r *Request, op *Op, inv Invocation, draining bool) error {
	err := inv.Resolve(r)
	if err == nil {
		// The role is asked only while draining or on an expired credential,
		// so a request served normally pays for no second lookup.
		expired := p.expired(r)
		role := op.Role
		if draining || expired {
			role = roleOf(op, inv, r)
		}
		switch {
		case expired && !op.Expired && role != RoleRelease:
			err = ErrExpired
		case draining && role == RoleNone:
			err = p.drainErr(tr.ctx, r.Call, handlesOf(inv))
		}
	}
	if err != nil {
		p.refused(tr, stageResolveIDs)
		p.lapAttempt(tr, stageResolveIDs)
	}
	return err
}

// finish answers a request whose call, or refusal, gave err: its status
// mapped by errs, encoded into reply or, with none reserved, a status's
// buffer, stored where its row is cacheable, and handed to a; or, for a
// NoReply row, a's unanswered. A request pd says is pending is answered
// under Final's header. A request whose context the connection's close
// cancelled is dropped instead: its reservations released, nothing encoded,
// and its span ended unsent (§3.1, §6.5). Its error is still mapped, unless
// it is the close's own cancel, which is no error.
func (p *pipeline) finish(tr *track, h Header, tb *OpTable, errs *ErrorTable, op *Op, inv Invocation, reply *resources.Buf, err error, release func(), a answerer, pd *pending) {
	ctx := tr.ctx
	var s Status
	send, notFailure := true, false
	switch {
	case p.endedByClose(err):
		// The close's own cancel is mapped by no row, counted and logged by
		// none.
	case errors.Is(err, errPanicked):
		// decision: a recovered panic is answered the fault status without
		// Map, so it is counted once, as a panic, and never also as an error
		// no row maps. Overturned if a protocol must answer a panic otherwise.
		s = errs.fault
	case err != nil:
		s, send, notFailure = p.mapped(tr, errs, err, op, h.Version(), true)
	}
	p.lap(tr, stageMapError)
	if ctx.Err() != nil {
		if reply != nil {
			reply.Release()
		}
		release()
		p.end(tr, false, 0, 0, err)
		return
	}
	if !send {
		a.unanswered(tr, h, tb, reply, err, release)
		return
	}
	if reply == nil {
		if reply = a.status(ctx, p.st.Header.ReplyLen(h)); reply == nil {
			release()
			p.end(tr, false, 0, 0, err)
			return
		}
	}
	eh := h
	if pd != nil && pd.pend {
		eh = p.st.Hold.Final(h, pd.grant)
	}
	reply.B = reply.B[:0] // a buffer comes back as long as its reservation, for a read
	if encErr := p.encode(eh, s, reply, inv); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, stageFault(encErr))
		return
	}
	p.lap(tr, stageEncode)
	release()
	if op != nil && op.Cacheable {
		p.st.Replay.Store(h, reply)
	}
	var key uint64
	if tb != nil && tb.Holds {
		key = p.st.Hold.Key(h)
	}
	if pd != nil && err == ErrNotLast {
		// Its status is ErrNotLast's; its span's result is the error the
		// service returned (§4.7, §10.2).
		err = pd.last
	}
	a.answered(tr, eh, reply, key, s, s != 0 && !notFailure, err)
}

// track is what the pipeline keeps of one request for its span, its log
// lines and its counts, from serve taking its record to its reply going to
// out or being dropped.
type track struct {
	ctx       context.Context // &req, the request's context
	req       requestCtx
	try       endedCtx      // ctx ended, for a first try; reused so a try boxes nothing
	span      trace.Span    // nil on a server with no tracer
	id        trace.TraceID // drawn even when the request is not traced
	mark      time.Time     // the end of its last step, which the stage running now is timed from (§10.1)
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

// begin draws a request's trace ID from Deps.TraceIDs as serve takes its
// record, which its log lines carry (RFC 25 §5.3), and samples the request on
// it (RFC 20 §10.2). Only a sampled request calls the tracer, so an unsampled
// one allocates nothing for tracing (RFC 25 §6.3). Its span, named after the
// connection's label until Route names its operation, opens through
// adapter.StartRequestSpan, so it takes the drawn ID through the root's ID
// generator; with a provider that draws its own, the request's lines carry
// the span's. The request's context starts its first attempt now, the
// request's arrival (§6.4), and so does decode_header, the first stage timed.
func (p *pipeline) begin(rec *resources.Buf) *track {
	tr := &track{label: p.label, protocol: p.st.Protocol, bytes: len(rec.B), id: p.sh.traceIDs()}
	parent := p.ctx
	if p.sh.tracer != nil && sampled(tr.id, p.sh.sample) {
		parent, tr.span = adapter.StartRequestSpan(p.ctx, p.sh.tracer, tr.id, p.name)
		if id := tr.span.SpanContext().TraceID(); id.IsValid() {
			tr.id = id
		}
	}
	tr.req = requestCtx{Context: parent, start: p.sh.clock.Now()}
	tr.ctx, tr.mark = &tr.req, tr.req.start
	return tr
}

// lap ends the request's run of stage st now: the time since its last step
// ended is observed under st where the connection times st, and the next
// stage is timed from here. An interim, which answers no request alone, has
// no track and is timed under no stage.
//
// ponytail: each lap is a clock read and an Observe on a histogram every
// connection of the protocol shares, about 11 per request. Upgrade to
// per-connection sums flushed periodically, or sharded series, when Observe
// contention shows in a profile.
func (p *pipeline) lap(tr *track, st stage) {
	if tr == nil {
		return
	}
	if d := p.since(tr); p.times[st] {
		tr.series().observe(st, d)
	}
}

// called takes the marks the metrics hinge left on r once a request alone's
// call returns: resolve_ids ends at the call's start, added to the request's
// sums in a table that holds, as lapAttempt does, and map_error is timed from
// the call's end (§10.1).
func (p *pipeline) called(tr *track, r *Request) {
	if d := r.callStart.Sub(tr.mark); tr.req.holds {
		tr.req.runs.add(stageResolveIDs, d)
	} else {
		tr.series().observe(stageResolveIDs, d)
	}
	tr.mark = r.callEnd
}

// since ends the request's run of the stage running now: the time since its
// last step ended, the next stage timed from here.
func (p *pipeline) since(tr *track) time.Duration {
	now := p.sh.clock.Now()
	d := now.Sub(tr.mark)
	tr.mark = now
	return d
}

// lapAttempt is lap for reply_wait and resolve_ids, the stages each attempt
// of a request runs: in a table that holds, where a re-drive runs them again,
// the run is added to the request's sums, which end observes once (§4.6,
// §10.1).
func (p *pipeline) lapAttempt(tr *track, st stage) {
	if !tr.req.holds {
		p.lap(tr, st)
		return
	}
	now := p.sh.clock.Now()
	if p.times[st] {
		tr.req.runs.add(st, now.Sub(tr.mark))
	}
	tr.mark = now
}

// sums is a request's time in each stage it runs more than once, over a held
// request's attempts or a compound's members, its runs summed, so each is
// observed once (§10.1).
type sums struct {
	d   [stages]time.Duration
	ran [stages]bool
}

func (s *sums) add(st stage, d time.Duration) {
	s.d[st] += d
	s.ran[st] = true
}

// observe observes each stage s holds once, under tr's series.
func (s *sums) observe(tr *track) {
	for st, ran := range s.ran {
		if ran {
			tr.series().observe(stage(st), s.d[st])
		}
	}
}

// requestCtx is a request's one context: its connection's, through the
// request span where it has one, so the connection's close cancels the
// request (§6.5, §6.9). It keeps what each attempt reads, and answers
// attemptKey with itself, so an attempt adds no context to carry them (§4.6,
// §6.4).
type requestCtx struct {
	context.Context
	start time.Time // the attempt's
	hold  time.Time // a re-drive's hold deadline; zero for none
	holds bool      // its table holds
	clone time.Time // in a table that holds, the clone wait's deadline its row returned at arrival; zero for none
	runs  sums      // with holds, its attempts' runs of reply_wait and resolve_ids, observed once as it ends
}

func (c *requestCtx) Value(key any) any {
	if _, ok := key.(attemptKey); ok {
		return c
	}
	return c.Context.Value(key)
}

// ended is the request's context ended, for its first try at the replay
// cache or at flow control: its one endedCtx, repointed at its context,
// which a stage, as any call's, keeps no further than its return.
func (tr *track) ended() context.Context {
	tr.try.Context = tr.ctx
	return &tr.try
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
// its sentinel when it is the service's. send and notFailure are Map's reply
// and notFailure.
//
// decision: error level is for what no row maps and for a panic; a refusal
// or a service error a row answers is expected, and the service logs its own
// faults. RFC 20 §4.5 gives examples of each level, not a closed set.
// Overturned if an operator needs a mapped error at a level that pages.
func (p *pipeline) mapped(tr *track, errs *ErrorTable, err error, op *Op, v Version, log bool) (s Status, send, notFailure bool) {
	r, matched := errs.row(err, op, v)
	if !matched {
		tr.series().unmappedErr()
		p.sh.log.LogAttrs(tr.ctx, slog.LevelError, "an error no row maps", tr.attrs(err)...)
		return r.Status, true, false
	}
	tr.series().statusMapped(err)
	if log && p.sh.log.Enabled(tr.ctx, slog.LevelDebug) {
		p.sh.log.LogAttrs(tr.ctx, slog.LevelDebug, "an error its row answers", tr.attrs(err)...)
	}
	return r.Status, !r.NoReply, r.NotFailure
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
	if tr == nil {
		// The interim of a request still pending: its request goes on.
		return
	}
	tr.req.runs.observe(tr)
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
// or a call under its context, or a handler's ErrParked on a closed
// connection, whose reply Park drops as a close would (§4.7). That is
// no error: nothing can be sent, and the request is neither counted as a
// refusal or an error no row maps nor logged at error (§10.1).
func (p *pipeline) endedByClose(err error) bool {
	return p.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ErrParked))
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

// newTraceID draws a request's trace ID where Deps.TraceIDs is nil, from
// math/rand/v2's global source, a per-thread generator seeded by the runtime,
// which every connection shares without a lock.
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

// send wraps reply under h and hands it to out, timing wrap and send as tr's,
// the request it answers, or as no stage for an interim, which has none. It
// reports whether the writer took it.
func (p *pipeline) send(tr *track, h Header, reply *resources.Buf, key uint64) bool {
	if err := p.st.Security.Wrap(h, reply); err != nil {
		reply.Release()
		p.l.close(stageFault(err))
		return false
	}
	p.lap(tr, stageWrap)
	sent := p.out(reply, key, key)
	p.lap(tr, stageSend)
	return sent
}

// interim sends the interim a NoReply row gets in a table that holds, which
// grants the request's credits back, in the request's own reply buffer while
// it still holds one, or else in a status's buffer, which waits for room:
// releasing the reservation first could hand its bytes to a request waiting
// on the pool and leave the interim, and so the client's credits, behind it.
func (p *pipeline) interim(ctx context.Context, h Header, buf *resources.Buf) {
	if buf == nil {
		if buf = p.sh.reserveStatus(ctx, p.l.remote, p.st.Header.ReplyLen(h), time.Time{}, p.l.close); buf == nil {
			return
		}
	}
	buf.B = buf.B[:0]
	if err := p.st.Hold.Interim(h, true, nil, buf); err != nil {
		buf.Release()
		p.l.close(stageFault(err))
		return
	}
	p.send(nil, h, buf, 0)
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
// decided once from every filehandle hs it names, asked of the service under
// the request's call c: ErrMoved when it names at least one and the
// service's MoveTarget names a gateway node for the share of each, and
// ErrDraining otherwise (§3.1, §4.7).
//
// decision: a MoveTarget that answers an error names no target, so the
// request gets ErrDraining: no reply, and its client sends it again once it
// reconnects. Overturned if the RFC gives that error an answer of its own.
func (p *pipeline) drainErr(ctx context.Context, c vfs.Call, hs []vfs.Handle) error {
	if len(hs) == 0 {
		return ErrDraining
	}
	for _, h := range hs {
		if to, err := p.sh.shares.MoveTarget(ctx, c, h); err != nil || !to.IsValid() {
			return ErrDraining
		}
	}
	return ErrMoved
}

// drain begins the connection's drain: from here on a request whose role is
// none, and a park, is refused with drainErr, and every request parked is
// cancelled, on a goroutine of its own, since its handler's cancel may call
// the service, done's error replaced by drainErr's (§3.1, §4.7).
//
// decision: a park's drain error is decided with no filehandle, so it is
// ErrDraining, asked of no service: the arguments that name one went with
// its record. Overturned if a park keeps its filehandles.
func (p *pipeline) drain() {
	p.draining.Store(true)
	t := &p.pend
	t.mu.Lock()
	var parks []*pending
	for _, pd := range t.m {
		if pd.park != nil {
			parks = append(parks, pd)
		}
	}
	t.mu.Unlock()
	for _, pd := range parks {
		go p.cancelPark(pd, p.drainErr(pd.tr.ctx, pd.r.Call, nil))
	}
}

// wait returns nil once nothing runs, is held, is parked or waits for a
// status, or ctx's error.
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
// context, which ends every wait a request or a status is in, every hold
// between attempts and every park (§4.6, §4.7, §6.5).
//
// decision: the table of holds is marked closed before the context ends. A
// handler the cancel releases then reaches Park on a closed table, which
// calls its cancel at once and makes no call under the park, where marking
// after the cancel let it park live and run its service call before the
// close's own cancel reached it. Overturned if a park the close races may
// keep its call.
func (p *pipeline) close() {
	p.pend.mu.Lock()
	p.pend.closed = true
	p.pend.mu.Unlock()
	p.cancel()
	p.endHolds()
}

// add counts a request, a hold, a park or a status wait starting; done, one
// ending. Each also reports the turn to the link, under the same mutex, so a
// connection's idle deadline learns its arm was superseded before the quiet is
// visible.
func (p *pipeline) add() {
	p.mu.Lock()
	p.runs++
	if p.l.turn != nil {
		p.l.turn()
	}
	p.mu.Unlock()
}

func (p *pipeline) done() {
	p.mu.Lock()
	quiet := false
	if p.runs--; p.runs == 0 {
		if p.idle != nil {
			close(p.idle)
			p.idle = nil
		}
		quiet = true
	}
	if p.l.turn != nil {
		p.l.turn()
	}
	p.mu.Unlock()
	if quiet && p.l.idle != nil {
		p.l.idle()
	}
}

// quiet reports whether nothing runs: no request and no status wait, the
// moment wait returns at once.
func (p *pipeline) quiet() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs == 0
}
