package resources

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
)

// classes are the buffer sizes a pool hands out, smallest first: small,
// directory, transfer and large. A request draws the smallest class that holds
// it.
var classes = [...]int{4 << 10, 64 << 10, 1 << 20, 8 << 20}

// headerRoom is what a class that carries a payload holds past its size, so a
// payload of the class's size still fits with its protocol's headers.
//
// ponytail: 4 KiB, fixed, the same for every protocol, and charged. It is a
// cost figure and not a limit: a record past its class's size plus this room
// draws the next class and costs more. Make it the protocol's own figure when
// a protocol's records are shown to pass it often enough to matter.
const headerRoom = 4 << 10

// overhead is charged with every buffer for what its request holds besides
// it: its goroutine's stack, its decoded arguments, its context.
//
// ponytail: a flat 8 KiB per request, not measured, so a request whose stack
// grows past it is under-charged. Charge the measured p99 stack when RFC 24
// §8's M1 shows stacks past an eighth of the unpooled quarter at the receive
// pool's ceiling.
const overhead = 8 << 10

// poisonByte is what a released buffer is filled with under the race detector.
const poisonByte = 0xdb

// unpooled is the class of a buffer larger than the largest class.
const unpooled = len(classes)

// capacity is the bytes a buffer of class holds, and is charged for: its size,
// plus the header room on every class but the small one, which carries
// headers only.
func capacity(class int) int {
	if class == 0 {
		return classes[0]
	}
	return classes[class] + headerRoom
}

// classOf is the smallest class whose capacity holds n bytes, or unpooled.
func classOf(n int) int {
	for class := range classes {
		if n <= capacity(class) {
			return class
		}
	}
	return unpooled
}

// Buf is one pooled buffer, shared by reference count. Its reservation is
// released with its bytes, so budget and memory never disagree.
type Buf struct {
	// B has the record's or reply's length. Its capacity is its class's: the
	// class size plus, on every class but the small one, 4 KiB of header room.
	B []byte

	// decision: the pool keeps its own slice of the bytes Get took, since a
	// holder may cut or reassign B: the last Release poisons and pools these,
	// not whatever B holds by then.
	mem   []byte
	pool  *BufPool // nil on a literal Buf, which has no bytes to return
	class int
	res   Reservation

	// decision: borrowers counts the holders beyond the first, so a literal
	// Buf, as a codec test builds one, is one reference like a buffer from
	// Get: Retain and Release pair on it and its last Release frees nothing.
	// The last Release leaves it at -1, where one more Release and any Retain
	// panic, in tests and in production. That holds only while the buffer
	// sits released: once the pool hands it out again, a stale Retain or
	// Release cannot be told from the new holder's, and what catches the
	// stale holder is the poison it reads under the race detector.
	borrowers atomic.Int32
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
	free   [len(classes)]sync.Pool // of *Buf
}

// NewBufPool builds a pool whose every buffer is reserved from b, a receive
// or reply budget.
func NewBufPool(b *WaitBudget) *BufPool {
	p := &BufPool{budget: b}
	for class := range p.free {
		p.free[class].New = func() any {
			return &Buf{mem: make([]byte, capacity(class)), pool: p, class: class}
		}
	}
	return p
}

// Get takes one reservation for key, as Reserve does, of the capacity of n's
// class plus the fixed 8 KiB per-request overhead, released with the buffer;
// then a buffer with one reference, whose B is n bytes long.
func (p *BufPool) Get(ctx context.Context, key netip.Addr, n int) (*Buf, error) {
	if n < 0 {
		return nil, fmt.Errorf("resources: get a %d-byte buffer", n)
	}
	class := classOf(n)
	charge := n
	if class != unpooled {
		charge = capacity(class)
	}
	res, err := p.budget.Reserve(ctx, key, int64(charge)+overhead)
	if err != nil {
		return nil, fmt.Errorf("resources: get a %d-byte buffer: %w", n, err)
	}
	// decision: a buffer past the largest class is charged n and the
	// overhead exactly, allocated at n, and dropped at its last Release. The
	// largest class is the pool's own, since a pool is told no protocol.
	if class == unpooled {
		mem := make([]byte, n)
		return &Buf{B: mem, mem: mem, pool: p, class: unpooled, res: res}, nil
	}
	b := p.free[class].Get().(*Buf)
	b.borrowers.Store(0)
	b.res = res
	// decision: B is handed out at the length asked, as make([]byte, n) would
	// be, so a reader fills it as it is; a caller that appends cuts it to
	// B[:0] first.
	b.B = b.mem[:n]
	return b, nil
}

// Retain adds a borrower: the service, the writer, a held request.
func (b *Buf) Retain() *Buf {
	b.borrow(1, "resources: retain of a released buffer")
	return b
}

// borrow moves the count of borrowers by delta and returns what it was. On a
// released buffer it panics having written nothing, so a Get that hands the
// buffer out in the same moment finds its count intact.
func (b *Buf) borrow(delta int32, released string) (was int32) {
	for {
		was = b.borrowers.Load()
		if was < 0 {
			panic(released)
		}
		if b.borrowers.CompareAndSwap(was, was+delta) {
			return was
		}
	}
}

// Release drops a borrower; the last release returns the bytes and the
// reservation. One more panics.
func (b *Buf) Release() {
	if b.borrow(-1, "resources: release of a released buffer") > 0 || b.pool == nil {
		return
	}
	poison(b.mem)
	res := b.res
	b.res = Reservation{}
	if b.class != unpooled {
		b.pool.free[b.class].Put(b)
	}
	res.Release()
}
