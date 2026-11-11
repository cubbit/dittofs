package testproto

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Msg is one request message, as a test writes it.
type Msg struct {
	Op      uint32
	Version core.Version
	ID      uint64
	Session uint32 // all ones: inherited from the member before
	Tree    uint32
	Credits uint16 // charged by this message
	Seq     uint32 // 0: not sequenced, never cached; a request repeating a session's sequence is answered from the replay cache once a reply under it was stored
	Related bool
	Pend    bool       // HoldCodec.Pends answers true for this member
	Role    core.Role  // OpEcho: its per-request role
	Keep    bool       // OpSession: keep the connection past the idle timeout
	Expires time.Time  // OpSession: the credential's end
	Handle  vfs.Handle // OpPutFH
	Name    string     // OpCreate, OpPark
	Target  uint64     // OpNotify, OpCancel: the message ID of the request it names; a Cancel carries it as its own ID, and its ID is ignored
	Payload []byte     // OpEcho
	Members []Msg      // OpCompound
}

// Reply is one reply message, as a test reads it.
type Reply struct {
	ID      uint64 // the request's; with the top bit set on the interim and final replies to a held or parked request
	Status  core.Status
	Interim bool
	Credits uint16 // granted by this reply
	Body    []byte
}

// Append appends one record carrying msgs to b, chained when several.
func Append(b []byte, msgs ...Msg) []byte {
	start := len(b)
	b = append(b, 0, 0, 0, 0)
	body := len(b)
	var session uint32
	for i, m := range msgs {
		at := len(b)
		b = appendHeader(b, m)
		b = appendArguments(b, m)
		if i < len(msgs)-1 {
			binary.BigEndian.PutUint32(b[at+offNext:], uint32(len(b)-body))
		}
		if m.Session != inherited {
			session = m.Session
		}
		if session != 0 {
			xorKey(b[at+headerLen:], session)
		}
		binary.BigEndian.PutUint32(b[at+offChecksum:], checksum(b[at:], session))
	}
	binary.BigEndian.PutUint32(b[start:], uint32(len(b)-body))

	return b
}

// appendHeader appends m's header, its next and checksum zero.
func appendHeader(b []byte, m Msg) []byte {
	var flags uint8
	if m.Related {
		flags |= flagRelated
	}
	if m.Pend {
		flags |= flagPend
	}
	b = binary.BigEndian.AppendUint32(b, 0)
	b = append(b, flags, uint8(m.Version))
	b = binary.BigEndian.AppendUint16(b, m.Credits)
	id := m.ID
	if m.Op == OpCancel {
		id = m.Target
	}
	b = binary.BigEndian.AppendUint32(b, m.Op)
	b = binary.BigEndian.AppendUint64(b, id)
	b = binary.BigEndian.AppendUint32(b, m.Session)
	b = binary.BigEndian.AppendUint32(b, m.Tree)
	b = binary.BigEndian.AppendUint32(b, m.Seq)
	b = binary.BigEndian.AppendUint32(b, 0)

	return binary.BigEndian.AppendUint32(b, 0)
}

// appendArguments appends m's arguments, as the format states them for its
// operation.
func appendArguments(b []byte, m Msg) []byte {
	switch m.Op {
	case OpSession:
		var keep uint8
		if m.Keep {
			keep = 1
		}
		var expires int64
		if !m.Expires.IsZero() {
			expires = m.Expires.UnixNano()
		}
		b = append(b, keep)
		b = binary.BigEndian.AppendUint64(b, uint64(expires))
	case OpPutFH:
		b = append(b, m.Handle...)
	case OpEcho:
		b = append(b, uint8(m.Role))
		b = append(b, m.Payload...)
	case OpCreate, OpPark:
		b = append(b, m.Name...)
	case OpNotify:
		b = binary.BigEndian.AppendUint64(b, m.Target)
	case OpCompound:
		b = binary.BigEndian.AppendUint32(b, uint32(len(m.Members)))
		for _, member := range m.Members {
			b = binary.BigEndian.AppendUint32(b, member.Op)
			at := len(b)
			b = binary.BigEndian.AppendUint32(b, 0)
			b = appendArguments(b, member)
			binary.BigEndian.PutUint32(b[at:], uint32(len(b)-at-4))
		}
	}

	return b
}

// Client writes testproto records to a connection and keeps the credit
// balance its replies grant.
type Client struct {
	c       net.Conn
	balance atomic.Int64 // one goroutine may Send while another is in Recv
}

// NewClient returns a client over c with the one credit a connection opens
// with.
func NewClient(c net.Conn) *Client {
	cl := &Client{c: c}
	cl.balance.Store(1)

	return cl
}

// Send writes msgs as one record and charges their credits.
func (c *Client) Send(msgs ...Msg) error {
	if _, err := c.c.Write(Append(nil, msgs...)); err != nil {
		return fmt.Errorf("testproto: send: %w", err)
	}
	for _, m := range msgs {
		c.balance.Add(-int64(m.Credits))
	}

	return nil
}

// Recv reads one record and returns its replies, an interim included, adding
// the credits each grants.
func (c *Client) Recv() ([]Reply, error) {
	var prefix [lengthLen]byte
	if _, err := io.ReadFull(c.c, prefix[:]); err != nil {
		return nil, fmt.Errorf("testproto: receive: %w", err)
	}
	n, err := framer{}.BodyLen(prefix[:])
	if err != nil {
		return nil, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.c, body); err != nil {
		return nil, fmt.Errorf("testproto: receive: %w", err)
	}

	replies, err := openReplies(body)
	for _, r := range replies {
		c.balance.Add(int64(r.Credits))
	}

	return replies, err
}

// openReplies reads a record's replies, checking and opening each one sealed
// under a session.
func openReplies(body []byte) ([]Reply, error) {
	var replies []Reply
	for off := 0; ; {
		h, err := headerAt(body, off)
		if err != nil {
			return replies, err
		}
		args := arguments(body, off, h)
		if s := h.session(); s != 0 && s != inherited {
			if checksum(body[off:off+headerLen+len(args)], s) != h.checksum() {
				return replies, fmt.Errorf("%w: the reply to message %d", errChecksum, h.id())
			}
			xorKey(args, s)
		}
		replies = append(replies, Reply{
			ID:      h.id(),
			Status:  core.Status(h.status()),
			Interim: h.flags()&flagInterim != 0,
			Credits: h.credits(),
			Body:    args,
		})
		if off = h.Next(); off == 0 {
			return replies, nil
		}
	}
}

// Balance is the credits every Reply received granted, interims included, less
// the credits every Msg sent charged; it starts at 1, the credit a connection
// opens with.
func (c *Client) Balance() int { return int(c.balance.Load()) }
