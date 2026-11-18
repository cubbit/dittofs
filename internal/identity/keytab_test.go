package identity_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/pkg/secrets"
)

const (
	svc   = "cifs/nas.corp.example"
	realm = "CORP.EXAMPLE"
)

// keytabOf is a keytab of one entry for svc per etype, each under kvno and
// derived from password. A key that is not AES is an AES256 key relabelled,
// since only the label is read before a ticket decrypts.
func keytabOf(t *testing.T, password string, kvno uint8, etypes ...int32) []byte {
	t.Helper()
	kt := keytab.New()
	for i, et := range etypes {
		if err := kt.AddEntry(svc, realm, password, time.Unix(1, 0), kvno, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
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

// keyOf is the key of a one-entry AES256 keytab built by keytabOf.
func keyOf(t *testing.T, password string, kvno uint8) []byte {
	t.Helper()
	kt := keytab.New()
	if err := kt.Unmarshal(keytabOf(t, password, kvno, etypeID.AES256_CTS_HMAC_SHA1_96)); err != nil {
		t.Fatal(err)
	}
	return kt.Entries[0].Key.KeyValue
}

// A ticket under either version of a two-version keytab finds its key, the
// service name compared with ASCII letters folded; the next Apply without
// version 1 drops its key.
func TestApplyKeytabRotation(t *testing.T) {
	s := newServer(t)
	st := state(1)
	st.Keytab = []secrets.Version{
		{Number: 2, Value: keytabOf(t, "new", 2, etypeID.AES256_CTS_HMAC_SHA1_96)},
		{Number: 1, Value: keytabOf(t, "old", 1, etypeID.AES256_CTS_HMAC_SHA1_96)},
	}
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply of two keytab versions: %v", err)
	}
	for kvno, password := range map[uint32]string{1: "old", 2: "new"} {
		got, ok := s.KeyOf("CIFS/NAS.corp.example", realm, kvno, etypeID.AES256_CTS_HMAC_SHA1_96)
		if want := keyOf(t, password, uint8(kvno)); !ok || !bytes.Equal(got, want) {
			t.Errorf("key of kvno %d = %x (found %t), want %x", kvno, got, ok, want)
		}
	}
	st = state(2)
	st.Keytab = []secrets.Version{{Number: 2, Value: keytabOf(t, "new", 2, etypeID.AES256_CTS_HMAC_SHA1_96)}}
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply of version 2 alone: %v", err)
	}
	if _, ok := s.KeyOf(svc, realm, 1, etypeID.AES256_CTS_HMAC_SHA1_96); ok {
		t.Error("kvno 1's key found after an Apply without version 1, want gone")
	}
	if _, ok := s.KeyOf(svc, realm, 2, etypeID.AES256_CTS_HMAC_SHA1_96); !ok {
		t.Error("kvno 2's key not found after an Apply of version 2 alone")
	}
}

// A version holding an AES key and an RC4 key contributes no key, its AES
// one included, while the State, its generation and the other version's key
// are applied.
func TestApplyDropsNonAESVersion(t *testing.T) {
	s := newServer(t)
	st := state(7)
	st.Keytab = []secrets.Version{
		{Number: 2, Value: keytabOf(t, "new", 2, etypeID.AES256_CTS_HMAC_SHA1_96, etypeID.RC4_HMAC)},
		{Number: 1, Value: keytabOf(t, "old", 1, etypeID.AES128_CTS_HMAC_SHA1_96)},
	}
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply with a keytab version holding RC4 = %v, want nil", err)
	}
	if got := s.Applied().Generation; got != 7 {
		t.Errorf("applied Generation = %d, want 7", got)
	}
	if _, ok := s.KeyOf(svc, realm, 2, etypeID.AES256_CTS_HMAC_SHA1_96); ok {
		t.Error("the AES key of a version also holding RC4 found, want the version dropped whole")
	}
	if _, ok := s.KeyOf(svc, realm, 1, etypeID.AES128_CTS_HMAC_SHA1_96); !ok {
		t.Error("the AES128 key of the all-AES version not found")
	}
}

// Two versions holding one name, kvno and etype: the current version's key
// is the one held.
func TestApplyKeytabCurrentFirst(t *testing.T) {
	s := newServer(t)
	st := state(1)
	st.Keytab = []secrets.Version{
		{Number: 2, Value: keytabOf(t, "new", 1, etypeID.AES256_CTS_HMAC_SHA1_96)},
		{Number: 1, Value: keytabOf(t, "old", 1, etypeID.AES256_CTS_HMAC_SHA1_96)},
	}
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	got, _ := s.KeyOf(svc, realm, 1, etypeID.AES256_CTS_HMAC_SHA1_96)
	if want := keyOf(t, "new", 1); !bytes.Equal(got, want) {
		t.Errorf("key of kvno 1 = %x, want the current version's %x", got, want)
	}
}
