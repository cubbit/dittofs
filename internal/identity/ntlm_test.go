package identity

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The published NTLMv2 example of MS-NLMP §4.2. The inputs are §4.2.1's
// common values: user "User", domain "Domain", password "Password", the
// server challenge, the client challenge, a zero time and a random session
// key of 0x55 bytes. The NT hash is §4.2.2.1.2's NTOWFv1 of the password;
// the client challenge's AV pairs (MsvAvNbDomainName "Domain",
// MsvAvNbComputerName "Server") and the outputs are §4.2.4's.
var (
	nlmpNTHash          = unhex16("a4f49c406510bdcab6824ee7c30fd852")
	nlmpServerChallenge = [8]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	nlmpFlags           = uint32(0xe28a8233) // §4.2.4's NegotiateFlags, 33 82 8a e2 on the wire
	nlmpRandomKey       = unhex16("55555555555555555555555555555555")

	// §4.2.4.1.3's temp: the NTLMv2_CLIENT_CHALLENGE the proof covers,
	// with the four zero bytes that follow its MsvAvEOL.
	nlmpTemp = unhex("0101000000000000" + // RespType, HiRespType, reserved
		"0000000000000000" + // TimeStamp
		"aaaaaaaaaaaaaaaa" + // ChallengeFromClient
		"00000000" + // Reserved3
		"02000c0044006f006d00610069006e00" + // MsvAvNbDomainName "Domain"
		"01000c00530065007200760065007200" + // MsvAvNbComputerName "Server"
		"00000000" + // MsvAvEOL
		"00000000")

	nlmpNTOWFv2       = unhex16("0c868a403bfd7a93a3001ef22ef02e3f") // §4.2.4.1.1
	nlmpSessionBase   = unhex16("8de40ccadbc14a82f15cb0ad0de95ca3") // §4.2.4.1.2
	nlmpNTProofStr    = unhex16("68cd0ab851e51c96aabc927bebef6a1c") // §4.2.4.2.2
	nlmpEncryptedSKey = unhex("c5dad2544fc9799094ce1ce90bc9d03e")   // §4.2.4.2.3
)

func unhex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func unhex16(s string) [16]byte { return [16]byte(unhex(s)) }

// nlmpResponse is §4.2.4.2.2's NtChallengeResponse: NTProofStr ‖ temp.
func nlmpResponse() []byte { return append(nlmpNTProofStr[:], nlmpTemp...) }

// MS-NLMP §4.2.4's published vectors pass through identity's code: the
// response decodes, NTOWFv2 and the proof match, the session base key is the
// published one and the random session key unwraps from the encrypted one.
// Catches an upper-cased domain, a reversed concatenation and a key derived
// from the wrong input.
func TestNTLMv2PublishedVectors(t *testing.T) {
	prof.Watch(t, prof.Options{})

	proof, blob, avFlags, err := parseNTLMv2(nlmpResponse())
	if err != nil {
		t.Fatalf("parseNTLMv2(§4.2.4.2.2's response): %v", err)
	}
	if proof != nlmpNTProofStr || !bytes.Equal(blob, nlmpTemp) || avFlags != 0 {
		t.Fatalf("parseNTLMv2 = proof %x, blob %x, MsvAvFlags %#x; want %x, %x, 0", proof, blob, avFlags, nlmpNTProofStr, nlmpTemp)
	}

	key := ntowfv2(nlmpNTHash, "User", "Domain")
	if key != nlmpNTOWFv2 {
		t.Fatalf("ntowfv2 = %x, want §4.2.4.1.1's %x", key, nlmpNTOWFv2)
	}
	base, ok := checkProof(key, nlmpServerChallenge, proof, blob)
	if !ok {
		t.Fatal("checkProof refused §4.2.4.2.2's NTProofStr")
	}
	if base != nlmpSessionBase {
		t.Fatalf("session base key = %x, want §4.2.4.1.2's %x", base, nlmpSessionBase)
	}
	exported, err := exportedKey(base, nlmpFlags, nlmpEncryptedSKey)
	if err != nil {
		t.Fatalf("exportedKey(§4.2.4.2.3's encrypted key): %v", err)
	}
	if exported != nlmpRandomKey {
		t.Fatalf("exported session key = %x, want §4.2.1's RandomSessionKey %x", exported, nlmpRandomKey)
	}
}

