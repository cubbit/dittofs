package gss

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// captureKeytab is the service's keytab committed with knfsd's capture.
const captureKeytab = "../../../test/pcap-corpus/nfs-gss/nfs.keytab"

// The keys tshark 4.6.4 reads in each captured INIT's AP-REQ, decrypted with
// the committed keytab: every ticket's session key, the same for the one
// ticket the user's principal holds, and each authenticator's subkey.
const capturedTicketKey = "67ddc03452028ad534877bc7ba17287d21c81acce7f4c00497c0187ed7aca84a"

var capturedSubkeys = map[string]string{
	"knfsd-nfs3-gss3-krb5-init":   "6ed5743cb72f2ffc5b01f925c39c2f199dd5f20785e1ea1788a2bc27e7b77b47",
	"knfsd-nfs3-gss3-krb5i-init":  "77678365d7e54044af08e297bee67d7c8b36db0533406ed33ea1a43745bfe9fe",
	"knfsd-nfs3-gss3-krb5p-init":  "1b83f0db8e1553b01734166dc9a4d0e6f3418a4f1eb6eb0c635ea20858ab24d5",
	"knfsd-nfs4-gss40-krb5-init":  "6cc7a8adc08623e03542d9d1e6657e3f0706123667a226620826309995735821",
	"knfsd-nfs4-gss40-krb5i-init": "f16fe1ac5ccec11d5c8b375aba6bff05cd2eedf2461f70d5d2afb1c22fe58e3a",
	"knfsd-nfs4-gss40-krb5p-init": "c0908a695427ff7c696d7220b3cf1f36fb50993609afe8c980a0f9c82040d340",
	"knfsd-nfs4-gss41-krb5-init":  "4712fc836948585ae48372fb9f938a6bc9c474de6f1c76f54fe1c8e9390473ba",
	"knfsd-nfs4-gss41-krb5i-init": "887f5f9d2780dae11b314193321bc08411fc76287fd3d6f505d1c5114d30d8f1",
	"knfsd-nfs4-gss41-krb5p-init": "7d2e05c8a09a1b4bcf2caca5776d4e272dd4af1001a8437c975eb7764b2bf960",
}

// capturedContextKey is the context key of the captured INIT name, chosen as
// RFC 18 §3.1 has it from what the capture holds: the acceptor's subkey when
// knfsd's AP-REP sends one, else the authenticator's subkey, else the
// ticket's session key. The ticket decrypts under the committed keytab.
func capturedContextKey(t testing.TB, name string) vfs.Keys {
	t.Helper()
	b, err := os.ReadFile(captureKeytab)
	if err != nil {
		t.Fatal(err)
	}
	kt := keytab.New()
	if err := kt.Unmarshal(b); err != nil {
		t.Fatalf("the capture's keytab: %v", err)
	}
	apReq, err := unframe(capturedInitToken(t, name), tokAPReq)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var req messages.APReq
	if err := req.Unmarshal(apReq); err != nil {
		t.Fatalf("%s: AP-REQ: %v", name, err)
	}
	if err := req.Ticket.DecryptEncPart(kt, &req.Ticket.SName); err != nil {
		t.Fatalf("%s: the ticket does not decrypt under the capture's keytab: %v", name, err)
	}
	session := req.Ticket.DecryptedEncPart.Key
	if err := req.DecryptAuthenticator(session); err != nil {
		t.Fatalf("%s: authenticator: %v", name, err)
	}
	if got := hex.EncodeToString(session.KeyValue); got != capturedTicketKey {
		t.Errorf("%s: the ticket's session key %s, want tshark's %s", name, got, capturedTicketKey)
	}
	if rep := capturedInitReplyToken(t, name); len(rep) != 0 {
		t.Fatalf("%s: knfsd's INIT reply carries a token of %d bytes; its AP-REP's subkey is not read here", name, len(rep))
	}
	key := session.KeyValue
	if sub := req.Authenticator.SubKey.KeyValue; len(sub) > 0 {
		key = sub
	}
	if got := hex.EncodeToString(key); got != capturedSubkeys[name] {
		t.Errorf("%s: context key %s, want the authenticator's subkey tshark reads, %s", name, got, capturedSubkeys[name])
	}
	return vfs.Keys{Session: key}
}

// capturedMIC is one MIC token in the capture and the bytes it covers.
type capturedMIC struct {
	what     string
	msg, tok []byte
	acceptor bool // knfsd's, verified at the initiator
}

// gssCredSvc is an RPCSEC_GSS credential's sequence number and service
// (RFC 2203 §5): version, procedure, sequence number, service, handle.
func gssCredSvc(t testing.TB, name string, cred []byte) (seq, svc uint32) {
	t.Helper()
	if len(cred) < 16 {
		t.Fatalf("golden %s: a credential of %d bytes", name, len(cred))
	}
	return binary.BigEndian.Uint32(cred[8:]), binary.BigEndian.Uint32(cred[12:])
}

const (
	authGSS      = 6
	gssSvcInteg  = 2
	gssSvcPrivac = 3
)

