package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/keyfile"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// Both views satisfy KeyRecords with methods written over the literal struct,
// importing nothing of this package.
var (
	_ KeyRecords = (*keystest.Records)(nil)
	_ KeyRecords = kvstore.KeyRecords{}
)

const passphrase = "correct horse battery staple"

// keyFile writes a fresh key file under t.TempDir() and returns its path, its
// master key ID and its master key.
func keyFile(t *testing.T) (path, id string, key []byte) {
	t.Helper()

	raw, err := keyfile.Generate(passphrase, true)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id, key, err = keyfile.Open(raw, passphrase)
	if err != nil {
		t.Fatalf("Open of a generated key file: %v", err)
	}
	path = filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	return path, id, key
}

// newProvider builds a provider over a fresh key file and Records, drawing
// from rnd.
func newProvider(t *testing.T, rnd *bytes.Reader) (*Provider, *keystest.Records) {
	t.Helper()

	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	deps := Deps{Records: records, Rand: rand.Reader}
	if rnd != nil {
		deps.Rand = rnd
	}
	p, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p, records
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func span(from, to byte) []byte {
	b := make([]byte, 0, int(to)-int(from))
	for c := from; c < to; c++ {
		b = append(b, c)
	}
	return b
}

// The vector testdata/stored-wrap-v1.py computes from RFC 5 Appendix B.1's
// text with another AES-GCM implementation: the AAD from its fields, the
// stored wrap from its master key, nonce and key byte for byte, and the
// vector's stored wrap unwrapping to its key. A field order, a length width
// or an endianness apart from RFC 5's fails here.
func TestStoredWrapVector(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		wantAAD    = "00000010303132333435363738393a3b3c3d3e3f00000008646174612d6b657900000010404142434445464748494a4b4c4d4e4f"
		wantStored = "010010101112131415161718191a1b1c1d1e1f0000003ca0a1a2a3a4a5a6a7a8a9aaabb6492e7e119e54e83a3cdd885b279e8110cd3b73f6d2240bf4674ced13c61b6e1c55fe711f450481e2d4ff6ca5737d42"
	)

	var masterKeyID [16]byte
	copy(masterKeyID[:], span(0x10, 0x20))
	nonce, ns, keyID, key := span(0xA0, 0xAC), span(0x30, 0x40), string(span(0x40, 0x50)), span(0x50, 0x70)

	rnd := bytes.NewReader(nonce)
	p, err := build(masterKeyID, span(0x00, 0x20), Deps{Records: keystest.NewRecords(), Rand: rnd})
	if err != nil {
		t.Fatal(err)
	}

	aad := wrapAAD(ns, keys.KindDataKey, keyID)
	if got := hex.EncodeToString(aad); got != wantAAD {
		t.Errorf("key wrap AAD %s, want the vector's %s", got, wantAAD)
	}

	wraps, err := p.wrap(context.Background(), sealIn{aad: aad, key: key})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if got := hex.EncodeToString(wraps[0]); got != wantStored {
		t.Errorf("stored wrap %s, want the vector's %s", got, wantStored)
	}
	if rnd.Len() != 0 {
		t.Errorf("wrap left %d of the vector's 12 nonce bytes unread, want 0", rnd.Len())
	}

	got, err := p.unwrap(mustHex(t, wantStored), mustHex(t, wantAAD))
	if err != nil {
		t.Fatalf("unwrap of the vector's stored wrap: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Errorf("the vector's stored wrap unwrapped to %x, want its key %x", got, key)
	}
}

// A wrap unwraps only under the AAD it was made with, and only unchanged; the
// master key ID it records is the provider's. A wrap that binds nothing, or
// binds only some of the namespace, kind and ID, fails here.
func TestWrapBindsNamespaceKindAndID(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, _ := newProvider(t, nil)
	ns, other := entity.NewNamespaceID(), entity.NewNamespaceID()
	key := span(0x50, 0x70)
	aad := wrapAAD(ns[:], keys.KindDataKey, "id-1")

	wraps, err := p.wrap(context.Background(), sealIn{aad: aad, key: key})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	stored := wraps[0]

	got, err := p.unwrap(stored, aad)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap under its own AAD = %x, %v; want the key", got, err)
	}

	s, err := parseStoredWrap(stored)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.kind != kindKeyFile || !bytes.Equal(s.masterKeyID, p.masterKeyID[:]) {
		t.Errorf("stored wrap records kind %#x, master key %x; want %#x, %x", s.kind, s.masterKeyID, kindKeyFile, p.masterKeyID)
	}

	for _, tc := range []struct {
		name string
		aad  []byte
	}{
		{"another namespace", wrapAAD(other[:], keys.KindDataKey, "id-1")},
		{"another kind", wrapAAD(ns[:], keys.KindChunkingKey, "id-1")},
		{"another key ID", wrapAAD(ns[:], keys.KindDataKey, "id-2")},
		{"no namespace", wrapAAD(nil, keys.KindDataKey, "id-1")},
	} {
		if got, err := p.unwrap(stored, tc.aad); err == nil {
			t.Errorf("unwrap under %s's AAD = %x, nil; want an error", tc.name, got)
		}
	}

	for i := range stored {
		flipped := bytes.Clone(stored)
		flipped[i] ^= 0x01
		if got, err := p.unwrap(flipped, aad); err == nil {
			t.Errorf("unwrap with byte %d flipped = %x, nil; want an error", i, got)
		}
	}
}