// NTOWFv2 upper-cases the user and takes the domain as the message names it.
func TestNTOWFv2Case(t *testing.T) {
	prof.Watch(t, prof.Options{})

	if got := ntowfv2(nlmpNTHash, "uSeR", "Domain"); got != nlmpNTOWFv2 {
		t.Errorf("ntowfv2 of the user in another case = %x, want %x", got, nlmpNTOWFv2)
	}
	if got := ntowfv2(nlmpNTHash, "User", "DOMAIN"); got == nlmpNTOWFv2 {
		t.Error("ntowfv2 of the domain upper-cased matched the domain as named; the domain must not be folded")
	}
}

// A proof made under another NT hash or another server challenge fails.
func TestCheckProofRefusesWrongInputs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	wrongHash := nlmpNTHash
	wrongHash[0] ^= 1
	wrongChallenge := nlmpServerChallenge
	wrongChallenge[7] ^= 1
	for _, tc := range []struct {
		name      string
		key       [16]byte
		challenge [8]byte
	}{
		{"wrong password", ntowfv2(wrongHash, "User", "Domain"), nlmpServerChallenge},
		{"wrong domain", ntowfv2(nlmpNTHash, "User", "Other"), nlmpServerChallenge},
		{"wrong server challenge", nlmpNTOWFv2, wrongChallenge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if base, ok := checkProof(tc.key, tc.challenge, nlmpNTProofStr, nlmpTemp); ok || base != ([16]byte{}) {
				t.Errorf("checkProof = %x, %v; want a zero key and false", base, ok)
			}
		})
	}
}

// clientChallenge builds an NTLMv2_CLIENT_CHALLENGE whose AV pairs are pairs,
// each written as given, with no MsvAvEOL added.
func clientChallenge(pairs ...[]byte) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 1, 1
	for _, p := range pairs {
		b = append(b, p...)
	}
	return b
}

// avPair is one AV pair with the length its value has.
func avPair(id uint16, value []byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, id)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(value)))
	return append(b, value...)
}

var avEOL = avPair(0, nil)

func flagsPair(f uint32) []byte { return avPair(6, binary.LittleEndian.AppendUint32(nil, f)) }

// response is a 16-byte NTProofStr of 0xEE ahead of blob.
func response(blob []byte) []byte { return append(bytes.Repeat([]byte{0xEE}, 16), blob...) }

// The decoder classifies a response before any proof is computed: NTLMv1
// and LM are refused as a logon failure, and a response that is not a
// well-formed NTLMv2_RESPONSE (MS-NLMP 2.2.2.7) as invalid.
func TestParseNTLMv2Refusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The AV pair smb2.session.ntlmssp_bug14932 sends: AvId 0xa211 with an
	// AvLen of 0x8108, overrunning the buffer.
	overrun := []byte{0x11, 0xa2, 0x08, 0x81}
	for _, tc := range []struct {
		name string
		resp []byte
		want error
	}{
		{"NTLMv1, 24 bytes", make([]byte, 24), errNTLMv1},
		{"LM only, no NT response", nil, errNTLMv1},
		{"shorter than the client challenge's header", make([]byte, 16+27), errMalformed},
		{"wrong RespType", func() []byte { b := response(clientChallenge(avEOL)); b[16] = 2; return b }(), errMalformed},
		{"wrong HiRespType", func() []byte { b := response(clientChallenge(avEOL)); b[17] = 2; return b }(), errMalformed},
		{"no AV pairs, so no MsvAvEOL", response(clientChallenge()), errMalformed},
		{"pairs ending without MsvAvEOL", response(clientChallenge(avPair(2, []byte("D\x00")))), errMalformed},
		{"a pair overrunning the buffer", response(clientChallenge(overrun)), errMalformed},
		{"MsvAvEOL with a value", response(clientChallenge(avPair(0, []byte{1}))), errMalformed},
		{"MsvAvFlags of 3 bytes", response(clientChallenge(avPair(6, []byte{2, 0, 0}), avEOL)), errMalformed},
		{"MsvAvFlags of 5 bytes", response(clientChallenge(avPair(6, []byte{2, 0, 0, 0, 0}), avEOL)), errMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseNTLMv2(tc.resp)
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseNTLMv2 = %v, want %v", err, tc.want)
			}
		})
	}
	if !errors.Is(errNTLMv1, vfserr.ErrLogonFailure) || !errors.Is(errMalformed, vfserr.ErrInvalid) {
		t.Error("errNTLMv1 must be a logon failure and errMalformed invalid")
	}
}

