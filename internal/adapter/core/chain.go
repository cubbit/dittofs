package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// endedCtx is a member's context with its deadline passed: Admit under it
// tries once and, where it would wait, answers its error at once, and Lookup
// tries its copy's reservation once, answering ErrDelay of DelayResources
// (§4.7). It keeps the member's values, its trace ID and span among them.
type endedCtx struct{ context.Context }

// closedDone is the done channel of every endedCtx.
var closedDone = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func (endedCtx) Deadline() (time.Time, bool) { return time.Time{}, true }
func (endedCtx) Done() <-chan struct{}       { return closedDone }
func (endedCtx) Err() error                  { return context.DeadlineExceeded }

// entry is one chain member as the walk reaches it.
type entry struct {
	off       int
	tr        *track
	h, u      Header // as Decode and Unwrap returned them
	unwrapped bool   // Unwrap ran on the reader, for the first member
	cancel    bool   // it cancels a hold, and is answered nothing
	st        stage  // the stage that refused it, read only with err
	err       error
	release   func() // its admission
}

// held is one member's reply in the record a chain is building.
type held struct {
	tr    *track
	h     Header
	reply *resources.Buf
	key   uint64
	s     Status
	n     int  // the reply's bytes, before the join
	final bool // a final reply, not an interim
	err   error
}

// chain is one record of chained messages as it runs (§4.7): the replies it
// holds for the record it is building, and what the member before left.
type chain struct {
	p        *pipeline
	rec      *resources.Buf
	held     []held
	charge   int64 // the held replies' charges
	clear    bool  // the record is a header's refusal, which no Wrap sees, until it ends
	isolated bool  // the record is a refusal's, which ends before the next unrelated member
	prevH    Header
	prevR    *Request
	failed   bool // the member before failed, with failS
	failS    Status
	cur      entry    // the member running
	stopped  bool     // a NoReply member ended the chain, or the connection closed
	slot     bool     // the chain holds the connection's one status wait
	kept     func()   // the place of the last member admitted, until the next is admitted or would wait
	try      endedCtx // the running member's context, ended, for its first try; reused so a try boxes nothing
}

// serveChain takes a record whose messages chain (§4.7). On the reader it
// unwraps the first member and admits it, blocking in Admit as serve does,
// so a connection at its bound is not read; the chain runs on a goroutine of
// its own from there. A refusal of the first member, at Decode, Unwrap or
// Admit, goes to the chain too, which answers it and runs on.
func (p *pipeline) serveChain(tr *track, rec *resources.Buf, h Header, derr error) {
	tr.bytes = memberBytes(rec, 0, h.Next())
	e := entry{tr: tr, h: h, unwrapped: true}
	switch {
	case derr != nil:
		e.st, e.err = stageDecodeHeader, fmt.Errorf("%w: %v", ErrDecode, derr)
	default:
		u, err := p.st.Security.Unwrap(h, rec, nil)
		if u == nil {
			if err == nil {
				err = errUnwrapNeither
			}
			p.drop(tr, rec, err)
			return
		}
		e.u = u
		switch {
		case err != nil:
			e.st, e.err = stageUnwrap, err
		default:
			if _, ok := p.st.Hold.Cancels(u); ok {
				e.cancel = true
			} else if release, err := p.st.Flow.Admit(tr.ctx, u); err != nil {
				e.st, e.err = stageAdmit, err
			} else {
				e.release = release
			}
		}
	}
	// A first member refused takes the connection's one status wait here,
	// on the reader, as a refusal alone does: the next such chain then holds
	// the reader until this one's status is reserved, so the records waiting
	// for a status stay one per connection.
	slot := false
	if e.err != nil {
		select {
		case p.status <- struct{}{}:
			slot = true
		case <-p.ctx.Done():
		}
	}
	p.add()
	if p.ctx.Err() != nil {
		p.done()
		if slot {
			<-p.status
		}
		if e.release != nil {
			e.release()
		}
		p.end(tr, false, 0, 0, nil)
		rec.Release()
		return
	}
	c := &chain{p: p, rec: rec, slot: slot}
	go c.run(e)
}

