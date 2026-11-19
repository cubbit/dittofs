package identity

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/addrtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// clockSkew is how far a ticket's times and an authenticator's time may stray
// from the clock.
//
// ponytail: the clock skew, the old key's retention and the hour a pinned
// domain controller may go unanswered are constants. Make one a setting when a
// deployment's KDC or domain is shown to need another value.
const clockSkew = 5 * time.Minute

// retention is how long a derived key outlives its version, before the clock
// skew: a ticket's longest life, so a ticket issued under the previous key
// verifies until it expires.
const retention = 10 * time.Hour

// unanswered is how long a pinned domain controller may go unanswered, and
// how long the rotation waits for this node to hold the previous version,
// before the rotation fails.
const unanswered = time.Hour

// ticket is what a verified AP-REQ yields: the request with its ticket's
// encrypted part and its authenticator decrypted, the service key that
// decrypted the ticket, and the keys the adapter's mechanism needs.
type ticket struct {
	req  messages.APReq
	key  types.EncryptionKey
	keys identity.Keys
}

// serviceNames is the first component of a ticket's service name each
// Service accepts.
var serviceNames = map[identity.Service]string{
	identity.ServiceNFS:  "nfs",
	identity.ServiceCIFS: "cifs",
	identity.ServiceHost: "host",
}

// logonFailure is a refusal of a well-formed AP-REQ.
func logonFailure(format string, a ...any) error {
	return fmt.Errorf("%w: kerberos: %s", vfserr.ErrLogonFailure, fmt.Sprintf(format, a...))
}

// parseAPReq decodes an AP-REQ, refusing what does not decode ErrInvalid.
func parseAPReq(b []byte) (messages.APReq, error) {
	var req messages.APReq
	if err := req.Unmarshal(b); err != nil {
		return messages.APReq{}, fmt.Errorf("%w: kerberos: AP-REQ: %w", vfserr.ErrInvalid, err)
	}
	return req, nil
}

