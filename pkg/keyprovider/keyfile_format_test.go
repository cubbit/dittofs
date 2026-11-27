package keyprovider

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/internal/keyfile"
)

// The key file's format as these tests read and rewrite it, written here
// independently of internal/keyfile: a field this copy names differently,
// or a bound it records differently, fails the tests that mutate and reload
// a file through keyfile.Open.

const localPEMType = "DITTOFS ENCRYPTED MASTER KEY"

// The lowest Argon2 cost a key file may record, which a weak file records.
const (
	minArgonTime      = 1
	minArgonMemoryKiB = 8 * 1024
)

type argon2Params struct {
	Algo      string `json:"algo"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

type localKeyFile struct {
	Version          int          `json:"version"`
	MasterKeyID      string       `json:"master_key_id"`
	KDF              argon2Params `json:"kdf"`
	Wrap             string       `json:"wrap"`
	Nonce            string       `json:"nonce"`
	WrappedMasterKey string       `json:"wrapped_master_key"`
}

// decodeKeyFile reads raw's PEM block and JSON body, checking nothing else.
func decodeKeyFile(raw []byte) (*localKeyFile, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != localPEMType {
		return nil, fmt.Errorf("missing %q PEM block", localPEMType)
	}
	var kf localKeyFile
	if err := json.Unmarshal(block.Bytes, &kf); err != nil {
		return nil, err
	}
	return &kf, nil
}

// unwrapMasterKey re-encodes kf and opens it as the provider does.
func unwrapMasterKey(kf *localKeyFile, passphrase string) ([]byte, error) {
	body, err := json.Marshal(kf)
	if err != nil {
		return nil, err
	}
	_, key, err := keyfile.Open(pem.EncodeToMemory(&pem.Block{Type: localPEMType, Bytes: body}), passphrase)
	return key, keyFileError(err)
}

// A file internal/keyfile generates loads here as the master key it wraps:
// the provider reports its ID, and a block key it wraps opens under the key
// keyfile.Open returns.
func TestAFileKeyfileGeneratesLoadsHere(t *testing.T) {
	raw, err := keyfile.Generate(testPassphrase, true)
	if err != nil {
		t.Fatalf("keyfile.Generate: %v", err)
	}
	id, masterKey, err := keyfile.Open(raw, testPassphrase)
	if err != nil {
		t.Fatalf("keyfile.Open: %v", err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewProviderWithPassphrase(context.Background(), Config{Kind: KindLocal, File: path}, testPassphrase)
	if err != nil {
		t.Fatalf("NewProviderWithPassphrase: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got := p.CurrentMasterKeyID(); got != id {
		t.Errorf("CurrentMasterKeyID: got %q, want the file's %q", got, id)
	}
	blockKey := bytes.Repeat([]byte{0x5a}, 32)
	wrapped, _, err := p.Wrap(context.Background(), blockKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, wrapped[:n], wrapped[n:], nil)
	if err != nil || !bytes.Equal(plain, blockKey) {
		t.Errorf("a block key wrapped by the provider, opened under keyfile.Open's key: got %x, %v; want %x", plain, err, blockKey)
	}
}