// run takes the chain's members in turn, from e, each at the next of the one
// before, and sends what the chain holds once the last has run. A step that
// does not lead forward to a header inside the record ends the chain: what it
// holds goes out, and the connection closes, since the members past it can
// be neither read nor answered (§4.7).
func (c *chain) run(e entry) {
	p := c.p
	defer p.done()
	defer c.rec.Release()
	defer c.free()
	defer c.giveBack()
	for {
		c.one(&e)
		c.free() // a first member answered with no status wait gives the slot back
		if c.stopped {
			return
		}
		next := e.h.Next()
		if next == 0 {
			break
		}
		if next <= e.off || next >= len(c.rec.B) {
			c.flush(splits, nil)
			if !c.stopped {
				p.l.close(fmt.Errorf("%w: a chain's step from %d to %d, in a record of %d bytes", ErrDecode, e.off, next, len(c.rec.B)))
			}
			return
		}
		if e = c.at(next); e.tr == nil {
			return
		}
	}
	c.flush(splits, nil)
}

// at is the member whose header is at off. A nil header closes the
// connection, and nothing more is sent.
func (c *chain) at(off int) entry {
	p := c.p
	tr := p.begin(c.rec)
	h, err := p.st.Header.Decode(c.rec, off)
	if h == nil {
		c.abort(tr, fmt.Errorf("%w: %v", ErrDecode, err))
		return entry{}
	}
	tr.version, tr.versioned = h.Version(), true
	tr.bytes = memberBytes(c.rec, off, h.Next())
	e := entry{off: off, tr: tr, h: h}
	if err != nil {
		e.st, e.err = stageDecodeHeader, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return e
}

// memberBytes is the bytes of the member at off: to its next where that
// leads forward inside the record, and to the record's end otherwise.
func memberBytes(rec *resources.Buf, off, next int) int {
	if next > off && next < len(rec.B) {
		return next - off
	}
	return len(rec.B) - off
}

// one runs one member: unwrapped under the member before where related,
// admitted, and run as a request alone runs, its answer held for the record;
// or refused, which for a header that did not decode or that Unwrap refused
// splits the chain (§4.7).
func (c *chain) one(e *entry) {
	p := c.p
	c.cur = *e
	related := e.h.Related()
	if c.isolated && !related {
		c.flush(splits, nil) // a refusal's record ends before the next unrelated member
		c.clear, c.isolated = false, false
	}
	if e.err != nil && e.st == stageDecodeHeader {
		c.flush(splitDecode, e.tr)
		c.clear, c.isolated = true, true
		c.prevH = e.h
		c.refusal(e, e.h)
		return
	}
	if !e.unwrapped {
		var prev Header
		if related {
			prev = c.prevH
		}
		u, err := p.st.Security.Unwrap(e.h, c.rec, prev)
		if u == nil {
			if err == nil {
				err = errUnwrapNeither
			}
			c.abort(e.tr, err)
			return
		}
		e.u = u
		switch {
		case err != nil:
			e.st, e.err = stageUnwrap, err
		default:
			if _, ok := p.st.Hold.Cancels(u); ok {
				e.cancel = true
				break
			}
			c.admit(e)
			if c.stopped || (p.ctx.Err() != nil && (e.err == nil || p.endedByClose(e.err))) {
				// A member admitted after close, one whose admission the
				// close ended, or one after a split that closed the
				// connection, is not run, as a first message is not:
				// nothing more is sent, counted or logged.
				if e.release != nil {
					e.release()
				}
				c.drop(e.tr, nil)
				return
			}
		}
	}
	c.prevH = e.u
	switch {
	case e.cancel:
		// The cancel is the member before the next and sets no handle, so a
		// related member after it starts from no request (§4.7, Related)
		// and is not answered a failure from before the cancel (§4.3).
		c.prevR = nil
		c.failed, c.failS = false, 0
		p.end(e.tr, false, 0, 0, nil)
		return
	case e.err == nil:
	case e.st == stageUnwrap:
		c.flush(splitUnwrap, e.tr)
		c.clear, c.isolated = false, true
		c.refusal(e, e.u)
		return
	case e.st == stageAdmit:
		c.refusal(e, e.u)
		return
	}
	e.tr.label.admitted()
	e.tr.flight = true
	if related && c.failed {
		c.answerFailed(e)
		return
	}
	var prev *Request
	if related {
		prev = c.prevR
	}
	// decision: a replay hit builds no request, so a related member after a
	// related hit is handed the request from before the hit, whose state the
	// cached reply's call had started from; after an unrelated hit it starts
	// from no request, as that call did. Overturned if a protocol that chains
	// returns cached copies and a hit's own state must carry over.
	release := e.release
	keep := func() { c.keep(release) }
	if r := p.message(e.tr, c.rec, e.u, prev, keep, c); r != nil || !e.tr.cached || !related {
		c.prevR = r
	}
}

// admit admits a member after the first, as §4.7 has it: tried once under
// an ended context in the place of the member before it, so a member flow
// control refuses waits for its status in that place, and a try that admits
// holds both places for that moment; where the try would wait, the place is
// given back and it is tried once more; and where it would still wait, the
// replies the chain holds go out first. A chain splits only where it would
// wait anyway, and never waits on flow control holding reply reservations or
// a place.
func (c *chain) admit(e *entry) {
	p := c.p
	try := c.ended(e.tr.ctx)
	release, err := p.st.Flow.Admit(try, e.u)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		c.giveBack() // never wait at flow control holding a place
		if release, err = p.st.Flow.Admit(try, e.u); err != nil && errors.Is(err, context.DeadlineExceeded) {
			c.flush(splitFlow, e.tr)
			release, err = p.st.Flow.Admit(e.tr.ctx, e.u)
		}
	}
	if err != nil {
		// Refused on the first try, the member keeps the chain's place for
		// its status; refused once the place was given back, it holds none,
		// which core's cap does only on close.
		e.st, e.err = stageAdmit, err
		return
	}
	c.giveBack()
	e.release = release
}

