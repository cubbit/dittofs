// Package testproto is a test-only protocol: the third implementation of
// core.Protocol, beside NFS's and SMB's, which core's tests run core on. It
// has what core's pipeline must handle and nothing else: frames, a handshake,
// session levels, kept state, held and parked requests, a cancel and chained
// messages.
//
// # Format
//
// Every integer is big-endian.
//
// A record is a 4-byte length and a body of that many bytes, at most 1 MiB.
// The body is one or more messages. A message is a 40-byte header and its
// arguments, which run to the next message or to the body's end:
//
//	offset  size  field
//	0       4     next: the next message's offset in the body, 0 for the last
//	4       1     flags: 1 related, 2 pend, 4 reply, 8 interim
//	5       1     version: 1 or 2
//	6       2     credits: charged by a request, granted by a reply
//	8       4     operation
//	12      8     message ID
//	20      4     session, all ones for the session of the member before
//	24      4     tree
//	28      4     sequence: with the session, the key of the replay cache
//	32      4     status, 0 in a request
//	36      4     checksum
//
// A next that is not 0 points past its own message's header and leaves room
// for a whole header before the body's end.
//
// A reply grants the credits its request charged, and at least one, as far as
// the connection's 64 leave room. The interim and final replies to a held or
// parked request carry its message ID with the top bit set. A reply never
// carries an all-ones session: it carries the one its request resolved to.
//
// Under a session s other than 0, a message is sealed. Its arguments are
// XORed with the session's key, byte i of them with byte i mod 4 of s times
// 0x01000193, big-endian. Its checksum is then the sum of its bytes as sent,
// header and arguments, with the checksum field read as zero, plus s times
// 0x01000193, modulo 2^32. A member whose session field is all ones is sealed
// under the session it inherits. Under session 0 nothing is XORed and the
// field is 0.
//
// The arguments, by operation:
//
//	Session   keep (1 byte, 0 or 1), expires (8 bytes, Unix nanoseconds, 0 for none)
//	PutFH     the handle's bytes
//	Echo      role (1 byte), then the payload
//	Create    the name
//	Park      the name
//	Notify    target (8 bytes), the message ID of the request it names
//	Compound  count (4 bytes), then per member: operation (4), length (4), arguments
//
// Every other operation has none. A Cancel names its target by carrying the
// target's message ID as its own. A reply's arguments are its body.
package testproto

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

const (
	lengthLen = 4       // the record's length prefix
	headerLen = 40      // a message's header
	maxBody   = 1 << 20 // the cap on a record's body

	inherited = 1<<32 - 1 // the session field of a member that takes the one before's
	sumKey    = 0x01000193
)

// The header's flags.
const (
	flagRelated = 1 << iota
	flagPend
	flagReply
	flagInterim
)

// The header's field offsets.
const (
	offNext     = 0
	offFlags    = 4
	offVersion  = 5
	offCredits  = 6
	offOp       = 8
	offID       = 12
	offSession  = 20
	offTree     = 24
	offSeq      = 28
	offStatus   = 32
	offChecksum = 36
)

// The operation codes, typed as core.Op.Code and core.Header.Op are.
const (
	OpNegotiate uint32 = iota + 1 // NeedsNothing; the higher version both sides offer
	OpSession                     // NeedsNegotiated, Expired; Conn.Established, Conn.Keep when Msg.Keep
	OpLogoff                      // RoleRelease, Expired; ends the session and its keep
	OpTree                        // NeedsSession
	OpPutFH                       // NeedsTree, RoleContext; Invocation.Handle is Msg.Handle
	OpEcho                        // NeedsNothing; Invocation.Role is Msg.Role
	OpCreate                      // NeedsTree|NeedsFH, Cacheable; one vfs.Service.Create of Msg.Name
	OpPanic                       // its handler panics
	OpPark                        // version 2: Request.Park, then a Create of Msg.Name
	OpNotify                      // Conn.Send ahead of the reply keyed by Msg.Target
	OpViolate                     // Conn.Close
	OpCancel                      // never routed: HoldCodec.Cancels names the request keyed by Msg.Target
	OpCompound                    // version 1: the compound row, its members Msg.Members
)

var (
	errChecksum = errors.New("testproto: a message whose checksum does not match")
	errTooLong  = errors.New("testproto: a record past the 1 MiB cap")
	errShort    = errors.New("testproto: a header that runs past the record")
	errNext     = errors.New("testproto: a next offset that points backwards or past the record")
)

// framer finds a record's length from its 4-byte prefix.
type framer struct{}

func (framer) HeaderLen() int { return lengthLen }

