package identity

import (
	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/msgtype"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
)

// apRep is the raw AP-REP answering auth (RFC 4120 §5.5.2), not wrapped for
// GSS-API: its encrypted part carries the authenticator's time and
// microseconds under the ticket's session key, and no subkey. Each adapter
// adds its own framing.
//
// It carries no subkey even when the authenticator sent one: under RFC 4121
// §4.2.6 an acceptor subkey asserts a new key that every later per-message
// token must flag and key from, and echoing the initiator's would make
// Windows and Samba clients reject the mechListMIC. The context key stays the
// initiator's subkey. gofork's asn1 marshals it, as gokrb5 does, since the
// standard library's mis-encodes Kerberos times.
func apRep(auth *types.Authenticator, sessionKey types.EncryptionKey) ([]byte, error) {
	part, err := asn1.Marshal(messages.EncAPRepPart{CTime: auth.CTime, Cusec: auth.Cusec})
	if err != nil {
		return nil, err
	}
	enc, err := crypto.GetEncryptedData(asn1tools.AddASNAppTag(part, asnAppTag.EncAPRepPart), sessionKey, keyusage.AP_REP_ENCPART, 0)
	if err != nil {
		return nil, err
	}
	rep, err := asn1.Marshal(messages.APRep{PVNO: 5, MsgType: msgtype.KRB_AP_REP, EncPart: enc})
	if err != nil {
		return nil, err
	}
	return asn1tools.AddASNAppTag(rep, asnAppTag.APREP), nil
}
