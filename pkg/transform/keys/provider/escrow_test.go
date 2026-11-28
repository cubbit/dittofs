package provider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/cubbit/dittofs/internal/keyfile"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// escrowProvider builds a provider over a fresh key file with escrowed as its
// passes, and returns the file's path, master key ID and master key beside it.
func escrowProvider(t *testing.T, escrowed ...Escrow) (p *Provider, records *keystest.Records, path, id string, key []byte) {
	t.Helper()

	path, id, key = keyFile(t)
	records = keystest.NewRecords()
	p, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase, Escrowed: escrowed}, Deps{Records: records, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, records, path, id, key
}

func read(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// isNotEscrowed reports whether err is ErrNotEscrowed naming p's master key.
func isNotEscrowed(p *Provider, err error) bool {
	return errors.Is(err, keys.ErrNotEscrowed) && strings.Contains(err.Error(), fmt.Sprintf("%x", p.masterKeyID))
}

// A copy of the master key's file passes and returns its ID and fingerprint;
// a copy of another key file, a copy under the master key's ID holding other
// bytes, and an unreadable copy each fail with ErrNotEscrowed naming the key.
// No check, passed or failed, opens the gate. A check of the ID alone, or one
// that never unwraps, fails here.
func TestVerifyEscrow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, _, path, id, key := escrowProvider(t)
	otherPath, _, _ := keyFile(t)
	ctx := context.Background()

	got, err := p.VerifyEscrow(ctx, read(t, path))
	if err != nil {
		t.Fatalf("VerifyEscrow of the master key file's copy = %v, want nil", err)
	}
	if want := (Escrow{MasterKeyID: id, Fingerprint: fingerprint(key)}); got != want {
		t.Errorf("VerifyEscrow pass = %+v, want %+v", got, want)
	}
	if err := p.checkEscrow(); !isNotEscrowed(p, err) {
		t.Errorf("gate after a pass not handed back = %v, want ErrNotEscrowed naming the key", err)
	}

	for _, tc := range []struct {
		name string
		copy []byte
	}{
		{"another key file", read(t, otherPath)},
		{"other bytes under the master key's ID", read(t, relabel(t, otherPath, id))},
		{"unreadable bytes", []byte("not a key file")},
	} {
		got, err := p.VerifyEscrow(ctx, tc.copy)
		if !isNotEscrowed(p, err) {
			t.Errorf("VerifyEscrow of %s = %+v, %v; want ErrNotEscrowed naming the key", tc.name, got, err)
		}
		if err := p.checkEscrow(); !isNotEscrowed(p, err) {
			t.Errorf("gate after VerifyEscrow of %s = %v, want ErrNotEscrowed naming the key", tc.name, err)
		}
	}
}

// Each escrow check's test record raises the wrap count by exactly one: from
// 2³² − 2, two checks pass and the third is refused with ErrWrapLimit. An
// uncounted test record, or one counted twice, fails here.
func TestVerifyEscrowCountsOneWrap(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, records, path, _, _ := escrowProvider(t)
	ctx := context.Background()
	if ok, err := records.RaiseWrapCount(ctx, p.masterKeyID[:], 1<<32-2, 1<<32); !ok || err != nil {
		t.Fatalf("raise the count to 2³² − 2 = %v, %v; want true, nil", ok, err)
	}

	copy := read(t, path)
	for i := 1; i <= 2; i++ {
		if _, err := p.VerifyEscrow(ctx, copy); err != nil {
			t.Fatalf("escrow check %d from 2³² − 2 = %v, want nil", i, err)
		}
	}
	if _, err := p.VerifyEscrow(ctx, copy); !errors.Is(err, keys.ErrWrapLimit) {
		t.Errorf("escrow check past 2³² = %v, want keys.ErrWrapLimit", err)
	}
}

// The gate opens only for a pass in Config.Escrowed naming the master key's
// ID and fingerprint: a fresh provider given the pass VerifyEscrow returned is
// open, one given no pass, or a pass for the ID with another fingerprint, is
// shut. A pass kept only in memory, or a key file replaced under its ID let
// through, fails here.
func TestEscrowGateFromConfig(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, _, path, id, _ := escrowProvider(t)
	pass, err := p.VerifyEscrow(context.Background(), read(t, path))
	if err != nil {
		t.Fatalf("VerifyEscrow: %v", err)
	}

	for _, tc := range []struct {
		name     string
		escrowed []Escrow
		open     bool
	}{
		{"the pass", []Escrow{pass}, true},
		{"no pass", nil, false},
		{"the ID with another fingerprint", []Escrow{{MasterKeyID: id, Fingerprint: [32]byte{1}}}, false},
	} {
		fresh, err := New(context.Background(), Config{KeyFile: path, Passphrase: passphrase, Escrowed: tc.escrowed}, Deps{Records: keystest.NewRecords(), Rand: rand.Reader})
		if err != nil {
			t.Fatalf("New with %s: %v", tc.name, err)
		}
		err = fresh.checkEscrow()
		if tc.open && err != nil {
			t.Errorf("gate of a fresh provider with %s = %v, want open", tc.name, err)
		}
		if !tc.open && !isNotEscrowed(fresh, err) {
			t.Errorf("gate of a fresh provider with %s = %v, want ErrNotEscrowed naming the key", tc.name, err)
		}
	}
}