// refusal answers e's refusal at its stage, before Route, as the stages'
// table maps it, under h, into the record the chain is building.
func (c *chain) refusal(e *entry, h Header) {
	p := c.p
	p.refused(e.tr, e.st)
	s, send, _ := p.mapped(e.tr, p.st.Errors, e.err, nil, h.Version(), e.st != stageUnwrap)
	if e.st == stageUnwrap && p.misbehaved.CompareAndSwap(false, true) {
		// A client that sends what Unwrap refuses is logged once per
		// connection, not once per request (§10.3).
		p.sh.log.LogAttrs(e.tr.ctx, slog.LevelWarn, "a message the connection's security refused", e.tr.attrs(e.err)...)
	}
	c.failed, c.failS = true, s
	if !send {
		p.end(e.tr, false, 0, 0, e.err)
		return
	}
	c.status1(e, h, s, e.err)
}

// answerFailed answers a related member after a failed one the failed
// member's status, with no call (§4.3).
func (c *chain) answerFailed(e *entry) {
	if e.release != nil {
		c.keep(e.release)
	}
	c.status1(e, e.u, c.failS, nil)
}

// status1 holds a reply of status s alone to e, under h.
func (c *chain) status1(e *entry, h Header, s Status, cause error) {
	p := c.p
	buf := c.status(e.tr.ctx, p.st.Header.ReplyLen(h))
	if buf == nil {
		p.end(e.tr, false, 0, 0, cause)
		return
	}
	buf.B = buf.B[:0]
	if err := p.st.Header.Encode(h, s, buf); err != nil {
		buf.Release()
		c.abort(e.tr, err)
		return
	}
	c.hold(held{tr: e.tr, h: h, reply: buf, s: s, final: true, err: cause})
}

