package identitytest

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"strings"
	"unicode/utf16"

	"golang.org/x/crypto/md4" //nolint:staticcheck // NTOWFv1 is MD4 (MS-NLMP 3.3.1); nothing else here hashes with it
)

// NTLMv2 as MS-NLMP 3.3.2 writes it, for the in-memory domain controller's
// check and the suite's responses. It imports nothing of identity's own NTLM
// code, so the controller is not a copy of what it stands in front of; the
// suite's responses are checked by a real controller, which keeps the two
// halves here honest.

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func hmacMD5(key []byte, parts ...[]byte) []byte {
	m := hmac.New(md5.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// ntowfv2 is NTOWFv2(password, user, domain).
func ntowfv2(password []byte, user, domain string) []byte {
	h := md4.New()
	h.Write(utf16le(string(password)))
	return hmacMD5(h.Sum(nil), utf16le(strings.ToUpper(user)+domain))
}

// checkNTLMv2 checks an NTLMv2 response to challenge against password and
// answers the session base key, or false.
func checkNTLMv2(password []byte, user, domain string, challenge [8]byte, nt []byte) ([]byte, bool) {
	if len(nt) < 16+28 { // NTProofStr and the smallest client blob
		return nil, false
	}
	key := ntowfv2(password, user, domain)
	proof := hmacMD5(key, challenge[:], nt[16:])
	if !hmac.Equal(proof, nt[:16]) {
		return nil, false
	}
	return hmacMD5(key, proof), true
}

// NTLMv2Response answers challenge for user in domain with password, as a
// client does: the NT and LM responses, and the session base key a
// controller derives from them. The blob carries the domain's NetBIOS name
// and a fixed timestamp, a FILETIME.
func NTLMv2Response(password []byte, user, domain string, challenge, client [8]byte) (nt, lm, session []byte) {
	key := ntowfv2(password, user, domain)
	name := utf16le(domain)
	blob := make([]byte, 0, 28+4+len(name)+4+4)
	blob = append(blob, 1, 1, 0, 0, 0, 0, 0, 0)
	blob = binary.LittleEndian.AppendUint64(blob, 0x01dc000000000000)
	blob = append(blob, client[:]...)
	blob = append(blob, 0, 0, 0, 0)
	blob = binary.LittleEndian.AppendUint16(blob, 2) // MsvAvNbDomainName
	blob = binary.LittleEndian.AppendUint16(blob, uint16(len(name)))
	blob = append(blob, name...)
	blob = append(blob, 0, 0, 0, 0, 0, 0, 0, 0) // MsvAvEOL, then the blob's trailing reserved bytes
	proof := hmacMD5(key, challenge[:], blob)
	nt = append(proof, blob...)
	lm = append(hmacMD5(key, challenge[:], client[:]), client[:]...)
	return nt, lm, hmacMD5(key, proof)
}