// Reconfigure refuses a key file holding another master key, by ID or by
// bytes under the same ID, naming both IDs, and changes nothing: the provider
// still wraps and unwraps under its own key and its gate stays as it was. It
// takes a new passphrase and an added pass at once: the gate opens and the
// next escrow check opens its copy with the new passphrase. A master key
// swapped at run time, or a live change that needs a restart, fails here.
func TestReconfigure(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, _, path, id, key := escrowProvider(t)
	otherPath, otherID, _ := keyFile(t)
	ctx := context.Background()
	pass := Escrow{MasterKeyID: id, Fingerprint: fingerprint(key)}

	if err := p.Reconfigure(ctx, Config{KeyFile: path}); err == nil {
		t.Errorf("Reconfigure with no passphrase = nil, want its refusal")
	}
	for _, tc := range []struct{ name, path, id string }{
		{"another key file", otherPath, otherID},
		{"other bytes under the master key's ID", relabel(t, otherPath, id), id},
	} {
		err := p.Reconfigure(ctx, Config{KeyFile: tc.path, Passphrase: passphrase, Escrowed: []Escrow{pass}})
		if err == nil || !strings.Contains(err.Error(), hexID(t, tc.id)) || !strings.Contains(err.Error(), hexID(t, id)) {
			t.Errorf("Reconfigure with %s = %v, want a refusal naming %s and %s", tc.name, err, tc.id, id)
		}
		if err := p.checkEscrow(); !isNotEscrowed(p, err) {
			t.Errorf("gate after the refused Reconfigure with %s = %v, want still shut", tc.name, err)
		}
	}

	ns := entity.NewNamespaceID()
	aad := wrapAAD(ns[:], keys.KindDataKey, "k1")
	stored, err := p.wrap(ctx, sealIn{aad: aad, key: make([]byte, 32)})
	if err != nil {
		t.Fatalf("wrap after the refused Reconfigures: %v", err)
	}
	if _, err := p.unwrap(stored[0], aad); err != nil {
		t.Errorf("unwrap after the refused Reconfigures: %v", err)
	}

	const newPassphrase = "a new passphrase for the same key"
	newPath := repassphrase(t, path, newPassphrase)
	if err := p.Reconfigure(ctx, Config{KeyFile: newPath, Passphrase: newPassphrase, Escrowed: []Escrow{pass}}); err != nil {
		t.Fatalf("Reconfigure with a new passphrase and a pass = %v, want nil", err)
	}
	if err := p.checkEscrow(); err != nil {
		t.Errorf("gate after Reconfigure handed it the pass = %v, want open", err)
	}
	if _, err := p.VerifyEscrow(ctx, read(t, newPath)); err != nil {
		t.Errorf("VerifyEscrow of a copy under the new passphrase = %v, want nil", err)
	}
	if _, err := p.VerifyEscrow(ctx, read(t, path)); !isNotEscrowed(p, err) {
		t.Errorf("VerifyEscrow of a copy under the old passphrase = %v, want ErrNotEscrowed", err)
	}
}

// hexID is a UUID's text as the provider's errors print it.
func hexID(t *testing.T, id string) string {
	t.Helper()

	b, err := parseMasterKeyID(id)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}

// repassphrase copies the key file at path with its master key re-wrapped
// under passphrase, keeping its ID, salt and cost.
func repassphrase(t *testing.T, path, newPassphrase string) string {
	t.Helper()

	raw := read(t, path)
	_, key, err := keyfile.Open(raw, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	var body struct {
		KDF struct {
			Salt      string `json:"salt"`
			Time      uint32 `json:"time"`
			MemoryKiB uint32 `json:"memory_kib"`
			Threads   uint8  `json:"threads"`
		} `json:"kdf"`
	}
	if err := json.Unmarshal(block.Bytes, &body); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(block.Bytes, &fields); err != nil {
		t.Fatal(err)
	}
	salt, err := base64.StdEncoding.DecodeString(body.KDF.Salt)
	if err != nil {
		t.Fatal(err)
	}
	c, err := aes.NewCipher(argon2.IDKey([]byte(newPassphrase), salt, body.KDF.Time, body.KDF.MemoryKiB, body.KDF.Threads, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(c)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	fields["nonce"] = base64.StdEncoding.EncodeToString(nonce)
	fields["wrapped_master_key"] = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, key, nil))
	if block.Bytes, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "repassphrased.key")
	if err := os.WriteFile(out, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}