// verifyAPReq verifies the AP-REQ b for svc, arriving over tr from client at
// now, under st's key ring. A step naming no transport or no service, and
// bytes that do not decode, are ErrInvalid; every other refusal is
// ErrLogonFailure.
//
// decision: the AP-REQ is decrypted with gokrb5's primitives and every check
// is made here, never by gokrb5's VerifyAPREQ, which reads the wall clock,
// keeps a replay cache of its own and picks the key itself; the skew must run
// on the injected clock, the PAC's checksum must use the key that decrypted
// the ticket, and the key is chosen from the ring Apply built. Overturned if
// gokrb5 takes the clock, the key and the replay cache from its caller.
func verifyAPReq(st *State, b []byte, svc identity.Service, tr identity.Transport, client netip.Addr, now time.Time) (ticket, error) {
	// decision: a step that names no transport is refused, never read as one
	// without the address rule, so a caller that forgets to set it fails
	// closed. The rule keys on the transport, not on Service, since a host/
	// ticket may arrive over SMB, where the rule applies. Overturned if the
	// address rule ever applies over every transport alike.
	if tr != identity.TransportNFS && tr != identity.TransportSMB {
		return ticket{}, fmt.Errorf("%w: kerberos: no transport", vfserr.ErrInvalid)
	}
	want, ok := serviceNames[svc]
	if !ok {
		return ticket{}, fmt.Errorf("%w: kerberos: no service", vfserr.ErrInvalid)
	}
	req, err := parseAPReq(b)
	if err != nil {
		return ticket{}, err
	}
	tkt := &req.Ticket
	// decision: the service name is checked twice: the ring must hold a key
	// under the name as the ticket spells it, ASCII letters folded, which the
	// lookup below does, and its first component must be the step's service,
	// so a cifs/ ticket presented over NFS is refused even though the ring
	// holds its key. Overturned if one service is ever to accept another's
	// tickets.
	if len(tkt.SName.NameString) == 0 || foldASCII(tkt.SName.NameString[0]) != want {
		return ticket{}, logonFailure("ticket for %q presented as %s", tkt.SName.PrincipalNameString(), want)
	}
	if !aesEtype(tkt.EncPart.EType) {
		return ticket{}, logonFailure("ticket under encryption type %d", tkt.EncPart.EType)
	}
	keys, err := st.serviceKeys(tkt.SName.PrincipalNameString(), tkt.Realm, uint32(tkt.EncPart.KVNO), tkt.EncPart.EType, now)
	if err != nil {
		return ticket{}, logonFailure("%v", err)
	}
	var key types.EncryptionKey
	for _, k := range keys {
		if err = tkt.Decrypt(k); err == nil {
			key = k
			break
		}
	}
	if err != nil {
		return ticket{}, logonFailure("ticket does not decrypt: %v", err)
	}
	enc := &tkt.DecryptedEncPart
	if !aesKey(enc.Key) {
		return ticket{}, logonFailure("session key of encryption type %d", enc.Key.KeyType)
	}
	if err := req.DecryptAuthenticator(enc.Key); err != nil {
		return ticket{}, logonFailure("authenticator does not decrypt: %v", err)
	}
	auth := &req.Authenticator
	hasSubkey := auth.SubKey.KeyType != 0 || len(auth.SubKey.KeyValue) != 0
	if hasSubkey && !aesKey(auth.SubKey) {
		return ticket{}, logonFailure("subkey of encryption type %d", auth.SubKey.KeyType)
	}
	if auth.CRealm != enc.CRealm || !auth.CName.Equal(enc.CName) {
		return ticket{}, logonFailure("authenticator for another client")
	}
	// RFC 4120 §2.2.4: a ticket marked invalid, a postdated one not yet
	// validated at the KDC, must not be accepted.
	if types.IsFlagSet(&enc.Flags, flags.Invalid) {
		return ticket{}, logonFailure("ticket marked invalid")
	}
	start := enc.StartTime
	if start.IsZero() {
		start = enc.AuthTime
	}
	switch {
	case start.Sub(now) > clockSkew:
		return ticket{}, logonFailure("ticket not yet valid")
	case now.Sub(enc.EndTime) > clockSkew:
		return ticket{}, logonFailure("ticket expired")
	case auth.CTime.Sub(now) > clockSkew || now.Sub(auth.CTime) > clockSkew:
		return ticket{}, logonFailure("authenticator outside the clock skew")
	}
	if tr == identity.TransportSMB && !addressAllowed(client, enc.CAddr) {
		return ticket{}, logonFailure("client address not among the ticket's")
	}
	session := enc.Key
	if hasSubkey {
		session = auth.SubKey
	}
	rep, err := apRep(auth, enc.Key)
	if err != nil {
		return ticket{}, logonFailure("AP-REP: %v", err)
	}
	return ticket{req: req, key: key, keys: identity.Keys{Session: session.KeyValue, APRep: rep}}, nil
}

// aesEtype reports whether etype is aes128- or aes256-cts-hmac-sha1-96.
func aesEtype(etype int32) bool {
	return etype == etypeID.AES128_CTS_HMAC_SHA1_96 || etype == etypeID.AES256_CTS_HMAC_SHA1_96
}

// aesKey reports whether k is an AES key of its type's length.
func aesKey(k types.EncryptionKey) bool {
	switch k.KeyType {
	case etypeID.AES128_CTS_HMAC_SHA1_96:
		return len(k.KeyValue) == 16
	case etypeID.AES256_CTS_HMAC_SHA1_96:
		return len(k.KeyValue) == 32
	}
	return false
}

// addressAllowed reports whether a ticket listing caddr may be used from
// client: only its IPv4 and IPv6 addresses restrict it, a NetBIOS one names a
// workstation and is ignored, and a ticket listing none is usable from any
// address.
func addressAllowed(client netip.Addr, caddr types.HostAddresses) bool {
	restricted := false
	for _, a := range caddr {
		if a.AddrType != addrtype.IPv4 && a.AddrType != addrtype.IPv6 {
			continue
		}
		restricted = true
		if ip, ok := netip.AddrFromSlice(a.Address); ok && ip.Unmap() == client.Unmap().WithZone("") {
			return true
		}
	}
	return !restricted
}
