// Package resources holds the node's memory budgets and the pooled buffers
// drawn from them. It is a leaf: it imports only the standard library
// and the metrics registry, and every component that charges memory imports
// it, never the reverse.
//
// Nothing here has a lifecycle: NewBudget, NewWaitBudget and NewBufPool start
// no goroutine and open nothing, and there is no Start or Close.
//
// ponytail: RFC 24's Detect, Split, ShareCap, Reclaim and §4.1's turns are
// absent; each lands with its first caller.
package resources

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
)

// count is the bytes reserved from one budget of either kind, under its one
// mutex.
type count struct {
	name    string
	ceiling int64

	panics bool         // a release past zero panics: set in a test binary, read once at construction
	log    *slog.Logger // where a clamp is logged; nil is slog's default

	mu       sync.Mutex
	reserved int64
	pastZero bool // a release past zero was already logged
}

func newCount(name string, ceiling int64) count {
	return count{name: name, ceiling: ceiling, panics: testing.Testing()}
}

func (c *count) read() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved
}

// lowerLocked takes n bytes off the count. It reports whether they would have
// taken it below zero, and whether that is the first time on this budget.
//
// decision: a count that would go below zero is clamped to zero before
// anything else happens, so a test that recovers the panic, and production
// after the clamp, both read zero. A second Release through one Reservation
// never gets here, since Release zeroes it; a copy released twice does, and
// only once the count would go negative. Withdraw the clamp if a budget is
// ever allowed to run a debt.
func (c *count) lowerLocked(n int64) (past, first bool) {
	c.reserved -= n
	if c.reserved >= 0 {
		return false, false
	}
	c.reserved = 0
	first = !c.pastZero
	c.pastZero = true
	return true, first
}

// reportPastZero is called with the mutex released, so the budget stays usable
// after the panic.
func (c *count) reportPastZero(first bool) {
	if c.panics {
		panic(fmt.Sprintf("resources: release past zero on %s", c.name))
	}
	// ponytail: one Error record per budget stands in for RFC 24 §10's count
	// of releases past zero, since a budget is given no registry and no
	// logger; the counter replaces it when a budget can reach a registry.
	if first {
		log := c.log
		if log == nil {
			log = slog.Default()
		}
		log.Error("resources: release past zero, count clamped", "budget", c.name)
	}
}

// Budget is a counter of bytes reserved that refuses at its ceiling: the
// connection-state and hold budgets.
type Budget struct {
	c count
}

// NewBudget builds a refusing budget of ceiling bytes.
func NewBudget(name string, ceiling int64) *Budget {
	return &Budget{c: newCount(name, ceiling)}
}

// Reserve charges n bytes, or refuses at once past the ceiling. It decides
// from the count alone and never waits.
func (b *Budget) Reserve(n int64) (Reservation, error) {
	c := &b.c
	c.mu.Lock()
	if n < 0 || n > c.ceiling-c.reserved {
		reserved := c.reserved
		c.mu.Unlock()
		return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %d of %d reserved", n, c.name, reserved, c.ceiling)
	}
	c.reserved += n
	c.mu.Unlock()
	return Reservation{b: b, n: n}, nil
}

// Reserved is the bytes reserved now, for gauges and the leak check.
func (b *Budget) Reserved() int64 {
	return b.c.read()
}

func (b *Budget) release(n int64) {
	c := &b.c
	c.mu.Lock()
	past, first := c.lowerLocked(n)
	c.mu.Unlock()
	if past {
		c.reportPastZero(first)
	}
}

// WaitBudget is a counter of bytes reserved that waits at its ceiling: the
// receive and reply budgets.
//
// ponytail: waiters are granted in arrival order from one queue, and nothing
// passes the head, so a large reservation is not starved by small ones but
// one busy address can take the whole budget while the others wait their
// place. RFC 24 §4.1's turns per key replace the queue before an adapter
// serves clients it does not trust on a WaitBudget; the key is already in
// Reserve's signature and in the Reservation, so no caller changes.
//
// ponytail: one mutex over the count and the queue; take the fast path as one
// atomic step with no waiter when a profile shows the lock limiting the
// request rate.
type WaitBudget struct {
	c       count
	waiters list.List // of *waiter, oldest first
}

