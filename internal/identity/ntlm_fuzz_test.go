package identity

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// ntlmv2Seeds are the fuzz target's seed responses: MS-NLMP §4.2.4's, one
// flagging a MIC, and each refusal class of TestParseNTLMv2Refusals.
func ntlmv2Seeds() [][]byte {
	return [][]byte{
		nlmpResponse(),
		response(clientChallenge(avPair(2, []byte("D\x00")), flagsPair(avFlagMIC), avEOL)),
		response(clientChallenge(flagsPair(1), flagsPair(avFlagMIC), avEOL, avEOL)),
		make([]byte, 24),
		nil,
		response(clientChallenge()),
		response(clientChallenge([]byte{0x11, 0xa2, 0x08, 0x81})),
		response(clientChallenge(avPair(0, []byte{1}))),
		response(clientChallenge(avPair(6, []byte{2, 0, 0}), avEOL)),
		make([]byte, 16+27),
		withByte(response(clientChallenge(avEOL)), 16, 2), // RespType
		withByte(response(clientChallenge(avEOL)), 17, 2), // HiRespType
		response(clientChallenge(avPair(2, []byte("D\x00")))),
		response(clientChallenge(avPair(6, []byte{2, 0, 0, 0, 0}), avEOL)),
	}
}

// withByte returns b with b[i] set to v.
func withByte(b []byte, i int, v byte) []byte {
	b[i] = v
	return b
}

// refWalk is a reference decoder written apart from parseNTLMv2, by offsets
// into the whole response: it reports whether resp is a well-formed NTLMv2
// response and the MsvAvFlags its pairs carry, and whether it is NTLMv1 or
// LM.
func refWalk(resp []byte) (ok, v1 bool, flags uint32) {
	if len(resp) == 0 || len(resp) == 24 {
		return false, true, 0
	}
	if len(resp) < 44 || resp[16] != 1 || resp[17] != 1 {
		return false, false, 0
	}
	for off := 44; off+4 <= len(resp); {
		id := int(resp[off]) | int(resp[off+1])<<8
		n := int(resp[off+2]) | int(resp[off+3])<<8
		end := off + 4 + n
		if end > len(resp) {
			return false, false, 0
		}
		switch {
		case id == 0:
			return n == 0, false, flags
		case id == 6 && n != 4:
			return false, false, 0
		case id == 6:
			v := resp[off+4 : end]
			flags |= uint32(v[0]) | uint32(v[1])<<8 | uint32(v[2])<<16 | uint32(v[3])<<24
		}
		off = end
	}
	return false, false, 0
}

// FuzzNTLMv2Response holds the NTLMv2 response decoder to its invariants: it
// never panics; it accepts exactly the responses the reference walk accepts
// and reads the same MsvAvFlags; an accepted response's proof is its first 16
// bytes and its blob the rest of the same buffer, so nothing it returns lies
// outside resp; and a refusal is errNTLMv1 exactly where the walk says NTLMv1
// or LM, else errMalformed.
func FuzzNTLMv2Response(f *testing.F) {
	for _, s := range ntlmv2Seeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, resp []byte) {
		in := bytes.Clone(resp)
		proof, blob, flags, err := parseNTLMv2(resp)
		ok, v1, wantFlags := refWalk(in)
		if !bytes.Equal(resp, in) {
			t.Fatal("parseNTLMv2 changed its input")
		}
		if (err == nil) != ok {
			t.Fatalf("parseNTLMv2 err = %v, the reference walk accepts = %v", err, ok)
		}
		if err != nil {
			want := errMalformed
			if v1 {
				want = errNTLMv1
			}
			if !errors.Is(err, want) || blob != nil || flags != 0 {
				t.Fatalf("parseNTLMv2 refused with %v, blob %x, MsvAvFlags %#x; want %v, nil, 0", err, blob, flags, want)
			}
			return
		}
		if flags != wantFlags {
			t.Fatalf("MsvAvFlags = %#x, the reference walk reads %#x", flags, wantFlags)
		}
		if !bytes.Equal(proof[:], resp[:16]) {
			t.Fatalf("proof = %x, want the response's first 16 bytes %x", proof, resp[:16])
		}
		if len(blob) != len(resp)-16 || &blob[0] != &resp[16] {
			t.Fatal("blob is not the response's own bytes after NTProofStr")
		}
	})
}

// The decoder allocates nothing: blob is a sub-slice of the response.
func TestParseNTLMv2AllocatesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for i, s := range ntlmv2Seeds() {
		if n := testing.AllocsPerRun(100, func() { _, _, _, _ = parseNTLMv2(s) }); n != 0 {
			t.Errorf("parseNTLMv2(seed %d) allocates %v times per call, want 0", i, n)
		}
	}
}
