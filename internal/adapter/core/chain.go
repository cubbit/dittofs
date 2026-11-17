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

// ended is a context whose deadline has passed: Admit under it tries once
// and, where it would wait, answers its error at once, and Lookup tries its
// copy's reservation once, answering ErrDelay of DelayResources (§4.7).
var ended = func() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	cancel()
	return ctx
}()

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
	clear    bool  // the record is a header's refusal, which no Wrap sees
	isolated bool  // the record is a refusal's, ended before the next unrelated member
	prevH    Header
	prevR    *Request
	failed   bool // the member before failed, with failS
	failS    Status
	cur      entry // the member running
	stopped  bool  // a NoReply member ended the chain, or the connection closed
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
	p.add()
	if p.ctx.Err() != nil {
		p.done()
		if e.release != nil {
			e.release()
		}
		p.end(tr, false, 0, 0, nil)
		rec.Release()
		return
	}
	c := &chain{p: p, rec: rec}
	go c.run(e)
}

// run takes the chain's members in turn, from e, each at the next of the one
// before, and sends what the chain holds once the last has run. A step that
// does not lead forward to a header inside the record ends the chain: what it
// holds goes out, and the connection closes, since the members past it can
// be neither read nor answered (R93).
func (c *chain) run(e entry) {
	p := c.p
	defer p.done()
	defer c.rec.Release()
	for {
		c.one(&e)
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
	related := e.h.Related()
	if c.isolated && !related {
		c.flush(splits, nil) // a refusal's record ends before the next unrelated member
	}
	if e.err != nil && e.st == stageDecodeHeader {
		c.flush(splitDecode, e.tr)
		c.clear, c.isolated = true, true
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
		}
	}
	c.prevH = e.u
	switch {
	case e.cancel:
		p.end(e.tr, false, 0, 0, nil)
		return
	case e.err == nil:
	case e.st == stageUnwrap:
		c.flush(splitUnwrap, e.tr)
		c.isolated = true
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
	c.cur = *e
	c.prevR = p.message(e.tr, c.rec, e.u, prev, e.release, c)
}

// admit admits a member after the first: tried once under an ended context,
// and where it would wait while the chain holds replies, those go out first,
// so a chain never waits on flow control holding reply reservations (§4.7).
func (c *chain) admit(e *entry) {
	p := c.p
	release, err := p.st.Flow.Admit(ended, e.u)
	if err != nil && errors.Is(err, ended.Err()) {
		c.flush(splitFlow, e.tr)
		release, err = p.st.Flow.Admit(e.tr.ctx, e.u)
	}
	if err != nil {
		e.st, e.err = stageAdmit, err
		return
	}
	e.release = release
}

// refusal answers e's refusal at its stage, before Route, as the stages'
// table maps it, under h, into the record the chain is building.
func (c *chain) refusal(e *entry, h Header) {
	p := c.p
	p.refused(e.tr, e.st)
	s, send := p.mapped(e.tr, p.st.Errors, e.err, nil, h.Version(), e.st != stageUnwrap)
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
// member's status, with no call (R42).
func (c *chain) answerFailed(e *entry) {
	if e.release != nil {
		e.release()
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
		p.end(e.tr, false, 0, 0, err)
		c.close(err)
		return
	}
	c.hold(held{tr: e.tr, h: h, reply: buf, s: s, final: true, err: cause})
}

// reserve takes a member's reply buffer for the chain: when its charge would
// take the chain's held reservations past a quarter of the reply pool's
// ceiling, or it does not fit at once while the chain holds replies, those
// go out first, so a chain never waits on the reply pool holding it (§4.7).
func (c *chain) reserve(ctx context.Context, n int) (*resources.Buf, error) {
	p := c.p
	if len(c.held) > 0 && c.charge+p.sh.reply.Charge(n) > p.sh.reply.Ceiling()/4 {
		c.flush(splitQuarter, c.cur.tr)
	}
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

// status takes a status's buffer for the chain, as reserve takes a reply's.
func (c *chain) status(ctx context.Context, n int) *resources.Buf {
	p := c.p
	if buf, ok := p.sh.reply.TryGet(p.l.remote, n); ok {
		return buf
	}
	c.flush(splitReplyPool, c.cur.tr)
	return p.sh.reserveStatus(ctx, p.l.remote, n, p.l.close)
}

// lookup asks the replay cache under an ended context first: when its
// copy's reservation would wait, the replies the chain holds go out, and it
// is asked again under the member's deadline, so the chain never waits on
// the reply pool holding it (§4.7).
func (c *chain) lookup(ctx context.Context, h Header) (*resources.Buf, error) {
	cached, err := c.p.st.Replay.Lookup(ended, h)
	var d *vfs.Delay
	if errors.As(err, &d) && d.Cause == vfs.DelayResources {
		c.flush(splitReplyPool, c.cur.tr)
		return c.p.st.Replay.Lookup(ctx, h)
	}
	return cached, err
}

func (c *chain) answered(tr *track, h Header, reply *resources.Buf, key uint64, s Status, err error) {
	c.failed, c.failS = s != 0, s
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
	c.stopped = true
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

// hold adds a reply to the record the chain is building.
func (c *chain) hold(m held) {
	m.n = len(m.reply.B)
	c.held = append(c.held, m)
	c.charge += m.reply.Charge()
}

// flush sends the record the chain holds, joined and wrapped once, unless it
// is a header's refusal, which no Wrap sees; why, where it is one of the
// split reasons, counts the split under tr's series.
func (c *chain) flush(why split, tr *track) {
	p := c.p
	defer func() { c.held, c.charge, c.clear, c.isolated = c.held[:0], 0, false, false }()
	if len(c.held) == 0 {
		return
	}
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
	p.out(rec, c.held[len(c.held)-1].key)
	for _, m := range c.held {
		p.end(m.tr, m.final, m.s, m.n, m.err)
	}
}

// join makes one record of the held replies: one alone as it is, more
// through resources.Join, each appended after HeaderCodec.Chain points the
// one before at it, and released once copied, so its bytes are held past its
// charge for that copy alone (RFC 24 §2.5).
func (c *chain) join() (*resources.Buf, error) {
	if len(c.held) == 1 {
		return c.held[0].reply, nil
	}
	n := 0
	bufs := make([]*resources.Buf, len(c.held))
	for i, m := range c.held {
		n += cap(m.reply.B)
		bufs[i] = m.reply
	}
	rec := resources.Join(n, bufs...)
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

// abort ends the chain on a nil header: the connection closes with err, and
// nothing more is sent, the held replies included.
func (c *chain) abort(tr *track, err error) {
	p := c.p
	for _, m := range c.held {
		m.reply.Release()
		p.end(m.tr, false, 0, 0, err)
	}
	c.held, c.charge = c.held[:0], 0
	p.end(tr, false, 0, 0, err)
	c.close(err)
}

// close closes the connection with err and stops the chain.
func (c *chain) close(err error) {
	c.stopped = true
	c.p.l.close(err)
}
