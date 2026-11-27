// Package nfsclient is the scripted client every NFS loopback test is written
// on: one TCP connection that sends exactly the bytes a test gives it, a whole
// call, a record, or raw bytes, and reads back what the server wrote. It
// holds its own call encoder and reply decoder, written from RFC 5531 and not
// taken from the adapter's wire package, so a mistake of that package is not
// repeated by the client that tests it.
package nfsclient

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

// Record marking (RFC 5531 section 11): a record is fragments, each behind a
// four-byte mark of its length and, in the high bit, whether it ends the record.
const (
	lastFragment = 1 << 31
	markLen      = 4

	// maxRecord bounds what Read allocates for one record: the adapter's own
	// cap, 1 MiB and 4 KiB, which a reply never passes.
	maxRecord = 1<<20 + 4<<10
)

// Auth is an opaque_auth: a flavour and its body.
type Auth struct {
	Flavour uint32
	Body    []byte
}

// Call is one RPC call (RFC 5531 section 9), every field the test sets, sent
// as it is.
type Call struct {
	XID              *uint32 // nil: the client's next; set to send any XID, 0 included
	Prog, Vers, Proc uint32
	Cred, Verf       Auth
	Args             []byte
}

// Reply is one RPC reply, decoded.
type Reply struct {
	XID       uint32
	Denied    bool   // MSG_DENIED
	Stat      uint32 // the accept state, or the reject state's auth_stat or RPC_MISMATCH
	Verf      Auth   // an accepted reply's verifier
	Low, High uint32 // PROG_MISMATCH's or RPC_MISMATCH's range
	Body      []byte // an accepted success's results
}

// Client is one TCP connection and the XID its next call takes. A test uses it
// from one goroutine at a time.
type Client struct {
	conn net.Conn
	xid  atomic.Uint32 // the last XID handed out; the first call takes 1
}

// Dial connects to addr.
func Dial(ctx context.Context, addr netip.AddrPort) (*Client, error) {
	var d net.Dialer
	return dial(ctx, &d, addr)
}

// DialFrom connects to addr from the source address from, so a server sees
// that address as the client's.
func DialFrom(ctx context.Context, from netip.Addr, addr netip.AddrPort) (*Client, error) {
	d := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(from, 0))}
	return dial(ctx, &d, addr)
}

func dial(ctx context.Context, d *net.Dialer, addr netip.AddrPort) (*Client, error) {
	nc, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("nfsclient: dial %s: %w", addr, err)
	}

	return &Client{conn: nc}, nil
}

// Do sends call as one record and reads the next record as its reply. A call
// with no XID takes the client's next, which counts up from 1.
func (c *Client) Do(ctx context.Context, call Call) (Reply, error) {
	var xid uint32
	if call.XID != nil {
		xid = *call.XID
	} else {
		xid = c.xid.Add(1)
	}

	if err := c.Send(encodeCall(xid, call)); err != nil {
		return Reply{}, err
	}

	rec, err := c.Read(ctx)
	if err != nil {
		return Reply{}, err
	}

	return decodeReply(rec)
}

// Send writes rec as one record, in one fragment, behind its record mark.
func (c *Client) Send(rec []byte) error {
	b := binary.BigEndian.AppendUint32(
		make([]byte, 0, markLen+len(rec)),
		lastFragment|uint32(len(rec)),
	)

	return c.Write(append(b, rec...))
}

// Write writes b as it is: any record marks it holds are its own.
func (c *Client) Write(b []byte) error {
	_, err := c.conn.Write(b)
	return err
}

// Read reads the next record, its fragments joined, and returns its body: io.EOF
// when the server closed the connection before the record's first byte,
// io.ErrUnexpectedEOF when it closed inside one, and ctx's error once ctx ends.
func (c *Client) Read(ctx context.Context) ([]byte, error) {
	_ = c.conn.SetReadDeadline(time.Time{})

	// The socket's deadline is set only once ctx has ended, so ctx.Err() is
	// already set when a read wakes on it; fired lets Read wait out a closure
	// that stop could not prevent, so it cannot reach the next Read.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)

		_ = c.conn.SetReadDeadline(time.Unix(1, 0))
	})

	rec, err := c.readRecord()

	if !stop() {
		<-fired
	}

	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return rec, err
}

func (c *Client) readRecord() ([]byte, error) {
	var rec []byte

	for first := true; ; first = false {
		var mark [markLen]byte
		if _, err := io.ReadFull(c.conn, mark[:]); err != nil {
			if !first && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}

			return nil, err
		}

		m := binary.BigEndian.Uint32(mark[:])

		n := int(m &^ lastFragment)
		if len(rec)+n > maxRecord {
			return nil, fmt.Errorf("nfsclient: a record past %d bytes", maxRecord)
		}

		at := len(rec)

		rec = append(rec, make([]byte, n)...)
		if _, err := io.ReadFull(c.conn, rec[at:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}

			return nil, err
		}

		if m&lastFragment != 0 {
			return rec, nil
		}
	}
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// AuthSys is an AUTH_SYS credential of stamp 0 and no machine name, for uid
// and gid and the groups gids (RFC 5531 Appendix A).
func AuthSys(uid, gid uint32, gids ...uint32) Auth {
	return Auth{Flavour: authSys, Body: authSysBody(uid, gid, gids)}
}
