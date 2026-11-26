package gss

import (
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// RFC 4121 §4.2.6.1's MIC token: the token ID, a flags byte, five filler
// bytes of 0xff, the sender's 64-bit sequence number, then the checksum over
// the message followed by those 16 header bytes.
const (
	tokMIC       uint16 = 0x0404
	micHeaderLen        = 16
	micTokenLen         = micHeaderLen + hmacSHA1Size
)

// RFC 4121 §4.2.2's flags.
const (
	flagSentByAcceptor byte = 0x01
	flagAcceptorSubkey byte = 0x04
)

// KerberosContext is a Kerberos context under keys for the side that holds
// it: the acceptor's, or with initiator the initiator's. The side picks the
// key usages and flags its tokens carry, and the key usage of its peer's.
// keys.Session is the context key, of one of the two types the tokens are
// built for.
//
// decision: the MIC token keeps the layout, key usages and acceptor-subkey
// flag of the per-message code it was lifted from, but not that code's
// gokrb5 checksum calls, which copy the message on every call; the checksum
// is the standard library's HMAC fed the message where it lies, under keys
// gokrb5 derives once here. Overturned if gokrb5 gains a checksum over a
// message in place.
func KerberosContext(ev vfs.Evidence, keys vfs.Keys, initiator bool) (*Context, error) {
	sendUsage, recvUsage := usageAcceptorSign, usageInitiatorSign
	flags := flagSentByAcceptor
	if initiator {
		sendUsage, recvUsage = recvUsage, sendUsage
		flags = 0
	}
	// RFC 4121 §4.2.2: the acceptor flags its tokens when its subkey, sent in
	// the AP-REP, is the context key.
	if len(keys.Subkey) > 0 && !initiator {
		flags |= flagAcceptorSubkey
	}
	send, err := newChecksummer(keys.Session, sendUsage)
	if err != nil {
		return nil, err
	}
	recv, err := newChecksummer(keys.Session, recvUsage)
	if err != nil {
		return nil, err
	}
	m := &micKeys{send: send, recv: recv, flags: flags}
	return &Context{Evidence: ev, Keys: keys, MIC: m.mic, VerifyMIC: m.verify}, nil
}

// micKeys is one side's MIC state: its checksum keys for each direction, its
// flags, and its sent tokens' sequence numbers.
type micKeys struct {
	send, recv *checksummer
	flags      byte
	seq        atomic.Uint64
}

// mic is msg's MIC token. It reads msg where it lies; the token is the one
// allocation, room for the HMAC's whole sum before its truncation.
func (m *micKeys) mic(msg []byte) ([]byte, error) {
	tok := appendMICHeader(make([]byte, 0, micHeaderLen+20), m.flags, m.seq.Add(1)-1)
	return m.send.appendSum(tok, msg, tok), nil
}

// verify checks the peer's MIC token tok over msg. A token of this side's
// own direction is refused by its checksum, under the other key usage.
//
// decision: the token's sequence number is not checked. RPCSEC_GSS keeps its
// own window over its calls, and its clients ask for no GSS replay or
// sequence detection. Overturned if a client is ever seen asking for either.
func (m *micKeys) verify(msg, tok []byte) error {
	_, _, sum, err := parseMIC(tok)
	if err != nil {
		return err
	}
	var buf [20]byte // the HMAC-SHA1 before its truncation
	if !hmac.Equal(m.recv.appendSum(buf[:0], msg, tok[:micHeaderLen]), sum) {
		return errors.New("gss: a MIC token whose checksum does not verify")
	}
	return nil
}

// appendMICHeader appends a MIC token's header to dst.
func appendMICHeader(dst []byte, flags byte, seq uint64) []byte {
	dst = binary.BigEndian.AppendUint16(dst, tokMIC)
	dst = append(dst, flags, 0xff, 0xff, 0xff, 0xff, 0xff)
	return binary.BigEndian.AppendUint64(dst, seq)
}

// parseMIC reads a MIC token: its flags, its sequence number and its
// checksum, a slice of tok.
func parseMIC(tok []byte) (flags byte, seq uint64, sum []byte, err error) {
	if len(tok) != micTokenLen {
		return 0, 0, nil, fmt.Errorf("gss: a MIC token of %d bytes, want %d", len(tok), micTokenLen)
	}
	if id := binary.BigEndian.Uint16(tok); id != tokMIC {
		return 0, 0, nil, fmt.Errorf("gss: token ID %#04x, want a MIC token's %#04x", id, tokMIC)
	}
	for _, b := range tok[3:8] {
		if b != 0xff {
			return 0, 0, nil, errors.New("gss: a MIC token whose filler is not 0xff")
		}
	}
	return tok[2], binary.BigEndian.Uint64(tok[8:]), tok[micHeaderLen:], nil
}
