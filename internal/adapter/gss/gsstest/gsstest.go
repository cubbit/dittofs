// Package gsstest is the initiator side of gss's mechanisms, for tests only,
// so an exchange runs in process on real messages with no KDC.
package gsstest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// krb5OID is the Kerberos V5 mechanism's OID, tag and length included.
var krb5OID = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

// KerberosInitiator is the Kerberos mechanism's initiator for one AP-REQ a
// test minted, such as identitytest.Realm's. Its first Step, on no token,
// answers the AP-REQ framed as RFC 2743 §3.1's initial context token; its
// second takes the acceptor's AP-REP and is done once that proves the
// acceptor decrypted the AP-REQ's authenticator.
//
// decision: it frames and unframes tokens with code of its own, not gss's,
// so an acceptor whose framing is wrong cannot pass because its initiator
// shares the mistake. Overturned if the framing ever needs more than the
// fixed OID and token ID.
type KerberosInitiator struct {
	apReq   []byte
	auth    types.Authenticator
	session types.EncryptionKey
	sent    bool
	ctx     *gss.Context
}

var _ gss.Mechanism = (*KerberosInitiator)(nil)

// Kerberos is the initiator of apReq, whose ticket carries the session key
// session.
func Kerberos(apReq []byte, session types.EncryptionKey) (*KerberosInitiator, error) {
	var req messages.APReq
	if err := req.Unmarshal(apReq); err != nil {
		return nil, fmt.Errorf("gsstest: AP-REQ: %w", err)
	}
	if err := req.DecryptAuthenticator(session); err != nil {
		return nil, fmt.Errorf("gsstest: authenticator: %w", err)
	}
	return &KerberosInitiator{apReq: apReq, auth: req.Authenticator, session: session}, nil
}

// Step answers the framed AP-REQ to the first call, and checks the framed
// AP-REP in on the second: its encrypted part must decrypt under the ticket's
// session key and carry the authenticator's time.
func (i *KerberosInitiator) Step(_ context.Context, in []byte) ([]byte, bool, error) {
	if !i.sent {
		i.sent = true
		return frame(i.apReq, 0x0100), false, nil
	}
	if i.ctx != nil {
		return nil, false, errors.New("gsstest: a token after the AP-REP")
	}
	raw, err := unframe(in, 0x0200)
	if err != nil {
		return nil, false, err
	}
	var rep messages.APRep
	if err := rep.Unmarshal(raw); err != nil {
		return nil, false, fmt.Errorf("gsstest: AP-REP: %w", err)
	}
	b, err := crypto.DecryptEncPart(rep.EncPart, i.session, keyusage.AP_REP_ENCPART)
	if err != nil {
		return nil, false, fmt.Errorf("gsstest: AP-REP does not decrypt: %w", err)
	}
	var part messages.EncAPRepPart
	if err := part.Unmarshal(b); err != nil {
		return nil, false, fmt.Errorf("gsstest: AP-REP encrypted part: %w", err)
	}
	if !part.CTime.Equal(i.auth.CTime) || part.Cusec != i.auth.Cusec {
		return nil, false, errors.New("gsstest: AP-REP answers another authenticator")
	}
	// RFC 4121 §2: the context key is the acceptor's subkey when the AP-REP
	// sends one, else the authenticator's, else the ticket's session key.
	keys := vfs.Keys{Session: i.session.KeyValue, APRep: raw}
	switch {
	case len(part.Subkey.KeyValue) > 0:
		keys.Session, keys.Subkey = part.Subkey.KeyValue, part.Subkey.KeyValue
	case len(i.auth.SubKey.KeyValue) > 0:
		keys.Session = i.auth.SubKey.KeyValue
	}
	i.ctx = &gss.Context{Keys: keys}
	return nil, true, nil
}

// Context is the initiator's context once the AP-REP checked: its Keys, the
// same context key as the acceptor's, and no Evidence.
func (i *KerberosInitiator) Context() *gss.Context { return i.ctx }

func frame(inner []byte, id uint16) []byte {
	n := len(krb5OID) + 2 + len(inner)
	out := []byte{0x60}
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	default:
		out = append(out, 0x82, byte(n>>8), byte(n))
	}
	out = append(out, krb5OID...)
	out = binary.BigEndian.AppendUint16(out, id)
	return append(out, inner...)
}

// unframe accepts the token frame writes, with a length of at most two bytes,
// which every AP-REP is.
func unframe(tok []byte, id uint16) ([]byte, error) {
	if len(tok) < 2 || tok[0] != 0x60 {
		return nil, errors.New("gsstest: not an initial context token")
	}
	n, body := int(tok[1]), tok[2:]
	switch tok[1] {
	case 0x81:
		if len(body) < 1 {
			return nil, errors.New("gsstest: truncated length")
		}
		n, body = int(body[0]), body[1:]
	case 0x82:
		if len(body) < 2 {
			return nil, errors.New("gsstest: truncated length")
		}
		n, body = int(binary.BigEndian.Uint16(body)), body[2:]
	}
	if n != len(body) || !bytes.HasPrefix(body, krb5OID) || len(body) < len(krb5OID)+2 {
		return nil, errors.New("gsstest: not a Kerberos context token of its length")
	}
	body = body[len(krb5OID):]
	if binary.BigEndian.Uint16(body) != id {
		return nil, fmt.Errorf("gsstest: token ID %#04x, want %#04x", binary.BigEndian.Uint16(body), id)
	}
	return body[2:], nil
}
