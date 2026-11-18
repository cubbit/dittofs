package core

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter"
)

// connCharge is each connection's fixed cost to the connection-state budget,
// its read buffer included, as RFC 24 section 4.4 proposes it.
const connCharge = 64 << 10

// The accept backoff's bounds, lifted from the accept loop the node runs
// today. An unexpected Accept error, the process at its file-descriptor limit
// say, otherwise spins the loop hot. The sleep starts small so a transient
// failure costs nothing, doubles per consecutive failure, and caps at a
// ceiling that keeps the loop responsive to Stop (RFC 20 section 6.4).
const (
	acceptRetryBase = 10 * time.Millisecond
	acceptRetryMax  = 1 * time.Second
)

// listenerLostAfter is how long a bound socket's Accept may keep failing from
// its first failure with no connection accepted since before the server
// reports the listener lost (RFC 20 section 6.4). The evidence runs both
// ways: the first failure 60 s back, and the last one within twice the
// backoff's cap, since a listener that stopped failing is not lost however
// long it has not accepted.
const listenerLostAfter = 60 * time.Second

// conditionListenerLost is the condition Health reports for a lost listener,
// RFC 25 section 3.1's name for it.
const conditionListenerLost = "adapter_listener_lost"

// acceptIdle is how long an accept loop may sit in Accept before it reports
// itself idle, 10 s, inside RFC 25 section 3.5's 30 s; a loop that reports
// neither a turn nor an idle within 60 s fails the node's liveness.
//
// decision: the deadline wakes the loop by setting the listener's own
// deadline in the past, so Accept returns os.ErrDeadlineExceeded; the
// listener carries no deadline of its own while the loop waits (RFC 20
// section 6.4). The span is armed on the server's clock, the listener's
// deadline on the runtime's, since the socket's deadlines are its own.
const acceptIdle = 10 * time.Second

// acceptLoop serves one bound socket until its listener closes, which Stop
// does. It registers itself with Deps.Watch at bind and closes the watch at
// unbind: a Turn per accepted connection, an Idle at each accept deadline
// the clock reaches while no client arrives; a nil Watch watches nothing
// (RFC 20 sections 3.4 and 3.7). Every accepted socket is charged to the
// connection-state budget, closed at once when the budget refuses it, before
// any byte is read, and handed to serveConn with the charge when it does not.
// An Accept error but the listener's own close and the deadline's own wake
// is retried after the backoff; the first and the last failure of each
// streak are recorded on the server's clock, so Health derives the lost
// condition from the evidence.
func (s *Server) acceptLoop(b *bound) {
	defer close(b.loopDone)
	var watch adapter.Loop
	if s.d.Watch != nil {
		watch = s.d.Watch(fmt.Sprintf("accept %v on %s", b.l.Services, b.addr))
		defer watch.Close()
	}
	dl, wakeable := b.ln.(interface{ SetDeadline(time.Time) error })
	var idle timer
	if wakeable {
		// A listener with no deadline of its own, a test's, parks as before.
		idle = s.armIdle(dl)
		defer func() { idle.Stop() }()
	}
	backoff := time.Duration(0)
	for {
		nc, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				// Stop closed the listener: the loop ends.
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// The accept deadline's own wake: no Accept failure, so no
				// backoff and no failure recorded. The loop reports Idle,
				// clears the listener's deadline and arms the next span
				// counted from now, never from the one that passed, so a
				// deadline already past cannot spin it (RFC 20 section 6.4).
				if watch != nil {
					watch.Idle()
				}
				_ = dl.SetDeadline(time.Time{})
				idle = s.armIdle(dl)
				continue
			}
			if backoff == 0 {
				backoff = acceptRetryBase
			} else if backoff < acceptRetryMax {
				backoff = min(2*backoff, acceptRetryMax)
			}
			f := s.sh.clock.Now().UnixNano()
			if last := b.lastFailure.Load(); last != 0 && f-last > int64(2*acceptRetryMax) {
				// The last run stopped failing: this failure starts a new
				// one, its backoff over (RFC 20 section 6.4 clears the
				// condition once the failures stop).
				b.firstFailure.Store(f)
				backoff = acceptRetryBase
			}
			b.firstFailure.CompareAndSwap(0, f)
			b.lastFailure.Store(f)
			s.sh.log.Debug("core: accept failed", "err", err, "services", b.l.Services, "retry_in", backoff)
			if s.sleep(b, backoff) {
				// Stop closed the socket mid-backoff: not one more Accept.
				return
			}
			continue
		}
		// The listener is alive again: the streak ends, and with it the lost
		// condition's evidence.
		backoff = 0
		b.firstFailure.Store(0)
		b.lastFailure.Store(0)
		// ponytail: the gate here is the connection-state budget's one global
		// charge alone, with no per-address cap, so one address can take every
		// slot. Lift it when connection_limit and the per-address quarter land
		// (RFC 24 sections 4.4 and 9 R10).
		if watch != nil {
			watch.Turn()
		}
		charge, cerr := s.d.Budgets.Connection.Reserve(connCharge)
		if cerr != nil {
			// The budget is full: the socket closes at once, before any byte
			// is read, and the server holds nothing of it.
			_ = nc.Close()
			continue
		}
		s.serveConn(nc, b.l, b, &charge)
	}
}

// armIdle arms the accept deadline's next span: the wake, firing on the
// server's clock, sets the listener's deadline a second behind the runtime's
// now, which ends the Accept the loop sits in.
func (s *Server) armIdle(dl interface{ SetDeadline(time.Time) error }) timer {
	return s.sh.clock.AfterFunc(acceptIdle, func() { _ = dl.SetDeadline(time.Now().Add(-time.Second)) })
}

// lost reports whether b's listener is lost: its Accept has failed from 60 s
// back and within twice the backoff's cap, with no connection accepted since.
// Health derives the condition from this evidence, and clears it at the next
// accepted connection.
func (b *bound) lost(now time.Time) bool {
	first := b.firstFailure.Load()
	if first == 0 {
		return false
	}
	last := b.lastFailure.Load()
	return now.Sub(time.Unix(0, first)) >= listenerLostAfter &&
		now.Sub(time.Unix(0, last)) <= 2*acceptRetryMax
}

// sleep waits for d on the server's clock, and reports whether the socket's
// listener closed instead, as Stop does: the caller ends at once, and does
// not spin one more Accept against a closed socket.
func (s *Server) sleep(b *bound, d time.Duration) (stopped bool) {
	wake := make(chan struct{})
	t := s.sh.clock.AfterFunc(d, func() { close(wake) })
	defer t.Stop()
	select {
	case <-wake:
	case <-b.stopped:
		return true
	}
	return false
}

// stopOnLoops waits for each of bs's accept loops to end. The sockets are
// closed already, which ends each loop, in its Accept or in its backoff's
// sleep; a connection its loop hands to serveConn as it closes finds the
// server draining, is refused and is torn down on the loop, OnClose
// included, before the loop ends.
func (s *Server) stopOnLoops(bs []*bound) {
	for _, b := range bs {
		<-b.loopDone
	}
}
