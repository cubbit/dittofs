package core

import (
	"context"
	"errors"
	"net"
	"time"
)

// connCharge is each connection's fixed cost to the connection-state budget,
// its read buffer included, as RFC 24 section 4.4 proposes it (R7).
const connCharge = 64 << 10

// The accept backoff's bounds, lifted from the accept loop develop runs. An
// unexpected Accept error, the process at its file-descriptor limit say,
// otherwise spins the loop hot. The sleep starts small so a transient failure
// costs nothing, doubles per consecutive failure, and caps at a ceiling that
// keeps the loop responsive to Stop (RFC 20 section 6.4).
const (
	acceptRetryBase = 10 * time.Millisecond
	acceptRetryMax  = 1 * time.Second
)

// listenerLostAfter is how long a bound socket's Accept may keep failing with
// no connection accepted since the first failure before the server raises
// conditionListenerLost (R10, RFC 20 section 6.4).
const listenerLostAfter = 60 * time.Second

// conditionListenerLost is the condition Health reports for a lost listener,
// RFC 25 section 3.1's name for it.
const conditionListenerLost = "adapter_listener_lost"

// acceptLoop serves one bound socket until its listener closes, which Stop
// does: every accepted socket is charged to the connection-state budget,
// closed at once when the budget refuses it, before any byte is read, and
// handed to serveConn with the charge when it does not (R7, D6). An Accept
// error but the listener's own close is retried after the backoff; the first
// failure of each streak is recorded on the server's clock, so Health derives
// the lost condition from the evidence (R10, D3).
func (s *Server) acceptLoop(b *bound) {
	defer close(b.loopDone)
	backoff := time.Duration(0)
	for {
		nc, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				// Stop closed the listener: the loop ends.
				return
			}
			if backoff == 0 {
				backoff = acceptRetryBase
			} else if backoff < acceptRetryMax {
				backoff = min(2*backoff, acceptRetryMax)
			}
			b.firstFailure.CompareAndSwap(0, s.sh.clock.Now().UnixNano())
			s.sh.log.Debug("core: accept failed", "err", err, "services", b.l.Services, "retry_in", backoff)
			s.sleep(b, backoff)
			continue
		}
		// The listener is alive again: the streak ends, and with it the lost
		// condition's evidence.
		backoff = 0
		b.firstFailure.Store(0)
		charge, cerr := s.d.Budgets.Connection.Reserve(connCharge)
		if cerr != nil {
			// The budget is full: the socket closes at once, before any byte
			// is read, and the server holds nothing of it (R7).
			_ = nc.Close()
			continue
		}
		s.serveConn(nc, b.l, &charge)
	}
}

// lost reports whether b's listener has failed for listenerLostAfter with no
// connection accepted since: the evidence Health derives the condition from
// (R10).
func (b *bound) lost(now time.Time) bool {
	since := b.firstFailure.Load()
	if since == 0 {
		return false
	}
	return now.Sub(time.Unix(0, since)) >= listenerLostAfter
}

// sleep waits for d on the server's clock, or ends at once once the socket's
// listener has closed, so Stop does not wait out a backoff (D3).
func (s *Server) sleep(b *bound, d time.Duration) {
	wake := make(chan struct{})
	t := s.sh.clock.AfterFunc(d, func() { close(wake) })
	defer t.Stop()
	select {
	case <-wake:
	case <-b.stopped:
	}
}

// stopOnLoops waits for each of bs's accept loops to end. The sockets are
// closed already, which ends each loop, in its Accept or in its backoff's
// sleep; ctx bounds the wait.
func (s *Server) stopOnLoops(bs []*bound, ctx context.Context) error {
	for _, b := range bs {
		select {
		case <-b.loopDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
