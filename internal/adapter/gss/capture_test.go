package gss

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// goldenDir holds the RPC records cut from knfsd's RPCSEC_GSS capture
// (test/pcap-corpus/nfs-gss/CAPTURE.md).
const goldenDir = "../nfs/wire/testdata/golden/"

// capturedInit is one mount's RPCSEC_GSS_INIT in the capture: the golden
// files' name less its -call or -reply, and what tshark 4.6.4's Kerberos
// dissector reads in the call's token: the AP-REQ (spnego.krb5.blob less its
// token ID), its length and SHA-256.
type capturedInit struct {
	name   string
	apReq  int
	sha256 string
}

var capturedInits = []capturedInit{
	{"knfsd-nfs3-gss3-krb5-init", 736, "fa8f8cc48119fa1f78452277b7c8b4569b11ba7a82210dc831ec51603d966fe0"},
	{"knfsd-nfs3-gss3-krb5i-init", 736, "53f2a28e91db796b4f13950414c6643fc7dee08043bf1cf9faf319bd039c2676"},
	{"knfsd-nfs3-gss3-krb5p-init", 736, "262231142a42b9a6cd3ace7642adeb5f2aef1113f521255b50fb00791ad79699"},
	{"knfsd-nfs4-gss40-krb5-init", 736, "7b146b48403604f8c3139e5315c48183ee67ba3746a4a5d391e516a3fc36e277"},
	{"knfsd-nfs4-gss40-krb5i-init", 736, "755c1a961de56195c5275a67a107598ef38cc142d2c0589ba84ca64952e31199"},
	{"knfsd-nfs4-gss40-krb5p-init", 736, "f3fdfdabfbb1f9029a824bfa3d3ab408c5509d273ef9adb6dec1b2a435ab4f2e"},
	{"knfsd-nfs4-gss41-krb5-init", 736, "ccffe0af10078a8f6206df98e54712a8b35e5d4119201262a511af446afb8121"},
	{"knfsd-nfs4-gss41-krb5i-init", 736, "9933a010e1e7b7349c37ec6e27bfbe6bb7a1be0eaa462f1e11cd97ca09def32e"},
	{"knfsd-nfs4-gss41-krb5p-init", 736, "b63143755edd4de4f495aa13c4262e85bc8bcbc95540e4ea7197f532005f992d"},
}

// readGolden is a golden file's bytes: its hex digits, '#' comments dropped.
func readGolden(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(goldenDir + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return b
}

// xdrReader reads XDR words and opaques off a captured record, failing the
// test on a record shorter than its own lengths say.
type xdrReader struct {
	t    testing.TB
	name string
	b    []byte
}

func (r *xdrReader) u32() uint32 {
	r.t.Helper()
	if len(r.b) < 4 {
		r.t.Fatalf("golden %s ends inside a word", r.name)
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *xdrReader) opaque() []byte {
	r.t.Helper()
	n := int(r.u32())
	if pad := (n + 3) &^ 3; pad > len(r.b) {
		r.t.Fatalf("golden %s: an opaque of %d bytes, %d left", r.name, n, len(r.b))
	}
	v := r.b[:n]
	r.b = r.b[(n+3)&^3:]
	return v
}

// capturedInitToken is the initial context token an INIT call carries: an RPC
// call's header, its credential and verifier, then rpc_gss_init_arg, an opaque
// (RFC 2203 §5.2.2).
func capturedInitToken(t testing.TB, name string) []byte {
	t.Helper()
	r := &xdrReader{t: t, name: name, b: readGolden(t, name+"-call")}
	for range 6 { // xid, CALL, RPC version, program, version, procedure
		r.u32()
	}
	r.u32()
	r.opaque() // the credential
	r.u32()
	r.opaque() // the verifier
	tok := r.opaque()
	if len(r.b) != 0 {
		t.Fatalf("golden %s: %d bytes after the INIT token", name, len(r.b))
	}
	return tok
}

// capturedInitReplyToken is the token knfsd's INIT reply carries: an accepted
// reply's header and verifier, its accept state, then rpc_gss_init_res's
// handle, major and minor status, window and token (RFC 2203 §5.2.3.1).
func capturedInitReplyToken(t testing.TB, name string) []byte {
	t.Helper()
	r := &xdrReader{t: t, name: name, b: readGolden(t, name+"-reply")}
	r.u32() // xid
	if r.u32() != 1 || r.u32() != 0 {
		t.Fatalf("golden %s: not an accepted reply", name)
	}
	r.u32()
	r.opaque() // the verifier
	if r.u32() != 0 {
		t.Fatalf("golden %s: not SUCCESS", name)
	}
	r.opaque() // the handle
	r.u32()    // major
	r.u32()    // minor
	r.u32()    // window
	return r.opaque()
}

// Each INIT token the Linux client sent knfsd unframes to the AP-REQ tshark
// reads in it, and frames back to its own bytes. Catches a framing Linux uses
// that unframe refuses, and an AP-REQ cut from the wrong offset.
//
// decision: the capture holds no AP-REP, so the reply half of the row, that
// knfsd's AP-REP framed here gives the captured reply token, cannot be
// checked: the Linux client asks for no mutual authentication, and every
// INIT reply's token is empty, which this test asserts so that a capture
// re-recorded with an AP-REP fails here and gets that check written.
func TestKerberosCapturedInit(t *testing.T) {
	for _, c := range capturedInits {
		t.Run(c.name, func(t *testing.T) {
			tok := capturedInitToken(t, c.name)
			apReq, err := unframe(tok, tokAPReq)
			if err != nil {
				t.Fatalf("unframe(the captured INIT token) = %v", err)
			}
			if sum := sha256.Sum256(apReq); len(apReq) != c.apReq || hex.EncodeToString(sum[:]) != c.sha256 {
				t.Errorf("AP-REQ of %d bytes, SHA-256 %x; want tshark's %d bytes, %s", len(apReq), sum, c.apReq, c.sha256)
			}
			if again := frame(apReq, tokAPReq); string(again) != string(tok) {
				t.Errorf("the AP-REQ frames back to %x, want the captured token %x", again, tok)
			}
			if rep := capturedInitReplyToken(t, c.name); len(rep) != 0 {
				t.Errorf("knfsd's INIT reply carries a token of %d bytes, want the empty one the capture has without mutual authentication", len(rep))
			}
		})
	}
}
