package gss

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// krb5OID is the DER encoding, tag and length included, of the Kerberos V5
// mechanism's OID, 1.2.840.113554.1.2.2 (RFC 4121 §1).
var krb5OID = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

// The token IDs RFC 4121 §4.1 puts before a context token's Kerberos message.
const (
	tokAPReq uint16 = 0x0100
	tokAPRep uint16 = 0x0200
)

// Kerberos is the Kerberos mechanism's acceptor. It accepts one initial
// context token: the AP-REQ inside it goes to svc.SignIn with step's Service,
// Transport and Client, and the AP-REP SignIn returns comes back framed. It
// checks no ticket itself.
func Kerberos(svc vfs.Service, step vfs.SignInStep) Mechanism {
	return &kerberos{svc: svc, step: step}
}

type kerberos struct {
	svc     vfs.Service
	step    vfs.SignInStep
	stepped bool
	ctx     *Context
}

// Step takes the initiator's one token. Every error, SignIn's and the
// mechanism's own, is ErrLogonFailure, and leaves no Context.
func (k *kerberos) Step(ctx context.Context, in []byte) ([]byte, bool, error) {
	if k.stepped {
		return nil, false, logonFailure(errors.New("a token after the first"))
	}
	k.stepped = true
	apReq, err := unframe(in, tokAPReq)
	if err != nil {
		return nil, false, logonFailure(err)
	}
	step := k.step
	step.Kerberos = apReq
	res, err := k.svc.SignIn(ctx, step)
	if err != nil {
		return nil, false, logonFailure(err)
	}
	// decision: the context key's type is read from its length, 16 bytes
	// for aes128-cts-hmac-sha1-96 and 32 for aes256-cts-hmac-sha1-96, the
	// two types the per-message tokens are built for, since Keys carries no
	// type. SignIn answers no other type of key, an RFC 8009 subkey of the
	// same lengths included, so the length names it. Overturned if SignIn
	// ever accepts another type, which would put the type in Keys.
	if n := len(res.Keys.Session); n != 16 && n != 32 {
		return nil, false, logonFailure(fmt.Errorf("a context key of %d bytes", n))
	}
	k.ctx = &Context{Evidence: res.Identity.Evidence, Keys: res.Keys}
	return frame(res.Keys.APRep, tokAPRep), true, nil
}

func (k *kerberos) Context() *Context { return k.ctx }

// logonFailure is err as the one failure an exchange answers.
func logonFailure(err error) error {
	return fmt.Errorf("%w: gss: kerberos: %v", vfs.ErrLogonFailure, err)
}

// unframe is the Kerberos message inside tok, an RFC 2743 §3.1 initial
// context token of the Kerberos mechanism whose RFC 4121 §4.1 token ID is
// id: the tag 0x60, a DER length covering every byte after it, the
// mechanism's OID, the token ID and the message. The message is a slice of
// tok.
//
// decision: develop's framing, which this one lifts, also accepted a raw
// AP-REQ with no RFC 2743 §3.1 wrapper, bytes past the token's length, and
// any mechanism OID, Microsoft's legacy Kerberos OID among them. Each is
// refused here: RFC 2743 §3.1 allows none, the Linux client's captured INIT
// tokens use none, and Microsoft's OID arrives only inside SPNEGO, whose
// mechanism list maps it before this mechanism is reached. Overturned if a
// client the capture corpus holds sends any of the three.
func unframe(tok []byte, id uint16) ([]byte, error) {
	if len(tok) == 0 || tok[0] != 0x60 {
		return nil, errors.New("not an initial context token")
	}
	n, l, err := derLength(tok[1:])
	if err != nil {
		return nil, err
	}
	body := tok[1+l:]
	if n != uint64(len(body)) {
		return nil, fmt.Errorf("token length %d, %d bytes follow", n, len(body))
	}
	if !bytes.HasPrefix(body, krb5OID) {
		return nil, errors.New("not the Kerberos mechanism")
	}
	body = body[len(krb5OID):]
	if len(body) < 2 {
		return nil, errors.New("no token ID")
	}
	if got := binary.BigEndian.Uint16(body); got != id {
		return nil, fmt.Errorf("token ID %#04x, want %#04x", got, id)
	}
	return body[2:], nil
}

// derLength reads a DER length from b: its value and the bytes it takes. A
// long form is at most 4 bytes and must be minimal, as DER has it, so a
// length has one encoding and a token re-encodes to its own bytes.
func derLength(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errors.New("no length")
	}
	if b[0] < 0x80 {
		return uint64(b[0]), 1, nil
	}
	m := int(b[0] & 0x7f)
	if m == 0 || m > 4 || m >= len(b) {
		return 0, 0, errors.New("a length that is not DER, or runs past the token")
	}
	var n uint64
	for _, c := range b[1 : 1+m] {
		n = n<<8 | uint64(c)
	}
	if b[1] == 0 || n < 0x80 {
		return 0, 0, errors.New("a long-form length that is not minimal")
	}
	return n, 1 + m, nil
}

// frame is inner as an initial context token of the Kerberos mechanism under
// id, the inverse of unframe. MIT's clients want this framing on the AP-REP
// too, though RFC 4121 §4.1 lets a reply omit it.
func frame(inner []byte, id uint16) []byte {
	n := len(krb5OID) + 2 + len(inner)
	out := make([]byte, 0, 6+n)
	out = append(out, 0x60)
	out = appendDERLength(out, n)
	out = append(out, krb5OID...)
	out = binary.BigEndian.AppendUint16(out, id)
	return append(out, inner...)
}

// appendDERLength appends n's DER length.
func appendDERLength(dst []byte, n int) []byte {
	if n < 0x80 {
		return append(dst, byte(n))
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	i := 0
	for b[i] == 0 {
		i++
	}
	dst = append(dst, 0x80|byte(8-i))
	return append(dst, b[i:]...)
}
