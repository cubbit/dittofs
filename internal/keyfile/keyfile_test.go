package keyfile

import (
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

const testPassphrase = "keyfile test passphrase"

// writtenByKeyprovider is a weak key file pkg/keyprovider generated before
// the format moved here, with the ID it records and the master key it wraps,
// read back by that code at the time. A test key, protecting nothing.
const (
	writtenByKeyprovider = "testdata/written-by-keyprovider.key"
	writtenID            = "1f86d48e-b5c9-475b-b6c6-135c65279dfb"
	writtenMasterKeyHex  = "7b7664d4243927dc6f2127dc2745433f7deb6b08e92e456c8c4c9ef35ab26e08"
)

func TestOpenReadsAFileTheOldCodeWrote(t *testing.T) {
	raw, err := os.ReadFile(writtenByKeyprovider)
	if err != nil {
		t.Fatal(err)
	}
	id, key, err := Open(raw, testPassphrase)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if id != writtenID {
		t.Errorf("Open's ID: got %q, want %q", id, writtenID)
	}
	if got := hex.EncodeToString(key); got != writtenMasterKeyHex {
		t.Errorf("Open's master key: got %s, want %s", got, writtenMasterKeyHex)
	}
	if _, _, err := Open(raw, "another passphrase"); !errors.Is(err, ErrUnwrap) {
		t.Errorf("Open under the wrong passphrase: got %v, want ErrUnwrap", err)
	}
}

func TestGenerateRecordsItsCostAndOpens(t *testing.T) {
	for _, tc := range []struct {
		weak    bool
		t, mKiB uint32
	}{{true, minTime, minMemoryKiB}, {false, 3, 64 * 1024}} {
		raw, err := Generate(testPassphrase, tc.weak)
		if err != nil {
			t.Fatalf("Generate(weak=%v): %v", tc.weak, err)
		}
		kf, err := decode(raw)
		if err != nil {
			t.Fatalf("decode(weak=%v): %v", tc.weak, err)
		}
		if kf.KDF.Time != tc.t || kf.KDF.MemoryKiB != tc.mKiB {
			t.Errorf("Generate(weak=%v)'s cost: got time %d memory %d KiB, want %d and %d", tc.weak, kf.KDF.Time, kf.KDF.MemoryKiB, tc.t, tc.mKiB)
		}
		id, key, err := Open(raw, testPassphrase)
		if err != nil {
			t.Fatalf("Open(Generate(weak=%v)): %v", tc.weak, err)
		}
		if id != kf.MasterKeyID || len(key) != keyLen {
			t.Errorf("Open(Generate(weak=%v)): got ID %q and %d key bytes, want %q and %d", tc.weak, id, len(key), kf.MasterKeyID, keyLen)
		}
	}
}

func TestAnEmptyPassphraseIsRefused(t *testing.T) {
	if _, err := Generate("", true); !errors.Is(err, ErrPassphrase) {
		t.Errorf("Generate with an empty passphrase: got %v, want ErrPassphrase", err)
	}
	raw, err := os.ReadFile(writtenByKeyprovider)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(raw, ""); !errors.Is(err, ErrPassphrase) {
		t.Errorf("Open with an empty passphrase: got %v, want ErrPassphrase", err)
	}
}

// A nonce of any length but GCM's is corrupt; GCM's Open panics on one.
func TestANonceOfTheWrongLengthIsCorrupt(t *testing.T) {
	raw, err := os.ReadFile(writtenByKeyprovider)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, nonce := range []string{"", "AAAA", strings.Repeat("A", 24)} {
		kf.Nonce = nonce
		if _, _, err := Open(encode(t, kf), testPassphrase); !errors.Is(err, ErrCorrupt) {
			t.Errorf("Open with nonce %q: got %v, want ErrCorrupt", nonce, err)
		}
	}
}

func encode(t testing.TB, kf *file) []byte {
	body, err := json.Marshal(kf)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: body})
}

// FuzzOpen holds the decoder to its invariants on any bytes: it never panics;
// what it accepts records parameters inside the bounds, so Argon2 cannot panic
// on them, and re-encodes to bytes that decode to the same file; and Open on a
// file at the floor's cost either fails with one of the format's errors or
// returns a 32-byte key and the ID the file records. Files above the floor's
// cost are decoded only, so a mutated memory_kib cannot take the fuzzer down.
func FuzzOpen(f *testing.F) {
	raw, err := os.ReadFile(writtenByKeyprovider)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw, testPassphrase)
	f.Add(raw, "")
	f.Add([]byte("-----BEGIN DITTOFS ENCRYPTED MASTER KEY-----\n\n-----END DITTOFS ENCRYPTED MASTER KEY-----\n"), testPassphrase)
	f.Fuzz(func(t *testing.T, raw []byte, passphrase string) {
		kf, err := decode(raw)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("decode: got %v, want ErrCorrupt", err)
			}
			return
		}
		p := kf.KDF
		if p.Time < minTime || p.Time > maxTime || p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB || p.Threads < minThreads || p.Threads > maxThreads {
			t.Fatalf("decode accepted parameters outside the bounds: %+v", p)
		}
		again, err := decode(encode(t, kf))
		if err != nil {
			t.Fatalf("decode of the re-encoded file: %v", err)
		}
		if !reflect.DeepEqual(again, kf) {
			t.Fatalf("re-encoded file decodes to %+v, want %+v", again, kf)
		}
		if p.Time != minTime || p.MemoryKiB != minMemoryKiB {
			return
		}
		id, key, err := Open(raw, passphrase)
		switch {
		case err != nil:
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnwrap) && !errors.Is(err, ErrPassphrase) {
				t.Fatalf("Open: got %v, want one of the format's errors", err)
			}
		case id != kf.MasterKeyID || len(key) != keyLen:
			t.Fatalf("Open: got ID %q and %d key bytes, want %q and %d", id, len(key), kf.MasterKeyID, keyLen)
		}
	})
}
