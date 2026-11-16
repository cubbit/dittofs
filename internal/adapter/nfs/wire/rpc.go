package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	// ErrUnaddressable is a record too short to hold a call's transaction ID
	// and message type: nothing can be answered, and the connection closes.
	ErrUnaddressable = errors.New("a record too short to address")

	// ErrNotCall is a message of another type where a call was expected: it is
	// dropped and nothing is answered.
	ErrNotCall = errors.New("not a call")

	errPastRecord = errors.New("a value runs past the record")
	errTooLong    = errors.New("an opaque past its limit")
)

// fields are one call's offsets, as the parse bounded them: the credential's
// and verifier's bodies, a COMPOUND's tag and minor version.
type fields struct {
	typ, rpcvers, prog, vers, proc uint32
	credFlavour                    uint32
	credAt, credLen                int
	verfFlavour                    uint32
	verfAt, verfLen                int
	tagAt, tagLen                  int
	minor                          uint32
	compound                       bool
}

// Call is a parsed call: a view over the record's bytes that allocates nothing
// (RFC 20 section 5). Its accessors read the fields the parse bounded.
type Call []byte

// ParseCall parses the call at the start of rec, a record's body: the
// transaction ID and message type, the RPC version, the program, version and
// procedure, the credential and the verifier, and, for procedure 1 of program
// 100003 version 4, the COMPOUND prefix's tag and minor version (RFC 5531
// section 9, RFC 20 section 4.7). Every credential and verifier body is at most
// 400 bytes (RFC 5531 section 8.2) and lies inside the record with its padding.
//
// A record too short for its transaction ID and message type is refused with
// ErrUnaddressable, and a message of another type than a call with ErrNotCall;
// both come back with no call. An RPC version other than 2 comes back with the
// call and no error, its credential and verifier unread, so the router answers
// RPC_MISMATCH rather than a decode error. A call whose later fields do not
// decode comes back with its error (RFC 21 section 11.5).
func ParseCall(rec []byte) (Call, error) {
	if _, err := walk(rec); err != nil {
		if errors.Is(err, ErrUnaddressable) {
			return nil, err
		}
		return Call(rec), err
	}
	return Call(rec), nil
}

// walk validates the call at the start of c and returns its fields.
func walk(c []byte) (fields, error) {
	var f fields
	r := reader{b: c}
	xid, ok := r.u32()
	if !ok {
		return f, ErrUnaddressable
	}
	_ = xid
	if f.typ, ok = r.u32(); !ok {
		return f, ErrUnaddressable
	}
	if f.typ != callType {
		return f, ErrNotCall
	}
	target := []*uint32{&f.rpcvers, &f.prog, &f.vers, &f.proc}
	for i, p := range target {
		v, isOK := r.u32()
		if !isOK {
			return f, fmt.Errorf("nfs/wire: %w: the call's field %d", errPastRecord, i)
		}
		*p = v
	}
	_ = target
	if f.rpcvers != rpcVersion {
		return f, nil
	}
	var err error
	if f.credFlavour, f.credAt, f.credLen, err = r.opaqueAuth(); err != nil {
		return f, err
	}
	if f.verfFlavour, f.verfAt, f.verfLen, err = r.opaqueAuth(); err != nil {
		return f, err
	}
	if isCompound(f) {
		if f.tagAt, f.tagLen, err = r.opaque(); err != nil {
			return f, fmt.Errorf("nfs/wire: a COMPOUND's tag: %w", err)
		}
		if f.minor, ok = r.u32(); !ok {
			return f, fmt.Errorf("nfs/wire: %w: a COMPOUND's minor version", errPastRecord)
		}
		f.compound = true
	}
	return f, nil
}

// isCompound reports whether f is an NFSv4 COMPOUND call, the only one whose
// tag and minor version the parse reads (RFC 20 section 4.7).
func isCompound(f fields) bool {
	return f.prog == ProgNFS && f.vers == VersNFS4 && f.proc == ProcCompound
}

// fields returns c's fields; its error cannot occur on a call ParseCall
// accepted.
func (c Call) fields() fields {
	f, _ := walk(c)
	return f
}

