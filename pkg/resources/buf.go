package resources

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
)

// classes are the buffer sizes a pool hands out, smallest first: small,
// directory, transfer and large. A request draws the smallest class that holds
// it.
var classes = [...]int{4 << 10, 64 << 10, 1 << 20, 8 << 20}

// Buf is one pooled buffer, shared by reference count. Its reservation is
// released with its bytes, so budget and memory never disagree.
//
//nolint:unused // class, refs and res are what Retain and Release keep; these stubs read none yet
type Buf struct {
	B     []byte // len is the record's or reply's; cap is the class size
	class uint8
	refs  atomic.Int32
	res   Reservation
}

// BufPool hands out buffers in the classes above, each reserved from its
// budget before it is taken, so a Get waits in the budget, never in the
// allocator.
//
// ponytail: a sync.Pool per class behind the reservation counter, so
// collection scans and frees idle pooled buffers. Replace it with a
// fixed-size slab per class, allocated once at start, when a profile shows
// collection time from transfer-class buffers limiting throughput.
type BufPool struct {
	budget *WaitBudget
}

// NewBufPool builds a pool whose every buffer is reserved from b, a receive
// or reply budget.
func NewBufPool(b *WaitBudget) *BufPool {
	return &BufPool{budget: b}
}

// Get takes one reservation for key, as Reserve does, of n's class plus the
// fixed 8 KiB per-request overhead, released with the buffer; then a buffer
// with one reference.
func (p *BufPool) Get(ctx context.Context, key netip.Addr, n int) (*Buf, error) {
	return nil, fmt.Errorf("resources: get a %d-byte buffer: %w", n, errors.ErrUnsupported)
}

// Retain adds a borrower: the service, the writer, a held request.
func (b *Buf) Retain() *Buf {
	panic("not implemented")
}

// Release drops a borrower; the last release returns the bytes and the
// reservation.
func (b *Buf) Release() {
	panic("not implemented")
}
