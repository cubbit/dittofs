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
// The length's 31 low bits are the body's size, and its top bit set means the
// body is one fragment of a record, followed by more such marks until one with
// the bit clear ends it: every body may come in fragments, at most four of
// them, and this protocol's own records never use the bit, writing every
// record as one fragment with the bit clear.
// The body is one or more messages. A message is a 40-byte header and its
// arguments, which run to the next message or to the body's end:
//
//	offset  size  field
//	0       4     next: the next message's offset in the body, 0 for the last
//	4       1     flags: 1 related, 2 pend, 4 reply, 8 interim, 16 clear, 32 signed
//	5       1     version: 1 or 2
//	6       2     credits: charged by a request, granted by a reply
//	8       4     operation
//	12      8     message ID
//	20      4     session, all ones for the session of the member before
//	24      4     tree
//	28      4     sequence: with the session, the key of the replay cache; 0: not sequenced, never cached
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
// Under a session s other than 0, a message is sealed, unless it is flagged
// clear, as a client that skips the seal sends it, or a reply is flagged
// signed, as a refusal whose seal failed is answered. Its arguments are
// XORed with the session's key, byte i of them with byte i mod 4 of s times
// 0x01000193, big-endian. Its checksum is then the sum of its bytes as sent,
// header and arguments, with the checksum field read as zero, plus s times
// 0x01000193, modulo 2^32. A signed reply's arguments are not XORed; its
// checksum is the same sum. A member whose session field is all ones is
// sealed under the session it inherits. Under session 0, and on a message
// flagged clear, nothing is XORed and the field is 0.
//
// The arguments, by operation:
//
//	Session   keep (1 byte, 0 or 1), expires (8 bytes, Unix nanoseconds, 0 for none)
//	PutFH     the handle's bytes
//	Echo      role (1 byte), then the payload
//	Create    the name
//	Park      the name
//	Notify    target (8 bytes), the message ID of the request it names
//	Compound  count (4 bytes), tag length (4), the tag, then per member: operation (4), length (4), arguments
//
// Every other operation has none. A Cancel names its target by carrying the
// target's message ID as its own. A reply's arguments are its body; a Park's
// is its name; a compound's reply body is its status (4 bytes), its tag's
// length (4), the tag and its count of results (4), then per result:
// operation (4), status (4), length (4), body.
package testproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

const (
	lengthLen     = 4       // the record's length prefix
	moreFragments = 1 << 31 // the length prefix's flag: set, more fragments follow
	maxFragments  = 4       // the most fragments one record comes in
	headerLen     = 40      // a message's header
	maxBody       = 1 << 20 // the cap on a record's body

	inherited = 1<<32 - 1 // the session field of a member that takes the one before's
	sumKey    = 0x01000193
)

// The header's flags.
const (
	flagRelated = 1 << iota
	flagPend
	flagReply
	flagInterim
	flagClear  // a request sent neither sealed nor checksummed under its session
	flagSigned // a reply checksummed under its session and its arguments left clear
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
	OpPark                        // version 2: a GetAttr of the current filehandle, Request.Park, then a GetAttr and a Create of Msg.Name; its reply carries the name
	OpNotify                      // Conn.Send ahead of the reply keyed by Msg.Target
	OpViolate                     // Conn.Close
	OpCancel                      // never routed: HoldCodec.Cancels names the request keyed by Msg.Target
	OpCompound                    // version 1: the compound row, its members Msg.Members
	OpRead                        // NeedsTree|NeedsFH; one vfs.Service.Read of Msg.Count bytes at Msg.Offset, its count clamped to its room
)

// ErrFragmentedReply is a reply whose record mark says more fragments follow:
// this protocol never sends one.
var ErrFragmentedReply = errors.New("testproto: a reply in fragments")

var (
	errChecksum = errors.New("testproto: a message whose checksum does not match")
	errTooLong  = errors.New("testproto: a record past the 1 MiB cap")
	errShort    = errors.New("testproto: a header that runs past the record")
	errNext     = errors.New("testproto: a next offset that points backwards or past the record")
)

// framer finds a fragment's length from its 4-byte prefix, its top bit saying
// whether more fragments follow.
type framer struct{}

func (framer) HeaderLen() int { return lengthLen }

// Frame appends the 4-byte length of a record whose body is body bytes, its
// fragment flag clear: this protocol writes one fragment per record.
func (framer) Frame(dst []byte, body int) []byte {
	return binary.BigEndian.AppendUint32(dst, uint32(body))
}

// BodyLen reads a fragment's length and whether it ends the record. The prefix's
// top bit set means more fragments follow.
//
// decision: the prefix's flag is inverted from RFC 5531 section 11's record
// mark, which sets its top bit on the last fragment; this protocol's vectors are
// written with the bits clear, so every one of them still ends with its first
// header. Overturned if the test protocol ever needs to match the mark byte for
// byte.
func (framer) BodyLen(hdr []byte) (int, bool, error) {
	if len(hdr) < lengthLen {
		return 0, false, errShort
	}
	m := binary.BigEndian.Uint32(hdr)
	n, last := int(m&^moreFragments), m&moreFragments == 0
	if n > maxBody {
		return 0, false, fmt.Errorf("%w: %d bytes", errTooLong, n)
	}
	return n, last, nil
}