// MsvAvFlags is read from the response the proof covers, wherever it sits
// before MsvAvEOL, and a second copy adds its bits.
func TestParseNTLMv2ReadsMsvAvFlags(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name string
		blob []byte
		want uint32
	}{
		{"none", clientChallenge(avPair(2, []byte("D\x00")), avEOL), 0},
		{"MIC present", clientChallenge(avPair(2, []byte("D\x00")), flagsPair(avFlagMIC), avEOL), avFlagMIC},
		{"two pairs", clientChallenge(flagsPair(1), flagsPair(avFlagMIC), avEOL), 1 | avFlagMIC},
		{"after MsvAvEOL, unread", append(clientChallenge(avEOL), flagsPair(avFlagMIC)...), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, blob, got, err := parseNTLMv2(response(tc.blob))
			if err != nil {
				t.Fatalf("parseNTLMv2: %v", err)
			}
			if got != tc.want || !bytes.Equal(blob, tc.blob) {
				t.Errorf("parseNTLMv2 = MsvAvFlags %#x, blob %x; want %#x, %x", got, blob, tc.want, tc.blob)
			}
		})
	}
}

// The exported session key is the base key with no key exchange, and an
// RC4 unwrap under it with one. Signing without a key exchange is a logon
// failure, and a key exchange whose encrypted key is not 16 bytes is invalid,
// a malformed message rather than a wrong credential. checkKeyExchange, which
// a sign-in runs before the proof, and exportedKey answer alike.
func TestExportedKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	if got, err := exportedKey(nlmpSessionBase, 0, nil); err != nil || got != nlmpSessionBase {
		t.Errorf("exportedKey with neither SIGN nor KEY_EXCH = %x, %v; want the base key %x", got, err, nlmpSessionBase)
	}
	for _, tc := range []struct {
		name      string
		flags     uint32
		encrypted []byte
		want      error
	}{
		{"key exchange with 15 bytes", 0x40000000, nlmpEncryptedSKey[:15], vfserr.ErrInvalid},
		{"key exchange with 17 bytes", 0x40000000, append(nlmpEncryptedSKey[:16:16], 0), vfserr.ErrInvalid},
		{"key exchange with none", 0x40000000, nil, vfserr.ErrInvalid},
		{"signing without key exchange", 0x10, nlmpEncryptedSKey, vfserr.ErrLogonFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkKeyExchange(tc.flags, tc.encrypted); !errors.Is(err, tc.want) {
				t.Errorf("checkKeyExchange = %v, want %v", err, tc.want)
			}
			got, err := exportedKey(nlmpSessionBase, tc.flags, tc.encrypted)
			if !errors.Is(err, tc.want) || got != ([16]byte{}) {
				t.Errorf("exportedKey = %x, %v; want a zero key and %v", got, err, tc.want)
			}
		})
	}
}

// The wire constants are MS-NLMP's literal values, written here rather than
// read from the code, so a wrong one fails although every fixture built from
// it would agree with the code.
func TestNTLMConstantsMatchSpec(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want uint32
	}{
		{"NTLMSSP_NEGOTIATE_SIGN (2.2.2.5)", ntlmFlagSign, 0x00000010},
		{"NTLMSSP_NEGOTIATE_KEY_EXCH (2.2.2.5)", ntlmFlagKeyExch, 0x40000000},
		{"MsvAvFlags MIC present (2.2.2.1)", avFlagMIC, 0x00000002},
		{"AUTHENTICATE MIC offset (2.2.1.3)", authMICOffset, 72},
		{"AUTHENTICATE MIC size (2.2.1.3)", authMICSize, 16},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
}