// XID is the call's transaction ID.
func (c Call) XID() uint32 { return binary.BigEndian.Uint32(c[0:]) }

// Type is the message type: a call, unless ParseCall refused it as one.
func (c Call) Type() uint32 { return c.fields().typ }

// RPCVersion is the call's RPC version, 2 for every call served.
func (c Call) RPCVersion() uint32 { return c.fields().rpcvers }

// Program, Version and Procedure are the call's target.
func (c Call) Program() uint32   { return c.fields().prog }
func (c Call) Version() uint32   { return c.fields().vers }
func (c Call) Procedure() uint32 { return c.fields().proc }

// Credential is the call's credential: its flavour, and its body as a slice of
// the record, never a copy.
func (c Call) Credential() (flavour uint32, body []byte) {
	f := c.fields()
	return f.credFlavour, c[f.credAt : f.credAt+f.credLen]
}

// Verifier is the call's verifier, as a slice of the record.
func (c Call) Verifier() (flavour uint32, body []byte) {
	f := c.fields()
	return f.verfFlavour, c[f.verfAt : f.verfAt+f.verfLen]
}

// Tag is a COMPOUND's tag, as a slice of the record; nil for any other call.
func (c Call) Tag() []byte {
	f := c.fields()
	if !f.compound {
		return nil
	}
	return c[f.tagAt : f.tagAt+f.tagLen]
}

// MinorVersion is a COMPOUND's minor version; ok is false for any other call.
func (c Call) MinorVersion() (minor int, ok bool) {
	f := c.fields()
	return int(f.minor), f.compound
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

// opaque reads an XDR opaque's length and bounds it: its bytes and their
// padding to four must lie inside the record, its body at most MaxRecord. It
// returns the body's offset and length, no copy and no allocation.
func (r *reader) opaque() (at, n int, err error) {
	l, ok := r.u32()
	if !ok {
		return 0, 0, fmt.Errorf("nfs/wire: %w: an opaque's length", errPastRecord)
	}
	pad := (4 - l%4) % 4
	if l > MaxRecord || r.off+int(l)+int(pad) > len(r.b) {
		return 0, 0, fmt.Errorf("nfs/wire: %w: an opaque of %d bytes", errPastRecord, l)
	}
	at = r.off
	r.off += int(l) + int(pad)
	return at, int(l), nil
}

// opaqueAuth reads an opaque_auth: its flavour and the bounds of its body, at
// most 400 bytes.
func (r *reader) opaqueAuth() (flavour uint32, at, n int, err error) {
	if flavour, ok := r.u32(); ok {
		if at, n, err = r.opaque(); err != nil {
			return 0, 0, 0, err
		}
		if n > maxAuthBody {
			return 0, 0, 0, fmt.Errorf("nfs/wire: %w: a body of %d bytes, past %d", errTooLong, n, maxAuthBody)
		}
		return flavour, at, n, nil
	}
	return 0, 0, 0, fmt.Errorf("nfs/wire: %w: a flavour", errPastRecord)
}

// AppendAccepted appends an accepted reply for xid: the reply state, an empty
// AUTH_NONE verifier, and stat (RFC 5531 section 9.2.2). The program's reply
// follows, at its call's procedure.
func AppendAccepted(dst []byte, xid, stat uint32) []byte {
	dst = binary.BigEndian.AppendUint32(dst, xid)
	dst = binary.BigEndian.AppendUint32(dst, replyType)
	dst = binary.BigEndian.AppendUint32(dst, replyAccepted)
	dst = binary.BigEndian.AppendUint32(dst, AuthNone)
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

// AppendCompoundStatus appends a COMPOUND's reply body: its status, its tag as
// it came, and n results, none for a reply that fails before its first member
// (RFC 20 section 4.7).
func AppendCompoundStatus(dst []byte, status uint32, tag []byte, n int) []byte {
	dst = binary.BigEndian.AppendUint32(dst, status)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(tag)))
	dst = append(dst, tag...)
	for i := len(tag); i%4 != 0; i++ {
		dst = append(dst, 0)
	}
	return binary.BigEndian.AppendUint32(dst, uint32(n))
}
