package core

import (
	"net"
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

// queued is one message the writer holds: its record, and the key of the
// request whose reply it must precede.
type queued struct {
	msg *resources.Buf
	key uint64
}

// writer sends a connection's messages: a bounded queue, one net.Buffers write
// per wake with each record framed, a write deadline per gathered write, and a
// failed or late write that closes the connection (RFC 20 sections 6.2 and
// 6.4).
type writer struct {
	nc      net.Conn
	framer  Framer
	timeout time.Duration
	close   func(error)
	onBatch func(n int) // a test's view of what one write gathered; nil otherwise

	mu       sync.Mutex
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
	w := &writer{
		nc:      nc,
		framer:  f,
		timeout: timeout,
		close:   closeConn,
		wake:    make(chan struct{}, 1),
		room:    make(chan struct{}),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	close(w.room) // nothing is queued
	go w.run()
	return w
}

// out queues msg, returning before it is sent: past the bound the queue still
// takes it, so a caller never blocks. A message handed with before goes out
// ahead of the reply of that key still queued. The writer takes the message's
// reference whatever it returns.
func (w *writer) out(msg *resources.Buf, key, before uint64) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		msg.Release()
		return net.ErrClosed
	}
	at := len(w.queue)
	if before != 0 {
		for i := range w.queue {
			if w.queue[i].key == before {
				at = i
				break
			}
		}
	}
	w.queue = append(w.queue, queued{})
	copy(w.queue[at+1:], w.queue[at:])
	w.queue[at] = queued{msg: msg, key: key}
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
// empty.
func (w *writer) run() {
	defer close(w.done)
	for {
		select {
		case <-w.wake:
		case <-w.stopped:
			return
		}
		for w.queued() {
			if err := w.flush(); err != nil {
				w.fail(err)
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

// take removes every queued message and arms the write deadline for them, all
// under the mutex: a stop that set a deadline in the past is never overwritten,
// because a closed writer takes nothing.
func (w *writer) take() []queued {
	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.queue
	w.queue = nil
	if len(q) == 0 {
		return nil
	}
	if !w.closed && w.timeout > 0 {
		_ = w.nc.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	return q
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
	bufs := make(net.Buffers, 0, 2*len(q))
	for _, m := range q {
		bufs = append(bufs, w.framer.Frame(nil, len(m.msg.B)), m.msg.B)
	}
	_, err := bufs.WriteTo(w.nc)
	w.written(len(q))
	for _, m := range q {
		m.msg.Release()
	}
	return err
}

// fail closes the connection on a failed or late write and releases what is
// queued.
func (w *writer) fail(err error) {
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
	if !closed {
		w.close(err)
	}
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
