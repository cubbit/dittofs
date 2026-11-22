package identity

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/chksumtype"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/pac"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
)

// captures are the PACs the Samba fixture issued (testdata/pac/README.md),
// with the keytab holding the key of the ticket each came in: alice's of
// DITTOFS.AD, signed in at captureAuthTime, her SID the one alice.json holds.
var captures = []struct {
	pac, keytab string
	extra       []uint32 // buffer types the capture holds beyond the ones identity reads
}{
	{"tgt.pac", "krbtgt.keytab", []uint32{12, 17, 18}},
	{"service.pac", "service.keytab", []uint32{12, 16, 19}},
}

var (
	captureClient   = types.PrincipalName{NameType: 1, NameString: []string{"alice"}}
	captureAuthTime = time.Date(2026, 11, 21, 8, 55, 34, 0, time.UTC)
)

// capture is a captured PAC's bytes and its keytab's AES256 key, the type of
// both captures' server signatures.
func capture(t testing.TB, pacFile, keytabFile string) ([]byte, types.EncryptionKey) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pac", pacFile))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(filepath.Join("testdata", "pac", keytabFile))
	if err != nil {
		t.Fatal(err)
	}
	kt := keytab.New()
	if err := kt.Unmarshal(kb); err != nil {
		t.Fatalf("%s: %v", keytabFile, err)
	}
	for _, e := range kt.Entries {
		if e.Key.KeyType == etypeID.AES256_CTS_HMAC_SHA1_96 {
			return raw, e.Key
		}
	}
	t.Fatalf("%s holds no AES256 key", keytabFile)
	return nil, types.EncryptionKey{}
}

func userSID(v *pac.KerbValidationInfo) string {
	return fmt.Sprintf("%s-%d", v.LogonDomainID.String(), v.UserID)
}

// Both PACs the Samba fixture issued verify under their tickets' keys, the
// TGT's PAC_ATTRIBUTES_INFO and PAC_REQUESTOR and the service ticket's
// ticket and full checksums passing as types identity does not read, and each
// decodes to the user SID its capture recorded. Catches a verifier and a
// builder that agree only with each other.
func TestVerifyPACCaptures(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "pac", "alice.json"))
	if err != nil {
		t.Fatal(err)
	}
	var alice struct {
		SIDs map[string]string `json:"sids"`
	}
	if err := json.Unmarshal(b, &alice); err != nil || alice.SIDs["alice"] == "" {
		t.Fatalf("alice.json: %v, sids %v; want alice's SID", err, alice.SIDs)
	}
	for _, c := range captures {
		raw, key := capture(t, c.pac, c.keytab)
		bufs, err := pacBuffers(raw)
		if err != nil {
			t.Fatalf("%s: pacBuffers = %v", c.pac, err)
		}
		for _, typ := range c.extra {
			if !containsType(bufs, typ) {
				t.Errorf("%s holds no buffer of type %d, which the capture recorded", c.pac, typ)
			}
		}
		v, err := verifyPAC(raw, key, captureClient, captureAuthTime)
		if err != nil || v == nil {
			t.Fatalf("%s: verifyPAC = %v, %v; want its logon information", c.pac, v, err)
		}
		if got := userSID(v); got != alice.SIDs["alice"] {
			t.Errorf("%s: user SID = %s, want alice's %s from alice.json", c.pac, got, alice.SIDs["alice"])
		}
	}
}

// Each bit of service.pac flipped fails verification, padding and the
// header included, but for the KDC signature's value, which MS-PAC 2.8.4
// zeroes before the server signature is computed and which only a domain
// controller could check. Catches a checksum over a re-encoding, which skips
// a byte the decoder skips.
func TestVerifyPACRefusesEveryFlippedBit(t *testing.T) {
	raw, key := capture(t, "service.pac", "service.keytab")
	bufs, err := pacBuffers(raw)
	if err != nil {
		t.Fatal(err)
	}
	kdc, _ := bufferOf(bufs, pacKDCSignature)
	accepted := 0
	for i := range raw {
		if i >= kdc.off+4 && i < kdc.off+16 {
			continue
		}
		for bit := range 8 {
			b := append([]byte(nil), raw...)
			b[i] ^= 1 << bit
			if _, err := verifyPAC(b, key, captureClient, captureAuthTime); err == nil {
				if accepted++; accepted <= 5 {
					t.Errorf("service.pac with bit %d of byte %d flipped verifies, want refused", bit, i)
				}
			}
		}
	}
	if accepted > 0 {
		t.Errorf("%d flipped bits of service.pac verify, want none", accepted)
	}
}

