package identity

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// NTLMv2's server-side cryptography (MS-NLMP 3.3.2): the response's decoder,
// the response key, the proof check, the exported session key and the MIC.
// Each function reads only its arguments.

const (
	ntlmFlagSign    = 0x00000010 // NTLMSSP_NEGOTIATE_SIGN
	ntlmFlagKeyExch = 0x40000000 // NTLMSSP_NEGOTIATE_KEY_EXCH

	avFlagMIC = 0x00000002 // MsvAvFlags: the AUTHENTICATE carries a MIC

	// The AUTHENTICATE's MIC sits after the 64-byte header and the 8-byte
	// Version (MS-NLMP 2.2.1.3).
	authMICOffset = 72
	authMICSize   = 16
)

var (
	// errNTLMv1 is an NtChallengeResponse of 24 bytes (NTLMv1) or of none
	// (LM only): refused before any check.
	errNTLMv1 = fmt.Errorf("%w: NTLMv1 or LM response", vfserr.ErrLogonFailure)
	// errMalformed is a response that is not a well-formed NTLMv2_RESPONSE
	// (MS-NLMP 2.2.2.7), decided before the proof is computed.
	errMalformed = fmt.Errorf("%w: malformed NTLMv2 response", vfserr.ErrInvalid)
)

// parseNTLMv2 splits an NTLMv2_RESPONSE into its NTProofStr and the
// NTLMv2_CLIENT_CHALLENGE the proof covers, walking the client challenge's
// AV pairs to its MsvAvEOL and returning the MsvAvFlags it passes. blob is a
// sub-slice of resp.
//
// The header's RespType and HiRespType must be 1; its reserved fields are not
// checked, since Windows clients have been seen to set them. A pair that
// overruns the buffer, a list with no MsvAvEOL, an MsvAvEOL with a value and
// an MsvAvFlags whose value is not 4 bytes are each errMalformed.
func parseNTLMv2(resp []byte) (proof [16]byte, blob []byte, avFlags uint32, err error) {
	const (
		header    = 28 // RespType through Reserved3
		pairHead  = 4  // AvId, AvLen
		avEOL     = 0x0000
		avIDFlags = 0x0006
	)
	if len(resp) == 0 || len(resp) == 24 {
		return proof, nil, 0, errNTLMv1
	}
	if len(resp) < len(proof)+header {
		return proof, nil, 0, errMalformed
	}
	copy(proof[:], resp)
	blob = resp[len(proof):]
	if blob[0] != 1 || blob[1] != 1 {
		return proof, nil, 0, errMalformed
	}
	for pairs := blob[header:]; ; {
		if len(pairs) < pairHead {
			return proof, nil, 0, errMalformed
		}
		id := binary.LittleEndian.Uint16(pairs)
		n := int(binary.LittleEndian.Uint16(pairs[2:]))
		if n > len(pairs)-pairHead {
			return proof, nil, 0, errMalformed
		}
		value := pairs[pairHead : pairHead+n]
		pairs = pairs[pairHead+n:]
		switch id {
		case avEOL:
			if n != 0 {
				return proof, nil, 0, errMalformed
			}
			return proof, blob, avFlags, nil
		case avIDFlags:
			if n != 4 {
				return proof, nil, 0, errMalformed
			}
			// A second MsvAvFlags adds its bits, so no pair can hide the
			// MIC flag another one sets.
			avFlags |= binary.LittleEndian.Uint32(value)
		}
	}
}

// ntowfv2 is the NTLMv2 response key, HMAC_MD5 keyed by the NT hash over
// UNICODE(Uppercase(user) ‖ domain): the user is upper-cased, the domain is
// taken as the message names it.
func ntowfv2(ntHash [16]byte, user, domain string) [16]byte {
	units := utf16.Encode([]rune(strings.ToUpper(user) + domain))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	var out [16]byte
	mac := hmac.New(md5.New, ntHash[:])
	mac.Write(b)
	mac.Sum(out[:0])
	return out
}

// checkProof recomputes NTProofStr, HMAC_MD5 keyed by the response key over
// the server's challenge ‖ blob, compares it with proof in constant time and,
// when they match, returns the session base key, HMAC_MD5 keyed by the
// response key over NTProofStr.
func checkProof(key [16]byte, challenge [8]byte, proof [16]byte, blob []byte) (base [16]byte, ok bool) {
	var want [16]byte
	mac := hmac.New(md5.New, key[:])
	mac.Write(challenge[:])
	mac.Write(blob)
	mac.Sum(want[:0])
	if !hmac.Equal(want[:], proof[:]) {
		return base, false
	}
	mac.Reset()
	mac.Write(proof[:])
	mac.Sum(base[:0])
	return base, true
}

