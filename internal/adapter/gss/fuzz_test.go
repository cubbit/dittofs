package gss

import (
	"bytes"
	"encoding/asn1"
	"encoding/binary"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// krb5 is the Kerberos V5 mechanism's OID as RFC 4121 §1 writes it.
var krb5 = asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}

// refUnframe is RFC 2743 §3.1's initial context token read with
// encoding/asn1, a decoder written apart from unframe: an [APPLICATION 0]
// constructed value spanning tok, holding a mechanism OID and then the inner
// token, whose first two bytes are RFC 4121 §4.1's token ID.
func refUnframe(tok []byte) (oid asn1.ObjectIdentifier, id uint16, inner []byte, ok bool) {
	var raw asn1.RawValue
	rest, err := asn1.Unmarshal(tok, &raw)
	if err != nil || len(rest) != 0 || raw.Class != asn1.ClassApplication || raw.Tag != 0 || !raw.IsCompound {
		return nil, 0, nil, false
	}
	rest, err = asn1.Unmarshal(raw.Bytes, &oid)
	if err != nil || len(rest) < 2 {
		return nil, 0, nil, false
	}
	return oid, binary.BigEndian.Uint16(rest), rest[2:], true
}

// FuzzKrb5Token holds unframe to the second decoder: it accepts exactly the
// Kerberos tokens of the AP-REQ's token ID the second decoder reads, with the
// same message, and what it accepts frames back to the same bytes. Catches a
// length read wrongly, an OID or token ID unchecked, trailing bytes accepted
// and a non-minimal length that would re-encode otherwise.
func FuzzKrb5Token(f *testing.F) {
	realm := identitytest.NewRealm("TEST.REALM", "nfs/nas.test")
	apReq := realm.Ticket(identitytest.TicketOpts{Service: "nfs/nas.test", Now: time.Unix(0, 0)})
	f.Add(frame(apReq, tokAPReq))
	f.Add(frame(apReq, tokAPRep))
	f.Add(frame(apReq[:100], tokAPReq)) // a one-byte long-form length
	f.Add(frame(nil, tokAPReq))
	f.Add([]byte{0x60, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x82, 0xf7, 0x12, 0x01, 0x02, 0x02, 0x01, 0x00})       // Microsoft's legacy OID
	f.Add(append(frame(apReq, tokAPReq), 0))                                                                      // a trailing byte
	f.Add([]byte{0x60, 0x81, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02, 0x01, 0x00}) // a non-minimal length
	f.Add([]byte{0x60, 0x80, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02, 0x01, 0x00, 0x00, 0x00})
	for _, c := range capturedInits {
		f.Add(capturedInitToken(f, c.name))
	}
	f.Fuzz(func(t *testing.T, tok []byte) {
		inner, err := unframe(tok, tokAPReq)
		oid, id, refInner, ok := refUnframe(tok)
		want := ok && oid.Equal(krb5) && id == tokAPReq
		if (err == nil) != want {
			t.Fatalf("unframe(%x) = %v; the second decoder reads OID %v, token ID %#04x, ok %v", tok, err, oid, id, ok)
		}
		if err != nil {
			return
		}
		if !bytes.Equal(inner, refInner) {
			t.Fatalf("unframe(%x) = %x, the second decoder reads %x", tok, inner, refInner)
		}
		if again := frame(inner, tokAPReq); !bytes.Equal(again, tok) {
			t.Fatalf("unframe(%x) frames back to %x", tok, again)
		}
	})
}
