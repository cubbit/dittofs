package secrets

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// katAD is the additional data of katBinding, written by hand from the
// envelope's layout: each field a four-byte big-endian length, then its bytes.
const katAD = "" +
	"00000001" + "01" + // role
	"00000001" + "02" + // kind
	"00000001" + "08" + // owner's record kind
	"00000002" + "6964" + // owner's record ID "id"
	"00000001" + "03" + // owner's scope tag
	"00000010" + "0102030405060708090a0b0c0d0e0f10" + // owner's scope ID
	"00000001" + "6b" + // owner's key "k"
	"00000010" + "1112131415161718191a1b1c1d1e1f20" + // reference
	"00000008" + "0102030405060708" + // version
	"00000001" + "01" // handed over

// katSealed is "secret value" sealed with AES-256-GCM under the key
// 00 01 .. 1f, the nonce a0 .. ab and katAD, as nonce, ciphertext and tag. It
// was computed by a program outside this package, so it pins the sealed
// layout and the cipher rather than whatever this code produces.
const katSealed = "a0a1a2a3a4a5a6a7a8a9aaab" +
	"957d1f5f20bf22c90309f2b6" +
	"5571e69481941f98cd740c69b1d5c526"

// katBinding is the binding katAD encodes.
func katBinding() binding {
	var scopeID [16]byte
	for i := range scopeID {
		scopeID[i] = byte(0x01 + i)
	}

	var ref entity.SecretRef
	for i := range ref {
		ref[i] = byte(0x11 + i)
	}

	return binding{
		role: Protocol,
		kind: entity.SecretNTHash,
		owner: entity.SecretOwner{
			Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte("id")},
			Scope:  entity.SettingScope{Kind: entity.ScopeNamespace, ID: scopeID},
			Key:    "k",
		},
		ref:        ref,
		version:    0x0102030405060708,
		handedOver: true,
	}
}

func katKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	return key
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}

	return b
}

// TestEnvelopeOpensOnlyWithItsAdditionalData seals under one binding and
// opens under the same binding with one field changed, so a field left out
// of the additional data lets a value moved onto another holder, version or
// mark still open.
func TestEnvelopeOpensOnlyWithItsAdditionalData(t *testing.T) {
	prof.Watch(t, prof.Options{})

	value := []byte("canary value")

	cases := []struct {
		name string
		// seal and open change the binding the value is sealed under and
		// the one it is opened under; nil leaves katBinding as it is.
		seal, open func(b *binding)
	}{
		{"role", nil, func(b *binding) {
			b.role = Storage
		}},
		{"kind", nil, func(b *binding) {
			b.kind = entity.SecretKeytab
		}},
		{"record kind", nil, func(b *binding) {
			b.owner.Record.Kind = entity.ResourceGroup
		}},
		{"record ID", nil, func(b *binding) {
			b.owner.Record.ID = []byte("ie")
		}},
		{"scope tag", nil, func(b *binding) {
			b.owner.Scope.Kind = entity.ScopeShare
		}},
		{"scope ID", nil, func(b *binding) {
			b.owner.Scope.ID[15] ^= 0xff
		}},
		{"key", nil, func(b *binding) {
			b.owner.Key = "j"
		}},
		{"reference first byte", nil, func(b *binding) {
			b.ref[0] ^= 0xff
		}},
		{"reference last byte", nil, func(b *binding) {
			b.ref[15] ^= 0xff
		}},
		{"version high byte", func(b *binding) {
			b.version = 1
		}, func(b *binding) {
			b.version = 1<<32 | 1
		}},
		{"handed over", nil, func(b *binding) {
			b.handedOver = false
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sealing := katBinding()
			if tc.seal != nil {
				tc.seal(&sealing)
			}

			dataKey, sealed, err := sealValue(value, sealing.additionalData())
			if err != nil {
				t.Fatalf("sealValue: %v", err)
			}

			got, err := openValue(dataKey, sealed, sealing.additionalData())
			if err != nil || !bytes.Equal(got, value) {
				t.Fatalf("open under the sealing binding = %q, %v; want %q, nil", got, err, value)
			}

			opening := sealing
			opening.owner.Record.ID = bytes.Clone(sealing.owner.Record.ID)
			tc.open(&opening)

			got, err = openValue(dataKey, sealed, opening.additionalData())
			if !errors.Is(err, errOpen) {
				t.Errorf("open with %s changed = %q, %v; want errOpen", tc.name, got, err)
			}
		})
	}
}