// signedAuthenticate returns an AUTHENTICATE of n bytes whose MIC, at its
// fixed offset, is computed here from MS-NLMP 3.2.5.1.2 independently of the
// code under test: HMAC_MD5 keyed by the exported session key over the three
// messages with the MIC zeroed.
func signedAuthenticate(key [16]byte, negotiate, challenge []byte, n int) []byte {
	raw := make([]byte, n)
	copy(raw, "NTLMSSP\x00")
	raw[8] = 3
	for i := authMICOffset + authMICSize; i < n; i++ {
		raw[i] = byte(i)
	}
	concat := bytes.Join([][]byte{negotiate, challenge, raw}, nil)
	mac := hmac.New(md5.New, key[:])
	mac.Write(concat)
	copy(raw[authMICOffset:], mac.Sum(nil))
	return raw
}

// A flagged MIC verifies over the exact bytes of the three messages; a
// changed byte in the MIC or in any message, a zero MIC and one too short to
// be present are each refused. With no MIC flagged, none is needed.
func TestCheckMIC(t *testing.T) {
	prof.Watch(t, prof.Options{})

	key := nlmpRandomKey
	negotiate := []byte("NTLMSSP\x00\x01\x00\x00\x00negotiate")
	challenge := []byte("NTLMSSP\x00\x02\x00\x00\x00challenge")
	signed := func() *identity.NTLMAuthenticate {
		return &identity.NTLMAuthenticate{
			Negotiate: bytes.Clone(negotiate),
			Challenge: bytes.Clone(challenge),
			Raw:       signedAuthenticate(key, negotiate, challenge, 120),
		}
	}

	t.Run("a valid MIC verifies and the message is left unchanged", func(t *testing.T) {
		a := signed()
		before := bytes.Clone(a.Raw)
		if err := checkMIC(key, a, avFlagMIC); err != nil {
			t.Fatalf("checkMIC refused a valid MIC: %v", err)
		}
		if !bytes.Equal(a.Raw, before) {
			t.Error("checkMIC changed the AUTHENTICATE's bytes")
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(a *identity.NTLMAuthenticate)
	}{
		{"a byte of the MIC flipped, the adapter's copy left right", func(a *identity.NTLMAuthenticate) { a.Raw[authMICOffset+5] ^= 0xFF }},
		{"a byte of the AUTHENTICATE flipped", func(a *identity.NTLMAuthenticate) { a.Raw[12] ^= 1 }},
		{"a byte after the MIC flipped", func(a *identity.NTLMAuthenticate) { a.Raw[len(a.Raw)-1] ^= 1 }},
		{"a byte of the NEGOTIATE flipped", func(a *identity.NTLMAuthenticate) { a.Negotiate[9] ^= 1 }},
		{"a byte of the CHALLENGE flipped", func(a *identity.NTLMAuthenticate) { a.Challenge[9] ^= 1 }},
		{"the MIC zeroed", func(a *identity.NTLMAuthenticate) { clear(a.Raw[authMICOffset : authMICOffset+authMICSize]) }},
		{"an AUTHENTICATE of 87 bytes", func(a *identity.NTLMAuthenticate) { a.Raw = a.Raw[:authMICOffset+authMICSize-1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := signed()
			tc.mutate(a)
			if err := checkMIC(key, a, avFlagMIC); !errors.Is(err, vfserr.ErrLogonFailure) {
				t.Errorf("checkMIC = %v, want ErrLogonFailure", err)
			}
		})
	}

	t.Run("no MIC flagged in the response needs none, whatever the adapter's copy says", func(t *testing.T) {
		a := signed()
		clear(a.Raw[authMICOffset : authMICOffset+authMICSize])
		a.Raw = a.Raw[:authMICOffset+1]
		if err := checkMIC(key, a, 0); err != nil {
			t.Errorf("checkMIC with no MIC flagged = %v, want nil", err)
		}
	})
}