// reserve takes a member's reply buffer for the chain: when its charge would
// take the chain's held reservations past a quarter of the reply pool's
// ceiling, its bytes the record join frames past the protocol's cap, or it
// does not fit at once while the chain holds replies, those go out first, so
// a chain never waits on the reply pool holding it (§4.7).
func (c *chain) reserve(ctx context.Context, n int) (*resources.Buf, error) {
	p := c.p
	c.room(n)
	// decision: a member that does not fit at once is not refused: the
	// replies before it go out, and it waits under its deadline, since a
	// refusal's status would itself need a buffer from the pool its chain
	// fills. Overturned if a protocol must answer such a member at once.
	//
	// ponytail: TryGet grants only when nothing waits, so under any
	// contention a chain goes out one record per member, each wrapped and
	// written apart, which the reply_pool count shows. Split only when
	// waiting could stall on the chain's own bytes, once the pool reports
	// what it holds outside one request.
	if buf, ok := p.sh.reply.TryGet(p.l.remote, n); ok {
		return buf, nil
	}
	c.flush(splitReplyPool, c.cur.tr)
	return p.sh.reserveReply(ctx, p.l.remote, n)
}

// room sends what the chain holds first when a reservation of n bytes would
// take its held reservations past a quarter of the reply pool's ceiling, or
// the record join frames past the protocol's record cap, so one Frame header
// can state it (§4.7). The record is counted as join frames it: the replies
// held, as encoded, n, Chain's padding between them, and what Wrap adds,
// which the first member's ReplyLen bounds.
func (c *chain) room(n int) {
	p := c.p
	if len(c.held) == 0 {
		return
	}
	if c.charge+p.sh.reply.Charge(n) > p.sh.reply.Ceiling()/4 {
		c.flush(splitQuarter, c.cur.tr)
		return
	}
	size := n + chainPadding*len(c.held) + p.st.Header.ReplyLen(c.held[0].h)
	for _, m := range c.held {
		size += m.n
	}
	if size > p.l.record {
		c.flush(splitRecord, c.cur.tr)
	}
}

// status takes a status's buffer for the chain, as reserve takes a reply's,
// and waits for it only holding the connection's one status wait, the slot
// a refusal alone holds too.
//
// A later member's status takes the slot on the chain's own goroutine, not
// the reader's, while the chain holds the place of the last member it
// admitted (keep), so the chains waiting for the slot stay within the flow
// cap.
func (c *chain) status(ctx context.Context, n int) *resources.Buf {
	p := c.p
	c.room(n)
	if buf, ok := p.sh.reply.TryGet(p.l.remote, n); ok {
		c.free()
		return buf
	}
	c.flush(splitReplyPool, c.cur.tr)
	if !c.slot {
		select {
		case p.status <- struct{}{}:
			c.slot = true
		case <-ctx.Done():
			return nil
		}
	}
	defer c.free()
	return p.sh.reserveStatus(ctx, p.l.remote, n, p.l.close)
}

// keep holds the place of a member just answered, where message would give
// it back: the chain gives it back once the next member is admitted, once
// that member's first try would wait, or at the chain's end. A refused
// member, which holds no admission of its own, so waits for its status while
// the chain still holds one place, and the chains waiting for a status stay
// within the flow cap (§4.7). The chain never waits holding a place; admit
// makes its first try in one.
func (c *chain) keep(release func()) {
	c.giveBack()
	c.kept = release
}

// giveBack gives back the place keep holds, if any.
func (c *chain) giveBack() {
	if c.kept != nil {
		k := c.kept
		c.kept = nil
		k()
	}
}

// free gives back the connection's status wait, where the chain holds it.
func (c *chain) free() {
	if c.slot {
		c.slot = false
		<-c.p.status
	}
}

// ended is ctx ended, for a member's first try: the chain's one endedCtx,
// repointed at ctx, which a stage, as any call's, keeps no further than its
// return.
func (c *chain) ended(ctx context.Context) context.Context {
	c.try.Context = ctx
	return &c.try
}

