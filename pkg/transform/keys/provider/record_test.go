package provider

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
)

// vectorFingerprint is the fingerprint testdata/fingerprint-v1.py computes, of
// the stored-wrap vector's key, bytes 0x50 to 0x6f.
//
// ponytail: the vector is this package's own, computed by a script beside it,
// until RFC 5's golden vectors have a shared loader; move it there then.
const vectorFingerprint = "66ddc3662036343666cf46b9c3e161bf2e81e1d00119f62f96e4c0ec19780ec9"

// The fingerprint of the vector's material equals the vector, which another
// HMAC implementation computed from RFC 5 §2.5's text. A label, a key and
// message swapped, or another hash fails here.
func TestFingerprintVector(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fp := fingerprint(span(0x50, 0x70))
	if got := hex.EncodeToString(fp[:]); got != vectorFingerprint {
		t.Errorf("fingerprint of the vector's material = %s, want the vector's %s", got, vectorFingerprint)
	}
}

// recordSeeds are a wrapped record, a clear one and a destroyed one.
func recordSeeds() [][]byte {
	fp := fingerprint(span(0x50, 0x70))
	stored, err := hex.DecodeString(vectorStored)
	if err != nil {
		panic(err)
	}
	return [][]byte{
		keyRecord{fingerprint: fp, since: 1, masterKeyID: span(0x10, 0x20), key: stored}.encode(),
		keyRecord{fingerprint: fp, since: 2, key: span(0x50, 0x70)}.encode(),
		keyRecord{fingerprint: fp, since: 3}.encode(),
	}
}

// Version 1's layout, field by field, and its round trip. A field moved, a
// length width or an endianness changed fails here.
func TestKeyRecordLayout(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := keyRecord{fingerprint: [32]byte{0: 0xf0, 31: 0xf1}, since: 0x0102030405060708, masterKeyID: []byte{0xaa, 0xbb}, key: []byte{0xcc}}
	want := append([]byte{recordVersion1}, r.fingerprint[:]...)
	want = append(want, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 2, 0xaa, 0xbb, 0, 0, 0, 1, 0xcc)
	if got := r.encode(); !bytes.Equal(got, want) {
		t.Fatalf("encode = %x, want %x", got, want)
	}

	got, err := decodeKeyRecord(want)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.fingerprint != r.fingerprint || got.since != r.since || !bytes.Equal(got.masterKeyID, r.masterKeyID) || !bytes.Equal(got.key, r.key) {
		t.Errorf("decode = %+v, want %+v", got, r)
	}
}

// Every malformed record is refused as errKeyRecord. A decoder that trusts a
// length, reads an unknown version as version 1 or ignores trailing bytes
// fails here.
func TestKeyRecordRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	good := recordSeeds()[0]
	bumped := func(at int, by uint32) []byte {
		b := bytes.Clone(good)
		binary.BigEndian.PutUint32(b[at:], binary.BigEndian.Uint32(b[at:])+by)
		return b
	}

	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"version 0", append([]byte{0}, good[1:]...)},
		{"version 2", append([]byte{2}, good[1:]...)},
		{"cut inside the header", good[:40]},
		{"no key field", good[:41+4+16]},
		{"master key ID past the end", bumped(41, 1<<20)},
		{"key past the end", bumped(41+4+16, 1)},
		{"a byte after the key", append(bytes.Clone(good), 0)},
	} {
		if _, err := decodeKeyRecord(tc.b); !errors.Is(err, errKeyRecord) {
			t.Errorf("decode of %s = %v, want errKeyRecord", tc.name, err)
		}
	}
}

// FuzzKeyRecord holds the decoder to: no panic, nothing read past the input,
// and what it decodes re-encodes to the input itself, since version 1 has one
// encoding of each value.
func FuzzKeyRecord(f *testing.F) {
	for _, s := range recordSeeds() {
		f.Add(s)
	}
	good := recordSeeds()[0]
	f.Add(append([]byte{2}, good[1:]...))
	f.Add(good[:50])
	f.Add(append(bytes.Clone(good[:41]), 0xff, 0xff, 0xff, 0xff))

	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := decodeKeyRecord(b)
		if err != nil {
			if !errors.Is(err, errKeyRecord) {
				t.Fatalf("decode refused with %v, want errKeyRecord", err)
			}
			return
		}
		if got := r.encode(); !bytes.Equal(got, b) {
			t.Fatalf("decode then encode = %x, want the input %x", got, b)
		}
	})
}

// A record the provider builds enters its state at the injected clock's time.
// A since taken from the wall clock, or in another unit, fails here.
func TestRecordTakesTheInjectedClock(t *testing.T) {
	prof.Watch(t, prof.Options{})

	at := time.Date(2026, 11, 28, 12, 0, 0, 7, time.UTC)
	p, err := build([16]byte{}, make([]byte, 32), Deps{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeKeyRecord(p.record([32]byte{}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(0, r.since).UTC(); !got.Equal(at) {
		t.Errorf("record entered its state at %v, want the injected clock's %v", got, at)
	}
}
