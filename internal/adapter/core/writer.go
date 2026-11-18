package core

import (
	"net"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
)

// writeBound is the writer's queue's bound, by messages: the reader stops
// reading the connection at it, so the client's TCP window pushes back, and a
// worker that hands over a reply is never blocked (RFC 20 sections 6.1 and
// 6.2).
//
// ponytail: a fixed bound of 64 messages, which is also testproto's credit
// target. Set it from the connection's flow-control bound when a protocol
// bounds its session below that and the queue, not the credits, turns out to
// bind (RFC 24 section 5.1).
const writeBound = 64

// queued is one message the writer holds: its record, and the least and
// greatest keys of the replies it carries, 0 for none.
type queued struct {
	msg    *resources.Buf
	lo, hi uint64
}

// writer sends a connection's messages: a bounded queue, net.Buffers writes
// that each gather up to a record cap of bodies with each record framed, a
// write deadline per gathered write, and a failed or late write that closes
// the connection (RFC 20 sections 6.2, 6.4 and 6.6).
type writer struct {
	nc       net.Conn
	framer   Framer
	capBytes int // the bodies one gathered write carries past its first: the protocol's record cap
	timeout  time.Duration
	close    func(error) // Conn.Close: a failed write calls it on the writer's goroutine once run has ended, so it may stop the writer
	onBatch  func(n int) // a test's view of what one write gathered; nil otherwise

	mu       sync.Mutex
	batch    []queued    // the messages of the write in flight, reused across batches; run only
	bufs     net.Buffers // each batch's gathered write, reused across batches; run only
	hdrs     []byte      // their framed headers, appended into one; run only
	queue    []queued
	held     int // messages out took and has not yet written or released
	wake     chan struct{}
	room     chan struct{} // closed while the messages held are below the bound
	closed   bool
	stopped  chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// newWriter starts a writer over nc, framing with f and bounding each gathered
// write by timeout, where it is not zero; closeConn is the connection's close.
func newWriter(nc net.Conn, f Framer, timeout time.Duration, closeConn func(error)) *writer {
	capBytes, _ := f.MaxRecord()
	w := &writer{
		nc:       nc,
		framer:   f,
		capBytes: capBytes,
		timeout:  timeout,
		close:    closeConn,
		wake:     make(chan struct{}, 1),
		room:     make(chan struct{}),
		stopped:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	close(w.room) // nothing is queued
	go w.run()
	return w
}

// out queues msg, returning before it is sent: past the bound the queue still
// takes it, so a caller never blocks. lo and hi are the least and greatest
// keys of the replies msg carries; a message handed with before goes out
// ahead of the first queued message whose keys span it. The writer takes the
// message's reference whatever it returns.
func (w *writer) out(msg *resources.Buf, lo, hi, before uint64) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		msg.Release()
		return net.ErrClosed
	}
	at := len(w.queue)
	if before != 0 {
		for i := range w.queue {
			if w.queue[i].lo <= before && before <= w.queue[i].hi {
				at = i
				break
			}
		}
	}
	w.queue = append(w.queue, queued{})
	copy(w.queue[at+1:], w.queue[at:])
	w.queue[at] = queued{msg: msg, lo: lo, hi: hi}
	w.held++
	if w.held >= writeBound && isClosed(w.room) {
		w.room = make(chan struct{})
	}
	w.mu.Unlock()

	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// pause returns a channel closed once the messages held fall below the bound,
// and whether they are at it: the reader waits there rather than refuse a record
// (RFC 20 section 6.2).
func (w *writer) pause() (<-chan struct{}, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held < writeBound {
		return nil, false
	}
	return w.room, true
}

// stop ends the writer: what is queued is released unsent. The connection's
// teardown calls it last.
func (w *writer) stop() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		// A write the peer holds ends now rather than at write_timeout.
		_ = w.nc.SetWriteDeadline(time.Now().Add(-time.Second))
		q := w.queue
		w.queue = nil
		w.held -= len(q)
		if !isClosed(w.room) {
			close(w.room)
		}
		w.mu.Unlock()
		for _, m := range q {
			m.msg.Release()
		}
		close(w.stopped)
		<-w.done
	})
}

// run writes each wake's messages in gathered writes, until the queue is
// empty. A failed write ends it, and the connection's close runs once done is
// closed, so a close that stops the writer does not wait on itself.
func (w *writer) run() {
	var failed error
	defer func() {
		close(w.done)
		if failed != nil {
			w.close(failed)
		}
	}()
	for {
		select {
		case <-w.wake:
		case <-w.stopped:
			return
		}
		for w.queued() {
			if err := w.flush(); err != nil {
				if w.fail() {
					failed = err
				}
				return
			}
		}
	}
}

// queued reports whether any message waits.
func (w *writer) queued() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue) > 0
}

// take moves one gathered write's messages into w.batch, the first queued and
// those behind it while their bodies stay within the record cap, so
// write_timeout bounds no more bytes than the largest single record's. It arms
// the write deadline for them under the mutex: a stop that set a deadline in
// the past is never overwritten, because a closed writer takes nothing.
func (w *writer) take() []queued {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return nil
	}
	n, size := 1, len(w.queue[0].msg.B)
	for n < len(w.queue) && size+len(w.queue[n].msg.B) <= w.capBytes {
		size += len(w.queue[n].msg.B)
		n++
	}
	w.batch = append(w.batch[:0], w.queue[:n]...)
	k := copy(w.queue, w.queue[n:])
	clear(w.queue[k:])
	w.queue = w.queue[:k]
	if !w.closed && w.timeout > 0 {
		_ = w.nc.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	return w.batch
}

// written accounts for n messages once their write returned: they leave the
// bound's count, and a paused reader is released when it falls below it.
func (w *writer) written(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held -= n
	if w.held < writeBound && !isClosed(w.room) {
		close(w.room)
	}
}

// flush sends the messages one gathered write takes in one net.Buffers write,
// its records framed and its deadline armed, and releases each once written.
func (w *writer) flush() error {
	q := w.take()
	if len(q) == 0 {
		return nil
	}
	if w.onBatch != nil {
		w.onBatch(len(q))
	}
	w.hdrs = slices.Grow(w.hdrs[:0], w.framer.HeaderLen()*len(q))
	w.bufs = w.bufs[:0]
	for _, m := range q {
		at := len(w.hdrs)
		w.hdrs = w.framer.Frame(w.hdrs, len(m.msg.B))
		w.bufs = append(w.bufs, w.hdrs[at:], m.msg.B)
	}
	b := w.bufs // WriteTo consumes the slice it is called on
	_, err := b.WriteTo(w.nc)
	clear(w.bufs)
	w.written(len(q))
	for _, m := range q {
		m.msg.Release()
	}
	clear(q)
	return err
}

// fail ends the writer on a failed or late write: it releases what is queued
// and reports whether the connection's close is owed, which it is unless a
// stop came first.
func (w *writer) fail() bool {
	w.mu.Lock()
	q := w.queue
	w.queue = nil
	w.held -= len(q)
	if !isClosed(w.room) {
		close(w.room)
	}
	closed := w.closed
	w.closed = true
	w.mu.Unlock()
	for _, m := range q {
		m.msg.Release()
	}
	return !closed
}

// isClosed reports whether ch is closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