// lookup asks the replay cache under an ended context first: when its
// copy's reservation would wait, the replies the chain holds go out, and it
// is asked again under the member's deadline, so the chain never waits on
// the reply pool holding it (§4.7).
func (c *chain) lookup(ctx context.Context, h Header) (*resources.Buf, error) {
	cached, err := c.p.st.Replay.Lookup(c.ended(ctx), h)
	var d *vfs.Delay
	if errors.As(err, &d) && d.Cause == vfs.DelayResources {
		c.flush(splitReplyPool, c.cur.tr)
		return c.p.st.Replay.Lookup(ctx, h)
	}
	return cached, err
}

func (c *chain) answered(tr *track, h Header, reply *resources.Buf, key uint64, s Status, failed bool, err error) {
	c.failed, c.failS = failed, s
	if tr.cached {
		// A hit's copy is reserved under no client's key, which Join refuses
		// beside the chain's buffers: it is copied into a buffer of the
		// chain's own, tried at once. When that does not fit, the replies
		// before it go out and the hit goes in a record of its own, so
		// nothing waits holding the cache's copy (§4.7).
		//
		// ponytail: only a test protocol both chains and returns copies.
		// Drop the copy once Lookup is handed the client's key, when a
		// protocol that chains returns copies.
		c.room(len(reply.B))
		own, ok := c.p.sh.reply.TryGet(c.p.l.remote, len(reply.B))
		if !ok {
			c.flush(splitReplyPool, tr)
			c.hold(held{tr: tr, h: h, reply: reply, key: key, final: true, err: err})
			c.flush(splits, nil)
			return
		}
		own.B = append(own.B[:0], reply.B...)
		reply.Release()
		reply = own
	}
	c.hold(held{tr: tr, h: h, reply: reply, key: key, s: s, final: true, err: err})
}

// unanswered ends the chain at a member a NoReply row answered: the members
// after it do not run, and the replies before it go out with its interim,
// where its table holds, which grants its credits and those of the members
// after it, whose headers alone are decoded (§4.7).
func (c *chain) unanswered(tr *track, h Header, tb *OpTable, reply *resources.Buf, err error, release func()) {
	p := c.p
	release()
	defer func() { c.stopped = true }() // once its interim, if any, is out
	if tb == nil || !tb.Holds {
		if reply != nil {
			reply.Release()
		}
		p.end(tr, false, 0, 0, err)
		c.flush(splits, nil)
		return
	}
	if reply == nil {
		if reply = c.status(tr.ctx, p.st.Header.ReplyLen(h)); reply == nil {
			p.end(tr, false, 0, 0, err)
			c.flush(splits, nil)
			return
		}
	}
	after := laterHeaders.Get().(*[]Header)
	*after = c.later(*after)
	reply.B = reply.B[:0]
	ierr := p.st.Hold.Interim(h, true, *after, reply)
	clear(*after)
	*after = (*after)[:0]
	laterHeaders.Put(after)
	if ierr != nil {
		reply.Release()
		p.end(tr, false, 0, 0, ierr)
		c.flush(splits, nil)
		c.close(ierr)
		return
	}
	c.hold(held{tr: tr, h: h, reply: reply, err: err})
	c.flush(splits, nil)
}

// laterHeaders is the slices of later members' headers an interim is handed,
// taken only on a drain's refusal: a connection runs up to its cap of
// requests at once, so no per-connection slice is safe (§4.7).
var laterHeaders = sync.Pool{New: func() any { s := make([]Header, 0, 8); return &s }}

// later appends to hs the headers of the members after the one running, each
// at the next of the one before; one that does not decode, or a step that is
// not forward inside the record, ends them.
func (c *chain) later(hs []Header) []Header {
	p := c.p
	off, next := c.cur.off, c.cur.h.Next()
	for next > off && next < len(c.rec.B) {
		h, err := p.st.Header.Decode(c.rec, next)
		if h == nil || err != nil {
			break
		}
		hs = append(hs, h)
		off, next = next, h.Next()
	}
	return hs
}

