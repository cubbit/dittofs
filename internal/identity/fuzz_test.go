package identity

import (
	"errors"
	"net/netip"
	"reflect"
	"runtime"
	"testing"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
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
