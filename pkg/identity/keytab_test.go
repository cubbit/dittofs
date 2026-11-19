package identity_test

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// keytabOf is a keytab of one cifs/ entry per etype. A key that is not AES is
// an AES256 key relabelled, since the check reads only the label.
func keytabOf(t testing.TB, etypes ...int32) []byte {
	t.Helper()
	kt := keytab.New()
	for i, et := range etypes {
		if err := kt.AddEntry("cifs/nas.corp.example", "CORP.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
			t.Fatal(err)
		}
		kt.Entries[i].Key.KeyType = et
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withHole is b with a deleted entry's hole of n bytes before its entries.
func withHole(b []byte, n int) []byte {
	out := append([]byte{}, b[:2]...)
	out = binary.BigEndian.AppendUint32(out, uint32(-int32(n)))
	out = append(out, make([]byte, n)...)
	return append(out, b[2:]...)
}

func TestCheckKeytab(t *testing.T) {
	aes256 := keytabOf(t, etypeID.AES256_CTS_HMAC_SHA1_96)
	for _, tc := range []struct {
		name   string
		b      []byte
		refuse bool
	}{
		{"AES128 alone", keytabOf(t, etypeID.AES128_CTS_HMAC_SHA1_96), false},
		{"AES256 alone", aes256, false},
		{"AES128 and AES256", keytabOf(t, etypeID.AES128_CTS_HMAC_SHA1_96, etypeID.AES256_CTS_HMAC_SHA1_96), false},
		{"AES256 after a hole", withHole(aes256, 12), false},
		{"AES256, then a zero length ending it", append(append([]byte{}, aes256...), 0, 0, 0, 0, 9, 9), false},
		{"RC4", keytabOf(t, etypeID.RC4_HMAC), true},
		{"DES", keytabOf(t, etypeID.DES_CBC_MD5), true},
		{"AES256 and RC4", keytabOf(t, etypeID.AES256_CTS_HMAC_SHA1_96, etypeID.RC4_HMAC), true},
		{"RC4 and AES256", keytabOf(t, etypeID.RC4_HMAC, etypeID.AES256_CTS_HMAC_SHA1_96), true},
		{"no entry", []byte{5, 2}, true},
		{"a hole alone", withHole([]byte{5, 2}, 12), true},
		{"version 1", append([]byte{5, 1}, aes256[2:]...), true},
		{"garbage", []byte("not a keytab"), true},
		{"empty", nil, true},
		{"truncated", aes256[:len(aes256)-10], true},
		{"trailing bytes", append(append([]byte{}, aes256...), 1, 2), true},
	} {
		err := identity.CheckKeytab(tc.b)
		if (err != nil) != tc.refuse {
			t.Errorf("%s: CheckKeytab = %v, want refused %t", tc.name, err, tc.refuse)
		}
		if err != nil && !errors.Is(err, vfserr.ErrInvalid) {
			t.Errorf("%s: CheckKeytab = %v, want vfserr.ErrInvalid", tc.name, err)
		}
	}
}

// KeytabNames names each key's service and realm, and refuses what
// CheckKeytab refuses.
func TestKeytabNames(t *testing.T) {
	names, err := identity.KeytabNames(keytabOf(t, etypeID.AES128_CTS_HMAC_SHA1_96, etypeID.AES256_CTS_HMAC_SHA1_96))
	want := identity.KeytabName{Name: "cifs/nas.corp.example", Realm: "CORP.EXAMPLE"}
	if err != nil || len(names) != 2 || names[0] != want || names[1] != want {
		t.Errorf("KeytabNames = %q, %v; want %v twice", names, err, want)
	}
	if _, err := identity.KeytabNames(keytabOf(t, etypeID.RC4_HMAC)); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("KeytabNames of an RC4 keytab = %v, want vfserr.ErrInvalid", err)
	}
}

// FuzzCheckKeytab holds CheckKeytab to gokrb5's decoder: no panic, and a
// keytab it accepts decodes there to at least one entry, every one an AES
// key.
func FuzzCheckKeytab(f *testing.F) {
	f.Add(keytabOf(f, etypeID.AES256_CTS_HMAC_SHA1_96))
	f.Add(keytabOf(f, etypeID.AES128_CTS_HMAC_SHA1_96, etypeID.RC4_HMAC))
	f.Add(withHole(keytabOf(f, etypeID.AES128_CTS_HMAC_SHA1_96), 4))
	f.Add([]byte{5, 2, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		if identity.CheckKeytab(b) != nil {
			return
		}
		kt := keytab.New()
		if err := kt.Unmarshal(b); err != nil {
			t.Fatalf("CheckKeytab accepted %x, gokrb5 refused it: %v", b, err)
		}
		if len(kt.Entries) == 0 {
			t.Fatalf("CheckKeytab accepted %x, gokrb5 decodes no entry", b)
		}
		names, err := identity.KeytabNames(b)
		if err != nil || len(names) != len(kt.Entries) {
			t.Fatalf("KeytabNames(%x) = %q, %v; gokrb5 decodes %d entries", b, names, err, len(kt.Entries))
		}
		for i, e := range kt.Entries {
			if want := (identity.KeytabName{Name: strings.Join(e.Principal.Components, "/"), Realm: e.Principal.Realm}); names[i] != want {
				t.Fatalf("KeytabNames(%x)[%d] = %q, gokrb5 decodes %q", b, i, names[i], want)
			}
		}
		for _, e := range kt.Entries {
			if et := e.Key.KeyType; et != etypeID.AES128_CTS_HMAC_SHA1_96 && et != etypeID.AES256_CTS_HMAC_SHA1_96 {
				t.Fatalf("CheckKeytab accepted %x, gokrb5 decodes a key of etype %d", b, et)
			}
		}
	})
}
