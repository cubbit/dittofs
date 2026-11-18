package kvstore

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Each version is its own key under the reference, numbered as the user's
// deletion scans them, so a deletion finds every version.
func TestSecretKeyLayout(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	cp := s.ControlPlane()
	ref := entity.SecretRef(uuid.New())

	owner := entity.SecretOwner{
		Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{1}},
		Key:    "password",
	}
	for v := uint64(1); v <= 2; v++ {
		sec := entity.Secret{ID: ref, Kind: entity.SecretPassword, Version: v, Owner: owner}
		if err := cp.PutSecret(ctx, sec, v-1); err != nil {
			t.Fatal(err)
		}
	}

	var got [][]byte

	err = db.View(ctx, func(r kv.Reader) error {
		for row, err := range r.Scan(keys.ID([]byte{keys.KindSEC}, ref[:]), nil) {
			if err != nil {
				return err
			}

			got = append(got, slices.Clone(row.Key))
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := [][]byte{secretVersionKey(ref, 1), secretVersionKey(ref, 2)}
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("keys under the reference: got %x, want %x", got, want)
	}
}

// secretValueOf encodes the fields of a Secret's value with kind as given,
// independently of the encoder under test.
func secretValueOf(kind uint8) *codec.Writer {
	return codec.NewWriter(v1).Uint8(kind).String("k").
		Uint8(uint8(entity.ResourceUser)).Bytes([]byte{1}).
		Uint8(0).Fixed(make([]byte, 16)).String("password").
		Bytes([]byte("sealed")).Uint32(0).Bool(false)
}

// A value this build cannot read as a Secret is refused, never read as some
// other kind or with fields missing.
func TestSecretCodecRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ref := entity.SecretRef{1}

	good := secretValueOf(uint8(entity.SecretPassword)).Encode()
	if _, err := decodeSecret(ref, 1, good); err != nil {
		t.Fatalf("a valid value: %v", err)
	}

	for _, c := range []struct {
		name string
		b    []byte
		want error
	}{
		{"kind 0", secretValueOf(0).Encode(), errMalformed},
		{"kind 200", secretValueOf(200).Encode(), errMalformed},
		{"truncated", good[:len(good)-1], nil},
		{"a trailing byte", append(slices.Clone(good), 0), nil},
		{"another codec version", append([]byte{v1 + 1}, good[1:]...), nil},
	} {
		_, err := decodeSecret(ref, 1, c.b)
		if err == nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}

// A decoded Secret owns its bytes: the buffer it was read from may be
// reused once the read moves on, and the Secret must not change with it.
func TestSecretDecodeCopies(t *testing.T) {
	prof.Watch(t, prof.Options{})

	b := secretValueOf(uint8(entity.SecretPassword)).Encode()

	s, err := decodeSecret(entity.SecretRef{1}, 1, b)
	if err != nil {
		t.Fatal(err)
	}

	for i := range b {
		b[i] = 0xee
	}

	if !bytes.Equal(s.Value, []byte("sealed")) {
		t.Errorf("Value after the buffer changed: got %q, want %q", s.Value, "sealed")
	}

	if !bytes.Equal(s.Owner.Record.ID, []byte{1}) {
		t.Errorf("Owner.Record.ID after the buffer changed: got %x, want 01", s.Owner.Record.ID)
	}
}

// The current version is found below a bound above every version, the
// largest one included.
func TestSecretCurrentAtMaxVersion(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	ref := entity.SecretRef{2}
	value := secretValueOf(uint8(entity.SecretPassword)).Encode()

	err = db.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(secretKey(ref, math.MaxUint64), value)
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		got entity.Secret
		ok  bool
	)

	err = db.View(ctx, func(r kv.Reader) (err error) {
		got, ok, err = secretIn(r, ref, 0)

		return err
	})
	if err != nil || !ok || got.Version != math.MaxUint64 {
		t.Errorf("current: got version %d, %v, %v, want %d, true, nil", got.Version, ok, err,
			uint64(math.MaxUint64))
	}
}
