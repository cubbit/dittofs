package gss_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/crypto/rfc3962"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// MIT krb5's published Wrap tokens (t_invalid.c's test_cfx_unwrap): the
// initiator's first tokens over "message" under the aes128 key
// "1234567887654321", one with integrity only and one sealed.
var (
	mitWrapUnsealed = mustHex("050400ff000c0000" + "0000000000000000" + "6d657373616765df57b95ea2b17331dbce6162")
	mitWrapSealed   = mustHex("050402ff00000000" + "0000000000000000" + "72bbd7cfdeb0f920e29a98a7a4e7c99b30d3fe61512e1b56" +
		"88b78af5a9bf8f82b1ebcc88e63313bf524bc03b243f3ef5f1e064")
)

// wrap seals msg through c.Wrap, in a buffer with the room Wrap reserves.
func wrap(t testing.TB, c *gss.Context, msg []byte) []byte {
	t.Helper()
	buf := make([]byte, gss.WrapHeadroom+len(msg), gss.WrapHeadroom+len(msg)+gss.WrapTailroom)
	copy(buf[gss.WrapHeadroom:], msg)
	tok, err := c.Wrap(buf)
	if err != nil {
		t.Fatalf("Wrap = %v", err)
	}
	return tok
}

// mitSeal is gokrb5's RFC 3961 encryption of plain under MIT's key and the
// initiator's seal usage, after hdr: a token built as t_invalid.c builds its
// malformed ones, by an encryption written apart from gss's.
func mitSeal(t testing.TB, hdr, plain []byte) []byte {
	t.Helper()
	et, err := crypto.GetEtype(etypeID.AES128_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	_, ct, err := et.EncryptMessage(mitKey, plain, 24)
	if err != nil {
		t.Fatal(err)
	}
	return append(bytes.Clone(hdr), ct...)
}

// cfxHeader is t_invalid.c's write_cfx_header: a sealed initiator token's
// header with EC ec, RRC 0 and sequence number 0.
func cfxHeader(ec uint16) []byte {
	h := []byte{0x05, 0x04, 0x02, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(h[4:], ec)
	return h
}

// MIT's published sealed token unwraps at the acceptor to "message". Catches
// a wrong key usage, a confounder, filler or header copy cut otherwise, and
// a decryption that is not RFC 3962's.
func TestUnwrapPublishedVector(t *testing.T) {
	acc := kerberosContext(t, vfs.Keys{Session: mitKey}, false)
	got, err := acc.Unwrap(bytes.Clone(mitWrapSealed))
	if err != nil || !bytes.Equal(got, mitMsg) {
		t.Errorf("the acceptor's Unwrap of MIT's sealed token = %q, %v; want %q", got, err, mitMsg)
	}
}

// The initiator's first Wrap of "message", its confounder drawn as MIT's
// token drew it, is MIT's sealed token byte for byte. The confounder is read
// out of the token by gokrb5's decryption. Catches a Wrap token laid out,
// keyed or checksummed otherwise than RFC 4121's, which a round trip through
// gss's own Unwrap would share.
func TestWrapPublishedVector(t *testing.T) {
	et, err := crypto.GetEtype(etypeID.AES128_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	ke, err := et.DeriveKey(mitKey, common.GetUsageKe(24))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rfc3962.DecryptData(ke, mitWrapSealed[16:len(mitWrapSealed)-12], et)
	if err != nil {
		t.Fatal(err)
	}
	init, err := gss.KerberosContextWithRand(vfs.Evidence{}, vfs.Keys{Session: mitKey}, true, bytes.NewReader(plain[:16]))
	if err != nil {
		t.Fatal(err)
	}
	if tok := wrap(t, init, mitMsg); !bytes.Equal(tok, mitWrapSealed) {
		t.Errorf("the initiator's first Wrap = %x, want MIT's token %x", tok, mitWrapSealed)
	}
}

// A token altered anywhere, or built wrong, is refused. Catches a check
// skipped: the Sealed flag unread, so a token carries a krb5p call's
// arguments in the clear; EC taken from the clear header rather than bound
// to the encrypted copy, so it cuts the message short; EC or the length
// unchecked against the message, so a slice runs out of bounds; the
// checksum, token ID, flags or filler unread.
func TestUnwrapRefusesAlteredTokens(t *testing.T) {
	acc := kerberosContext(t, vfs.Keys{Session: mitKey}, false)
	alter := func(tok []byte, i int, b byte) []byte {
		tok = bytes.Clone(tok)
		tok[i] ^= b
		return tok
	}
	reflected := wrap(t, acc, mitMsg)
	cases := []struct {
		name string
		tok  []byte
	}{
		{"MIT's token with integrity only", mitWrapUnsealed},
		{"the Sealed flag cleared", alter(mitWrapSealed, 2, 0x02)},
		{"the SentByAcceptor flag", alter(mitWrapSealed, 2, 0x01)},
		{"the AcceptorSubkey flag", alter(mitWrapSealed, 2, 0x04)},
		{"the token ID", alter(mitWrapSealed, 1, 0x01)},
		{"a MIC token's ID", alter(mitWrapSealed, 0, 0x01)},
		{"the filler", alter(mitWrapSealed, 3, 0x01)},
		{"EC raised in the clear header", alter(mitWrapSealed, 5, 0x03)},
		{"the sequence number", alter(mitWrapSealed, 15, 0x01)},
		{"the checksum's last byte", alter(mitWrapSealed, len(mitWrapSealed)-1, 0x01)},
		{"the ciphertext's first byte", alter(mitWrapSealed, 16, 0x01)},
		{"a token cut short", mitWrapSealed[:len(mitWrapSealed)-1]},
		{"a header alone", mitWrapSealed[:16]},
		{"no token", nil},
		{"the acceptor's own token", reflected},
		// t_invalid.c's test_cfx_altered_ec: EC 3 in the clear, 0 in the copy.
		{"EC altered against its copy", mitSeal(t, cfxHeader(3), append([]byte("truncate"), cfxHeader(0)...))},
		// test_cfx_short_plaintext: one byte sealed, no header copy.
		{"a plaintext shorter than the header copy", mitSeal(t, cfxHeader(0), []byte{0})},
		// test_cfx_large_ec: EC 5 in both, over a 4-byte message.
		{"EC longer than the message", mitSeal(t, cfxHeader(5), append([]byte("abcd"), cfxHeader(5)...))},
	}
	// MIT's own check: every byte of the token, one at a time. RRC's
	// two bytes are left out: a rotation is not an alteration.
	for i := range mitWrapSealed {
		if i == 6 || i == 7 {
			continue
		}
		cases = append(cases, struct {
			name string
			tok  []byte
		}{fmt.Sprintf("byte %d plus one", i), alter(mitWrapSealed, i, mitWrapSealed[i]^(mitWrapSealed[i]+1))})
	}
	for _, tc := range cases {
		if got, err := acc.Unwrap(bytes.Clone(tc.tok)); err == nil {
			t.Errorf("Unwrap with %s = %q, nil; want refused", tc.name, got)
		}
	}
}

// A token whose body is rotated right by its RRC, as a Windows peer rotates
// it, unwraps to the message, whatever the count. Catches a rotation unread,
// undone the wrong way, or not reduced modulo the body's length.
func TestUnwrapRotated(t *testing.T) {
	acc := kerberosContext(t, vfs.Keys{Session: mitKey}, false)
	init := kerberosContext(t, vfs.Keys{Session: mitKey}, true)
	msg := []byte("a krb5p call's arguments")
	tok := wrap(t, init, msg)
	body := len(tok) - 16
	for _, rrc := range []int{1, 12, 28, body - 1, body, body + 5} {
		rot := bytes.Clone(tok)
		k := rrc % body
		copy(rot[16:], tok[len(tok)-k:])
		copy(rot[16+k:], tok[16:len(tok)-k])
		binary.BigEndian.PutUint16(rot[6:], uint16(rrc))
		if got, err := acc.Unwrap(rot); err != nil || !bytes.Equal(got, msg) {
			t.Errorf("Unwrap with RRC %d = %q, %v; want %q", rrc, got, err, msg)
		}
	}
}

// Wrap refuses a buffer without the room it seals in, rather than writing
// past it or into another array. Catches either reservation unchecked.
func TestWrapNeedsItsRoom(t *testing.T) {
	init := kerberosContext(t, vfs.Keys{Session: mitKey}, true)
	for _, tc := range []struct {
		name     string
		len, cap int
	}{
		{"no headroom", gss.WrapHeadroom - 1, gss.WrapHeadroom - 1 + gss.WrapTailroom},
		{"no tailroom", gss.WrapHeadroom + 8, gss.WrapHeadroom + 8 + gss.WrapTailroom - 1},
	} {
		if _, err := init.Wrap(make([]byte, tc.len, tc.cap)); err == nil {
			t.Errorf("Wrap with %s = nil, want refused", tc.name)
		}
	}
}

// Wrap and Unwrap allocate the same bytes per call for a 4 KiB and a 1 MiB
// message, and Unwrap's message lies in the token it was given. Catches a
// copy of the message. Skipped under the race detector, where sync.Pool
// drops its HMACs and the count varies. It counts on one P with the
// collector held off, as a pool keeps its items per P and a collection
// empties it: a pool refilled mid-count is not a copy of the message.
func TestWrapAllocatesNoCopy(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	key := bytes.Repeat([]byte{7}, 32)
	acc := kerberosContext(t, vfs.Keys{Session: key}, false)
	init := kerberosContext(t, vfs.Keys{Session: key}, true)
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
		buf := make([]byte, gss.WrapHeadroom+n, gss.WrapHeadroom+n+gss.WrapTailroom)
		tok := wrap(t, init, make([]byte, n))
		in := make([]byte, len(tok))
		w, u := got["Wrap"], got["Unwrap"]
		w[i] = bytesPerOp(func() {
			if _, err := acc.Wrap(buf[:gss.WrapHeadroom+n]); err != nil {
				t.Error(err)
			}
		})
		u[i] = bytesPerOp(func() {
			copy(in, tok)
			msg, err := acc.Unwrap(in)
			if err != nil || len(msg) != n || &msg[0] != &in[gss.WrapHeadroom] {
				t.Errorf("Unwrap = %d bytes, %v; want the %d-byte message in place", len(msg), err, n)
			}
		})
		got["Wrap"], got["Unwrap"] = w, u
	}
	t.Logf("bytes per call at 4 KiB and 1 MiB: %v", got)
	for name, b := range got {
		if b[0] != b[1] {
			t.Errorf("%s allocates %d bytes per call at 4 KiB and %d at 1 MiB, want equal", name, b[0], b[1])
		}
	}
}

// golden is a capture's cut file: hex words, '#' lines its provenance.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "nfs", "wire", "testdata", "golden", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	var h strings.Builder
	for line := range strings.Lines(string(raw)) {
		if !strings.HasPrefix(line, "#") {
			h.WriteString(strings.Join(strings.Fields(line), ""))
		}
	}
	return mustHex(h.String())
}

// xdrReader reads RFC 4506's unsigned integers and variable-length opaques.
type xdrReader struct {
	t *testing.T
	b []byte
}

func (r *xdrReader) u32() uint32 {
	r.t.Helper()
	if len(r.b) < 4 {
		r.t.Fatalf("an XDR integer past the message's end")
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *xdrReader) opaque() []byte {
	r.t.Helper()
	n := int(r.u32())
	pad := (n + 3) &^ 3
	if len(r.b) < pad {
		r.t.Fatalf("an XDR opaque of %d bytes past the message's end", n)
	}
	v := r.b[:n]
	r.b = r.b[pad:]
	return v
}

// callBody splits an RPC call (RFC 5531 §9) into its RPCSEC_GSS credential's
// body and what follows the verifier.
func callBody(t *testing.T, msg []byte) (cred, body []byte) {
	r := &xdrReader{t: t, b: msg}
	for range 6 { // xid, CALL, rpcvers, prog, vers, proc
		r.u32()
	}
	if f := r.u32(); f != 6 {
		t.Fatalf("a call's credential flavour = %d, want RPCSEC_GSS's 6", f)
	}
	cred = r.opaque()
	r.u32()
	r.opaque()
	return cred, r.b
}

// replyBody is what follows an accepted RPC reply's verifier and status.
func replyBody(t *testing.T, msg []byte) []byte {
	r := &xdrReader{t: t, b: msg}
	r.u32() // xid
	if r.u32() != 1 || r.u32() != 0 {
		t.Fatal("not an accepted reply")
	}
	r.u32()
	r.opaque()
	if s := r.u32(); s != 0 {
		t.Fatalf("a reply's accept_stat = %d, want SUCCESS", s)
	}
	return r.b
}

// captureContextKey is the context key of a captured INIT: the ticket in its
// AP-REQ decrypted under the capture's committed keytab, then its
// authenticator under the ticket's session key, whose subkey is the context
// key, as knfsd sends no AP-REP (RFC 18 §3.1's choice, the capture's
// CAPTURE.md).
func captureContextKey(t *testing.T, initCall []byte) []byte {
	t.Helper()
	cred, body := callBody(t, initCall)
	if proc := binary.BigEndian.Uint32(cred[4:]); proc != 1 {
		t.Fatalf("gss_proc = %d, want INIT", proc)
	}
	tok := (&xdrReader{t: t, b: body}).opaque()
	// RFC 2743 §3.1's framing: 0x60, a DER length, the Kerberos OID, then
	// RFC 4121 §4.1's AP-REQ token ID.
	n := 1
	if tok[1]&0x80 != 0 {
		n += int(tok[1] & 0x7f)
	}
	inner := tok[2+n-1+11:]
	if binary.BigEndian.Uint16(inner) != 0x0100 {
		t.Fatalf("the INIT token's token ID = %#04x, want an AP-REQ's", binary.BigEndian.Uint16(inner))
	}
	var req messages.APReq
	if err := req.Unmarshal(inner[2:]); err != nil {
		t.Fatal(err)
	}
	kt, err := keytab.Load(filepath.Join("..", "..", "..", "test", "pcap-corpus", "nfs-gss", "nfs.keytab"))
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Ticket.DecryptEncPart(kt, nil); err != nil {
		t.Fatalf("the captured ticket under the committed keytab: %v", err)
	}
	if err := req.DecryptAuthenticator(req.Ticket.DecryptedEncPart.Key); err != nil {
		t.Fatal(err)
	}
	if k := req.Authenticator.SubKey; k.KeyType != etypeID.AES256_CTS_HMAC_SHA1_96 {
		t.Fatalf("the authenticator's subkey is of type %d, want aes256-cts-hmac-sha1-96's", k.KeyType)
	}
	return req.Authenticator.SubKey.KeyValue
}

// The Linux client's krb5p WRITE calls in knfsd's capture unwrap at the
// acceptor under the context key read from the INIT beside each, and knfsd's
// sealed replies unwrap at the initiator; each body starts with the
// credential's sequence number, as RFC 2203 §5.3.2.2 has rpc_gss_data_t.
// Catches a token Linux or knfsd sends that gss refuses: a rotation,
// filler or flag laid out otherwise than gss expects, and a context key
// chosen otherwise.
func TestUnwrapCapturedTokens(t *testing.T) {
	for _, mount := range []string{"knfsd-nfs3-gss3-krb5p", "knfsd-nfs4-gss40-krb5p", "knfsd-nfs4-gss41-krb5p"} {
		t.Run(mount, func(t *testing.T) {
			key := captureContextKey(t, golden(t, mount+"-init-call"))
			acc := kerberosContext(t, vfs.Keys{Session: key}, false)
			init := kerberosContext(t, vfs.Keys{Session: key}, true)

			cred, body := callBody(t, golden(t, mount+"-write-call"))
			if svc := binary.BigEndian.Uint32(cred[12:]); svc != 3 {
				t.Fatalf("the call's service = %d, want privacy", svc)
			}
			seq := cred[8:12]
			for _, side := range []struct {
				name string
				c    *gss.Context
				body []byte
			}{
				{"the client's call at the acceptor", acc, body},
				{"knfsd's reply at the initiator", init, replyBody(t, golden(t, mount+"-write-reply"))},
			} {
				tok := (&xdrReader{t: t, b: side.body}).opaque()
				got, err := side.c.Unwrap(bytes.Clone(tok))
				if err != nil {
					t.Errorf("%s: Unwrap = %v, want nil", side.name, err)
					continue
				}
				if len(got) < 4 || !bytes.Equal(got[:4], seq) {
					t.Errorf("%s: the body starts %x, want the credential's sequence number %x", side.name, got[:min(4, len(got))], seq)
				}
			}
		})
	}
}
