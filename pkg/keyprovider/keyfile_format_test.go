package keyprovider

import (
	"encoding/json"
	"encoding/pem"
	"errors"
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

// The lowest Argon2 cost a key file may record, which a weak file records,
// copied rather than read from internal/keyfile so a floor moved there fails.
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

// decodeKeyFile reads raw's PEM block and JSON body; unlike the format's
// decoder it validates nothing, so a test can read any field back.
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

// A file internal/keyfile generates loads here under the ID this package's
// own copy of the format reads from it, as a master key a block key wraps
// under and unwraps from.
func TestAFileKeyfileGeneratesLoadsHere(t *testing.T) {
	raw, err := keyfile.Generate(testPassphrase, true)
	if err != nil {
		t.Fatalf("keyfile.Generate: %v", err)
	}
	kf, err := decodeKeyFile(raw)
	if err != nil {
		t.Fatalf("decodeKeyFile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gotID, gotKey, err := loadLocalKeyFile(path, testPassphrase)
	if err != nil {
		t.Fatalf("loadLocalKeyFile: %v", err)
	}
	if gotID != kf.MasterKeyID || len(gotKey) != 32 {
		t.Errorf("loadLocalKeyFile: got ID %q and %d key bytes, want %q and 32", gotID, len(gotKey), kf.MasterKeyID)
	}
}

// A key-file error reaches this package's callers as this package's sentinel
// alone, with the message it always had.
func TestAKeyFileErrorKeepsThisPackagesSentinelAndMessage(t *testing.T) {
	kf := loadKeyFile(t, testPassphrase)
	kf.Version = 99
	_, err := newLocalProvider(Config{Kind: KindLocal, File: writeMutatedKeyFile(t, kf)}, testPassphrase)
	if want := "keyprovider: key file corrupt: unsupported version 99"; err == nil || err.Error() != want {
		t.Errorf("an unsupported version: got %v, want %q", err, want)
	}
	if errors.Is(err, keyfile.ErrCorrupt) {
		t.Errorf("an unsupported version: %v matches keyfile.ErrCorrupt, want only ErrKeyFileCorrupt", err)
	}
}
