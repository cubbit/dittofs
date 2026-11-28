package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// runCustody holds a custody to the rows every custody passes: a wrap
// unwraps under its own associated data and under no other, a changed byte
// of the stored wrap does not unwrap, every wrap records the master-key ID
// primary reports, and that master key's state is active.
func runCustody(t *testing.T, newCustody func(t *testing.T) custody) {
	t.Helper()

	ns := bytes.Repeat([]byte{0x30}, 16)
	aad := wrapAAD(ns, keys.KindDataKey, "k1")
	key := bytes.Repeat([]byte{0x42}, 32)

	t.Run("a wrap unwraps under its AAD only", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		ctx := context.Background()
		c := newCustody(t)

		stored, err := c.wrap(ctx, sealIn{aad: aad, key: key})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		got, err := c.unwrap(ctx, stored[0], aad)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("unwrap under the wrap's own AAD = %x, %v; want %x", got, err, key)
		}

		for _, tc := range []struct {
			name string
			aad  []byte
		}{
			{"another namespace", wrapAAD(bytes.Repeat([]byte{0x31}, 16), keys.KindDataKey, "k1")},
			{"another kind", wrapAAD(ns, keys.KindChunkingKey, "k1")},
			{"another key ID", wrapAAD(ns, keys.KindDataKey, "k2")},
		} {
			if got, err := c.unwrap(ctx, stored[0], tc.aad); err == nil {
				t.Errorf("unwrap under the AAD of %s = %x, want an error", tc.name, got)
			}
		}
	})

	t.Run("a changed byte does not unwrap", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		ctx := context.Background()
		c := newCustody(t)

		stored, err := c.wrap(ctx, sealIn{aad: aad, key: key})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		s := stored[0]
		w, err := parseStoredWrap(s)
		if err != nil {
			t.Fatalf("parse the stored wrap: %v", err)
		}
		// The kind, the master-key ID's last byte, and the wrap bytes' first,
		// fourth, middle and last: each field of either custody's layout.
		start := len(s) - len(w.wrap)
		for _, at := range []int{0, start - 5, start, start + 3, start + len(w.wrap)/2, len(s) - 1} {
			flipped := bytes.Clone(s)
			flipped[at] ^= 0x01
			if got, err := c.unwrap(ctx, flipped, aad); err == nil {
				t.Errorf("unwrap of the stored wrap with byte %d of %d flipped = %x, want an error", at, len(s), got)
			}
		}
	})

	t.Run("a wrap records the primary version, which is active", func(t *testing.T) {
		prof.Watch(t, prof.Options{})
		ctx := context.Background()
		c := newCustody(t)

		primary, err := c.primary(ctx)
		if err != nil {
			t.Fatalf("primary: %v", err)
		}
		stored, err := c.wrap(ctx, sealIn{aad: aad, key: key})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		s, err := parseStoredWrap(stored[0])
		if err != nil {
			t.Fatalf("parse the stored wrap: %v", err)
		}
		if s.kind != c.kind() || !bytes.Equal(s.masterKeyID, primary) {
			t.Errorf("stored wrap of kind %#x under master key %q; want the custody's kind %#x and primary's %q", s.kind, s.masterKeyID, c.kind(), primary)
		}
		if err := c.state(ctx, primary); err != nil {
			t.Errorf("state of the primary version %q = %v, want nil", primary, err)
		}
	})
}

func TestKeyFileCustody(t *testing.T) {
	newCustody := func(t *testing.T) custody {
		p, err := build([16]byte{1, 2, 3}, bytes.Repeat([]byte{0x07}, 32), Deps{Records: keystest.NewRecords(), Rand: rand.Reader})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return p.custody
	}
	runCustody(t, newCustody)

	// A key file holds one master key: any other ID is not one it can serve.
	if err := newCustody(t).state(context.Background(), make([]byte, 16)); !errors.Is(err, keys.ErrMaterialUnavailable) {
		t.Errorf("key-file state of another master key ID = %v, want ErrMaterialUnavailable", err)
	}
}