// hold adds a reply to the record the chain is building; once the chain has
// stopped, the reply is dropped and its request ended unsent.
func (c *chain) hold(m held) {
	if c.stopped {
		m.reply.Release()
		c.p.end(m.tr, false, 0, 0, m.err)
		return
	}
	m.n = len(m.reply.B)
	c.held = append(c.held, m)
	c.charge += m.reply.Charge()
}

// flush sends the record the chain holds, joined and wrapped once, unless it
// is a header's refusal, which no Wrap sees; why, where it is one of the
// split reasons, counts the split under tr's series. A flush that holds
// nothing changes nothing, and a refusal's record keeps its clear and
// isolated marks across a split inside it: only its end clears them.
func (c *chain) flush(why split, tr *track) {
	p := c.p
	if len(c.held) == 0 {
		return
	}
	defer func() { c.held, c.charge = c.held[:0], 0 }()
	if why < splits && tr != nil {
		tr.series().split(why)
	}
	rec, err := c.join()
	if err == nil && !c.clear {
		err = p.st.Security.Wrap(c.held[0].h, rec)
	}
	if err != nil {
		if rec != nil {
			rec.Release()
		}
		for _, m := range c.held {
			p.end(m.tr, false, 0, 0, err)
		}
		c.close(err)
		return
	}
	// ponytail: the record is queued under its last member's key, so a
	// message that must go before a middle member's reply, sent once the
	// record is queued, finds none to go before. Queue it under every
	// member's key once held requests send interims that can race their
	// final replies.
	p.out(rec, c.held[len(c.held)-1].key)
	for _, m := range c.held {
		p.end(m.tr, m.final, m.s, m.n, m.err)
	}
}

// chainPadding is the room a joined record keeps for what HeaderCodec.Chain
// adds before each reply, which the record cap's check counts too: each
// member's ReplyLen already counts its padding, and a record that grows past
// its room reallocates.
const chainPadding = 7

// join makes one record of the held replies: one alone as it is, more
// through resources.Join, each appended after HeaderCodec.Chain points the
// one before at it, and released once copied, so its bytes are held past its
// charge for that copy alone (RFC 24 §2.5). The record's room is its
// replies' bytes, Chain's padding between them, and what Wrap adds in place,
// which the first member's ReplyLen bounds, never past the members' charges,
// which Join refuses: a reply an encoder grew past its charge grows the
// record past its room, uncharged, as a request alone sends such a reply.
func (c *chain) join() (*resources.Buf, error) {
	if len(c.held) == 1 {
		return c.held[0].reply, nil
	}
	n := 0
	bufs := make([]*resources.Buf, len(c.held))
	for i, m := range c.held {
		n += len(m.reply.B)
		bufs[i] = m.reply
	}
	n += chainPadding*(len(c.held)-1) + c.p.st.Header.ReplyLen(c.held[0].h)
	rec := resources.Join(min(n, int(c.charge)), bufs...)
	prev := 0
	var err error
	for i, m := range c.held {
		if i > 0 && err == nil {
			err = c.p.st.Header.Chain(rec, prev)
		}
		prev = len(rec.B)
		rec.B = append(rec.B, m.reply.B...)
		m.reply.Release()
	}
	return rec, err
}

// abort ends the chain on a nil header or a reply that does not encode: the
// connection closes with err, and nothing more is sent.
func (c *chain) abort(tr *track, err error) {
	c.drop(tr, err)
	c.close(err)
}

// drop ends the chain with nothing more sent: each held reply is released
// and its request ended with err, and so is tr's.
func (c *chain) drop(tr *track, err error) {
	p := c.p
	for _, m := range c.held {
		m.reply.Release()
		p.end(m.tr, false, 0, 0, err)
	}
	c.held, c.charge = c.held[:0], 0
	p.end(tr, false, 0, 0, err)
	c.stopped = true
}

// halted says the chain has stopped, its connection closed or a NoReply
// member answered, so the member running neither calls nor answers.
func (c *chain) halted() bool { return c.stopped }

// close closes the connection with err and stops the chain.
func (c *chain) close(err error) {
	c.stopped = true
	c.p.l.close(err)
}
