// Package identitytest holds test helpers for identity: a Kerberos realm that
// issues tickets as a KDC does.
package identitytest

import (
	"net/netip"
	"strings"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/addrtype"
	"github.com/jcmturner/gokrb5/v8/iana/asnAppTag"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/iana/keyusage"
	"github.com/jcmturner/gokrb5/v8/iana/nametype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
)

// etypes are the encryption types a Realm holds a key of for each service
// name: AES, which its keytab carries, and RC4 and triple DES, which it does
// not, so a test can issue a ticket under a key no keytab may hold.
var etypes = []int32{
	etypeID.AES128_CTS_HMAC_SHA1_96,
	etypeID.AES256_CTS_HMAC_SHA1_96,
	etypeID.RC4_HMAC,
	etypeID.DES3_CBC_SHA1_KD,
}

// Realm is a Kerberos realm for tests: a key per service name and encryption
// type, derived from a fixed password, under key version number 1. It issues
// AP-REQs as a KDC issues the ticket and a client builds the authenticator.
//
// decision: Realm is no double: its tickets are real Kerberos encodings under
// real keys, decrypted by gokrb5 as any KDC's are, and its keytab is the
// format an operator imports. Overturned if a verifier is shown to accept one
// of its tickets that a KDC-issued ticket of the same content would fail.
type Realm struct {
	Name string
	kt   *keytab.Keytab
}

// NewRealm is a realm named name holding a key of each encryption type for
// each service name, such as "cifs/nas.test".
func NewRealm(name string, services ...string) *Realm {
	r := &Realm{Name: name, kt: keytab.New()}
	for _, s := range services {
		for _, et := range etypes {
			if err := r.kt.AddEntry(s, name, s+"-password", time.Unix(1, 0), 1, et); err != nil {
				panic(err) // only an unknown etype fails, and etypes are known
			}
		}
	}
	return r
}

// Key is the realm's key for service under etype.
func (r *Realm) Key(service string, etype int32) types.EncryptionKey {
	k, _, err := r.kt.GetEncryptionKey(principal(service), r.Name, 1, etype)
	if err != nil {
		panic("identitytest: no key for " + service)
	}
	return k
}

// Keytab is the realm's AES keys in MIT's keytab format.
func (r *Realm) Keytab() []byte {
	kt := keytab.New()
	for _, e := range r.kt.Entries {
		if e.Key.KeyType == etypeID.AES128_CTS_HMAC_SHA1_96 || e.Key.KeyType == etypeID.AES256_CTS_HMAC_SHA1_96 {
			kt.Entries = append(kt.Entries, e)
		}
	}
	b, err := kt.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

// TicketOpts is what an AP-REQ carries. Every zero field takes the default
// its comment names.
type TicketOpts struct {
	Client  string    // the ticket's client name, components split on "/"; "alice"
	Service string    // the ticket's service name, as written
	KeyOf   string    // the service name whose key encrypts the ticket; Service
	Etype   int32     // the ticket's encryption type; AES256
	Now     time.Time // the ticket's authentication time
	Start   time.Time // the ticket's start time; none, which reads as Now
	End     time.Time // the ticket's end time; Now plus 10 hours
	// Session is the ticket's session key; a fresh AES256 key.
	Session types.EncryptionKey
	// Subkey is the authenticator's subkey; none.
	Subkey     types.EncryptionKey
	AuthClient string    // the authenticator's client name; Client
	AuthTime   time.Time // the authenticator's time; Now
	Cusec      int       // the authenticator's microseconds
	Addresses  []netip.Addr
	NetBIOS    string // a NetBIOS address the ticket lists after Addresses; none
	Flags      []int  // the ticket flags set, as gokrb5's iana/flags numbers them; none
}

// Ticket is an AP-REQ carrying a ticket under o, its authenticator encrypted
// under the ticket's session key.
func (r *Realm) Ticket(o TicketOpts) []byte {
	if o.Client == "" {
		o.Client = "alice"
	}
	if o.KeyOf == "" {
		o.KeyOf = o.Service
	}
	if o.Etype == 0 {
		o.Etype = etypeID.AES256_CTS_HMAC_SHA1_96
	}
	if o.End.IsZero() {
		o.End = o.Now.Add(10 * time.Hour)
	}
	if o.Session.KeyType == 0 {
		o.Session = mustKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	}
	if o.AuthClient == "" {
		o.AuthClient = o.Client
	}
	if o.AuthTime.IsZero() {
		o.AuthTime = o.Now
	}
	var caddr types.HostAddresses
	for _, a := range o.Addresses {
		caddr = append(caddr, types.HostAddressFromNetIP(a.AsSlice()))
	}
	if o.NetBIOS != "" {
		caddr = append(caddr, types.HostAddress{AddrType: addrtype.NetBios, Address: []byte(o.NetBIOS)})
	}
	tf := types.NewKrbFlags()
	for _, f := range o.Flags {
		types.SetFlag(&tf, f)
	}
	etp := messages.EncTicketPart{
		Flags:     tf,
		Key:       o.Session,
		CRealm:    r.Name,
		CName:     principal(o.Client),
		AuthTime:  o.Now,
		StartTime: o.Start,
		EndTime:   o.End,
		CAddr:     caddr,
	}
	b, err := asn1.Marshal(etp)
	if err != nil {
		panic(err)
	}
	enc, err := crypto.GetEncryptedData(asn1tools.AddASNAppTag(b, asnAppTag.EncTicketPart), r.Key(o.KeyOf, o.Etype), keyusage.KDC_REP_TICKET, 1)
	if err != nil {
		panic(err)
	}
	tkt := messages.Ticket{TktVNO: 5, Realm: r.Name, SName: principal(o.Service), EncPart: enc}
	auth := types.Authenticator{
		AVNO:   5,
		CRealm: r.Name,
		CName:  principal(o.AuthClient),
		Cusec:  o.Cusec,
		CTime:  o.AuthTime,
		SubKey: o.Subkey,
	}
	req, err := messages.NewAPReq(tkt, o.Session, auth)
	if err != nil {
		panic(err)
	}
	out, err := req.Marshal()
	if err != nil {
		panic(err)
	}
	return out
}

// NewKey is a fresh random key of etype.
func NewKey(etype int32) types.EncryptionKey { return mustKey(etype) }

func mustKey(etype int32) types.EncryptionKey {
	et, err := crypto.GetEtype(etype)
	if err != nil {
		panic(err)
	}
	k, err := types.GenerateEncryptionKey(et)
	if err != nil {
		panic(err)
	}
	return k
}

// principal is name as a principal name, its components split on "/": a
// service instance when it has two, else a principal.
func principal(name string) types.PrincipalName {
	parts := strings.Split(name, "/")
	t := int32(nametype.KRB_NT_PRINCIPAL)
	if len(parts) > 1 {
		t = nametype.KRB_NT_SRV_INST
	}
	return types.PrincipalName{NameType: t, NameString: parts}
}