// TestAdditionalDataPrefixesEachOwnerPart uses two owners whose parts
// concatenate to the same bytes when nothing is length-prefixed: the record
// ID's tail in one is the other's key, across the fixed-size scope.
func TestAdditionalDataPrefixesEachOwnerPart(t *testing.T) {
	prof.Watch(t, prof.Options{})

	zeros := string(make([]byte, 17))

	a := katBinding()
	a.owner.Scope = entity.SettingScope{}
	a.owner.Record.ID = []byte("a")
	a.owner.Key = zeros

	b := katBinding()
	b.owner.Scope = entity.SettingScope{}
	b.owner.Record.ID = []byte("a" + zeros)
	b.owner.Key = ""

	if bytes.Equal(a.additionalData(), b.additionalData()) {
		t.Fatalf("two owners bind the same additional data %x", a.additionalData())
	}

	for _, tc := range []struct {
		name       string
		seal, open binding
	}{
		{"a opened as b", a, b},
		{"b opened as a", b, a},
	} {
		dataKey, sealed, err := sealValue([]byte("v"), tc.seal.additionalData())
		if err != nil {
			t.Fatalf("%s: sealValue: %v", tc.name, err)
		}

		got, err := openValue(dataKey, sealed, tc.open.additionalData())
		if !errors.Is(err, errOpen) {
			t.Errorf("%s = %q, %v; want errOpen", tc.name, got, err)
		}
	}
}

// TestSealUsesAFreshDataKey seals one value twice: a data key reused across
// versions, or a short one, would show here.
func TestSealUsesAFreshDataKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ad := katBinding().additionalData()
	value := []byte("same value")

	key1, sealed1, err := sealValue(value, ad)
	if err != nil {
		t.Fatalf("first sealValue: %v", err)
	}

	key2, sealed2, err := sealValue(value, ad)
	if err != nil {
		t.Fatalf("second sealValue: %v", err)
	}

	zero := make([]byte, 32)

	for i, key := range [][]byte{key1, key2} {
		if len(key) != 32 {
			t.Fatalf("data key %d is %d bytes, want 32", i+1, len(key))
		}

		if bytes.Equal(key, zero) || bytes.Equal(key[16:], zero[16:]) {
			t.Errorf("data key %d = %x, want random in all 32 bytes", i+1, key)
		}

		if bytes.Equal(key[:16], key[16:]) {
			t.Errorf("data key %d = %x has two equal halves, want 32 random bytes", i+1, key)
		}
	}

	if bytes.Equal(key1, key2) {
		t.Errorf("two seals used the same data key %x", key1)
	}

	if len(sealed1) < 12 || len(sealed2) < 12 || bytes.Equal(sealed1[:12], sealed2[:12]) {
		t.Errorf("nonces %x and %x, want two different 12-byte nonces", sealed1, sealed2)
	}

	got, err := openValue(key2, sealed1, ad)
	if !errors.Is(err, errOpen) {
		t.Errorf("first seal opened under the second key = %q, %v; want errOpen", got, err)
	}
}

// TestEnvelopeLayout pins the bytes: a reordered field or another prefix
// width passes every round trip, and makes every stored secret fail to open
// after an upgrade.
func TestEnvelopeLayout(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sealed := bytes.Repeat([]byte{0x5e}, 28)
	want := append(mustHex(t, "0100000003a1a2a3"), sealed...)

	if got := buildEnvelope([]byte{0xa1, 0xa2, 0xa3}, sealed); !bytes.Equal(got, want) {
		t.Errorf("buildEnvelope = %x, want %x", got, want)
	}

	if got := katBinding().additionalData(); !bytes.Equal(got, mustHex(t, katAD)) {
		t.Errorf("additionalData = %x, want %s", got, katAD)
	}
}

