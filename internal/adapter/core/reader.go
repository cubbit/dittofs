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

var (
	errFrameFragments = errors.New("core: a record past its fragment count")
	errFrameCap       = errors.New("core: a record past the protocol's cap")
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
	hdr, err := r.header()
	if err != nil {
		return nil, err
	}
	n, last, err := r.framer.BodyLen(hdr)
	if err != nil {
		return nil, err
	}
	r.off += r.framer.HeaderLen()

	// The record's first byte has arrived: the frame deadline runs from it.
	r.arm()
	reserve, frags := n, 1
	if !last {
		reserve, frags = r.framer.MaxRecord()
	}
	rec, err := r.reserve(ctx, reserve)
	if err != nil {
		return nil, err
	}
	// The grant starts the deadline afresh: a full read_timeout for the rest.
	r.arm()
	total := 0
	for i := 0; ; i++ {
		if i >= frags {
			rec.Release()
			return nil, fmt.Errorf("%w: past %d", errFrameFragments, frags)
		}
		if total+n > reserve {
			rec.Release()
			return nil, fmt.Errorf("%w: %d bytes past %d", errFrameCap, total+n, reserve)
		}
		if err := r.body(rec.B[total:], n); err != nil {
			rec.Release()
			return nil, err
		}
		total += n
		if last {
			break
		}
		if hdr, err = r.header(); err != nil {
			rec.Release()
			return nil, err
		}
		if n, last, err = r.framer.BodyLen(hdr); err != nil {
			rec.Release()
			return nil, err
		}
		r.off += r.framer.HeaderLen()
	}
	rec.B = rec.B[:total]
	if r.n == r.off {
		r.disarm()
	}
	return rec, nil
}

// header returns the next fragment's header, reading until it is whole.
func (r *reader) header() ([]byte, error) {
	l := r.framer.HeaderLen()
	for r.n-r.off < l {
		if err := r.fill(); err != nil {
			return nil, err
		}
	}
	return r.buf[r.off : r.off+l], nil
}

// body fills dst with the next n bytes of the record: those already read, then
// the rest straight from the connection.
func (r *reader) body(dst []byte, n int) error {
	dst = dst[:n]
	part := min(n, r.n-r.off)
	copy(dst, r.buf[r.off:r.off+part])
	r.off += part
	if part < n {
		if _, err := io.ReadFull(r.nc, dst[part:]); err != nil {
			return err
		}
	}
	return nil
}

// fill compacts the read buffer and reads once into its free part; the first
// bytes of a record arm the frame deadline.
func (r *reader) fill() error {
	if r.off > 0 {
		copy(r.buf, r.buf[r.off:r.n])
		r.n -= r.off
		r.off = 0
	}
	if r.n == len(r.buf) {
		return fmt.Errorf("%w: a header of %d bytes", errFrameCap, r.framer.HeaderLen())
	}
	m, err := r.nc.Read(r.buf[r.n:])
	r.n += m
	if m > 0 && !r.armed {
		r.arm()
	}
	return err
}

// reserve takes the record's charge, its deadline stopped while it waits, and
// starts a full read_timeout once the charge is granted (RFC 20 section 6.4).
func (r *reader) reserve(ctx context.Context, n int) (*resources.Buf, error) {
	r.disarm()
	rec, err := r.recv.Get(ctx, r.key, n)
	r.arm()
	return rec, err
}

// arm starts the frame deadline, and disarm stops it: a connection that sends
// nothing, or waits for receive room, is not timed out.
func (r *reader) arm() {
	if r.timeout > 0 {
		_ = r.nc.SetReadDeadline(time.Now().Add(r.timeout))
	}
	r.armed = true
}

func (r *reader) disarm() {
	if r.armed {
		_ = r.nc.SetReadDeadline(time.Time{})
		r.armed = false
	}
}
