package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
)

// readBufLen is one connection's read buffer: the small class's capacity, so a
// burst of small records is read once (RFC 20 section 6.8).
const readBufLen = 4 << 10

// errFrame wraps every framing refusal, so the connection's close can name the
// reason `frame` rather than the client's own.
var errFrame = errors.New("core: the record's framing")

var (
	errFragments = errors.New("a record past its fragment count")
	errCap       = errors.New("a record past the protocol's cap")
)

// reader cuts records from one connection: one small-class read buffer, each
// record returned as a receive Buf of its class whose charge was reserved
// before its body was read (RFC 24 section 3.2), a fragmented record charged
// once at the cap and counted, and the frame deadline running from the
// record's first byte (RFC 20 section 6.4).
type reader struct {
	nc      net.Conn
	framer  Framer
	recv    *resources.BufPool
	key     netip.Addr
	timeout time.Duration
	buf     []byte // buf[off:n] is read and not yet consumed
	off     int
	n       int
	armed   bool
}

// newReader wraps a connection's bytes: f frames them, recv reserves each
// record's charge for the connection, and timeout, where it is not zero, is
// read_timeout.
func newReader(nc net.Conn, f Framer, recv *resources.BufPool, key netip.Addr, timeout time.Duration) *reader {
	return &reader{nc: nc, framer: f, recv: recv, key: key, timeout: timeout, buf: make([]byte, readBufLen)}
}

// Read returns the next record: its body, in a receive Buf reserved at its
// class. Its length is checked against the protocol's cap before anything is
// reserved; a record in fragments reserves the cap's class once, before the
// first body is read, and is charged nothing more.
func (r *reader) Read(ctx context.Context) (*resources.Buf, error) {
	// A context that ended before the read started stops it before it blocks:
	// a close disarms, which clears the deadline its stop set.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A record's deadline runs while the reader cuts it: from its first byte,
	// here or already read.
	if r.n > r.off {
		if err := r.arm(ctx); err != nil {
			return nil, err
		}
	}
	hdr, err := r.header(ctx)
	if err != nil {
		return nil, err
	}
	n, last, err := r.framer.BodyLen(hdr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFrame, err)
	}
	r.off += r.framer.HeaderLen()

	reserve, frags := n, 1
	if !last {
		reserve, frags = r.framer.MaxRecord()
	}
	rec, err := r.reserve(ctx, reserve)
	if err != nil {
		return nil, err
	}
	// The grant starts the deadline afresh, once: a full read_timeout for the
	// rest of the record (RFC 20 section 6.4, as the amendment states it).
	if err := r.arm(ctx); err != nil {
		rec.Release()
		return nil, err
	}
	total := 0
	for i := 0; ; i++ {
		if i >= frags {
			rec.Release()
			return nil, fmt.Errorf("%w: %w past %d", errFrame, errFragments, frags)
		}
		if total+n > reserve {
			rec.Release()
			return nil, fmt.Errorf("%w: %w: %d bytes past %d", errFrame, errCap, total+n, reserve)
		}
		if err := r.body(rec.B[total:], n); err != nil {
			rec.Release()
			return nil, err
		}
		total += n
		if last {
			break
		}
		if hdr, err = r.header(ctx); err != nil {
			if err == io.EOF {
				// A fragment of the record is already served: the peer's end
				// here is the cut record's, not a clean close between records.
				err = io.ErrUnexpectedEOF
			}
			rec.Release()
			return nil, err
		}
		if n, last, err = r.framer.BodyLen(hdr); err != nil {
			rec.Release()
			return nil, fmt.Errorf("%w: %w", errFrame, err)
		}
		r.off += r.framer.HeaderLen()
	}
	rec.B = rec.B[:total]
	r.disarm()
	return rec, nil
}

// header returns the next fragment's header, reading until it is whole.
func (r *reader) header(ctx context.Context) ([]byte, error) {
	l := r.framer.HeaderLen()
	for r.n-r.off < l {
		if err := r.fill(ctx); err != nil {
			return nil, err
		}
	}
	return r.buf[r.off : r.off+l], nil
}

// body fills dst with the next n bytes of the record: those already read, then
// the rest straight from the connection.
//
// decision: a record that fits in what was read is copied into its own receive
// buffer rather than cut from the read buffer, since a Buf cannot be a slice of
// another. Overturned if Buf gains sub-slices.
func (r *reader) body(dst []byte, n int) error {
	dst = dst[:n]
	part := min(n, r.n-r.off)
	copy(dst, r.buf[r.off:r.off+part])
	r.off += part
	if part < n {
		if _, err := io.ReadFull(r.nc, dst[part:]); err != nil {
			if err == io.EOF {
				// The body the record's mark promised, cut by the peer's end:
				// the unexpected end inside a record, told apart from a clean
				// close between them.
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// fill compacts the read buffer and reads once into its free part; the first
// bytes of a record arm the frame deadline.
func (r *reader) fill(ctx context.Context) error {
	if r.off > 0 {
		copy(r.buf, r.buf[r.off:r.n])
		r.n -= r.off
		r.off = 0
	}
	if r.n == len(r.buf) {
		return fmt.Errorf("%w: %w: a header of %d bytes", errFrame, errCap, r.framer.HeaderLen())
	}
	m, err := r.nc.Read(r.buf[r.n:])
	r.n += m
	if m > 0 && !r.armed {
		if aerr := r.arm(ctx); aerr != nil {
			return aerr
		}
	}
	if err == io.EOF && m == 0 && r.n > r.off {
		// A part of what the record's mark promised is already in hand, so the
		// peer cut the record: the unexpected end, told apart from a clean
		// close between records.
		err = io.ErrUnexpectedEOF
	}
	return err
}

// reserve takes the record's charge. The deadline it holds is not stopped for
// the wait: nothing reads the connection while it waits, so a deadline set
// before it cannot expire there, and the grant arms the one that covers the
// record's rest (RFC 20 section 6.4).
func (r *reader) reserve(ctx context.Context, n int) (*resources.Buf, error) {
	return r.recv.Get(ctx, r.key, n)
}

// arm starts the frame deadline, and stops the reader at once if its context
// has ended: a connection's close cancels the context before it sets a deadline
// in the past, so the stop is never overwritten by an arm.
func (r *reader) arm(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.timeout > 0 {
		_ = r.nc.SetReadDeadline(time.Now().Add(r.timeout))
	}
	r.armed = true
	return ctx.Err()
}

func (r *reader) disarm() {
	if r.armed {
		_ = r.nc.SetReadDeadline(time.Time{})
		r.armed = false
	}
}
