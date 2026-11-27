package gss_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// MIT krb5's published MIC token (t_invalid.c's test_cfx_verify_mic): the
// initiator's first token over "message" under the aes128 key
// "1234567887654321", no flags, sequence number 0.
var (
	mitKey = []byte("1234567887654321")
	mitMsg = []byte("message")
	mitMIC = mustHex("040400ffffffffff000000000000000097e9633f9d822b7467948ad0")
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func kerberosContext(t testing.TB, keys vfs.Keys, initiator bool) *gss.Context {
	t.Helper()
	c, err := gss.KerberosContext(vfs.Evidence{}, keys, initiator)
	if err != nil {
		t.Fatalf("KerberosContext = %v", err)
	}
	return c
}

// MIT's published token verifies at the acceptor, and the initiator's first
// MIC over the same message is that token, byte for byte. Catches a wrong key
// usage, a checksum over the wrong bytes, a header laid out otherwise, and a
// first sequence number other than 0.
func TestMICPublishedVector(t *testing.T) {
	acc := kerberosContext(t, vfs.Keys{Session: mitKey}, false)
	if err := acc.VerifyMIC(mitMsg, mitMIC); err != nil {
		t.Errorf("the acceptor's VerifyMIC of MIT's token = %v, want nil", err)
	}
	init := kerberosContext(t, vfs.Keys{Session: mitKey}, true)
	tok, err := init.MIC(mitMsg)
	if err != nil || !bytes.Equal(tok, mitMIC) {
		t.Errorf("the initiator's first MIC = %x, %v; want MIT's token %x", tok, err, mitMIC)
	}
}

// A token altered anywhere is refused, and so is a token of the acceptor's
// direction where the initiator's is expected. Catches a check skipped: the
// checksum compared short, the header left out of it, the filler or token ID
// unread.
func TestVerifyMICRefusesAlteredTokens(t *testing.T) {
	acc := kerberosContext(t, vfs.Keys{Session: mitKey}, false)
	alter := func(i int, b byte) []byte {
		tok := bytes.Clone(mitMIC)
		tok[i] ^= b
		return tok
	}
	reflected, err := acc.MIC(mitMsg)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		msg, tok []byte
	}{
		{"the checksum's last byte", mitMsg, alter(27, 1)},
		{"the token ID", mitMsg, alter(1, 0x01)},
		{"a Wrap token's ID", mitMsg, alter(0, 0x01)},
		{"the SentByAcceptor flag", mitMsg, alter(2, 0x01)},
		{"the Sealed flag", mitMsg, alter(2, 0x02)},
		{"the AcceptorSubkey flag", mitMsg, alter(2, 0x04)},
		{"the filler", mitMsg, alter(5, 0x01)},
		{"the sequence number", mitMsg, alter(15, 0x01)},
		{"the message", []byte("messagf"), mitMIC},
		{"a token cut short", mitMsg, mitMIC[:len(mitMIC)-1]},
		{"a token with a byte more", mitMsg, append(bytes.Clone(mitMIC), 0)},
		{"no token", mitMsg, nil},
		{"the acceptor's own token", mitMsg, reflected},
	}
	// MIT's own check: every byte of the token, one at a time.
	for i := range mitMIC {
		cases = append(cases, struct {
			name     string
			msg, tok []byte
		}{fmt.Sprintf("byte %d plus one", i), mitMsg, alter(i, mitMIC[i]^(mitMIC[i]+1))})
	}
	for _, tc := range cases {
		if err := acc.VerifyMIC(tc.msg, tc.tok); err == nil {
			t.Errorf("VerifyMIC with %s = nil, want refused", tc.name)
		}
	}
}

// For each type the mechanism accepts, a context made through the exchange
// round-trips MIC and VerifyMIC, then Wrap and Unwrap, both ways, under the
// context key RFC 18 §3.1 names, and each side's sequence numbers count up
// from 0 across both kinds of token. Catches a type the mechanism accepts
// that the tokens cannot protect, the two sides keyed apart, a token of the
// sender's own direction accepted, and a sequence number not taken per
// token.
func TestTokensRoundTripPerType(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		subkey types.EncryptionKey
	}{
		{name: "aes256 session key"},
		{name: "aes128 authenticator subkey", subkey: identitytest.NewKey(etypeID.AES128_CTS_HMAC_SHA1_96)},
		{name: "aes256 authenticator subkey", subkey: identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin", Subkey: tc.subkey})
			mech := gss.Kerberos(f.svc, nfsStep)
			out, _, err := mech.Step(t.Context(), firstToken(t, init))
			if err != nil {
				t.Fatalf("Step = %v", err)
			}
			if _, _, err := init.Step(t.Context(), out); err != nil {
				t.Fatalf("the initiator's check of the AP-REP = %v", err)
			}
			for _, side := range []struct {
				name     string
				from, to *gss.Context
			}{{"initiator to acceptor", init.Context(), mech.Context()}, {"acceptor to initiator", mech.Context(), init.Context()}} {
				for seq := range uint64(2) {
					msg := []byte(side.name)
					tok, err := side.from.MIC(msg)
					if err != nil {
						t.Fatalf("%s: MIC = %v", side.name, err)
					}
					if got := binary.BigEndian.Uint64(tok[8:16]); got != seq {
						t.Errorf("%s: token %d's sequence number = %d, want %d", side.name, seq, got, seq)
					}
					if err := side.to.VerifyMIC(msg, tok); err != nil {
						t.Errorf("%s: VerifyMIC = %v, want nil", side.name, err)
					}
					if err := side.from.VerifyMIC(msg, tok); err == nil {
						t.Errorf("%s: the sender's own VerifyMIC of its token = nil, want refused", side.name)
					}
				}
				for seq := uint64(2); seq < 4; seq++ {
					msg := []byte(side.name)
					tok := wrap(t, side.from, msg)
					if got := binary.BigEndian.Uint64(tok[8:16]); got != seq {
						t.Errorf("%s: Wrap token %d's sequence number = %d, want %d", side.name, seq, got, seq)
					}
					if _, err := side.from.Unwrap(bytes.Clone(tok)); err == nil {
						t.Errorf("%s: the sender's own Unwrap of its token = nil, want refused", side.name)
					}
					if got, err := side.to.Unwrap(tok); err != nil || !bytes.Equal(got, msg) {
						t.Errorf("%s: Unwrap = %q, %v; want %q", side.name, got, err, msg)
					}
				}
			}
		})
	}
}