// checkKeyExchange classifies the message's key exchange from its flags and
// EncryptedRandomSessionKey alone, so a sign-in runs it before the proof is
// computed and its answer never tells a right password from a wrong one.
// Signing asked for without a key exchange is vfserr.ErrLogonFailure; a key
// exchange whose encrypted key is not 16 bytes is a malformed message,
// vfserr.ErrInvalid, as errMalformed is.
func checkKeyExchange(flags uint32, encrypted []byte) error {
	if flags&ntlmFlagKeyExch == 0 {
		// decision: MS-NLMP 3.1.5.1.2 still signs with the session base key when
		// no key exchange is negotiated; identity refuses that when SIGN is set.
		// A message with neither SIGN nor KEY_EXCH still gets the base key as its
		// session key (RFC 18 3.5), and SMB2 signs and encrypts with it whatever
		// the SIGN bit says (MS-SMB2 3.3.5.5.3). Refuse every message without
		// KEY_EXCH, changing RFC 18 3.5, once no client that must sign in is
		// found to omit it.
		if flags&ntlmFlagSign != 0 {
			return fmt.Errorf("%w: signing without key exchange", vfserr.ErrLogonFailure)
		}
		return nil
	}
	if len(encrypted) != 16 {
		return fmt.Errorf("%w: encrypted session key of %d bytes", vfserr.ErrInvalid, len(encrypted))
	}
	return nil
}

// exportedKey is the session key the client and server sign with: the RC4
// unwrap of EncryptedRandomSessionKey under the session base key when flags
// carry NTLMSSP_NEGOTIATE_KEY_EXCH, else the base key (MS-NLMP 3.1.5.1.2).
// It refuses what checkKeyExchange refuses, so it never unwraps a key of the
// wrong length.
func exportedKey(base [16]byte, flags uint32, encrypted []byte) ([16]byte, error) {
	var out [16]byte
	if err := checkKeyExchange(flags, encrypted); err != nil {
		return out, err
	}
	if flags&ntlmFlagKeyExch == 0 {
		return base, nil
	}
	c, _ := rc4.NewCipher(base[:]) // a 16-byte key never fails
	c.XORKeyStream(out[:], encrypted)
	return out, nil
}

// checkMIC verifies the AUTHENTICATE's MIC, HMAC_MD5 keyed by the exported
// session key over the NEGOTIATE, CHALLENGE and AUTHENTICATE bytes with the
// MIC zeroed (MS-NLMP 3.2.5.1.2), when avFlags marks one present. A flagged
// MIC that is missing, zero or wrong is vfserr.ErrLogonFailure.
//
// decision: avFlags is the MsvAvFlags parseNTLMv2 read from the response,
// which NTProofStr authenticates; NTLMAuthenticate carries no decoded copy
// of them, so none can be trusted in their place. Reading the flags where the proof covers them is what
// defeats a relay that strips the MIC and the flag announcing it. Overturned
// only if the adapter's copy is ever proven to be the authenticated one.
//
// decision: the MIC is read from a.Raw at its fixed offset, and
// NTLMAuthenticate carries no decoded copy, so no second copy can disagree with the bytes the HMAC covers. A Raw too
// short to hold one is a missing MIC. With no MIC flagged, those bytes are
// not read: a client that sends none may put payload there. Overturned if a
// client is found that flags a MIC but places it elsewhere.
func checkMIC(key [16]byte, a *identity.NTLMAuthenticate, avFlags uint32) error {
	if avFlags&avFlagMIC == 0 {
		return nil
	}
	if len(a.Raw) < authMICOffset+authMICSize {
		return fmt.Errorf("%w: MIC flagged but missing", vfserr.ErrLogonFailure)
	}
	// A zero MIC, a client's sign that it sent none, fails the compare below
	// as any wrong MIC does.
	mic := a.Raw[authMICOffset : authMICOffset+authMICSize]
	var zero, want [authMICSize]byte
	mac := hmac.New(md5.New, key[:])
	mac.Write(a.Negotiate)
	mac.Write(a.Challenge)
	mac.Write(a.Raw[:authMICOffset])
	mac.Write(zero[:])
	mac.Write(a.Raw[authMICOffset+authMICSize:])
	mac.Sum(want[:0])
	if !hmac.Equal(mic, want[:]) {
		return fmt.Errorf("%w: MIC does not verify", vfserr.ErrLogonFailure)
	}
	return nil
}
