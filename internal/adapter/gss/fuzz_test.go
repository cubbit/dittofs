package gss

import (
	"bytes"
	"crypto/hmac"
	"encoding/asn1"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
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

// refParseMIC reads RFC 4121 §4.2.6.1's MIC token as the RFC lays it out,
// apart from parseMIC: octets 0-1 TOK_ID 04 04, octet 2 Flags, octets 3-7
// Filler FF, octets 8-15 SND_SEQ big-endian, then SGN_CKSUM, 12 octets for
// the two aes*-cts-hmac-sha1-96 types.
func refParseMIC(tok []byte) (flags byte, seq uint64, sum []byte, ok bool) {
	if len(tok) != 28 || tok[0] != 0x04 || tok[1] != 0x04 || !bytes.Equal(tok[3:8], []byte{0xff, 0xff, 0xff, 0xff, 0xff}) {
		return 0, 0, nil, false
	}
	for _, b := range tok[8:16] {
		seq = seq<<8 | uint64(b)
	}
	return tok[2], seq, tok[16:], true
}

// FuzzMICToken holds parseMIC to the second parser and VerifyMIC to an
// independent checksum: parseMIC accepts exactly what the second parser
// reads, with the same fields, which re-encode to the token's bytes; and an
// acceptor's VerifyMIC accepts exactly the tokens the second parser reads
// whose checksum is gokrb5's own HMAC-SHA1-96 under the initiator's sign
// usage over the message and header. Catches a field read from the wrong
// octets, a length or filler unchecked, a checksum over other bytes or
// compared short, and a read past the token.
func FuzzMICToken(f *testing.F) {
	key := []byte("1234567887654321")
	acc, err := KerberosContext(vfs.Evidence{}, vfs.Keys{Session: key}, false)
	if err != nil {
		f.Fatal(err)
	}
	init, err := KerberosContext(vfs.Evidence{}, vfs.Keys{Session: key}, true)
	if err != nil {
		f.Fatal(err)
	}
	// MIT krb5's published token over "message", from t_invalid.c.
	f.Add([]byte("message"), []byte("\x04\x04\x00\xff\xff\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x97\xe9\x63\x3f\x9d\x82\x2b\x74\x67\x94\x8a\xd0"))
	f.Add([]byte("message"), []byte("\x04\x04\x00\xff\xff\xfe\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x97\xe9\x63\x3f\x9d\x82\x2b\x74\x67\x94\x8a\xd0")) // its filler altered
	f.Add([]byte("message"), []byte("\x05\x04\x00\xff\xff\xff\xff\xff\x00\x00\x00\x00\x00\x00\x00\x00\x97\xe9\x63\x3f\x9d\x82\x2b\x74\x67\x94\x8a\xd0")) // a Wrap token's ID
	for _, msg := range [][]byte{nil, []byte("a call's header"), bytes.Repeat([]byte{0x5a}, 1000)} {
		tok, err := init.MIC(msg)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(msg, tok)
		reflected, err := acc.MIC(msg)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(msg, reflected)
	}
	f.Fuzz(func(t *testing.T, msg, tok []byte) {
		flags, seq, sum, err := parseMIC(tok)
		rflags, rseq, rsum, ok := refParseMIC(tok)
		if (err == nil) != ok {
			t.Fatalf("parseMIC(%x) = %v; the second parser reads it: %v", tok, err, ok)
		}
		if err == nil {
			if flags != rflags || seq != rseq || !bytes.Equal(sum, rsum) {
				t.Fatalf("parseMIC(%x) = flags %#02x, seq %d, sum %x; the second parser reads %#02x, %d, %x", tok, flags, seq, sum, rflags, rseq, rsum)
			}
			if again := append(appendMICHeader(nil, flags, seq), sum...); !bytes.Equal(again, tok) {
				t.Fatalf("parseMIC(%x) re-encodes to %x", tok, again)
			}
		}
		want := false
		if ok {
			ref, err := common.GetChecksumHash(append(bytes.Clone(msg), tok[:16]...), key, usageInitiatorSign, crypto.Aes128CtsHmacSha96{})
			if err != nil {
				t.Fatal(err)
			}
			want = hmac.Equal(ref, tok[16:])
		}
		if err := acc.VerifyMIC(msg, tok); (err == nil) != want {
			t.Fatalf("VerifyMIC(%x, %x) = %v; the independent checksum verifies: %v", msg, tok, err, want)
		}
	})
}

// refParseWrap reads RFC 4121 §4.2.6.2's Wrap token header as the RFC lays
// it out, apart from parseWrap: octets 0-1 TOK_ID 05 04, octet 2 Flags,
// octet 3 Filler FF, octets 4-5 EC and 6-7 RRC big-endian, octets 8-15
// SND_SEQ big-endian.
func refParseWrap(tok []byte) (flags byte, ec, rrc uint16, seq uint64, ok bool) {
	if len(tok) < 16 || tok[0] != 0x05 || tok[1] != 0x04 || tok[3] != 0xff {
		return 0, 0, 0, 0, false
	}
	for _, b := range tok[8:16] {
		seq = seq<<8 | uint64(b)
	}
	return tok[2], uint16(tok[4])<<8 | uint16(tok[5]), uint16(tok[6])<<8 | uint16(tok[7]), seq, true
}

// refUnwrap is RFC 4121 §4.2.4's sealed token opened by gokrb5's RFC 3961
// decryption, apart from unwrap: the body rotated back by RRC, decrypted
// and checked under the initiator's seal usage, the header's copy matching
// the clear header but for RRC, and the message what precedes EC bytes of
// filler and the copy.
func refUnwrap(key, tok []byte) ([]byte, bool) {
	flags, ec, rrc, _, ok := refParseWrap(tok)
	if !ok || flags&0x02 == 0 || flags&0x01 != 0 || len(tok) < 16+16+16+12 {
		return nil, false
	}
	body := tok[16:]
	k := int(rrc) % len(body)
	rot := append(bytes.Clone(body[k:]), body[:k]...)
	plain, err := crypto.DecryptMessage(rot, types.EncryptionKey{KeyType: etypeID.AES128_CTS_HMAC_SHA1_96, KeyValue: key}, 24)
	if err != nil || len(plain) < 16+int(ec) {
		return nil, false
	}
	hdrCopy := plain[len(plain)-16:]
	if !bytes.Equal(hdrCopy[:6], tok[:6]) || !bytes.Equal(hdrCopy[8:], tok[8:16]) {
		return nil, false
	}
	return plain[:len(plain)-16-int(ec)], true
}

// FuzzWrapToken holds parseWrap to the second parser and unwrap to gokrb5's
// decryption: parseWrap accepts exactly the headers the second parser reads,
// with the same fields, which re-encode to the header's bytes; and an
// acceptor's Unwrap accepts exactly the tokens refUnwrap opens, to the same
// message. Catches a field read from the wrong octets, the Sealed flag, a
// length, EC or the header copy unchecked, a rotation undone otherwise, and
// a read past the token.
func FuzzWrapToken(f *testing.F) {
	key := []byte("1234567887654321")
	acc, err := KerberosContext(vfs.Evidence{}, vfs.Keys{Session: key}, false)
	if err != nil {
		f.Fatal(err)
	}
	init, err := KerberosContext(vfs.Evidence{}, vfs.Keys{Session: key}, true)
	if err != nil {
		f.Fatal(err)
	}
	seal := func(c *Context, msg []byte) []byte {
		buf := make([]byte, WrapHeadroom+len(msg), WrapHeadroom+len(msg)+WrapTailroom)
		copy(buf[WrapHeadroom:], msg)
		tok, err := c.Wrap(buf)
		if err != nil {
			f.Fatal(err)
		}
		return tok
	}
	// MIT krb5's published tokens over "message", from t_invalid.c: one
	// sealed, one with integrity only.
	f.Add([]byte("\x05\x04\x02\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x72\xbb\xd7\xcf\xde\xb0\xf9\x20\xe2\x9a\x98\xa7\xa4\xe7\xc9\x9b\x30\xd3\xfe\x61\x51\x2e\x1b\x56\x88\xb7\x8a\xf5\xa9\xbf\x8f\x82\xb1\xeb\xcc\x88\xe6\x33\x13\xbf\x52\x4b\xc0\x3b\x24\x3f\x3e\xf5\xf1\xe0\x64"))
	f.Add([]byte("\x05\x04\x00\xff\x00\x0c\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x6d\x65\x73\x73\x61\x67\x65\xdf\x57\xb9\x5e\xa2\xb1\x73\x31\xdb\xce\x61\x62"))
	f.Add([]byte("\x05\x04\x02\xfe\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")) // a header whose filler is altered
	f.Add([]byte("\x04\x04\x02\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")) // a MIC token's ID
	f.Add([]byte("\x05\x04\x02\xff\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))     // a header cut short
	for _, msg := range [][]byte{nil, []byte("a krb5p call's arguments"), bytes.Repeat([]byte{0x5a}, 1000)} {
		tok := seal(init, msg)
		f.Add(tok)
		rotated := bytes.Clone(tok)
		body := rotated[16:]
		k := 28 % len(body)
		copy(body, append(bytes.Clone(tok[len(tok)-k:]), tok[16:len(tok)-k]...))
		binary.BigEndian.PutUint16(rotated[6:], 28)
		f.Add(rotated)
		f.Add(seal(acc, msg))
	}
	f.Fuzz(func(t *testing.T, tok []byte) {
		flags, ec, rrc, seq, err := parseWrap(tok)
		rflags, rec, rrrc, rseq, ok := refParseWrap(tok)
		if (err == nil) != ok {
			t.Fatalf("parseWrap(%x) = %v; the second parser reads it: %v", tok, err, ok)
		}
		if err == nil {
			if flags != rflags || ec != rec || rrc != rrrc || seq != rseq {
				t.Fatalf("parseWrap(%x) = %#02x, %d, %d, %d; the second parser reads %#02x, %d, %d, %d", tok, flags, ec, rrc, seq, rflags, rec, rrrc, rseq)
			}
			if again := appendWrapHeader(nil, flags, ec, rrc, seq); !bytes.Equal(again, tok[:16]) {
				t.Fatalf("parseWrap(%x) re-encodes to %x", tok, again)
			}
		}
		want, wantOK := refUnwrap(key, tok)
		got, err := acc.Unwrap(bytes.Clone(tok))
		if (err == nil) != wantOK || !bytes.Equal(got, want) {
			t.Fatalf("Unwrap(%x) = %x, %v; gokrb5 opens it to %x, %v", tok, got, err, want, wantOK)
		}
	})
}
