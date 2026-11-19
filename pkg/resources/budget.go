// Package resources holds the node's memory budgets and the pooled buffers
// drawn from them. It is a leaf: it imports only the standard library
// and the metrics registry, and every component that charges memory imports
// it, never the reverse.
//
// Nothing here has a lifecycle: NewBudget, NewWaitBudget, NewConnLimit and
// NewBufPool start no goroutine and open nothing, and there is no Start or
// Close.
//
// ponytail: RFC 24's Detect, Split, ShareCap, Reclaim and §4.1's turns are
// absent. ShareCap, Reclaim and the turns each land with their first caller;
// Detect and SplitFor replace the fixed figures the node builds its budgets
// at.
package resources

import (
	"container/list"
	"context"
	"errors"
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

// negative refuses a charge below zero, which would lower the count with no
// trace.
func negative(c *count, n int64) error {
	return fmt.Errorf("resources: reserve %d bytes from %s: a charge cannot be negative", n, c.name)
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
	if n < 0 {
		return Reservation{}, negative(c, n)
	}
	c.mu.Lock()
	if n > c.ceiling-c.reserved {
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

// ErrTooLarge is what a WaitBudget's Reserve, and so a BufPool's Get, wraps
// when it refuses at once, under any ctx, a charge past the ceiling. It
// matches no context error, so a caller tells it from a wait's end.
var ErrTooLarge = errors.New("resources: charge past the ceiling")

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
	if n < 0 {
		return Reservation{}, negative(c, n)
	}
	// decision: a charge that can never fit is refused at once, under any ctx,
	// with an error that matches no context error: waiting could not grant it,
	// it would hold every waiter behind it, and the caller must read it as a
	// fault of configuration, not as a delay to retry. Withdraw it if a caller
	// is found that must queue a charge larger than its budget.
	if n > c.ceiling {
		return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s, whose ceiling is %d: %w", n, c.name, c.ceiling, ErrTooLarge)
	}

	c.mu.Lock()
	if b.chargeLocked(n) {
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
		past, first = b.releaseLocked(n)
	default:
		// If it was the head, those it held back may fit now.
		b.waiters.Remove(elem)
		b.grantLocked()
	}
	c.mu.Unlock()
	if past {
		c.reportPastZero(first)
	}
	return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, c.name, ctx.Err())
}

// try charges n bytes for key only when they fit and no waiter is ahead of
// them, and otherwise returns false at once, allocating nothing.
func (b *WaitBudget) try(key netip.Addr, n int64) (Reservation, bool) {
	c := &b.c
	c.mu.Lock()
	ok := b.chargeLocked(n)
	c.mu.Unlock()
	if !ok {
		return Reservation{}, false
	}
	return Reservation{w: b, key: key, n: n}, true
}

// chargeLocked charges n bytes when they fit under the ceiling and no waiter
// is ahead of them: Reserve's path that does not wait, and try's.
func (b *WaitBudget) chargeLocked(n int64) bool {
	c := &b.c
	if b.waiters.Len() > 0 || n > c.ceiling-c.reserved {
		return false
	}
	c.reserved += n
	return true
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

// ceiling is the most the budget reserves at once, and so the largest charge
// Reserve can grant.
func (b *WaitBudget) ceiling() int64 {
	return b.c.ceiling
}

func (b *WaitBudget) release(n int64) {
	c := &b.c
	c.mu.Lock()
	past, first := b.releaseLocked(n)
	c.mu.Unlock()
	if past {
		c.reportPastZero(first)
	}
}

// releaseLocked lowers the count by n and grants each waiter that then fits,
// under the mutex. Release and a cancelled waiter giving back its grant both
// call it, so the two stay one code.
func (b *WaitBudget) releaseLocked(n int64) (past, first bool) {
	past, first = b.c.lowerLocked(n)
	b.grantLocked()
	return past, first
}

// Reservation is bytes charged to one budget of either kind, for one key on
// a WaitBudget. It is a value, moved and never copied, so a grant that does
// not wait allocates nothing.
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

// Budgets is what an adapter is handed (RFC 20 section 3.4): the four budgets
// on the request path, and the node's connection count.
type Budgets struct {
	Receive, Reply   *WaitBudget
	Connection, Hold *Budget
	Conns            *ConnLimit // section 4.4: one count per node, shared by every adapter
}

// ConnLimit counts a node's open connections against connection_limit and
// each address's quarter of it (RFC 24 section 4.4), across every adapter. It
// counts connections, not bytes: their memory is the connection-state
// budget's.
type ConnLimit struct {
	mu    sync.Mutex
	limit int
	open  int                // the node's connections open now
	addrs map[netip.Addr]int // one address's connections open now, its entry dropped at zero
}

// quarter is the most connections one address may hold of l's limit now: a
// quarter of it, rounded down and at least one (RFC 24 section 4.4).
func (l *ConnLimit) quarter() int { return max(l.limit/4, 1) }

// NewConnLimit builds a count of limit connections for the node, its
// addresses each held to a quarter of it.
func NewConnLimit(limit int) *ConnLimit {
	return &ConnLimit{limit: limit, addrs: map[netip.Addr]int{}}
}

// Take counts one connection from addr at accept, or refuses at once:
// ErrConnLimit at the limit, ErrConnQuarter at addr's quarter. The node's
// limit is read before the address's quarter, the order RFC 24 section 4.4
// names them in, which decides the reason a socket both bounds would count
// its close under.
//
// decision: the addrs key is the address whole, so one host can fill the
// node's limit through four source addresses of one IPv6 /64: the line does
// not narrow to a /128 until RFC 24 grows that keying for it.
func (l *ConnLimit) Take(addr netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open >= l.limit {
		return ErrConnLimit
	}
	if l.addrs[addr] >= l.quarter() {
		return ErrConnQuarter
	}
	l.open++
	l.addrs[addr]++
	return nil
}

// Release returns one connection of addr, at the connection's teardown, its
// address's entry dropped at zero.
func (l *ConnLimit) Release(addr netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if l.addrs[addr] <= 1 {
		delete(l.addrs, addr)
		return
	}
	l.addrs[addr]--
}

// SetLimit applies a changed connection_limit to connections accepted
// afterwards; those open stay. An address's quarter is limit / 4, at least 1.
// It changes nothing else: the connections open stay counted, per address
// too, so a Take after it tells the new limit from the count as it was.
func (l *ConnLimit) SetLimit(limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
}

var (
	// ErrConnLimit is what Take refuses a connection at the node's
	// connection_limit with.
	ErrConnLimit = errors.New("resources: at the connection limit")
	// ErrConnQuarter is what Take refuses a connection from an address
	// already holding a quarter of the limit with.
	ErrConnQuarter = errors.New("resources: address at its quarter of the connection limit")
)
