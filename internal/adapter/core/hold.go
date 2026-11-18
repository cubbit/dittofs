package core

import (
	"errors"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// HoldCodec is the stage a table that Holds needs. The key doubles as the
// async ID the interim and final replies carry; no two of a connection's
// pending requests share one, since the replay check refuses a message ID
// already used (§4.1; MS-SMB2 3.3.5.2.3). The interim, like every reply,
// passes Security.Wrap, so an encrypting session sends nothing in clear.
// Cancels is asked after Unwrap, so an unverified cancel ends no hold. A
// cancel that ends no hold or park with its key, one that finds its target's
// attempt running included, is remembered for at most 5 s, and at most
// MaxPending per connection, the oldest dropped first; the first request with
// that key that holds or parks meanwhile ends as cancelled, its interim sent
// first and its final under Final's header, and uses the cancel up. A cancel
// whose target was already answered leaves nothing after 5 s.
//
// ponytail: a cancel that arrives more than 5 s before its target holds or
// parks is lost, and the target runs to its answer. Keep the record until the
// connection's message-ID window passes the key if a client is found that
// cancels that early.
type HoldCodec interface {
	Key(h Header) uint64 // never 0, which Conn.Send's before reserves for none
	// Interim encodes STATUS_PENDING: it grants the request's credits if
	// grant, else only what keeps the client's balance at one, and always
	// those of chain members answered before it. For a drain's refusal it is
	// handed the headers of the members after, and grants theirs too.
	//
	// decision: the credits of the members after a refused one are granted
	// from headers no Security stage verified, since those members are never
	// unwrapped. The grant is bounded by the sum's saturation at the
	// connection's credit target, so a forged value grants a client at most
	// the target it could already hold. Verify each header before granting if
	// a client is found that turns such a grant into more than the target.
	Interim(h Header, grant bool, after []Header, out *resources.Buf) error
	// Final is the final reply's header, with the async ID. If grant (true
	// only when its interim withheld), it grants the request's credits less
	// what the interim granted of them; otherwise none.
	Final(h Header, grant bool) Header
	Cancels(h Header) (key uint64, ok bool) // a CANCEL ends the hold it names and gets no reply
	// Pends says whether a chain member before the last, answered err whose
	// row holds, goes pending (true) or is answered at once as ErrNotLast. A
	// member the interim timer found with no buffer for its interim got none,
	// went on unannounced, and is asked like any other.
	//
	// decision: Pends is asked only for an error the service returned; a
	// member whose interim the timer sent while it was still in the service
	// always pends, since it can apply after any answer, and answering it at
	// once could report a failed WRITE whose data lands a moment later.
	// Withdraw it only if the service gains a way to withdraw a call it has
	// not yet applied.
	Pends(h Header, err error) bool
}

// ErrParked is what a handler returns, with its reply value, after Park: it
// waits for a callback (a waiting lock, a change notification) and fills the
// reply value before calling done.
var ErrParked = errors.New("core: parked")

// interimAfter is how long a call in a table that holds may run before the
// adapter's own timer sends its interim (§4.6).
//
// ponytail: one timer per call. Sweep each connection's calls on one timer
// when the pipeline benchmark shows the timers' cost, or at the interval
// RFC 22 sets when it sets one.
const interimAfter = time.Second

// A held request is re-driven on a backoff of 10 ms doubling to 1 s, never
// past its hold's deadline (RFC 22 §6.3).
//
// ponytail: the service signals nothing when a delay's cause ends, so a held
// request is re-driven on a backoff. Re-drive on that signal once the
// service gives one.
const (
	redriveFirst = 10 * time.Millisecond
	redriveMax   = time.Second
)

// cloneWait is the error a call waiting behind a clone is answered with,
// whose row's Hold bounds that wait (§4.6).
var cloneWait = &vfs.Delay{Cause: vfs.DelayClone}

// pends is a connection's table of pending requests, by HoldCodec.Key: those
// held between attempts and those the interim timer found in the service.
// Its mutex is held for a lookup or a count, never across a call (§6.6).
type pends struct {
	mu      sync.Mutex
	m       map[uint64]*pending
	n       int          // the pending, which Stages.MaxPending bounds
	floor   int          // those counted on the floor
	bytes   int64        // their records' receive charge
	closed  bool         // close ended every hold: no timer is armed after it
	cancels []remembered // the cancels that ended nothing, oldest first
}

// cancelKept is how long a cancel that ended nothing is remembered (§4.7);
// HoldCodec's ponytail says what that loses.
const cancelKept = 5 * time.Second

// remembered is a cancel that ended nothing: the key it names, and when it is
// forgotten.
type remembered struct {
	key   uint64
	until time.Time
}

// remember keeps a cancel of key that ended nothing until cancelKept from
// now, and at most max of them, the oldest dropped first (§4.7). The caller
// holds t.mu.
//
// decision: a remembered cancel is forgotten as the list is next read, not on
// a timer of its own: the list never passes MaxPending, so it holds no timer
// for close to stop. Overturned if a connection must give the memory back
// when no cancel or hold comes after.
func (t *pends) remember(key uint64, now time.Time, max int) {
	t.forget(now)
	t.cancels = append(t.cancels, remembered{key, now.Add(cancelKept)})
	if n := len(t.cancels) - max; n > 0 {
		t.cancels = slices.Delete(t.cancels, 0, n)
	}
}

// cancelledAt reports whether a cancel of key is remembered at now, and
// forgets it. The caller holds t.mu.
func (t *pends) cancelledAt(key uint64, now time.Time) bool {
	t.forget(now)
	i := slices.IndexFunc(t.cancels, func(c remembered) bool { return c.key == key })
	if i < 0 {
		return false
	}
	t.cancels = slices.Delete(t.cancels, i, i+1)
	return true
}

// forget drops the cancels remembered cancelKept or longer before now, the
// oldest first.
func (t *pends) forget(now time.Time) {
	n := 0
	for n < len(t.cancels) && !now.Before(t.cancels[n].until) {
		n++
	}
	t.cancels = slices.Delete(t.cancels, 0, n)
}

// pending is one request of a table that holds, from its call on: what its
// interim, its re-drives and its final reply need.
//
// decision: a request held between attempts keeps its record's Buf until its
// final answer, since core cannot tell which decoded arguments point into
// it; only a park releases its record. Overturned if a release hook lets
// core free a held request's arguments.
type pending struct {
	tr     *track
	rec    *resources.Buf
	h      Header
	r      *Request
	tb     *OpTable
	ts     *tableSeries
	op     *Op
	inv    Invocation
	a      answerer // where its interim and its answer go
	key    uint64
	start  time.Time // the hold's start: its first attempt's
	callAt time.Time // the runtime's time its first call started, for a sampled request's span

	mu      sync.Mutex // orders its interim before its final reply
	in      bool       // its call is in the service
	timer   timer      // the interim's, then each re-drive's, which pends.mu guards once it is in the table
	pend    bool       // in the table, counted toward MaxPending and in the held gauges
	held    bool       // held between attempts: its record retained, counted in runs
	grant   bool       // its interim withheld its credits, so its final reply grants them
	floor   bool       // counted on the floor
	pool    bool       // its record's charge moved to the hold pool
	charge  int64      // its record's charge, as counted
	backoff time.Duration
	last    error                            // its last attempt's error, whose row holds
	bound   func(error, time.Time) time.Time // that row's Hold
}

// watch arms the interim timer of a call about to start in a table that
// holds, and returns its record: a call still in the service when the timer
// fires gets its interim where the reply pool has a buffer at once (§4.6).
func (p *pipeline) watch(tr *track, rec *resources.Buf, h Header, r *Request, tb *OpTable, ts *tableSeries, op *Op, inv Invocation, a answerer) *pending {
	pd := &pending{tr: tr, rec: rec, h: h, r: r, tb: tb, ts: ts, op: op, inv: inv, a: a, key: p.st.Hold.Key(h), start: tr.req.start}
	if tr.span != nil {
		pd.callAt = time.Now()
	}
	pd.mu.Lock()
	pd.in = true
	pd.timer = p.sh.clock.AfterFunc(interimAfter, func() { p.found(pd) })
	pd.mu.Unlock()
	return pd
}

// found is the interim timer's: a call still in the service goes pending and
// gets its interim, only in a buffer the reply pool has at once; with none,
// it goes on unannounced and is answered as any other call (§4.6).
func (p *pipeline) found(pd *pending) {
	pd.mu.Lock()
	defer pd.mu.Unlock()
	if !pd.in {
		return
	}
	buf, ok := p.sh.reply.TryGet(p.l.remote, p.st.Header.ReplyLen(pd.h))
	if !ok {
		return
	}
	grant, _ := p.count(pd, false)
	p.interimIn(pd, buf, grant)
}

// count puts pd in the table, counted toward MaxPending and on the floor, or
// past the floor in the hold pool (§4.6; RFC 24 §4.3). A hold that starts on
// an error the service returned (refuse) is refused past MaxPending with
// ErrTooManyPending, and where neither the floor nor the pool can take it
// with the pool's refusal, ErrDelay of DelayResources, counted. A call found
// in the service is never refused: past MaxPending, or past the floor with
// the pool full, keeping its receive reservation, it goes pending with grant
// false, so its interim withholds its credits.
//
// decision: the floor counts each held request's record at its whole receive
// charge, its class and the per-request overhead, which is what the receive
// pool holds for it, not its length. Overturned if the floor must count what
// a request's write carries.
//
// decision: a call found in the service goes pending past MaxPending, and
// past the floor with the hold pool full, since answering it at once could
// report a failure for a call that applies a moment later (§4.6). Withdraw it
// if the service gains a way to withdraw a call it has not applied.
//
// decision: the floor's and the pool's refusal is ErrDelay of
// DelayResources, the one class RFC 20 §6.3 gives a node pool's refusal,
// which every table that holds answers at once by a typed row, so it never
// starts a hold. Overturned if a protocol must tell the hold pool's refusal
// from the reply pool's.
func (p *pipeline) count(pd *pending, refuse bool) (grant bool, err error) {
	t := &p.pend
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.n
	if !pd.pend {
		n++
	}
	grant = n <= p.st.MaxPending
	if !grant && refuse {
		return false, ErrTooManyPending
	}
	if !pd.floor && !pd.pool {
		c := pd.rec.Charge()
		switch {
		case t.floor < p.sh.floorRequests && t.bytes+c <= p.sh.floorBytes:
			t.floor++
			t.bytes += c
			pd.floor = true
		case p.sh.hold != nil && pd.rec.Recharge(p.sh.hold) == nil:
			pd.pool = true
			p.sh.holdPoolCharged(c)
		case refuse:
			pd.ts.holdRefused()
			return false, &vfs.Delay{Cause: vfs.DelayResources}
		default:
			grant = false
		}
	}
	if !pd.pend {
		if t.m == nil {
			t.m = map[uint64]*pending{}
		}
		t.m[pd.key] = pd
		t.n++
		pd.pend, pd.charge = true, pd.rec.Charge()
		pd.ts.held(pd.charge)
	}
	return grant, nil
}

// interimIn sends pd's interim in buf, granting its credits where grant: no
// headers, which only a NoReply row's interim is handed (§4.7).
func (p *pipeline) interimIn(pd *pending, buf *resources.Buf, grant bool) {
	buf.B = buf.B[:0]
	if err := p.st.Hold.Interim(pd.h, grant, nil, buf); err != nil {
		buf.Release()
		p.l.close(err)
		return
	}
	pd.grant = !grant
	pd.a.pended(pd.h, buf, pd.key)
}

// after ends an attempt of pd whose call returned err, reply its reply
// buffer, and stops the interim timer. Where err's row holds and its
// deadline, from the hold's start, is past now, the request is held: on its
// first such error its interim goes out in reply, unless it already has one,
// and it is re-driven on the backoff; release, its admission, is released,
// and after reports true; a cancel remembered for its key ends it there as
// cancelled. Otherwise it returns err, or the refusal that replaces it, for
// the request to be answered. A chain's member before the last with no
// interim yet holds only where Pends says it goes pending; otherwise
// ErrNotLast replaces err before its table maps it, since the row's status
// speaks for a hold that does not happen (§4.7), and its last keeps err for
// its span's result (§10.2). A member whose interim the timer sent while it
// was in the service is not asked Pends.
func (p *pipeline) after(pd *pending, err error, reply *resources.Buf, release func()) (bool, error) {
	pd.mu.Lock()
	pd.in = false
	pd.timer.Stop()
	if err == nil || p.ctx.Err() != nil {
		pd.mu.Unlock()
		return false, err
	}
	bound := pd.tb.Errors.hold(err, pd.op, pd.h.Version())
	if bound == nil {
		pd.mu.Unlock()
		return false, err
	}
	if pd.h.Next() != 0 && !pd.pend && !p.st.Hold.Pends(pd.h, err) {
		pd.last = err
		pd.mu.Unlock()
		return false, ErrNotLast
	}
	now := p.sh.clock.Now()
	at := bound(err, pd.start)
	if !at.After(now) {
		pd.mu.Unlock()
		return false, err
	}
	if !pd.held {
		interim := !pd.pend
		if _, rerr := p.count(pd, true); rerr != nil {
			pd.mu.Unlock()
			return false, rerr
		}
		if interim {
			p.interimIn(pd, reply, true)
		} else {
			reply.Release()
		}
		pd.rec.Retain()
		pd.held, pd.backoff = true, redriveFirst
		p.add()
		if pd.tr.span != nil {
			_, span := p.sh.tracer.Start(pd.tr.ctx, pd.ts.names[pd.op.Code], trace.WithTimestamp(pd.callAt))
			span.End()
		}
	} else {
		reply.Release()
	}
	pd.last, pd.bound = err, bound
	wait := min(pd.backoff, at.Sub(now))
	pd.backoff = min(2*pd.backoff, redriveMax)
	t := &p.pend
	t.mu.Lock()
	closed := t.closed
	// A cancel that came before it held, or while its attempt ran, ends it
	// now that it holds (§4.7).
	cancelled := !closed && t.cancelledAt(pd.key, now)
	if !closed && !cancelled {
		pd.timer = p.sh.clock.AfterFunc(wait, func() { p.redrive(pd) })
	}
	t.mu.Unlock()
	// Its interim is out; what follows may run the chain's later members or
	// wait for a status's buffer, so it holds no lock (§6.6).
	pd.mu.Unlock()
	release()
	switch {
	case closed:
		p.dropHeld(pd)
	case cancelled:
		p.answerHeld(pd, nil, ErrCancelled)
	}
	return true, nil
}

// endHold is a client's cancel of the request keyed key: a hold between
// attempts ends, answered ErrCancelled, which its table maps to the
// protocol's cancel status, on a goroutine of its own, since its status may
// wait for a buffer and a cancel comes on the reader; a running attempt
// finishes and answers its own result. A cancel that ends nothing is
// remembered, so a request with its key that holds meanwhile ends as
// cancelled (§4.6, §4.7, §6.5).
//
// decision: a cancel finds its target by the protocol's key alone, within its
// connection; core compares no session, so a request of one session can
// cancel another's when the key lets it, and a protocol that must stop that
// puts the verified session into its key. Overturned if core must check the
// session itself.
func (p *pipeline) endHold(key uint64) {
	t := &p.pend
	t.mu.Lock()
	pd := t.m[key]
	if pd == nil || !pd.timer.Stop() {
		t.remember(key, p.sh.clock.Now(), p.st.MaxPending)
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	go p.answerHeld(pd, nil, ErrCancelled)
}

// answerHeld answers pd, held between attempts, err, in reply where it has
// one, under Final's header, through its answerer: a chain's member answers
// into its chain, whose members after it then run (§4.7).
func (p *pipeline) answerHeld(pd *pending, reply *resources.Buf, err error) {
	pd.a.resume(func() {
		p.finish(pd.tr, pd.h, pd.tb, pd.tb.Errors, pd.op, pd.inv, reply, err, func() {}, pd.a, pd)
		p.unpend(pd)
	})
}

// dropHeld ends pd, held between attempts on a closed connection, unsent.
func (p *pipeline) dropHeld(pd *pending) {
	pd.a.resume(func() {
		p.end(pd.tr, false, 0, 0, nil)
		p.unpend(pd)
	})
}

// redrive is a held request's next attempt, on its timer's goroutine: from
// Resolve through the expiry check to its call, the same decoded arguments
// under a fresh attempt deadline, 30 s or the hold's deadline if sooner, and
// a fresh reply reservation, in a child of its request's span (§4.6, §6.4,
// §10.2). A hold whose deadline has passed since its last attempt is
// answered as that attempt's row says, with no attempt, since none fits
// inside the bound. A closed connection's hold is dropped.
func (p *pipeline) redrive(pd *pending) {
	tr := pd.tr
	if p.ctx.Err() != nil {
		p.dropHeld(pd)
		return
	}
	now := p.sh.clock.Now()
	at := pd.bound(pd.last, pd.start)
	if !at.After(now) {
		p.answerHeld(pd, nil, pd.last)
		return
	}
	tr.req.start, tr.req.hold = now, at
	ctx := tr.ctx
	var span trace.Span
	if tr.span != nil {
		ctx, span = p.sh.tracer.Start(ctx, pd.ts.names[pd.op.Code])
	}
	// A re-drive reserves as a request alone: its chain, if it has one, holds
	// nothing between attempts and is not the re-drive's until its answer.
	reply, err := p.reserve(tr, pd.ts, alone{p}, pd.inv, pd.h, pd.r)
	if err == nil {
		err = p.resolve(tr, pd.r, pd.op, pd.inv, false)
	}
	if err == nil {
		pd.r.invoke = pd.inv.Call
		err = pd.ts.chains[pd.op.Code](ctx, pd.r)
	}
	if span != nil {
		span.End()
	}
	held, err := p.after(pd, err, reply, func() {})
	if held {
		return
	}
	p.answerHeld(pd, reply, err)
}

// unpend takes pd out of the table and its counts, and, held between
// attempts, releases its record and stops counting it in runs.
func (p *pipeline) unpend(pd *pending) {
	if pd.pend {
		t := &p.pend
		t.mu.Lock()
		if t.m[pd.key] == pd {
			delete(t.m, pd.key)
		}
		t.n--
		if pd.floor {
			t.floor--
			t.bytes -= pd.charge
		}
		t.mu.Unlock()
		pd.ts.released(pd.charge)
		if pd.pool {
			p.sh.holdPoolCharged(-pd.charge)
		}
		pd.pend, pd.floor, pd.pool = false, false, false
	}
	if pd.held {
		pd.held = false
		pd.rec.Release()
		p.done()
	}
}

// endHolds ends every request held between attempts as close does: its
// timer stopped, its reply dropped, its record released. A request in an
// attempt, or found in the service, ends on its own goroutine, which the
// close's cancel reaches.
func (p *pipeline) endHolds() {
	t := &p.pend
	t.mu.Lock()
	t.closed = true
	var ended []*pending
	for _, pd := range t.m {
		if pd.timer.Stop() {
			ended = append(ended, pd)
		}
	}
	t.mu.Unlock()
	for _, pd := range ended {
		p.dropHeld(pd)
	}
}
