package gss

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// capturedWrapTokens is the Wrap tokens of a captured krb5p call and its
// reply, name less its -call or -reply: rpc_gss_priv_data's databody_priv
// after each one's verifier (RFC 2203 §5.3.2.3, §5.3.3.4.3).
func capturedWrapTokens(t testing.TB, name string) (call, reply []byte) {
	t.Helper()
	r := &xdrReader{t: t, name: name + "-call", b: readGolden(t, name+"-call")}
	for range 6 { // xid, CALL, RPC version, program, version, procedure
		r.u32()
	}
	if r.u32() != authGSS {
		t.Fatalf("golden %s-call: not an RPCSEC_GSS credential", name)
	}
	if _, svc := gssCredSvc(t, name, r.opaque()); svc != gssSvcPrivac {
		t.Fatalf("golden %s-call: service %d, want privacy", name, svc)
	}
	r.u32()
	r.opaque() // the verifier
	call = r.opaque()
	r = &xdrReader{t: t, name: name + "-reply", b: readGolden(t, name+"-reply")}
	r.u32() // xid
	if r.u32() != 1 || r.u32() != 0 {
		t.Fatalf("golden %s-reply: not an accepted reply", name)
	}
	r.u32()
	r.opaque() // the verifier
	if r.u32() != 0 {
		t.Fatalf("golden %s-reply: not SUCCESS", name)
	}
	return call, r.opaque()
}

// The Linux client's krb5p calls in knfsd's capture unwrap at the acceptor
// under the context key read from the INIT beside each, and knfsd's sealed
// replies unwrap at the initiator; each message starts with the call's
// sequence number, as RFC 2203 §5.3.2.2's rpc_gss_data_t has it, and a
// token altered in its last byte is refused. Catches a token Linux or knfsd
// sends that gss refuses, a context key chosen otherwise than RFC 18 §3.1
// says, and a flag, filler or EC expected otherwise than they send it.
func TestUnwrapCaptured(t *testing.T) {
	for _, base := range []string{"knfsd-nfs3-gss3-krb5p-", "knfsd-nfs4-gss40-krb5p-", "knfsd-nfs4-gss41-krb5p-"} {
		t.Run(base+"write", func(t *testing.T) {
			keys := capturedContextKey(t, base+"init")
			acc, err := KerberosContext(vfs.Evidence{}, keys, false)
			if err != nil {
				t.Fatal(err)
			}
			init, err := KerberosContext(vfs.Evidence{}, keys, true)
			if err != nil {
				t.Fatal(err)
			}
			_, seq, _ := callMICs(t, base+"write-call")
			call, reply := capturedWrapTokens(t, base+"write")
			for _, side := range []struct {
				what string
				at   *Context
				tok  []byte
			}{
				{"the client's call at the acceptor", acc, call},
				{"knfsd's reply at the initiator", init, reply},
			} {
				msg, err := side.at.Unwrap(bytes.Clone(side.tok))
				if err != nil {
					t.Errorf("%s: Unwrap = %v, want nil", side.what, err)
					continue
				}
				if len(msg) < 4 || uint32(msg[0])<<24|uint32(msg[1])<<16|uint32(msg[2])<<8|uint32(msg[3]) != seq {
					t.Errorf("%s: the message starts %x, want the sequence number %d", side.what, msg[:min(4, len(msg))], seq)
				}
				bad := bytes.Clone(side.tok)
				bad[len(bad)-1] ^= 1
				if _, err := side.at.Unwrap(bad); err == nil {
					t.Errorf("%s, its last byte altered: Unwrap = nil, want refused", side.what)
				}
			}
		})
	}
}