// namedEnvelope is one envelope of a table, named for its failure message.
type namedEnvelope struct {
	name string
	env  []byte
}

// malformedEnvelopes are the envelopes the parser refuses, in a fixed order so
// the fuzz seeds keep their numbers.
func malformedEnvelopes() []namedEnvelope {
	sealed := bytes.Repeat([]byte{0x5e}, 28)
	env := func(head string, tail ...byte) []byte {
		b, _ := hex.DecodeString(head)

		return append(b, tail...)
	}

	return []namedEnvelope{
		{"empty", []byte{}},
		{"format 0", env("0000000001aa", sealed...)},
		{"format 2", env("0200000001aa", sealed...)},
		{"format byte only", env("01")},
		{"length cut in two", env("01000000")},
		{"header only", env("0100000001")},
		{"wrapped length past end", env("0100000040aa", sealed...)},
		{"wrapped length 0xFFFFFFFF", env("01ffffffffaa", sealed...)},
		{"wrapped length 0", env("0100000000", sealed...)},
		{"sealed part of 27 bytes", env("0100000001aa", sealed[:27]...)},
	}
}

// validEnvelope is the shortest envelope the parser accepts: a one-byte
// wrapped key and 28 sealed bytes, the nonce and tag of an empty value.
func validEnvelope() []byte {
	return append([]byte{0x01, 0, 0, 0, 1, 0xaa}, bytes.Repeat([]byte{0x5e}, 28)...)
}

// TestParseEnvelopeRefusesMalformed checks that an unknown format or a
// truncated envelope is refused, never read short.
func TestParseEnvelopeRefusesMalformed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range malformedEnvelopes() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: parseEnvelope panicked: %v", tc.name, r)
				}
			}()

			wrapped, sealed, err := parseEnvelope(tc.env)
			if !errors.Is(err, errEnvelope) {
				t.Errorf(
					"%s: parseEnvelope = %x, %x, %v; want errEnvelope",
					tc.name,
					wrapped,
					sealed,
					err,
				)
			}
		}()
	}

	wrapped, sealed, err := parseEnvelope(validEnvelope())
	if err != nil {
		t.Fatalf("valid envelope: parseEnvelope: %v", err)
	}

	if !bytes.Equal(wrapped, []byte{0xaa}) || !bytes.Equal(sealed, bytes.Repeat([]byte{0x5e}, 28)) {
		t.Errorf("valid envelope parsed to %x, %x; want aa and 28 bytes of 5e", wrapped, sealed)
	}

	if cap(wrapped) != len(wrapped) {
		t.Errorf(
			"wrapped key has capacity %d over length %d, so an append overwrites the sealed part",
			cap(wrapped),
			len(wrapped),
		)
	}
}

// TestOpenValueRefusesBadInput checks that a short sealed value or a key of
// the wrong size is errOpen, never a panic.
func TestOpenValueRefusesBadInput(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ad := mustHex(t, katAD)
	sealed := mustHex(t, katSealed)

	cases := []struct {
		name        string
		key, sealed []byte
	}{
		{"sealed of 27 bytes", katKey(), sealed[:27]},
		{"sealed of 0 bytes", katKey(), nil},
		{"key of 31 bytes", katKey()[:31], sealed},
	}

	for _, tc := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: openValue panicked: %v", tc.name, r)
				}
			}()

			got, err := openValue(tc.key, tc.sealed, ad)
			if !errors.Is(err, errOpen) {
				t.Errorf("%s: openValue = %q, %v; want errOpen", tc.name, got, err)
			}
		}()
	}
}

// TestOpenValueKnownAnswer opens a value sealed outside this package, so the
// sealed layout and the cipher are pinned, not only agreed between seal and
// open.
func TestOpenValueKnownAnswer(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got, err := openValue(katKey(), mustHex(t, katSealed), mustHex(t, katAD))
	if err != nil || string(got) != "secret value" {
		t.Errorf("openValue of the known answer = %q, %v; want \"secret value\", nil", got, err)
	}
}
