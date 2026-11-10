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
	"context"
	"errors"
	"fmt"
	"net/netip"
)

// Budget is a counter of bytes reserved that refuses at its ceiling: the
// connection-state and hold budgets.
type Budget struct {
	name    string
	ceiling int64
}

// NewBudget builds a refusing budget of ceiling bytes.
func NewBudget(name string, ceiling int64) *Budget {
	return &Budget{name: name, ceiling: ceiling}
}

// Reserve charges n bytes, or refuses at once past the ceiling.
func (b *Budget) Reserve(n int64) (Reservation, error) {
	return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, b.name, errors.ErrUnsupported)
}

// Reserved is the bytes reserved now, for gauges and the leak check.
func (b *Budget) Reserved() int64 {
	panic("not implemented")
}

// WaitBudget is a counter of bytes reserved that waits at its ceiling, fair
// per key: the receive and reply budgets.
//
// ponytail: one mutex over the count and a fair queue of waiters per key, so
// clients take turns at the ceiling; take the fast path as one atomic step
// with no waiter when a profile shows the lock limiting the request rate.
type WaitBudget struct {
	name    string
	ceiling int64
}

// NewWaitBudget builds a waiting budget of ceiling bytes.
func NewWaitBudget(name string, ceiling int64) *WaitBudget {
	return &WaitBudget{name: name, ceiling: ceiling}
}

// Reserve charges n bytes for key, waiting its turn within ctx at the
// ceiling. Under a ctx already past its deadline it tries once and refuses at
// once.
func (b *WaitBudget) Reserve(ctx context.Context, key netip.Addr, n int64) (Reservation, error) {
	return Reservation{}, fmt.Errorf("resources: reserve %d bytes from %s: %w", n, b.name, errors.ErrUnsupported)
}

// Reserved is the bytes reserved now, for gauges and the leak check.
func (b *WaitBudget) Reserved() int64 {
	panic("not implemented")
}

// Reservation is bytes charged to one budget of either kind, for one key on
// a WaitBudget. It is a value, moved and never copied, so taking one
// allocates nothing.
//
//nolint:unused // its fields are what Release returns; this stub's Release reads none yet
type Reservation struct {
	b   *Budget
	w   *WaitBudget
	key netip.Addr
	n   int64
}

// Release returns the reservation's bytes to its budget, and to its key's
// count on a WaitBudget, then zeroes the Reservation, so a second Release does
// nothing.
func (r *Reservation) Release() {
	panic("not implemented")
}

// Budgets is what an adapter is handed: the four budgets on the request path.
type Budgets struct {
	Receive, Reply   *WaitBudget
	Connection, Hold *Budget
}
