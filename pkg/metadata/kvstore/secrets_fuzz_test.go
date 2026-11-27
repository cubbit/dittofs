package kvstore

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
)

// FuzzSecretCodec decodes any bytes as a Secret's value: what it accepts
// decodes to the same Secret once written again, what it refuses it refuses
// with a named error, and nothing panics. Bytes may differ, since the codec
// reads a length written in more bytes than it needs. The seeds' sizes are
// chosen to reach each length's edge, not limits an RFC sets.
func FuzzSecretCodec(f *testing.F) {
	user := entity.SecretOwner{
		Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Repeat([]byte{7}, 16)},
		Key:    "password",
	}

	setting := entity.SecretOwner{
		Scope: entity.SettingScope{Kind: entity.ScopeShare, ID: [16]byte{1, 2}},
		Key:   strings.Repeat("k", 255),
	}
	for k := entity.SecretPassword; k <= entity.SecretKey; k++ {
		f.Add(
			encodeSecret(
				entity.Secret{Kind: k, KeyID: "protocol-1", Owner: user, Value: []byte("v")},
			),
		)
	}

	f.Add(encodeSecret(entity.Secret{Kind: entity.SecretKeytab, Owner: setting}))
	f.Add(encodeSecret(entity.Secret{
		Kind:       entity.SecretMachinePassword,
		KeyID:      strings.Repeat("i", 255),
		Owner:      user,
		Value:      bytes.Repeat([]byte{0xff}, 64<<10),
		KeyVersion: math.MaxUint32,
		HandedOver: true,
	}))
	f.Add(encodeSecret(entity.Secret{
		Kind:       entity.SecretMachinePassword,
		Owner:      user,
		KeyVersion: 1,
		HandedOver: true,
	}))
	good := encodeSecret(entity.Secret{Kind: entity.SecretPassword, Owner: user})

	f.Add(secretValueOf(0).Encode())
	f.Add(secretValueOf(200).Encode())
	f.Add(good[:len(good)-1])
	f.Add(append(bytes.Clone(good), 0))
	f.Add(append([]byte{v1 + 1}, good[1:]...))

	f.Fuzz(func(t *testing.T, b []byte) {
		ref := entity.SecretRef{3}

		s, err := decodeSecret(ref, 9, b)
		if err != nil {
			if !errors.Is(err, codec.ErrVersion) && !errors.Is(err, codec.ErrTruncated) &&
				!errors.Is(err, codec.ErrTrailing) && !errors.Is(err, errMalformed) {
				t.Fatalf("%x: unnamed error %v", b, err)
			}

			return
		}

		again, err := decodeSecret(ref, 9, encodeSecret(s))
		if err != nil || !reflect.DeepEqual(again, s) {
			t.Fatalf("%x: decoded %+v, written again %+v, %v", b, s, again, err)
		}
	})
}
