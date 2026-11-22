package identity

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"runtime"
	"testing"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// apReqAllocPerByte bounds the bytes one AP-REQ decode allocates per input
// byte, beside apReqAllocBase for the decode's fixed cost.
const (
	apReqAllocPerByte = 64
	apReqAllocBase    = 16 << 10
)

// apReqSeeds are AP-REQs the test realm issued: plain, with a subkey and
// addresses, under AES128, and the first cut short.
func apReqSeeds() [][]byte {
	r := identitytest.NewRealm(testRealm, "cifs/nas.test")
	plain := r.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0})
	return [][]byte{
		plain[:len(plain)/2],
		r.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0}),
		r.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0, Subkey: identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96),
			Addresses: []netip.Addr{addr1, netip.MustParseAddr("2001:db8::1")}, NetBIOS: "WORKSTATION"}),
		r.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Now: t0, Etype: etypeID.AES128_CTS_HMAC_SHA1_96}),
	}
}

// FuzzAPReq holds the AP-REQ decode to its invariants: it never panics; it
// allocates at most a constant times its input's length; a refusal is
// ErrInvalid; and what it decodes, marshalled again with gokrb5, decodes to
// the same value. It reaches only the outer AP-REQ decode: the ticket's
// encrypted part and the authenticator decode after decryption, which fuzzed
// bytes do not pass.
func FuzzAPReq(f *testing.F) {
	for _, s := range apReqSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		req, err := parseAPReq(b)
		runtime.ReadMemStats(&after)
		if n, limit := after.TotalAlloc-before.TotalAlloc, uint64(apReqAllocBase+apReqAllocPerByte*len(b)); n > limit {
			t.Fatalf("decoding %d bytes allocated %d, want at most %d", len(b), n, limit)
		}
		if err != nil {
			if !errors.Is(err, vfserr.ErrInvalid) {
				t.Fatalf("parseAPReq refused with %v, want ErrInvalid", err)
			}
			return
		}
		again, err := req.Marshal()
		if err != nil {
			t.Fatalf("a decoded AP-REQ does not marshal: %v", err)
		}
		req2, err := parseAPReq(again)
		if err != nil {
			t.Fatalf("a re-marshalled AP-REQ does not decode: %v", err)
		}
		if !reflect.DeepEqual(req, req2) {
			t.Fatalf("decode∘encode changed the AP-REQ:\n%+v\nto\n%+v", req, req2)
		}
	})
}

// FuzzParsePHC holds a stored password hash's decode to its invariants: it
// never panics; what it accepts stays within the cost bounds; and what it
// decodes, written back in the PHC form, decodes to the same value.
func FuzzParsePHC(f *testing.F) {
	h, err := identity.NewHashes(entity.User{}, []byte("Password"), identity.NTLMSetting{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(h.Password)
	f.Add([]byte("$argon2id$v=19$m=4294967295,t=2,p=1$c2FsdA$a2V5"))
	f.Add([]byte("$argon2id$v=19$m=19456,t=4294967295,p=1$c2FsdA$a2V5"))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := parsePHC(b)
		if err != nil {
			return
		}
		if got.memory > phcMaxMemory || got.time > phcMaxTime || got.time == 0 || got.threads == 0 {
			t.Fatalf("parsePHC accepted m=%d t=%d p=%d, outside its bounds", got.memory, got.time, got.threads)
		}
		b64 := base64.RawStdEncoding
		again := fmt.Appendf(nil, "$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
			got.memory, got.time, got.threads, b64.EncodeToString(got.salt), b64.EncodeToString(got.key))
		got2, err := parsePHC(again)
		if err != nil {
			t.Fatalf("a re-encoded hash %q does not decode: %v", again, err)
		}
		if !reflect.DeepEqual(got, got2) {
			t.Fatalf("decode∘encode changed the hash:\n%+v\nto\n%+v", got, got2)
		}
	})
}

// pacAllocPerByte bounds the bytes one PAC walk and verification allocate
// per input byte, beside pacAllocBase for their fixed cost.
const (
	pacAllocPerByte = 16
	pacAllocBase    = 16 << 10
)

