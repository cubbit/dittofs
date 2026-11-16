package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The NFS program, version and procedure the COMPOUND prefix is read for
// (RFC 21 section 2.1, RFC 20 section 4.7).
const (
	progNFS      = 100003
	versNFS4     = 4
	procCompound = 1
)

var (
	// ErrUnaddressable is a record too short to hold a call's transaction ID
	// and message type: nothing can be answered, and the connection closes.
	ErrUnaddressable = errors.New("nfs/wire: a record too short to address")

	// ErrNotCall is a message of another type where a call was expected: it is
	// dropped and nothing is answered.
	ErrNotCall = errors.New("nfs/wire: not a call")

	errPastRecord  = errors.New("nfs/wire: a value runs past the record")
	errAuthTooLong = errors.New("nfs/wire: an opaque past its limit")
)

// Call is one parsed call: the header RFC 5531 section 9 defines, and, for an
// NFSv4 COMPOUND, its tag and minor version (RFC 20 section 4.7).
type Call struct {
	xid, typ, rpcvers     uint32
	prog, vers, proc      uint32
	credFlavour, verfFlav uint32
	cred, verf, tag       []byte
	minor                 uint32
	compound              bool
}

// XID is the call's transaction ID.
func (c *Call) XID() uint32 { return c.xid }

// Type is the message type: a call, unless the parse refused it as one.
func (c *Call) Type() uint32 { return c.typ }

// RPCVersion is the call's RPC version, 2 for every call served.
func (c *Call) RPCVersion() uint32 { return c.rpcvers }

// Program, Version and Procedure are the call's target.
func (c *Call) Program() uint32   { return c.prog }
func (c *Call) Version() uint32   { return c.vers }
func (c *Call) Procedure() uint32 { return c.proc }

// Credential is the call's credential: its flavour, and its body as a slice of
// the record, never a copy.
func (c *Call) Credential() (flavour uint32, body []byte) { return c.credFlavour, c.cred }

// Verifier is the call's verifier, as a slice of the record.
func (c *Call) Verifier() (flavour uint32, body []byte) { return c.verfFlav, c.verf }

// Tag is a COMPOUND's tag, as a slice of the record; nil for any other call.
func (c *Call) Tag() []byte { return c.tag }

// MinorVersion is a COMPOUND's minor version; ok is false for any other call.
func (c *Call) MinorVersion() (minor int, ok bool) { return int(c.minor), c.compound }

// ParseCall parses the call at the start of rec, a record's body: the
// transaction ID and message type, the RPC version, the program, version and
// procedure, the credential and the verifier, and, for procedure 1 of program
// 100003 version 4, the COMPOUND prefix's tag and minor version (RFC 5531
// section 9, RFC 20 section 4.7). Every credential and verifier body is at
// most 400 bytes (RFC 5531 section 8.2) and lies inside the record with its
// padding.
//
// A record too short for its transaction ID and message type is refused with
// ErrUnaddressable, and a message of another type than a call with ErrNotCall;
// both come back with no call. A call whose later fields do not decode comes
// back with its error (RFC 21 section 11.5).
func ParseCall(rec []byte) (*Call, error) {
	r := reader{b: rec}
	xid, ok := r.u32()
	if !ok {
		return nil, ErrUnaddressable
	}
	typ, ok := r.u32()
	if !ok {
		return nil, ErrUnaddressable
	}
	c := &Call{xid: xid, typ: typ}
	if typ != callType {
		return c, ErrNotCall
	}
	for _, f := range []*uint32{&c.rpcvers, &c.prog, &c.vers, &c.proc} {
		v, ok := r.u32()
		if !ok {
			return c, fmt.Errorf("nfs/wire: %w: the call's target", errPastRecord)
		}
		*f = v
	}
	var err error
	if c.credFlavour, c.cred, err = r.auth(); err != nil {
		return c, err
	}
	if c.verfFlav, c.verf, err = r.auth(); err != nil {
		return c, err
	}
	if c.prog == progNFS && c.vers == versNFS4 && c.proc == procCompound {
		if c.tag, err = r.opaque(MaxRecord); err != nil {
			return c, fmt.Errorf("nfs/wire: a COMPOUND's tag: %w", err)
		}
		minor, ok := r.u32()
		if !ok {
			return c, fmt.Errorf("nfs/wire: %w: a COMPOUND's minor version", errPastRecord)
		}
		c.minor, c.compound = minor, true
	}
	return c, nil
}

