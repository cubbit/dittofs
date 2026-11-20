package identity

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/iana/addrtype"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/flags"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/pac"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
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
	// RFC 4120 §2.7: a cross-realm ticket whose transited realms the KDC did
	// not check is refused, since nothing here checks them either.
	if !types.IsFlagSet(&enc.Flags, flags.TransitedPolicyChecked) && len(enc.Transited.Contents) > 0 {
		return ticket{}, logonFailure("transited realms not checked")
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

// signInKerberos verifies step's AP-REQ, throttles the ticket's client name
// from step.Client, records the authenticator in the replay cache, and binds
// the name to a principal by RFC 18 §4.2.
//
// decision: a ticket refused before its client name is read, one that does
// not decrypt, and one refused by verifyAPReq's later checks, its times, its
// addresses or its authenticator, is refused before the throttle and not
// counted: the throttle keys on a name, and verifyAPReq checks what the KDC
// issued, never a credential of the user's, which the KDC already checked.
// A replay is counted, since it reaches the throttle. Overturned if a client
// is seen sending tickets that fail those checks to probe names.
func (s *Server) signInKerberos(ctx context.Context, step identity.SignInStep, ev *LogonEvent) (identity.SignInResult, outcome, error) {
	st := s.state.Load()
	if st == nil {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	if !st.holdsKeys() {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: kerberos: no service key", vfserr.ErrUnavailable)
	}
	now := s.deps.Clock.Now()
	t, err := verifyAPReq(st, step.Kerberos, step.Service, step.Transport, step.Client, now)
	switch {
	case errors.Is(err, vfserr.ErrInvalid):
		return identity.SignInResult{}, outcomeInvalid, err
	case err != nil:
		return identity.SignInResult{}, outcomeLogonFailure, err
	}
	enc := &t.req.Ticket.DecryptedEncPart
	ev.Name = enc.CName.PrincipalNameString() + "@" + enc.CRealm
	// decision: the attempt is throttled under the ticket's client name, read
	// once the ticket decrypts under the service key, and before its
	// authenticator enters the replay cache, so a pair past its limit takes
	// no entry. A ticket that does not decrypt names no one and is refused
	// above, uncounted. Overturned if the throttle must run before any
	// decryption, which would leave it no name to key on.
	settle, ok := s.throttle.reserve(ev.Name, step.Client, now)
	if !ok {
		return identity.SignInResult{}, outcomeThrottled, vfserr.ErrLogonFailure
	}
	failed := false
	defer func() { settle(failed) }() // on every return and a panic, so no slot stays reserved
	res, oc, err := s.bindTicket(ctx, st, step, &t, ev.Name, now)
	failed = errors.Is(err, vfserr.ErrLogonFailure)
	return res, oc, err
}

// bindTicket records t's authenticator, verifies the PAC its ticket carries,
// and binds its client to the principal RFC 18 §4.2 maps it to, refusing a
// disabled one.
func (s *Server) bindTicket(ctx context.Context, st *State, step identity.SignInStep, t *ticket, name string, now time.Time) (identity.SignInResult, outcome, error) {
	switch err := s.replay.insert(t, now); {
	case errors.Is(err, errReplay):
		return identity.SignInResult{}, outcomeReplay, err
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	enc := &t.req.Ticket.DecryptedEncPart
	raw, ok, err := pacOf(enc.AuthorizationData)
	if err != nil {
		return identity.SignInResult{}, outcomeLogonFailure, logonFailure("authorization data: %v", err)
	}
	var logon *pac.KerbValidationInfo
	if ok {
		if logon, err = verifyPAC(raw, t.key, enc.CName, enc.AuthTime); err != nil {
			if errors.Is(err, errPACClient) {
				// decision: the mismatch is logged with the client address
				// and no name: RFC 25 §5.4 keeps user names out of log
				// lines, and neither name is bound to a principal yet, nor
				// is one bound to log it, since binding may mint. The logon
				// event carries the ticket's name as presented. Overturned
				// when RFC 25's logon stream, whose records may carry both
				// names, replaces log lines for logon events.
				s.logger.WarnContext(ctx, "identity: a PAC's client information differs from its ticket's", "client", step.Client.String())
			}
			return identity.SignInResult{}, outcomeLogonFailure, logonFailure("PAC: %v", err)
		}
	}
	if logon != nil {
		// decision: a ticket whose PAC verifies and holds logon information
		// is refused ErrUnavailable, uncounted on the throttle, in every
		// realm, since nothing yet checks the PAC's user SID against the
		// record its name maps to (RFC 18 §3.4). Binding it by name would
		// admit a recreated account under a deleted one's name, and a local
		// user's or root's SID, in the installation's domain, is one no PAC
		// carries. Overturned by the PAC's user SID bound to the record.
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: kerberos: the PAC's user SID is not bound to a record yet", vfserr.ErrUnavailable)
	}
	if st.Directory != nil && strings.EqualFold(enc.CRealm, st.Directory.Realm) {
		// RFC 18 §3.4: in the directory's realm, its name in any case, a
		// ticket without a PAC, or whose PAC holds no logon information, is
		// refused before any mapping, so a domain account cannot reach a
		// principal by its name alone.
		return identity.SignInResult{}, outcomeNoPAC, logonFailure("a ticket without a PAC in the directory's realm")
	}
	p, err := s.kerberosPrincipal(ctx, st, name, enc.CName, enc.CRealm)
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		// decision: an unknown user principal is refused, not made anonymous.
		// A Kerberos user the installation has no record of is either a
		// directory misconfiguration or someone outside the organisation the
		// KDC trusts; acting as anonymous would hide the first and admit the
		// second to every share with an everyone grant. Revisit if a
		// deployment is shown to need cross-realm users with no directory,
		// and then as a share-scoped option.
		return identity.SignInResult{}, outcomeLogonFailure, logonFailure("no principal for the ticket's client")
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	switch _, err := s.Account(ctx, p); {
	case errors.Is(err, errDisabled):
		return identity.SignInResult{}, outcomeDisabled, err
	case errors.Is(err, vfserr.ErrLogonFailure):
		return identity.SignInResult{}, outcomeLogonFailure, err
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	ev := identity.Evidence{Name: name, Principal: p, Expires: enc.EndTime}
	if logon != nil {
		// The PAC supplies groups only: the record was read above, and
		// nothing in the PAC enables a principal or changes its record.
		switch ev.SIDs, ev.DenyOnly, err = s.pacSIDs(ctx, logon); {
		case errors.Is(err, vfserr.ErrLogonFailure):
			return identity.SignInResult{}, outcomeLogonFailure, err
		case err != nil:
			return identity.SignInResult{}, outcomeUnavailable, err
		}
	}
	return identity.SignInResult{
		Identity: identity.Identity{
			Proof:      identity.ProofTicket,
			Protection: identity.ProtectionNone,
			Evidence:   ev,
			Client:     step.Client,
		},
		Keys: t.keys,
	}, outcomeOK, nil
}

// kerberosPrincipal maps a ticket's client name, cname in realm and name as
// user@realm, by RFC 18 §4.2, stopping at the first step that answers: (1)
// the krb row for name; (2) in a keytab realm other than the directory's,
// the local user named by the name, compared as user names are, unless it
// is a directory principal or has no uid; (3) the directory's entry under
// the name, minted on first sign-in; (4) ErrUnknownPrincipal. A name with a
// service part matches neither step 2 nor 3, and every realm is compared
// case and all. A store or directory fault is ErrUnavailable.
//
// decision: the directory's realm always skips step 2, as RFC 18 §4.2 has it
// where a PAC is required, the default: a PAC-less ticket there is how a
// domain account named like a local user would reach it (§3.4). The
// directory record carries no require_pac yet, so no deployment can turn the
// requirement off. Overturned when require_pac is read, which then runs step
// 2 there while it is false.
func (s *Server) kerberosPrincipal(ctx context.Context, st *State, name string, cname types.PrincipalName, realm string) (entity.Principal, error) {
	user := cname.PrincipalNameString()
	pr := s.deps.Store.Principals()
	p, err := pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
	switch {
	case err == nil:
		return p, nil
	case !merrs.IsNotFoundError(err):
		return entity.Principal{}, fmt.Errorf("%w: krb row: %w", vfserr.ErrUnavailable, err)
	case strings.Contains(user, "/"):
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	if st.keytabRealm(realm) && (st.Directory == nil || realm != st.Directory.Realm) {
		p, err := pr.ByName(ctx, entity.PrincipalUser, user)
		if err == nil {
			var u entity.User
			if u, err = pr.User(ctx, p); err == nil && has(u.IDs, entity.SchemeUID) && !has(u.IDs, entity.SchemeDir) {
				return p, nil
			}
		}
		if err != nil && !merrs.IsNotFoundError(err) {
			return entity.Principal{}, fmt.Errorf("%w: local user: %w", vfserr.ErrUnavailable, err)
		}
	}
	p, err = s.byKerberosName(ctx, st, user, realm, true)
	if err != nil && !errors.Is(err, vfserr.ErrUnknownPrincipal) && !errors.Is(err, vfserr.ErrUnavailable) {
		err = fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
	}
	return p, err
}

// pacOf is the raw bytes of the PAC a ticket's authorization data carries in
// an AD-IF-RELEVANT element (MS-PAC 3.1.5), and whether it carries one. An
// AD-IF-RELEVANT element that does not decode is an error.
func pacOf(ad types.AuthorizationData) ([]byte, bool, error) {
	for _, e := range ad {
		if e.ADType != adtype.ADIfRelevant {
			continue
		}
		var inner types.AuthorizationData
		if _, err := asn1.Unmarshal(e.ADData, &inner); err != nil {
			return nil, false, err
		}
		for _, in := range inner {
			if in.ADType == adtype.ADWin2KPAC {
				return in.ADData, true, nil
			}
		}
	}
	return nil, false, nil
}