// ValidateConfig reads no file: a valid Config whose key file is absent
// passes, and each malformed value is refused.
func TestValidateConfig(t *testing.T) {
	prof.Watch(t, prof.Options{})

	absent := filepath.Join(t.TempDir(), "absent.key")
	good := Config{KeyFile: absent, Passphrase: "p", Escrowed: []Escrow{{MasterKeyID: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}}
	if err := ValidateConfig(good); err != nil {
		t.Errorf("ValidateConfig of a valid Config whose key file is absent = %v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"an empty key file", func(c *Config) { c.KeyFile = "" }},
		{"an empty passphrase", func(c *Config) { c.Passphrase = "" }},
		{"an escrow ID that is not a UUID", func(c *Config) { c.Escrowed = []Escrow{{MasterKeyID: "master-1"}} }},
		{"an escrow ID that is not a UUID's text", func(c *Config) {
			c.Escrowed = []Escrow{{MasterKeyID: "{6ba7b810-9dad-11d1-80b4-00c04fd430c8}"}}
		}},
	} {
		cfg := good
		tc.edit(&cfg)
		if err := ValidateConfig(cfg); err == nil {
			t.Errorf("ValidateConfig with %s = nil, want an error", tc.name)
		}
	}
}

// With the count one short of 2³², one wrap succeeds and the next, in another
// namespace, is refused with ErrWrapLimit naming the master key, having drawn
// no nonce. A bound off by one, a count kept per namespace, or a nonce drawn
// before the count fails here.
func TestWrapCountStopsAtTwoToTheThirtyTwo(t *testing.T) {
	prof.Watch(t, prof.Options{})

	rnd := bytes.NewReader(make([]byte, 2*nonceLen))
	p, records := newProvider(t, rnd)
	ctx := context.Background()

	if ok, err := records.RaiseWrapCount(ctx, p.masterKeyID[:], wrapLimit-1, wrapLimit); !ok || err != nil {
		t.Fatalf("raise the count to 2³² − 1 = %v, %v; want true, nil", ok, err)
	}

	a, b := entity.NewNamespaceID(), entity.NewNamespaceID()
	if _, err := p.wrap(ctx, sealIn{aad: wrapAAD(a[:], keys.KindDataKey, "a"), key: make([]byte, 32)}); err != nil {
		t.Fatalf("the 2³²nd wrap: %v, want nil", err)
	}

	before := rnd.Len()
	_, err := p.wrap(ctx, sealIn{aad: wrapAAD(b[:], keys.KindDataKey, "b"), key: make([]byte, 32)})
	if !errors.Is(err, keys.ErrWrapLimit) {
		t.Fatalf("the wrap past 2³² = %v, want keys.ErrWrapLimit", err)
	}
	if want := fmt.Sprintf("%x", p.masterKeyID); !strings.Contains(err.Error(), want) {
		t.Errorf("ErrWrapLimit %q does not name master key %s", err, want)
	}
	if rnd.Len() != before {
		t.Errorf("the refused wrap drew %d random bytes, want 0", before-rnd.Len())
	}
}

// New fails for a key file it cannot read or unwrap, and for a master key
// that is a storage key by ID, by bytes under another ID, retired or current,
// or by ID with no fingerprint. A check of the ID alone, or of the current
// storage key alone, fails here.
func TestNewRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	path, id, key := keyFile(t)
	_, otherID, otherKey := keyFile(t)
	fp, otherFP := fingerprint(key), fingerprint(otherKey)

	for _, tc := range []struct {
		name    string
		cfg     Config
		storage []StorageKey
	}{
		{"a missing key file", Config{KeyFile: filepath.Join(t.TempDir(), "absent.key"), Passphrase: passphrase}, nil},
		{"a wrong passphrase", Config{KeyFile: path, Passphrase: "wrong"}, nil},
		{"the storage key's ID", Config{KeyFile: path, Passphrase: passphrase}, []StorageKey{{ID: id, Fingerprint: otherFP}}},
		{"the storage key's bytes under another ID", Config{KeyFile: path, Passphrase: passphrase}, []StorageKey{{ID: otherID, Fingerprint: fp}}},
		{"a retired storage key's bytes", Config{KeyFile: path, Passphrase: passphrase}, []StorageKey{{ID: otherID, Fingerprint: otherFP}, {ID: "retired", Fingerprint: fp}}},
		{"the master key's ID with a zero fingerprint", Config{KeyFile: path, Passphrase: passphrase}, []StorageKey{{ID: id}}},
	} {
		deps := Deps{Records: keystest.NewRecords(), Rand: rand.Reader, StorageKeys: tc.storage}
		if p, err := New(context.Background(), tc.cfg, deps); err == nil {
			t.Errorf("New with %s = %v, nil; want an error", tc.name, p)
		}
	}

	deps := Deps{Records: keystest.NewRecords(), Rand: rand.Reader, StorageKeys: []StorageKey{{ID: otherID, Fingerprint: otherFP}, {ID: "unread"}}}
	if _, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase}, deps); err != nil {
		t.Errorf("New beside storage keys that differ by ID and bytes = %v, want nil", err)
	}
}

