// Package wire holds SMB2's codecs: the Direct TCP header that frames each
// message, the SMB2 header in its sync and async forms, a reply's header and
// the ERROR response's body. It is pure: it keeps no state, does no I/O and
// reads nothing but the bytes it is handed (RFC 20 §2.5, §5.1).
//
// Every multi-byte field of an SMB2 message is little-endian (MS-SMB2 2.2);
// the Direct TCP header's length alone is big-endian (MS-SMB2 2.1).
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The SMB2 header's fields, by offset (MS-SMB2 2.2.1.1, 2.2.1.2).
const (
	offProtocolID    = 0
	offStructureSize = 4
	offCreditCharge  = 6
	offStatus        = 8 // a 3.x request's ChannelSequence and Reserved
	offCommand       = 12
	offCredits       = 14 // CreditRequest in a request, CreditResponse in a reply
	offFlags         = 16
	offNextCommand   = 20
	offMessageID     = 24
	offAsyncID       = 32 // the async form's; the sync form's Reserved and TreeId
	offTreeID        = 36 // the sync form's
	offSessionID     = 40
	offSignature     = 48
)

// ErrUnaddressable is wrapped by every refusal of DecodeHeader that returns
// no header: nothing can be answered, and the connection closes.
var ErrUnaddressable = errors.New("smb: an SMB2 header no reply can be addressed to")

// The refusals that wrap ErrUnaddressable are built once, so a refusal
// allocates nothing.
var (
	errShort      = fmt.Errorf("%w: a message shorter than the 64-byte header", ErrUnaddressable)
	errProtocolID = fmt.Errorf("%w: a protocol ID other than SMB2's", ErrUnaddressable)
	errCommand    = fmt.Errorf("%w: a command code no SMB2 request carries", ErrUnaddressable)
)

var (
	errNextCommand   = errors.New("smb: a NextCommand that is not a step to an 8-byte aligned header inside the message")
	errStructureSize = errors.New("smb: an SMB2 header whose StructureSize is not 64")
	errChain         = errors.New("smb: a chained reply that does not lie in the buffer")
)

// Header is an SMB2 header, read in place in the message that carries it.
type Header [HeaderLen]byte

// Command is the header's command code.
func (h *Header) Command() uint16 { return binary.LittleEndian.Uint16(h[offCommand:]) }

// Flags is the header's Flags field.
func (h *Header) Flags() uint32 { return binary.LittleEndian.Uint32(h[offFlags:]) }

// NextCommand is the offset of the next member of a chain from this header's
// start, 0 for the last.
func (h *Header) NextCommand() uint32 { return binary.LittleEndian.Uint32(h[offNextCommand:]) }

func (h *Header) structureSize() uint16 { return binary.LittleEndian.Uint16(h[offStructureSize:]) }
func (h *Header) creditCharge() uint16  { return binary.LittleEndian.Uint16(h[offCreditCharge:]) }
func (h *Header) credits() uint16       { return binary.LittleEndian.Uint16(h[offCredits:]) }
func (h *Header) messageID() uint64     { return binary.LittleEndian.Uint64(h[offMessageID:]) }
func (h *Header) asyncID() uint64       { return binary.LittleEndian.Uint64(h[offAsyncID:]) }
func (h *Header) treeID() uint32        { return binary.LittleEndian.Uint32(h[offTreeID:]) }
func (h *Header) sessionID() uint64     { return binary.LittleEndian.Uint64(h[offSessionID:]) }

// DecodeHeader returns the SMB2 header at off in msg, as a view of msg's
// bytes.
//
// A header no reply can be addressed to comes back nil with an error wrapping
// ErrUnaddressable, and its connection closes unanswered (MS-SMB2 3.3.5.2.6,
// RFC 22 §16.5): fewer than 64 bytes at off, a protocol ID other than SMB2's,
// or a command code MS-SMB2 2.2.1.2 does not define. A header that can be
// answered but is malformed comes back with an error that does not wrap it,
// which is answered STATUS_INVALID_PARAMETER: a NextCommand other than 0 that
// is not a multiple of 8, or does not step past this header to a whole header
// inside msg (MS-SMB2 2.2.1.2's subsequent, 8-byte aligned header), and then
// a StructureSize other than 64. NextCommand is judged first: its refusal
// ends the chain, so another refusal must not hide it.
func DecodeHeader(msg []byte, off int) (*Header, error) {
	if off < 0 || len(msg)-off < HeaderLen {
		return nil, errShort
	}
	h := (*Header)(msg[off : off+HeaderLen])
	if binary.LittleEndian.Uint32(h[offProtocolID:]) != protocolID {
		return nil, errProtocolID
	}
	if h.Command() > lastCommand {
		return nil, errCommand
	}
	next := h.NextCommand()
	if next != 0 && (next%8 != 0 || next < HeaderLen || uint64(next) > uint64(len(msg)-off-HeaderLen)) {
		return h, errNextCommand
	}
	if h.structureSize() != HeaderLen {
		return h, errStructureSize
	}

	return h, nil
}

// AppendReply appends to out the header of the reply to req, with status and
// a grant of credits, and returns it.
//
// The reply is req's header with MS-SMB2 3.3.4.4's changes: the status
// written, NextCommand 0 until Chain sets it, SERVER_TO_REDIR set, and the
// grant written as handed, which MS-SMB2 3.3.4.4 has be 0 for a request with
// an async ID. So it carries req's command, credit charge (MS-SMB2 3.3.4.1),
// flags, message ID, async ID or Reserved and tree ID, and session. SIGNED
// is cleared and the signature is zero: a reply is signed, when it is, after
// it is encoded. Its StructureSize is 64, whatever req's.
func AppendReply(out []byte, req *Header, status uint32, credits uint16) []byte {
	at := len(out)
	out = append(out, req[:]...)
	r := (*Header)(out[at : at+HeaderLen])
	binary.LittleEndian.PutUint16(r[offStructureSize:], HeaderLen)
	binary.LittleEndian.PutUint32(r[offStatus:], status)
	binary.LittleEndian.PutUint16(r[offCredits:], credits)
	binary.LittleEndian.PutUint32(r[offFlags:], FlagServerToRedir|req.Flags()&^flagSigned)
	binary.LittleEndian.PutUint32(r[offNextCommand:], 0)
	clear(r[offSignature:])

	return out
}

// AppendError appends to out the SMB2 ERROR response's body with no error
// data, and returns it: its StructureSize of 9, which counts one byte of
// ErrorData whatever ErrorData's length, an ErrorContextCount and a ByteCount
// of 0, and that byte, 0 (MS-SMB2 2.2.2, 3.3.4.4).
func AppendError(out []byte) []byte {
	return append(out,
		9, 0, // StructureSize
		0,          // ErrorContextCount
		0,          // Reserved
		0, 0, 0, 0, // ByteCount
		0, // ErrorData
	)
}

// Chain pads the reply at prev in out to a multiple of 8 and, when more is
// true, points it at the member appended after it: its NextCommand becomes
// that step, and the last member of a chain, which MS-SMB2 2.2.1.2 has end
// with 0, is padded with more false so the field stays as AppendReply left
// it (MS-SMB2 3.3.4.1.3). Chain is called between a chain's members, never
// after the last one. It returns out.
func Chain(out []byte, prev int, more bool) ([]byte, error) {
	if prev < 0 || len(out)-prev < HeaderLen {
		return out, errChain
	}
	step := (len(out) - prev + 7) &^ 7
	out = append(out, make([]byte, prev+step-len(out))...)
	if more {
		binary.LittleEndian.PutUint32(out[prev+offNextCommand:], uint32(step))
	}

	return out, nil
}