// Of a logon-information buffer repeated with a second user, the first is
// the one decoded. Catches the last of a type winning.
func TestVerifyPACUsesTheFirstOfAType(t *testing.T) {
	r := identitytest.NewRealm(testRealm, "cifs/nas.test")
	user := func(rid uint32) identitytest.LogonInfo {
		return identitytest.LogonInfo{Name: "alice", Domain: "CORP", DomainSID: "S-1-5-21-1-2-3", UserRID: rid}
	}
	raw := r.PAC(identitytest.PACOpts{Service: "cifs/nas.test", AuthTime: t0, Logon: []identitytest.LogonInfo{user(1105), user(1106)}})
	v, err := verifyPAC(raw, r.Key("cifs/nas.test", etypeID.AES256_CTS_HMAC_SHA1_96), captureClient, t0)
	if err != nil || v == nil {
		t.Fatalf("verifyPAC = %v, %v; want the first logon information", v, err)
	}
	if got := userSID(v); got != "S-1-5-21-1-2-3-1105" {
		t.Errorf("user SID = %s, want the first buffer's S-1-5-21-1-2-3-1105", got)
	}
}

// A PAC whose header claims 0xffffffff buffers in eight bytes is refused
// before anything is allocated for them.
func TestPACBuffersRefusesAHugeCount(t *testing.T) {
	b := []byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := pacBuffers(b)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("pacBuffers accepted a PAC claiming 0xffffffff buffers in eight bytes")
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<10 {
		t.Errorf("refusing it allocated %d bytes, want under 1 KiB", n)
	}
}

// A KDC signature of an unknown type, and one of a known type cut to its
// 4-byte type as the PAC's last buffer, are refused as such, before the
// server signature is checked, never by a panic zeroing past the PAC's end.
// The short one is also a FuzzPAC seed. Catches the KDC signature's type and
// length unchecked.
func TestVerifyPACRefusesAMalformedKDCSignature(t *testing.T) {
	key := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	srv := identitytest.PACBuffer{Type: identitytest.PACServerSignature, Data: append(le32(chksumtype.HMAC_SHA1_96_AES256), make([]byte, 12)...)}
	for _, c := range []struct {
		name string
		kdc  []byte
	}{
		{"an unknown type", append(le32(0x7777), make([]byte, 12)...)},
		{"a known type cut to 4 bytes", le32(chksumtype.HMAC_SHA1_96_AES256)},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := identitytest.EncodePAC([]identitytest.PACBuffer{srv, {Type: identitytest.PACKDCSignature, Data: c.kdc}})
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("verifyPAC panicked: %v; want it refused", p)
				}
			}()
			_, err := verifyPAC(raw, key, captureClient, t0)
			if err == nil || !strings.Contains(err.Error(), "KDC signature of an unknown type or length") {
				t.Errorf("verifyPAC = %v; want refused for the KDC signature's type or length", err)
			}
		})
	}
}

func le32(v int32) []byte { return binary.LittleEndian.AppendUint32(nil, uint32(v)) }

// Walking and verifying each FuzzPAC seed allocates at most a constant times
// its length. Catches an allocation sized by a count the PAC claims.
func TestPACAllocationBound(t *testing.T) {
	key := identitytest.NewKey(etypeID.AES256_CTS_HMAC_SHA1_96)
	for i, b := range pacSeeds(t) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		bufs, _ := pacBuffers(b)
		_, _ = verifyPAC(b, key, captureClient, t0)
		if ci, ok := bufferOf(bufs, pacClientInfo); ok {
			_, _, _ = clientInfo(ci.data)
		}
		runtime.ReadMemStats(&after)
		if n, limit := after.TotalAlloc-before.TotalAlloc, uint64(pacAllocBase+pacAllocPerByte*len(b)); n > limit {
			t.Errorf("seed %d: walking and verifying %d bytes allocated %d, want at most %d", i, len(b), n, limit)
		}
	}
}
