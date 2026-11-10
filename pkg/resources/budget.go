// Package resources holds the node's counted memory pools and the pooled
// buffers drawn from them. It is a leaf: it imports only the standard library
// and the metrics registry, and every component that charges memory imports
// it, never the reverse.
//
// Nothing here has a lifecycle: NewBudget and NewBufPool start no goroutine
// and open nothing, and there is no Start or Close.
//
// ponytail: RFC 24's Detect, Split, ShareCap, Reclaim and §4.1's turns are
// absent; each lands with its first caller.
package resources

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
)

// Budget is one counted pool: a counter of bytes reserved that waits or
// refuses at its ceiling. Whether it waits or refuses is fixed when it is
// built.
//
// ponytail: one mutex over the count and a fair queue of waiters per key, so
// clients take turns at the ceiling; take the fast path as one atomic step
// with no waiter when a profile shows the lock limiting the request rate.
type Budget struct {
	name    string
	ceiling int64
	waits   bool
}

// NewBudget builds a pool of ceiling bytes. A waiting budget waits at its
// ceiling within the caller's context; a refusing one refuses at once.
func NewBudget(name string, ceiling int64, waits bool) *Budget {
	return &Budget{name: name, ceiling: ceiling, waits: waits}
}

// Reserve charges n bytes for key. A waiting budget waits within ctx; a
// refusing one refuses at once and ignores key. Under a ctx already past its
// deadline a waiting budget tries once and refuses at once.
func (b *Budget) Reserve(ctx context.Context, key netip.Addr, n int64) (Reservation, error) {
	return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, b.name, errors.ErrUnsupported)
}

// Reserved is the bytes reserved now, for gauges and the leak check.
func (b *Budget) Reserved() int64 {
	panic("not implemented")
}

// Reservation is bytes charged to one budget for one key. It is a value,
// moved and never copied, so taking one allocates nothing.
//
//nolint:unused // its fields are what Release returns; this stub's Release reads none yet
type Reservation struct {
	b   *Budget
	key netip.Addr
	n   int64
}

// Release returns the reservation's bytes to its budget and to its key's
// count, then zeroes the Reservation, so a second Release does nothing.
func (r *Reservation) Release() {
	panic("not implemented")
}

// Pools is what an adapter is handed: the four budgets on the request path.
type Pools struct{ Receive, Reply, Conn, Hold *Budget }