// reader reads the record's big-endian fields, refusing each that does not lie
// inside it before it reads.
type reader struct {
	b   []byte
	off int
}

func (r *reader) u32() (v uint32, ok bool) {
	if r.off+4 > len(r.b) {
		return 0, false
	}
	v = binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, true
}

// opaque reads an XDR opaque: its length, its bytes and their padding to four,
// all inside the record, its body at most max bytes; the bytes are returned as
// a slice of the record.
func (r *reader) opaque(max uint32) (body []byte, err error) {
	n, ok := r.u32()
	if !ok {
		return nil, fmt.Errorf("nfs/wire: %w: an opaque's length", errPastRecord)
	}
	if n > max {
		return nil, fmt.Errorf("nfs/wire: %w: an opaque of %d bytes, past %d", errAuthTooLong, n, max)
	}
	pad := (4 - n%4) % 4
	if r.off+int(n)+int(pad) > len(r.b) {
		return nil, fmt.Errorf("nfs/wire: %w: an opaque of %d bytes", errPastRecord, n)
	}
	body = r.b[r.off : r.off+int(n)]
	r.off += int(n) + int(pad)
	return body, nil
}

// auth reads an opaque_auth: its flavour and its body, at most 400 bytes.
func (r *reader) auth() (flavour uint32, body []byte, err error) {
	flavour, ok := r.u32()
	if !ok {
		return 0, nil, fmt.Errorf("nfs/wire: %w: a flavour", errPastRecord)
	}
	body, err = r.opaque(maxAuthBody)
	if err != nil {
		return 0, nil, err
	}
	return flavour, body, nil
}

// AppendAccepted appends an accepted reply for xid: the reply state, an empty
// AUTH_NONE verifier, and stat (RFC 5531 section 9.2.2). The program's reply
// follows, at its call's procedure.
func AppendAccepted(dst []byte, xid, stat uint32) []byte {
	dst = binary.BigEndian.AppendUint32(dst, xid)
	dst = binary.BigEndian.AppendUint32(dst, replyType)
	dst = binary.BigEndian.AppendUint32(dst, replyAccepted)
	dst = binary.BigEndian.AppendUint32(dst, authNone)
	dst = binary.BigEndian.AppendUint32(dst, 0)
	return binary.BigEndian.AppendUint32(dst, stat)
}

// AppendProgMismatch appends an accepted reply answering AcceptProgMismatch,
// naming low and high, the lowest and highest versions served.
func AppendProgMismatch(dst []byte, xid, low, high uint32) []byte {
	dst = AppendAccepted(dst, xid, AcceptProgMismatch)
	dst = binary.BigEndian.AppendUint32(dst, low)
	return binary.BigEndian.AppendUint32(dst, high)
}

// AppendDenied appends a denied reply for xid: RPC_MISMATCH with its two
// versions when mismatch, else AUTH_ERROR with stat (RFC 5531 section 9.2.3).
func AppendDenied(dst []byte, xid uint32, mismatch bool, stat uint32) []byte {
	dst = binary.BigEndian.AppendUint32(dst, xid)
	dst = binary.BigEndian.AppendUint32(dst, replyType)
	dst = binary.BigEndian.AppendUint32(dst, replyDenied)
	if mismatch {
		dst = binary.BigEndian.AppendUint32(dst, rejectRPCMismatch)
		dst = binary.BigEndian.AppendUint32(dst, rpcVersion)
		return binary.BigEndian.AppendUint32(dst, rpcVersion)
	}
	dst = binary.BigEndian.AppendUint32(dst, rejectAuthError)
	return binary.BigEndian.AppendUint32(dst, stat)
}

// AppendCompoundHead appends a COMPOUND's reply body: its status, its tag, and
// n results, none for a reply that fails before its first member (RFC 20
// section 4.7).
func AppendCompoundHead(dst []byte, status uint32, tag []byte, n int) []byte {
	dst = binary.BigEndian.AppendUint32(dst, status)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(tag)))
	dst = append(dst, tag...)
	for i := len(tag); i%4 != 0; i++ {
		dst = append(dst, 0)
	}
	return binary.BigEndian.AppendUint32(dst, uint32(n))
}