// A stored wrap's parse never panics or reads past its buffer, and what it
// reads re-encodes to bytes that read back to the same value.
func FuzzStoredWrap(f *testing.F) {
	f.Add([]byte{})
	f.Add(storedWrap{kind: kindKeyFile, masterKeyID: span(0x10, 0x20), wrap: span(0, 60)}.encode())
	f.Add(storedWrap{kind: kindKeyFile, wrap: span(0, 28)}.encode())              // a zero-length master key ID
	f.Add([]byte{kindKeyFile, 0xFF, 0xFF, 1, 2, 3})                               // an ID length past the end
	f.Add([]byte{kindKeyService, 0x00, 0x01, 0x42, 0xFF, 0xFF, 0xFF, 0xFF, 1, 2}) // a wrap length past the end
	f.Add([]byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})                       // an unknown provider kind

	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parseStoredWrap(b)
		if err != nil {
			return
		}
		again := s.encode()
		if !bytes.Equal(again, b) {
			t.Fatalf("parse then encode of %x gave %x", b, again)
		}
		s2, err := parseStoredWrap(again)
		if err != nil || s2.kind != s.kind || !bytes.Equal(s2.masterKeyID, s.masterKeyID) || !bytes.Equal(s2.wrap, s.wrap) {
			t.Fatalf("re-encoded %x read back as %+v, %v; want %+v", again, s2, err, s)
		}
	})
}