// pacSeeds are the captured PACs, Samba's and MIT's, and PACs the test realm built: with no
// logon information, and repeating a logon-information, client-information
// and signature buffer; then a header claiming 0xffffffff buffers in eight
// bytes, and a buffer lying past the PAC's end.
func pacSeeds(t testing.TB) [][]byte {
	tgt, _ := capture(t, "tgt.pac", "krbtgt.keytab")
	svc, _ := capture(t, "service.pac", "service.keytab")
	mit, _ := capture(t, "mit-service.pac", "mit-service.keytab")
	r := identitytest.NewRealm(testRealm, "cifs/nas.test")
	logon := identitytest.LogonInfo{Name: "alice", Domain: "CORP", DomainSID: "S-1-5-21-1-2-3", UserRID: 1105,
		Groups: []identitytest.GroupRID{{RID: 513, Attributes: 7}}}
	pac := func(o identitytest.PACOpts) []byte {
		o.Service, o.AuthTime = "cifs/nas.test", t0
		return r.PAC(o)
	}
	past := identitytest.EncodePAC([]identitytest.PACBuffer{{Type: identitytest.PACClientInfo, Data: make([]byte, 16)}})
	binary.LittleEndian.PutUint64(past[16:], 1<<40)
	return [][]byte{
		tgt, svc, mit,
		pac(identitytest.PACOpts{}),
		pac(identitytest.PACOpts{Logon: []identitytest.LogonInfo{logon, logon}}),
		pac(identitytest.PACOpts{Logon: []identitytest.LogonInfo{logon}, Extra: []identitytest.PACBuffer{{Type: identitytest.PACClientInfo, Data: []byte("a second client")}}}),
		pac(identitytest.PACOpts{Logon: []identitytest.LogonInfo{logon}, Extra: []identitytest.PACBuffer{{Type: identitytest.PACServerSignature, Data: make([]byte, 16)}}}),
		{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0},
		past,
	}
}

// FuzzPAC holds the PAC's walk, its server signature's check and its client
// information's decode to their invariants: they never panic; the walk
// keeps only the first buffer of each type; and what it walks, laid out
// again by the test realm's encoder, walks to the same buffers. Their
// allocations are bounded over the seeds by TestPACAllocationBound, outside
// the target, since reading the allocator's statistics stops the program.
// The logon information is decoded only under a verified signature, which
// fuzzed bytes do not carry, so it is not reached here (verifyPAC's
// decision).
func FuzzPAC(f *testing.F) {
	for _, s := range pacSeeds(f) {
		f.Add(s)
	}
	// A key no seed is signed under: what a sender without the service key
	// reaches.
	key := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	client := types.PrincipalName{NameType: 1, NameString: []string{"alice"}}
	f.Fuzz(func(t *testing.T, b []byte) {
		bufs, err := pacBuffers(b)
		_, _ = verifyPAC(b, key, client, t0)
		if ci, ok := bufferOf(bufs, pacClientInfo); ok {
			_, _, _ = clientInfo(ci.data)
		}
		if err != nil {
			return
		}
		var again []identitytest.PACBuffer
		for i, x := range bufs {
			if containsType(bufs[:i], x.typ) {
				t.Fatalf("the walk kept a second buffer of type %d", x.typ)
			}
			again = append(again, identitytest.PACBuffer{Type: x.typ, Data: x.data})
		}
		bufs2, err := pacBuffers(identitytest.EncodePAC(again))
		if err != nil {
			t.Fatalf("a re-encoded PAC does not walk: %v", err)
		}
		if len(bufs2) != len(bufs) {
			t.Fatalf("a re-encoded PAC walks to %d buffers, want %d", len(bufs2), len(bufs))
		}
		for i := range bufs {
			if bufs2[i].typ != bufs[i].typ || !bytes.Equal(bufs2[i].data, bufs[i].data) {
				t.Fatalf("decode∘encode changed buffer %d: type %d %x, want type %d %x", i, bufs2[i].typ, bufs2[i].data, bufs[i].typ, bufs[i].data)
			}
		}
	})
}