// callMICs is a data or DESTROY call's MIC tokens: its verifier, over the
// RPC header through the credential (RFC 2203 §5.3.1), and under integrity
// its body's checksum over the body (§5.3.2.2). It returns the credential's
// sequence number and service for the reply's.
func callMICs(t testing.TB, name string) (mics []capturedMIC, seq, svc uint32) {
	t.Helper()
	rec := readGolden(t, name)
	r := &xdrReader{t: t, name: name, b: rec}
	for range 6 { // xid, CALL, RPC version, program, version, procedure
		r.u32()
	}
	if r.u32() != authGSS {
		t.Fatalf("golden %s: not an RPCSEC_GSS credential", name)
	}
	seq, svc = gssCredSvc(t, name, r.opaque())
	header := rec[:len(rec)-len(r.b)]
	if r.u32() != authGSS {
		t.Fatalf("golden %s: not an RPCSEC_GSS verifier", name)
	}
	mics = append(mics, capturedMIC{what: name + "'s verifier", msg: header, tok: r.opaque()})
	if svc == gssSvcInteg {
		body := r.opaque()
		mics = append(mics, capturedMIC{what: name + "'s integrity checksum", msg: body, tok: r.opaque()})
	}
	return mics, seq, svc
}

// replyMICs is an accepted reply's MIC tokens: its verifier over seq, the
// window an INIT reply grants or the call's sequence number (RFC 2203
// §5.2.3.1, §5.3.3.2), and under integrity its body's checksum.
func replyMICs(t testing.TB, name string, seq, svc uint32) []capturedMIC {
	t.Helper()
	r := &xdrReader{t: t, name: name, b: readGolden(t, name)}
	r.u32() // xid
	if r.u32() != 1 || r.u32() != 0 {
		t.Fatalf("golden %s: not an accepted reply", name)
	}
	if r.u32() != authGSS {
		t.Fatalf("golden %s: not an RPCSEC_GSS verifier", name)
	}
	mics := []capturedMIC{{what: name + "'s verifier", msg: binary.BigEndian.AppendUint32(nil, seq), tok: r.opaque(), acceptor: true}}
	if r.u32() != 0 {
		t.Fatalf("golden %s: not SUCCESS", name)
	}
	if svc == gssSvcInteg {
		body := r.opaque()
		mics = append(mics, capturedMIC{what: name + "'s integrity checksum", msg: body, tok: r.opaque(), acceptor: true})
	}
	return mics
}

// capturedMICs is every MIC token the capture's golden files hold for the
// INIT name's context: the INIT reply's verifier over its window, and each
// data and DESTROY call's and reply's tokens.
func capturedMICs(t testing.TB, name string) []capturedMIC {
	t.Helper()
	const window = 128 // knfsd's, CAPTURE.md
	mics := replyMICs(t, name+"-reply", window, 0)
	base := name[:len(name)-len("init")]
	for _, what := range []string{"write", "destroy"} {
		call := base + what + "-call"
		if _, err := os.Stat(goldenDir + call + ".hex"); err != nil {
			continue
		}
		c, seq, svc := callMICs(t, call)
		mics = append(mics, c...)
		if svc == gssSvcPrivac {
			svc = 0 // a Wrap token, not a MIC
		}
		mics = append(mics, replyMICs(t, base+what+"-reply", seq, svc)...)
	}
	return mics
}

// The Linux client's MIC tokens in knfsd's capture, its calls' verifiers and
// krb5i checksums, verify at the acceptor under the context key read from
// the capture, and knfsd's, its replies' verifiers and checksums, verify at
// the initiator under the same key; a token altered in its last byte is
// refused. Catches a token Linux or knfsd sends that gss refuses, a context
// key chosen otherwise than RFC 18 §3.1 says, a checksum or flag expected
// otherwise than knfsd sends it, and a keytab of another realm or version
// committed with the capture.
func TestMICCaptured(t *testing.T) {
	var n int
	for _, c := range capturedInits {
		t.Run(c.name, func(t *testing.T) {
			keys := capturedContextKey(t, c.name)
			acc, err := KerberosContext(vfs.Evidence{}, keys, false)
			if err != nil {
				t.Fatal(err)
			}
			init, err := KerberosContext(vfs.Evidence{}, keys, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range capturedMICs(t, c.name) {
				at, side := acc, "the acceptor"
				if m.acceptor {
					at, side = init, "the initiator"
				}
				if err := at.VerifyMIC(m.msg, m.tok); err != nil {
					t.Errorf("%s at %s: VerifyMIC = %v, want nil", m.what, side, err)
				}
				bad := bytes.Clone(m.tok)
				bad[len(bad)-1] ^= 1
				if err := at.VerifyMIC(m.msg, bad); err == nil {
					t.Errorf("%s at %s, its last byte altered: VerifyMIC = nil, want refused", m.what, side)
				}
				n++
			}
		})
	}
	// Nine INIT replies; nine write calls and replies; the krb5i writes'
	// three call and three reply checksums; two DESTROYs and their replies.
	if want := 9 + 2*9 + 2*3 + 2*2; n != want {
		t.Errorf("%d captured MIC tokens checked, want %d", n, want)
	}
}