// waiter is one Reserve that did not fit. ready is closed, under the mutex,
// by the release that charged its bytes.
type waiter struct {
	n     int64
	ready chan struct{}
}

// NewWaitBudget builds a waiting budget of ceiling bytes.
func NewWaitBudget(name string, ceiling int64) *WaitBudget {
	return &WaitBudget{c: newCount(name, ceiling)}
}

// Reserve charges n bytes for key. When they do not fit under the ceiling, or
// another Reserve is already waiting, it waits its place within ctx, holding
// nothing. Under a ctx already past its deadline or cancelled it tries once:
// it takes the charge when it fits and no waiter is ahead of it, and
// otherwise refuses at once.
func (b *WaitBudget) Reserve(ctx context.Context, key netip.Addr, n int64) (Reservation, error) {
	c := &b.c
	// decision: a charge that can never fit is refused at once, under any ctx,
	// with an error that matches no context error: waiting could not grant it,
	// it would hold every waiter behind it, and the caller must read it as a
	// fault of configuration, not as a delay to retry. Withdraw it if a caller
	// is found that must queue a charge larger than its budget.
	if n < 0 || n > c.ceiling {
		return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: its ceiling is %d", n, c.name, c.ceiling)
	}

	c.mu.Lock()
	if b.waiters.Len() == 0 && n <= c.ceiling-c.reserved {
		c.reserved += n
		c.mu.Unlock()
		return Reservation{w: b, key: key, n: n}, nil
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, c.name, err)
	}
	w := &waiter{n: n, ready: make(chan struct{})}
	elem := b.waiters.PushBack(w)
	c.mu.Unlock()

	select {
	case <-w.ready:
		return Reservation{w: b, key: key, n: n}, nil
	case <-ctx.Done():
	}

	c.mu.Lock()
	var past, first bool
	select {
	case <-w.ready:
		// A release granted it before the lock was taken: the bytes go back,
		// which may be the room the next waiter needs.
		past, first = c.lowerLocked(n)
	default:
		// If it was the head, those it held back may fit now.
		b.waiters.Remove(elem)
	}
	b.grantLocked()
	c.mu.Unlock()
	if past {
		c.reportPastZero(first)
	}
	return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, c.name, ctx.Err())
}

// grantLocked charges each waiter from the head while the head's bytes fit,
// on the calling goroutine, and stops at the first that does not.
func (b *WaitBudget) grantLocked() {
	c := &b.c
	for e := b.waiters.Front(); e != nil; e = b.waiters.Front() {
		w := e.Value.(*waiter)
		if w.n > c.ceiling-c.reserved {
			return
		}
		c.reserved += w.n
		b.waiters.Remove(e)
		close(w.ready)
	}
}

// Reserved is the bytes reserved now, for gauges and the leak check.
func (b *WaitBudget) Reserved() int64 {
	return b.c.read()
}

func (b *WaitBudget) release(n int64) {
	c := &b.c
	c.mu.Lock()
	past, first := c.lowerLocked(n)
	b.grantLocked()
	c.mu.Unlock()
	if past {
		c.reportPastZero(first)
	}
}

// Reservation is bytes charged to one budget of either kind, for one key on
// a WaitBudget. It is a value, moved and never copied, so taking one
// allocates nothing.
type Reservation struct {
	b *Budget
	w *WaitBudget
	// ponytail: the key is kept and no count per key is, since nothing reads
	// one; RFC 24 §4.1's cap adds the count when the turns land.
	key netip.Addr
	n   int64
}

// Release returns the reservation's bytes to its budget, granting on a
// WaitBudget every waiter they make room for before it returns, and zeroes
// the Reservation, so a second Release does nothing.
func (r *Reservation) Release() {
	was := *r
	*r = Reservation{}
	switch {
	case was.b != nil:
		was.b.release(was.n)
	case was.w != nil:
		was.w.release(was.n)
	}
}

// Budgets is what an adapter is handed: the four budgets on the request path.
type Budgets struct {
	Receive, Reply   *WaitBudget
	Connection, Hold *Budget
}
