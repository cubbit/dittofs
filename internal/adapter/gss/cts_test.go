package gss

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 3962's AES keys, and the checksum, encryption and integrity keys MIT
// krb5 publishes for them, come out of deriveKey. The first two rows are
// RFC 3962 Appendix B's string-to-key at iteration count 1, whose last step
// is DK(PBKDF2 output, "kerberos"); the rest are MIT krb5's t_derive.c for
// usage 2 under those keys. Catches a derivation that is not RFC 3961's DK,
// or that picks the wrong type for a key's length.
func TestDeriveKeyVectors(t *testing.T) {
	const (
		key128 = "42263c6e89f4fc28b8df68ee09799f15"
		key256 = "fe697b52bc0d3ce14432ba036a92e65bbb52280990a2fa27883998d72af30161"
	)
	for _, tc := range []struct {
		name, key, constant, want string
	}{
		{"RFC 3962 aes128 string-to-key", "cdedb5281bb2f801565a1122b2563515", hex.EncodeToString([]byte("kerberos")), key128},
		{"RFC 3962 aes256 string-to-key", "cdedb5281bb2f801565a1122b25635150ad1f7a04bb9f3a333ecc0e2e1f70837", hex.EncodeToString([]byte("kerberos")), key256},
		{"aes128 Kc", key128, "0000000299", "34280a382bc92769b2da2f9ef066854b"},
		{"aes128 Ke", key128, "00000002aa", "5b14fc4e250e14ddf9dccf1af6674f53"},
		{"aes128 Ki", key128, "0000000255", "4ed31063621684f09ae8d89991af3e8f"},
		{"aes256 Kc", key256, "0000000299", "bfab388bdcb238e9f9c98d6a878304f04d30c82556375ac507a7a852790f4674"},
		{"aes256 Ke", key256, "00000002aa", "c7cfd9cd75fe793a586a542d87e0d1396f1134a104bb1a9190b8c90ada3ddf37"},
		{"aes256 Ki", key256, "0000000255", "97151b4c76945063e2eb0529dc067d97d7bba90776d8126d91f34f3101aea8ba"},
	} {
		got, err := deriveKey(unhex(t, tc.key), unhex(t, tc.constant))
		if err != nil {
			t.Fatalf("%s: deriveKey = %v", tc.name, err)
		}
		if want := unhex(t, tc.want); !bytes.Equal(got, want) {
			t.Errorf("%s: deriveKey = %x, want %x", tc.name, got, want)
		}
	}
	if _, err := deriveKey(make([]byte, 24), []byte("kerberos")); err == nil {
		t.Error("deriveKey of a 24-byte key = nil error, want a type refused")
	}
}

// The checksummer gives MIT krb5's published HMAC-SHA1-96 checksums (its
// t_cksums.c) under their keys and usages, the message fed in two pieces as
// a MIC token feeds it. Catches a checksum key derived for the wrong usage,
// a sum truncated otherwise than to 96 bits, and a piece dropped.
func TestChecksumVectors(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		usage     uint32
		msg, want string
	}{
		{"aes128", "9062430c8cda3388922e6d6a509f5b7a", 3, "eight nine ten eleven twelve thirteen", "01a4b088d45628f6946614e3"},
		{"aes256", "b1ae4cd8462aff1677053cc9279aac30b796fb81ce21474dd3ddbcfea4ec76d7", 4, "fourteen", "e08739e3279e2903ec8e3836"},
	} {
		c, err := newChecksummer(unhex(t, tc.key), tc.usage)
		if err != nil {
			t.Fatalf("%s: newChecksummer = %v", tc.name, err)
		}
		msg := []byte(tc.msg)
		got := c.appendSum([]byte("dst"), msg[:5], msg[5:])
		if want := append([]byte("dst"), unhex(t, tc.want)...); !bytes.Equal(got, want) {
			t.Errorf("%s: appendSum = %x, want %x", tc.name, got, want)
		}
	}
}
