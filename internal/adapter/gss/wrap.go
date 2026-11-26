package gss

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// RFC 4121 §4.2.6.2's Wrap token: the token ID, a flags byte, one filler
// byte of 0xff, EC, RRC and the sender's 64-bit sequence number, then, when
// sealed, RFC 3961's encryption of a confounder, the message, EC bytes of
// filler and a copy of the header with RRC 0, and the checksum over those.
const (
	tokWrap       uint16 = 0x0504
	wrapHeaderLen        = 16
)

// WrapHeadroom and WrapTailroom are the room Wrap seals a message in: the
// token header and the confounder before it, and the header's copy and the
// checksum after it.
const (
	WrapHeadroom = wrapHeaderLen + aes.BlockSize
	WrapTailroom = wrapHeaderLen + hmacSHA1Size
)

// wrapMinLen is the shortest sealed token: a header, then a confounder, an
// empty message and the header's copy encrypted, then the checksum.
const wrapMinLen = WrapHeadroom + WrapTailroom

// decision: the Wrap token keeps the layout of the per-message code it was
// lifted from: the header and its encrypted copy, EC naming the filler, RRC,
// the seal key usages and the acceptor-subkey flag. It does not keep that
// code's gokrb5 encryption, which copies the message on every call: the
// message is sealed and unsealed where it lies, by AES-CTS and HMAC-SHA1-96
// over the standard library under keys gokrb5 derives once. Overturned if
// gokrb5 gains an encryption over a message in place.
//
// wrap seals msg's message, the bytes after its first WrapHeadroom, into a
// Wrap token in the same array, which needs WrapTailroom bytes of capacity
// after msg. It writes no filler and no rotation: AES-CTS takes a message of
// any length, and a receiver undoes any rotation.
func (m *tokenKeys) wrap(msg []byte) ([]byte, error) {
	n := len(msg)
	if n < WrapHeadroom || cap(msg)-n < WrapTailroom {
		return nil, fmt.Errorf("gss: Wrap wants %d bytes before the message and %d of capacity after it, has %d and %d",
			WrapHeadroom, WrapTailroom, n, cap(msg)-n)
	}
	tok := msg[:n+WrapTailroom]
	appendWrapHeader(tok[:0], m.flags|flagSealed, 0, 0, m.seq.Add(1)-1)
	if _, err := io.ReadFull(m.rand, tok[wrapHeaderLen:WrapHeadroom]); err != nil {
		return nil, fmt.Errorf("gss: a Wrap token's confounder: %w", err)
	}
	copy(tok[n:], tok[:wrapHeaderLen])
	plain := tok[wrapHeaderLen : n+wrapHeaderLen]
	var sum [20]byte // the HMAC-SHA1 before its truncation
	copy(tok[n+wrapHeaderLen:], m.seal.sum.appendSum(sum[:0], plain, nil))
	m.seal.encrypt(plain)
	return tok, nil
}

// unwrap unseals the peer's Wrap token tok in place and returns the message,
// a slice of tok. tok's bytes are not kept: it is decrypted where it lies,
// its rotation undone first. Its SentByAcceptor and AcceptorSubkey flags
// must be the peer's, as verify checks a MIC token's, so a token of this
// side's own direction is refused by them and by its checksum, under the
// other key usage. Its sequence number is not checked, as verify's is not.
func (m *tokenKeys) unwrap(tok []byte) ([]byte, error) {
	flags, ec, rrc, _, err := parseWrap(tok)
	if err != nil {
		return nil, err
	}
	// The Sealed flag travels in the clear, bound to nothing the caller
	// declared, so a token without it would carry a krb5p call's arguments
	// unencrypted while the call is taken as private.
	if flags&flagSealed == 0 {
		return nil, errors.New("gss: a Wrap token not sealed")
	}
	if got := flags & (flagSentByAcceptor | flagAcceptorSubkey); got != m.peer {
		return nil, fmt.Errorf("gss: a Wrap token flagged %#02x, want the peer's %#02x", got, m.peer)
	}
	if len(tok) < wrapMinLen {
		return nil, fmt.Errorf("gss: a sealed Wrap token of %d bytes, want at least %d", len(tok), wrapMinLen)
	}
	body := tok[wrapHeaderLen:]
	rotateLeft(body, int(rrc)%len(body))
	plain, sum := body[:len(body)-hmacSHA1Size], body[len(body)-hmacSHA1Size:]
	m.unseal.decrypt(plain)
	var buf [20]byte // the HMAC-SHA1 before its truncation
	if !hmac.Equal(m.unseal.sum.appendSum(buf[:0], plain, nil), sum) {
		return nil, errors.New("gss: a Wrap token whose checksum does not verify")
	}
	// The encrypted copy binds every field of the clear header but RRC, which
	// is applied after encryption. EC among them: raised in the clear, it
	// would cut the message's end off as filler.
	hdrCopy := plain[len(plain)-wrapHeaderLen:]
	if !bytes.Equal(hdrCopy[:6], tok[:6]) || !bytes.Equal(hdrCopy[8:], tok[8:wrapHeaderLen]) {
		return nil, errors.New("gss: a Wrap token whose header differs from its encrypted copy")
	}
	end := len(plain) - wrapHeaderLen - int(ec)
	if end < aes.BlockSize {
		return nil, fmt.Errorf("gss: a Wrap token whose EC %d runs past its message", ec)
	}
	return plain[aes.BlockSize:end], nil
}

// appendWrapHeader appends a Wrap token's header to dst.
func appendWrapHeader(dst []byte, flags byte, ec, rrc uint16, seq uint64) []byte {
	dst = binary.BigEndian.AppendUint16(dst, tokWrap)
	dst = append(dst, flags, 0xff)
	dst = binary.BigEndian.AppendUint16(dst, ec)
	dst = binary.BigEndian.AppendUint16(dst, rrc)
	return binary.BigEndian.AppendUint64(dst, seq)
}

// parseWrap reads a Wrap token's header: its flags, EC, RRC and sequence
// number.
func parseWrap(tok []byte) (flags byte, ec, rrc uint16, seq uint64, err error) {
	if len(tok) < wrapHeaderLen {
		return 0, 0, 0, 0, fmt.Errorf("gss: a Wrap token of %d bytes, shorter than its header", len(tok))
	}
	if id := binary.BigEndian.Uint16(tok); id != tokWrap {
		return 0, 0, 0, 0, fmt.Errorf("gss: token ID %#04x, want a Wrap token's %#04x", id, tokWrap)
	}
	if tok[3] != 0xff {
		return 0, 0, 0, 0, errors.New("gss: a Wrap token whose filler is not 0xff")
	}
	return tok[2], binary.BigEndian.Uint16(tok[4:]), binary.BigEndian.Uint16(tok[6:]), binary.BigEndian.Uint64(tok[8:]), nil
}

// rotateLeft rotates b left by n bytes in place, undoing a right rotation.
func rotateLeft(b []byte, n int) {
	if n == 0 {
		return
	}
	slices.Reverse(b[:n])
	slices.Reverse(b[n:])
	slices.Reverse(b)
}