// BodyLen refuses a length past the cap, so nothing is reserved for it.
func (framer) BodyLen(hdr []byte) (int, error) {
	if len(hdr) < lengthLen {
		return 0, errShort
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > maxBody {
		return 0, fmt.Errorf("%w: %d bytes", errTooLong, n)
	}

	return int(n), nil
}

// header is one message's header, read in place.
//
// decision: a header is a pointer to its bytes in the record, so handing it to
// core as a core.Header allocates nothing. It is valid while the record's Buf
// is retained, which core does for a request's life. Withdraw it, and copy the
// fields out, if core ever releases a record before its request ends.
type header [headerLen]byte

func (h *header) Op() uint32            { return binary.BigEndian.Uint32(h[offOp:]) }
func (h *header) Version() core.Version { return core.Version(h[offVersion]) }
func (h *header) Next() int             { return int(binary.BigEndian.Uint32(h[offNext:])) }
func (h *header) Related() bool         { return h[offFlags]&flagRelated != 0 }

func (h *header) flags() uint8     { return h[offFlags] }
func (h *header) credits() uint16  { return binary.BigEndian.Uint16(h[offCredits:]) }
func (h *header) id() uint64       { return binary.BigEndian.Uint64(h[offID:]) }
func (h *header) session() uint32  { return binary.BigEndian.Uint32(h[offSession:]) }
func (h *header) tree() uint32     { return binary.BigEndian.Uint32(h[offTree:]) }
func (h *header) seq() uint32      { return binary.BigEndian.Uint32(h[offSeq:]) }
func (h *header) status() uint32   { return binary.BigEndian.Uint32(h[offStatus:]) }
func (h *header) checksum() uint32 { return binary.BigEndian.Uint32(h[offChecksum:]) }

// final is the header HoldCodec.Final returns: a copy of a held request's,
// carrying its key as the message ID and exactly the credits its final reply
// grants. It is a type of its own, not a flag in the header, so nothing a
// client sends can pass for one.
type final struct{ header }

// headerAt is the header at off in body, checked to lie inside it with a next
// that is 0 or leaves room for a whole header after this one.
func headerAt(body []byte, off int) (*header, error) {
	if off < 0 || off > len(body)-headerLen {
		return nil, errShort
	}
	h := (*header)(body[off : off+headerLen])
	if next := h.Next(); next != 0 && (next < off+headerLen || next > len(body)-headerLen) {
		return nil, errNext
	}

	return h, nil
}

// arguments is the bytes of the message at off after its header, to the next
// message or the body's end. h is headerAt(body, off).
func arguments(body []byte, off int, h *header) []byte {
	end := len(body)
	if next := h.Next(); next != 0 {
		end = next
	}

	return body[off+headerLen : end]
}

// checksum is the sum the format states for msg, a whole message, under
// session.
func checksum(msg []byte, session uint32) uint32 {
	if session == 0 {
		return 0
	}
	sum := session * sumKey
	for i, b := range msg {
		if i < offChecksum || i >= offChecksum+4 {
			sum += uint32(b)
		}
	}

	return sum
}

// xorKey XORs b in place with session's key, as the format states.
func xorKey(b []byte, session uint32) {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], session*sumKey)
	for i := range b {
		b[i] ^= key[i&3]
	}
}

// headerCodec decodes a request's header and encodes its reply's. flow is the
// connection's credits, which a reply's grant is added to; nil grants without
// counting, as Bare does.
type headerCodec struct{ flow *flow }

// Decode returns the header at off in the record.
func (headerCodec) Decode(rec *resources.Buf, off int) (core.Header, error) {
	h, err := headerAt(rec.B, off)
	if err != nil {
		return nil, err
	}

	return h, nil
}

// Encode appends to out the header of the reply to h with status s: h's
// operation, version, IDs and sequence, the reply flag, and a grant of the
// credits h charged, at least one, so a client that charges nothing still
// earns credit; a header from HoldCodec.Final grants exactly what it carries.
// The grant stops at the connection's credit target. Its checksum is left
// zero for Security.Wrap.
func (c headerCodec) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	var req *header
	exact := false
	switch h := h.(type) {
	case *header:
		req = h
	case *final:
		req, exact = &h.header, true
	default:
		return fmt.Errorf("testproto: encode a reply to a %T header", h)
	}
	at := len(out.B)
	out.B = append(out.B, req[:]...)
	rep := (*header)(out.B[at : at+headerLen])
	binary.BigEndian.PutUint32(rep[offNext:], 0)
	grant := req.credits()
	if !exact {
		grant = max(grant, 1)
	}
	if c.flow != nil {
		grant = c.flow.grant(grant)
	}
	rep[offFlags] = flagReply
	binary.BigEndian.PutUint16(rep[offCredits:], grant)
	binary.BigEndian.PutUint32(rep[offStatus:], uint32(s))
	binary.BigEndian.PutUint32(rep[offChecksum:], 0)

	return nil
}

// Chain points the reply at prev in out to the one appended after it, which
// starts at out's end.
func (headerCodec) Chain(out *resources.Buf, prev int) error {
	if prev < 0 || prev > len(out.B)-headerLen {
		return errShort
	}
	binary.BigEndian.PutUint32(out.B[prev+offNext:], uint32(len(out.B)))

	return nil
}
