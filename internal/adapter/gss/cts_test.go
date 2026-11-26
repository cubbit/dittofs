package gss

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"testing"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/crypto/rfc3962"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
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
		c, err := newChecksummer(unhex(t, tc.key), common.GetUsageKc(tc.usage))
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

// RFC 3962 Appendix B's AES-CTS vectors, one per length class: a partial
// final block, a full one, and three, four and two blocks around it. Each
// encrypts in place to its published output and decrypts back. Catches a
// stolen block misplaced, the last two blocks left unswapped, and a
// partial block padded or cut otherwise.
func TestCTSVectors(t *testing.T) {
	key := unhex(t, "636869636b656e207465726979616b69")
	const in = "4920776f756c64206c696b652074686520" + "47656e6572616c20476175277320" + "43" + "6869636b656e2c20706c656173652c" + "20" + "616e6420776f6e746f6e20736f75702e"
	for _, tc := range []struct {
		n    int
		want string
	}{
		{17, "c6353568f2bf8cb4d8a580362da7ff7f97"},
		{31, "fc00783e0efdb2c1d445d4c8eff7ed2297687268d6ecccc0c07b25e25ecfe5"},
		{32, "39312523a78662d5be7fcbcc98ebf5a897687268d6ecccc0c07b25e25ecfe584"},
		{47, "97687268d6ecccc0c07b25e25ecfe584b3fffd940c16a18c1b5549d2f838029e39312523a78662d5be7fcbcc98ebf5"},
		{48, "97687268d6ecccc0c07b25e25ecfe5849dad8bbb96c4cdc03bc103e1a194bbd839312523a78662d5be7fcbcc98ebf5a8"},
		{64, "97687268d6ecccc0c07b25e25ecfe58439312523a78662d5be7fcbcc98ebf5a84807efe836ee89a526730dbc2f7bc8409dad8bbb96c4cdc03bc103e1a194bbd8"},
	} {
		sl, err := newCTS(key)
		if err != nil {
			t.Fatal(err)
		}
		plain := unhex(t, in)[:tc.n]
		buf := bytes.Clone(plain)
		sl.encrypt(buf)
		if want := unhex(t, tc.want); !bytes.Equal(buf, want) {
			t.Errorf("%d bytes: encrypt = %x, want %x", tc.n, buf, want)
		}
		sl.decrypt(buf)
		if !bytes.Equal(buf, plain) {
			t.Errorf("%d bytes: decrypt = %x, want %x", tc.n, buf, plain)
		}
	}
}

// At every length from one block to five, the in-place cipher agrees with
// gokrb5's AES-CTS, written apart from it, both ways. Catches a length the
// published vectors miss: a single block, and each partial final block.
func TestCTSAgreesWithAescts(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, 32)
	sl, err := newCTS(key)
	if err != nil {
		t.Fatal(err)
	}
	et, err := crypto.GetEtype(etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	for n := aes.BlockSize; n <= 5*aes.BlockSize; n++ {
		plain := make([]byte, n)
		for i := range plain {
			plain[i] = byte(i*7 + n)
		}
		_, want, err := rfc3962.EncryptData(key, plain, et)
		if err != nil {
			t.Fatalf("%d bytes: gokrb5's EncryptData = %v", n, err)
		}
		buf := bytes.Clone(plain)
		sl.encrypt(buf)
		if !bytes.Equal(buf, want) {
			t.Errorf("%d bytes: encrypt = %x, gokrb5 gives %x", n, buf, want)
		}
		sl.decrypt(buf)
		if !bytes.Equal(buf, plain) {
			t.Errorf("%d bytes: decrypt = %x, want %x", n, buf, plain)
		}
	}
}