// The flags byte names the sender's side, and, once the AP-REP sent the
// acceptor's subkey, carries AcceptorSubkey on either side's tokens (RFC 4121
// §2). Each side's token verifies at its peer, and a peer that disagrees about
// the subkey refuses it, though the key is the same. MIT's acceptor picks its
// key by the flag. Catches either flag lost on either side, and a receiver
// that leaves the flags unchecked.
func TestMICFlags(t *testing.T) {
	without, with := vfs.Keys{Session: mitKey}, vfs.Keys{Session: mitKey, Subkey: mitKey}
	for _, tc := range []struct {
		name            string
		keys, otherKeys vfs.Keys
		initiator       bool
		want            byte
	}{
		{"initiator", without, with, true, 0x00},
		{"initiator, an acceptor subkey", with, without, true, 0x04},
		{"acceptor", without, with, false, 0x01},
		{"acceptor, its subkey", with, without, false, 0x05},
	} {
		tok, err := kerberosContext(t, tc.keys, tc.initiator).MIC(mitMsg)
		if err != nil {
			t.Fatal(err)
		}
		if tok[2] != tc.want {
			t.Errorf("%s: flags = %#02x, want %#02x", tc.name, tok[2], tc.want)
		}
		if err := kerberosContext(t, tc.keys, !tc.initiator).VerifyMIC(mitMsg, tok); err != nil {
			t.Errorf("%s: the peer's VerifyMIC = %v, want nil", tc.name, err)
		}
		if err := kerberosContext(t, tc.otherKeys, !tc.initiator).VerifyMIC(mitMsg, tok); err == nil {
			t.Errorf("%s: VerifyMIC at a peer that disagrees about the acceptor's subkey = nil, want refused", tc.name)
		}
	}
}

// MIC and VerifyMIC allocate the same bytes per call for a 4 KiB and a
// 1 MiB message, and neither exceeds maxBytesPerCall, which a copy of a
// 4 KiB message already does. Catches a copy of the message. Skipped under
// the race detector, where sync.Pool drops its HMACs and the count varies.
// It counts on one P with the collector held off, as a pool keeps its items
// per P and a collection empties it: a pool refilled mid-count is not a copy
// of the message. Both settings are process-wide, so it must never call
// t.Parallel.
func TestMICAllocatesNoCopy(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// A copy of the smallest message is 4096 bytes per call; the honest
	// calls allocate under 64 bytes: MIC its token, VerifyMIC its checksum.
	const maxBytesPerCall = 1024
	acc := kerberosContext(t, vfs.Keys{Session: bytes.Repeat([]byte{7}, 32)}, false)
	init := kerberosContext(t, vfs.Keys{Session: bytes.Repeat([]byte{7}, 32)}, true)
	// bytesPerOp is the heap bytes per call over a fixed count of calls,
	// rounded down, so a stray allocation elsewhere in the process, smaller
	// than the count, does not change it while a copy of the message does.
	bytesPerOp := func(f func()) int64 {
		const calls = 256
		f() // the HMACs pooled
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range calls {
			f()
		}
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc-before.TotalAlloc) / calls
	}
	got := map[string][2]int64{}
	for i, n := range []int{4 << 10, 1 << 20} {
		msg := make([]byte, n)
		tok, err := init.MIC(msg)
		if err != nil {
			t.Fatal(err)
		}
		mic, verify := got["MIC"], got["VerifyMIC"]
		mic[i] = bytesPerOp(func() { _, _ = acc.MIC(msg) })
		verify[i] = bytesPerOp(func() {
			if err := acc.VerifyMIC(msg, tok); err != nil {
				t.Error(err)
			}
		})
		got["MIC"], got["VerifyMIC"] = mic, verify
	}
	t.Logf("bytes per call at 4 KiB and 1 MiB: %v", got)
	for name, b := range got {
		if b[0] != b[1] {
			t.Errorf("%s allocates %d bytes per call at 4 KiB and %d at 1 MiB, want equal", name, b[0], b[1])
		}

		for i, n := range []string{"4 KiB", "1 MiB"} {
			if b[i] > maxBytesPerCall {
				t.Errorf("%s at %s: %d bytes per call, want <= %d", name, n, b[i], maxBytesPerCall)
			}
		}
	}
}