// MaxRecord is the record cap and the most fragments a record comes in.
func (framer) MaxRecord() (int, int) { return maxBody, maxFragments }

// header is one message's header, read in place.
//
// decision: a header is a pointer to its bytes in the record, so handing it to
// core as a core.Header allocates nothing. It is valid while the record's Buf
// is retained, which core does for a request's life but a park's: that it
// releases once the handler returns ErrParked, having taken Final's header,
// which must not point into the record (RFC 20 section 4.7). Withdraw it, and
// copy the fields out, if core ever reads a header after it releases the
// record.
type header [headerLen]byte

// stepless is a header refused for its own next step: its Next is one the
// chain's walk refuses, so the chain ends there (RFC 20 §4.7).
type stepless struct{ *header }

func (stepless) Next() int { return -1 }

// tagged is a compound request's header with the tag read from its
// arguments, where they are clear: Encode echoes it in the head of a refusal
// answered with its status alone, and ReplyLen counts it.
type tagged struct {
	*header
	tag []byte
}

// withTag is h on a tagged header where h is a compound whose clear
// arguments, args, carry its tag whole, and h itself otherwise.
func withTag(h *header, args []byte) core.Header {
	if h.Op() == OpCompound && len(args) >= 8 {
		if n := binary.BigEndian.Uint32(args[4:]); int64(n) <= int64(len(args)-8) {
			return &tagged{h, bytes.Clone(args[8 : 8+n])}
		}
	}

	return h
}

// plain is h as the header in its record: a request's own, or a compound's
// under its tag.
func plain(h core.Header) (*header, bool) {
	switch h := h.(type) {
	case *header:
		return h, true
	case *tagged:
		return h.header, true
	case stepless:
		return h.header, true
	}

	return nil, false
}

// tagOf is a compound request's tag, as its header's codec or Unwrap read it,
// or none.
func tagOf(h core.Header) []byte {
	if t, ok := h.(*tagged); ok {
		return t.tag
	}

	return nil
}

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
// that is 0 or leaves room for a whole header after this one; a bad next
// comes back with the header.
func headerAt(body []byte, off int) (*header, error) {
	if off < 0 || off > len(body)-headerLen {
		return nil, errShort
	}
	h := (*header)(body[off : off+headerLen])
	if next := h.Next(); next != 0 && (next < off+headerLen || next > len(body)-headerLen) {
		return h, errNext
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

// Decode returns the header at off in the record. A header whose fields
// decoded and whose next is bad comes back with its error, so its refusal
// can be addressed; one that runs past the record, with none. A compound's
// tag is read with its header, as RFC 20 §4.1 reads an NFSv4 COMPOUND's,
// where its arguments are clear, under session 0, so stages with no Security
// echo and count it too; under a session, Unwrap reads it once they are.
func (headerCodec) Decode(rec *resources.Buf, off int) (core.Header, error) {
	h, err := headerAt(rec.B, off)
	if errors.Is(err, errNext) {
		return stepless{h}, err
	}
	if err != nil {
		return nil, err
	}
	if h.session() == 0 {
		return withTag(h, arguments(rec.B, off, h)), nil
	}

	return h, nil
}

// Encode appends to out the header of the reply to h with status s: h's
// operation, version, IDs and sequence, the reply flag and h's signed flag,
// which Unwrap's verdict set, and a grant of the
// credits h charged, at least one, so a client that charges nothing still
// earns credit; a header from HoldCodec.Final grants exactly what it carries.
// The grant stops at the connection's credit target. Its checksum is left
// zero for Security.Wrap.
func (c headerCodec) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	req, ok := asHeader(h)
	if !ok {
		return fmt.Errorf("testproto: encode a reply to a %T header", h)
	}
	_, exact := h.(*final)
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
	rep[offFlags] = flagReply | req.flags()&flagSigned
	binary.BigEndian.PutUint16(rep[offCredits:], grant)
	binary.BigEndian.PutUint32(rep[offStatus:], uint32(s))
	binary.BigEndian.PutUint32(rep[offChecksum:], 0)
	if req.Op() == OpCompound && s != 0 {
		// A compound answered a status alone carries its head, with the tag
		// its header's codec or Unwrap read and no results.
		out.B = appendHead(out.B, s, tagOf(h), 0)
	}

	return nil
}

// Args is the arguments of the message h in rec: from after its header to
// the next message or the body's end; nil for a header not in rec.
func (headerCodec) Args(rec *resources.Buf, h core.Header) []byte {
	hd, ok := plain(h)
	if !ok {
		return nil
	}
	off, ok := offsetOf(rec.B, hd)
	if !ok {
		return nil
	}

	return arguments(rec.B, off, hd)
}

// ReplyLen is the most Encode writes for h: a reply header's 40 bytes, since a
// status alone has no body, Chain pads nothing and Wrap adds nothing; and, for
// a compound, its head and the status-alone result of a member that did not
// decode, which its runner writes.
func (headerCodec) ReplyLen(h core.Header) int {
	if h.Op() == OpCompound {
		return headerLen + 12 + len(tagOf(h)) + resultHead
	}

	return headerLen
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
